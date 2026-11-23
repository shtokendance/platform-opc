package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"weknora-tokendance/pkg/model"
)

const (
	deploymentBuildSlow    = 10 * time.Minute
	deploymentBuildLimit   = 30 * time.Minute
	deploymentRecoverySlow = 5 * time.Minute
)

type deploymentAttemptProgress struct {
	Phase          string `json:"phase"`
	PhaseStartedAt int64  `json:"phase_started_at"`
	ActiveVersion  int    `json:"active_version,omitempty"`
	Stalled        bool   `json:"stalled,omitempty"`
	CanCancel      bool   `json:"can_cancel"`
	RetrySafe      bool   `json:"retry_safe"`
	FailureKind    string `json:"failure_kind,omitempty"`
}

func deploymentPhaseStarted(attempt model.AppDeploymentVersion) time.Time {
	if attempt.DeliveryPhaseStartedAt != nil {
		return *attempt.DeliveryPhaseStartedAt
	}
	return attempt.CreatedAt
}

func canCancelDelivery(attempt model.AppDeploymentVersion, now time.Time) bool {
	if attempt.Version != 0 || attempt.DeliveryRevision == uuid.Nil ||
		attempt.Status == model.AppStatusFailed || attempt.Status == model.AppStatusCancelled {
		return false
	}
	switch attempt.DeliveryStep {
	case deliveryBuildQueued, deliveryDomainBuildQueued:
		return attempt.DeliveryBuildState != "unknown"
	case "build_complete", deliveryPreviewQueued, deliveryPreviewRunning, deliveryCutoverQueued:
		var rehearsal postgresRehearsalStatus
		if json.Unmarshal([]byte(attempt.MigrationRehearsal), &rehearsal) == nil && rehearsal.Status == "running" &&
			now.Sub(rehearsal.StartedAt) <= deliveryRehearsalTimeout+time.Minute {
			return false
		}
		return attempt.DeliveryLeaseUntil == nil || !attempt.DeliveryLeaseUntil.After(now)
	default:
		return false
	}
}

func deliveryProgress(attempt model.AppDeploymentVersion, now time.Time) deploymentAttemptProgress {
	started := deploymentPhaseStarted(attempt)
	progress := deploymentAttemptProgress{Phase: "unknown", PhaseStartedAt: started.UnixMilli(),
		CanCancel: canCancelDelivery(attempt, now), FailureKind: attempt.DeliveryFailureKind}
	slowAfter := deploymentRecoverySlow
	switch {
	case attempt.Status == model.AppStatusRunning && attempt.Version > 0:
		progress.Phase = "succeeded"
	case attempt.DeliveryStep == deliveryCancelRequested:
		progress.Phase = "cancelling"
	case attempt.DeliveryStep == deliveryCleanup:
		progress.Phase = "cleaning"
	case attempt.DeliveryStep == deliveryRestorePrevious || attempt.DeliveryStep == deliveryRestoringCleanup:
		progress.Phase = "restoring"
	case attempt.DeliveryStep == model.DeliveryRecoveryRequiredStep:
		progress.Phase = "recovery_required"
	case attempt.DeliveryStep == deliveryFenceNew:
		progress.Phase = "restoring"
	case attempt.DeliveryBuildState == "unknown" && (attempt.DeliveryStep == deliverySwitchRequested ||
		attempt.DeliveryStep == deliveryCheckingDomain || attempt.DeliveryStep == deliveryRuntimeChecked):
		progress.Phase = "unknown"
	case attempt.Status == model.AppStatusCancelled:
		progress.Phase, progress.RetrySafe = "cancelled", true
	case attempt.Status == model.AppStatusFailed:
		progress.Phase = "failed"
		progress.RetrySafe = attempt.DeliveryStep == "" && attempt.DeliveryFailureKind != ""
		if attempt.DeliveryStep == "restored_before_switch" {
			progress.Phase, progress.RetrySafe = "restored", true
		}
	default:
		switch attempt.DeliveryStep {
		case deliveryBuildQueued, deliveryDomainBuildQueued:
			slowAfter = deploymentBuildSlow
			switch attempt.DeliveryBuildState {
			case "building", "unknown":
				progress.Phase = attempt.DeliveryBuildState
			default:
				progress.Phase = "queued"
			}
		case "build_complete", deliveryCutoverQueued, deliveryConfigStaged:
			progress.Phase = "preparing"
		case deliveryWriteHold, deliveryCopyStarted, deliveryDataCopied:
			progress.Phase = "migrating"
		case deliveryPreviewQueued, deliveryPreviewRunning, deliverySwitchRequested:
			progress.Phase = "starting"
			slowAfter = 45 * time.Second
		case deliveryCheckingDomain:
			progress.Phase = "checking_domain"
			slowAfter = 45 * time.Second
		case deliveryRuntimeChecked:
			progress.Phase = "confirming"
		}
	}
	switch progress.Phase {
	case "succeeded", "cancelled", "restored", "failed", "unknown", "recovery_required":
	default:
		progress.Stalled = !started.IsZero() && now.Sub(started) >= slowAfter
	}
	return progress
}

func (h *Handler) CancelAppPublishRequest(c *gin.Context) {
	db := h.storage.DeployDB().WithContext(c.Request.Context())
	attempt, ok := h.findDeliveryAttempt(db, c)
	if !ok {
		return
	}
	if attempt.Status == model.AppStatusCancelled {
		c.JSON(http.StatusOK, appPublishReceipt(attempt))
		return
	}
	if attempt.DeliveryStep == deliveryCancelRequested {
		c.JSON(http.StatusAccepted, appPublishReceipt(attempt))
		return
	}
	now := time.Now().UTC()
	if h.caproverClient == nil || !canCancelDelivery(attempt, now) {
		writeErrorEnvelope(c, http.StatusConflict, "APP_CANCEL_UNAVAILABLE", "当前阶段暂时无法取消，请重新检查部署进度")
		return
	}
	row := db.Model(&model.AppDeploymentVersion{}).Where("id = ? AND version = 0 AND delivery_step = ? AND status = ? AND delivery_build_state <> ?",
		attempt.ID, attempt.DeliveryStep, attempt.Status, "unknown")
	if attempt.DeliveryStep != deliveryBuildQueued && attempt.DeliveryStep != deliveryDomainBuildQueued {
		row = row.Where("(delivery_lease_until IS NULL OR delivery_lease_until <= ?) AND migration_rehearsal = ?", now, attempt.MigrationRehearsal)
	}
	updated := row.Updates(map[string]any{
		"delivery_step": deliveryCancelRequested, "delivery_phase_started_at": now,
	})
	if updated.Error != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "APP_CANCEL_UNAVAILABLE", "暂时无法提交取消请求，请重新检查部署进度")
		return
	}
	if updated.RowsAffected != 1 {
		writeErrorEnvelope(c, http.StatusConflict, "APP_CANCEL_UNAVAILABLE", "部署阶段已变化，请重新检查")
		return
	}
	attempt.DeliveryStep, attempt.DeliveryPhaseStartedAt = deliveryCancelRequested, &now
	c.JSON(http.StatusAccepted, appPublishReceipt(attempt))
}
