package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	deliveryBuildQueued     = "build_queued"
	deliveryPreviewQueued   = "preview_queued"
	deliveryPreviewRunning  = "preview_running"
	deliveryCutoverQueued   = "cutover_queued"
	deliveryWriteHold       = "write_hold"
	deliveryCopyStarted     = "copy_started"
	deliveryDataCopied      = "data_copied"
	deliveryConfigStaged    = "config_staged"
	deliverySwitchRequested = "switch_requested"
	deliveryRuntimeChecked  = "runtime_checked"
	deliveryRestorePrevious = "restore_previous"
	deliveryFenceNew        = "fence_candidate"
	deliveryCleanup         = "cleanup_preview"
)

func (h *Handler) buildArchivedDelivery(ctx context.Context, attempt *model.AppDeploymentVersion, _ []byte) {
	h.executeAppDelivery(ctx, *attempt)
}

type deliveryAssessment struct {
	Phase    string   `json:"phase"`
	Outcome  string   `json:"outcome"`
	Report   string   `json:"report"`
	Evidence []string `json:"evidence"`
}

func (h *Handler) activeDeliveryAttempt(ctx context.Context, slug string) (bool, error) {
	var count int64
	err := h.storage.DeployDB().WithContext(ctx).Model(&model.AppDeploymentVersion{}).
		Where("app_slug = ? AND version = 0 AND delivery_revision <> ? AND status IN ?", slug, uuid.Nil,
			[]string{model.AppStatusBuilding, model.AppStatusBuilt, model.AppStatusActivating, model.AppStatusRuntimeReady, model.AppStatusPromoting}).
		Count(&count).Error
	return count > 0, err
}

func (h *Handler) allowAppChange(c *gin.Context, slug string) bool {
	pending, err := h.activeDeliveryAttempt(c.Request.Context(), slug)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery state could not be read"})
		return false
	}
	if pending {
		c.JSON(http.StatusConflict, gin.H{"error": "complete the pending delivery assessment before modifying this application"})
		return false
	}
	return true
}

func (h *Handler) findDeliveryAttempt(db *gorm.DB, c *gin.Context) (model.AppDeploymentVersion, bool) {
	var attempt model.AppDeploymentVersion
	err := db.Where("app_slug = ? AND request_id = ? AND created_by = ?", c.Param("slug"), c.Param("request_id"), c.GetString(appOwnerKey)).First(&attempt).Error
	if err != nil || attempt.DeliveryRevision == uuid.Nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "delivery attempt not found"})
		return attempt, false
	}
	return attempt, true
}

func (h *Handler) AssessAppDelivery(c *gin.Context) {
	ctx := c.Request.Context()
	var input deliveryAssessment
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	if err := c.ShouldBindJSON(&input); err != nil ||
		(input.Phase != "build" && input.Phase != "runtime") ||
		(input.Outcome != "passed" && input.Outcome != "failed") ||
		strings.TrimSpace(input.Report) == "" || len(input.Report) > 8<<10 ||
		len(input.Evidence) == 0 || len(input.Evidence) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "assessment requires a phase, outcome, report and observed evidence"})
		return
	}
	if !h.tryLockDeploySlug(c.Param("slug")) {
		c.JSON(http.StatusConflict, gin.H{"error": "application deployment is busy"})
		return
	}
	defer h.unlockDeploySlug(c.Param("slug"))
	db := h.storage.DeployDB().WithContext(ctx)
	attempt, ok := h.findDeliveryAttempt(db, c)
	if !ok {
		return
	}
	if attempt.Status == model.AppStatusRunning || attempt.Status == model.AppStatusFailed || attempt.Status == model.AppStatusPromoting ||
		(input.Phase == "build" && (attempt.Status == model.AppStatusActivating || attempt.Status == model.AppStatusRuntimeReady)) {
		c.JSON(http.StatusOK, appPublishReceipt(attempt))
		return
	}
	if (input.Phase == "build" && attempt.Status != model.AppStatusBuilt) ||
		(input.Phase == "runtime" && attempt.Status != model.AppStatusRuntimeReady) {
		c.JSON(http.StatusConflict, gin.H{"error": "delivery phase changed"})
		return
	}
	reports := make(map[string]deliveryAssessment)
	if attempt.DeliveryAssessment != "" && json.Unmarshal([]byte(attempt.DeliveryAssessment), &reports) != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "stored assessment is invalid"})
		return
	}
	reports[input.Phase] = input
	reportsJSON, _ := json.Marshal(reports)
	step, status, message := deliveryCleanup, model.AppStatusFailed, input.Report
	if input.Outcome == "passed" {
		if attempt.DeliveryStep == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "this attempt predates isolated delivery. Confirm its actual running image before starting a new delivery."})
			return
		}
		plan, err := h.readAttemptDelivery(ctx, attempt)
		if err != nil || !assessmentHasEvidence(attempt, plan, input, h.cfg.Auth.Key) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "passed assessment requires evidence for the current delivery and phase"})
			return
		}
		step, status, message = deliveryCutoverQueued, model.AppStatusPromoting, ""
		if input.Phase == "build" {
			data, err := readDeliveryDataPlan(plan)
			if err != nil || (data.Strategy == "postgres_copy" && !completedMigrationRehearsal(attempt.MigrationRehearsal)) {
				c.JSON(http.StatusConflict, gin.H{"error": "an archived data plan and completed rehearsal are required before starting the isolated candidate"})
				return
			}
			step, status = deliveryPreviewQueued, model.AppStatusActivating
		}
	}
	result := db.Model(&model.AppDeploymentVersion{}).Where("id = ? AND status = ?", attempt.ID, attempt.Status).Updates(map[string]any{
		"status": status, "delivery_step": step, "delivery_assessment": string(reportsJSON), "error_message": message,
	})
	if result.Error != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assessment could not be saved"})
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "delivery phase changed"})
		return
	}
	attempt.Status, attempt.ErrorMessage, attempt.DeliveryStep = status, message, step
	attempt.DeliveryAssessment = string(reportsJSON)
	go h.executeAppDelivery(context.Background(), attempt)
	c.JSON(http.StatusAccepted, appPublishReceipt(attempt))
}

func assessmentHasEvidence(attempt model.AppDeploymentVersion, plan []byte, input deliveryAssessment, signingKey string) bool {
	archived, err := decodeDeliveryProbeContract(plan)
	if err != nil {
		return false
	}
	for _, entry := range input.Evidence {
		var evidence struct {
			RequestID string `json:"request_id"`
			Kind      string `json:"kind"`
			Target    string `json:"target"`
			Result    struct {
				Status json.RawMessage `json:"status"`
				Proof  string          `json:"proof"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(entry), &evidence) != nil || evidence.RequestID != attempt.RequestID {
			continue
		}
		if input.Phase == "build" && evidence.Kind == "state" && string(evidence.Result.Status) == `"built"` {
			return true
		}
		var status int
		if input.Phase != "runtime" || json.Unmarshal(evidence.Result.Status, &status) != nil || status < 100 || status > 599 {
			continue
		}
		for _, target := range archived.Contract.Verification.Targets {
			if target.ID == evidence.Target && target.Kind == evidence.Kind && (target.Kind == "http" || target.Kind == "operation") {
				proof := deliveryProbeProof(signingKey, attempt, evidence.Kind, evidence.Target, status)
				if proof != "" && hmac.Equal([]byte(proof), []byte(evidence.Result.Proof)) {
					return true
				}
			}
		}
	}
	return false
}

func (h *Handler) ProbeAppDelivery(c *gin.Context) {
	ctx := c.Request.Context()
	attempt, ok := h.findDeliveryAttempt(h.storage.DeployDB().WithContext(ctx), c)
	if !ok {
		return
	}
	if attempt.Status != model.AppStatusRuntimeReady || attempt.DeliveryStep != deliveryPreviewRunning || h.caproverClient == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "isolated delivery candidate is not awaiting runtime assessment"})
		return
	}
	var input struct {
		deploy.CandidateProbe
		Target string `json:"target"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	probe := &input.CandidateProbe
	if err := c.ShouldBindJSON(&input); err != nil ||
		!strings.HasPrefix(probe.Path, "/") || strings.HasPrefix(probe.Path, "//") || len(probe.Path) > 4096 ||
		strings.ContainsRune(probe.Path, '\\') || strings.ContainsFunc(probe.Path, unicode.IsControl) || len(probe.Body) > 64<<10 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid isolated candidate request"})
		return
	}
	kind := ""
	if input.Target != "" {
		data, err := h.readAttemptDelivery(ctx, attempt)
		contract, parseErr := decodeDeliveryProbeContract(data)
		if err != nil || parseErr != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approved verification targets are unavailable"})
			return
		}
		var allowed bool
		kind, allowed = contract.probeKind(input.Target, *probe)
		if !allowed {
			c.JSON(http.StatusBadRequest, gin.H{"error": "request differs from the approved verification target"})
			return
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := h.caproverClient.ProbeCandidate(probeCtx, attempt.AppSlug, attempt.ID.String(), attempt.ContainerPort, *probe)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "isolated candidate did not respond. Check its startup logs and temporary resource receipt."})
		return
	}
	c.Header("Cache-Control", "no-store")
	response := gin.H{"status": result.Status, "headers": result.Headers, "body": result.Body}
	if kind != "" {
		response["proof"] = deliveryProbeProof(h.cfg.Auth.Key, attempt, kind, input.Target, result.Status)
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) AppDeliveryCandidateLogs(c *gin.Context) {
	ctx := c.Request.Context()
	attempt, ok := h.findDeliveryAttempt(h.storage.DeployDB().WithContext(ctx), c)
	if !ok {
		return
	}
	if attempt.Status != model.AppStatusRuntimeReady || attempt.DeliveryStep != deliveryPreviewRunning || h.caproverClient == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "isolated delivery candidate is not awaiting runtime assessment"})
		return
	}
	logsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	logs, err := h.caproverClient.CandidateLogs(logsCtx, attempt.AppSlug, attempt.ID.String())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "isolated candidate logs are unavailable"})
		return
	}
	var configs []model.AppConfigItem
	if json.Unmarshal([]byte(attempt.AppConfigJSON), &configs) != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approved configuration could not be read"})
		return
	}
	configs, err = h.openAppConfig(configs)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approved configuration could not be read"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"logs": h.redactCandidateLogs(logs, attempt, configs)})
}

func (h *Handler) readAttemptDelivery(ctx context.Context, attempt model.AppDeploymentVersion) ([]byte, error) {
	store := h.resolveArtifactStore(ctx)
	if store == nil {
		return nil, fmt.Errorf("delivery artifact storage is unavailable")
	}
	key := fmt.Sprintf("apps/%s/attempts/%s/delivery.json", attempt.AppSlug, attempt.ID)
	plan, _, err := store.GetObject(ctx, key)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(plan)) != attempt.DeliveryPlanSHA256 {
		return nil, fmt.Errorf("archived delivery plan is unavailable or differs from the confirmed artifacts")
	}
	return plan, nil
}
