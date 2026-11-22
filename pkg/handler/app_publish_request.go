package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// A missing receipt does not prove that an upload was never received.
func (h *Handler) GetAppPublishRequest(c *gin.Context) {
	slug := c.Param("slug")
	reqID := c.Param("request_id")
	var version model.AppDeploymentVersion
	err := h.storage.DeployDB().Where("app_slug = ? AND request_id = ?", slug, reqID).First(&version).Error
	if err != nil {
		c.JSON(http.StatusAccepted, gin.H{"status": "unknown", "retry_safe": false, "request_id": reqID})
		return
	}
	canManage := true
	if owner := c.GetString(appOwnerKey); owner != "" {
		if app, appErr := h.storage.GetAppBySlug(c.Request.Context(), slug); appErr == nil && app != nil {
			canManage = owner == app.CreatedBy || app.CreatedBy == ""
		}
	}
	receipt := appPublishReceipt(version)
	if !canManage {
		delete(receipt, "build_log")
		delete(receipt, "error_message")
	}
	liveLogs := c.Query("live_logs") == "1" && version.DeliveryRevision != uuid.Nil
	checkStartup := c.Query("startup_check") == "1"
	if liveLogs || checkStartup {
		if owner := c.GetString(appOwnerKey); !canManage || (owner != "" && owner != version.CreatedBy) {
			c.JSON(http.StatusNotFound, gin.H{"error": "delivery attempt not found"})
			return
		}
		c.Header("Cache-Control", "no-store")
	}
	if liveLogs {
		h.addDeliveryLogs(c.Request.Context(), version, receipt)
	}
	if checkStartup {
		check, err := h.checkDeliveryStartup(c.Request.Context(), version)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		receipt["startup_check"] = check
	}
	c.JSON(http.StatusOK, receipt)
}

func (h *Handler) checkDeliveryStartup(ctx context.Context, attempt model.AppDeploymentVersion) (gin.H, error) {
	client := h.caproverClient
	if client == nil {
		return nil, fmt.Errorf("部署平台暂不可用")
	}
	app, err := h.storage.GetAppBySlug(ctx, attempt.AppSlug)
	if err != nil {
		return nil, fmt.Errorf("无法读取应用访问地址")
	}
	active, err := h.storage.GetActiveAppVersion(ctx, attempt.AppSlug)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("无法读取当前生效版本")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	reachable, _, err := client.ProbeAppEndpoint(probeCtx, app.AppURL)
	if err != nil {
		return nil, fmt.Errorf("无法探测应用域名")
	}
	targetRunning := false
	if attempt.DeployedImageName != "" {
		targetRunning, _ = client.CheckDeployedRuntime(probeCtx, attempt.AppSlug, attempt.DeployedImageName)
	}
	activeVersion, activeRunning := 0, false
	if active != nil {
		activeVersion = active.Version
		if active.DeployedImageName != "" {
			if active.DeployedImageName == attempt.DeployedImageName {
				activeRunning = targetRunning
			} else {
				activeRunning, _ = client.CheckDeployedRuntime(probeCtx, attempt.AppSlug, active.DeployedImageName)
			}
		}
	}
	return gin.H{"target_running": targetRunning, "domain_reachable": reachable, "active_version": activeVersion,
		"active_version_running": activeRunning}, nil
}

func (h *Handler) addDeliveryLogs(ctx context.Context, attempt model.AppDeploymentVersion, receipt gin.H) {
	delete(receipt, "build_log")
	delete(receipt, "error_message")
	var configs []model.AppConfigItem
	if err := json.Unmarshal([]byte(attempt.AppConfigJSON), &configs); err != nil {
		receipt["logs_unavailable"] = true
		return
	}
	configs, err := h.openAppConfig(configs)
	if err != nil {
		receipt["logs_unavailable"] = true
		return
	}
	receipt["build_log"] = redactAppRuntimeLogs(attempt.BuildLog, configs)
	receipt["error_message"] = redactAppRuntimeLogs(attempt.ErrorMessage, configs)
	if h.caproverClient == nil {
		return
	}
	logsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	switch attempt.DeliveryStep {
	case deliveryBuildQueued, deliveryDomainBuildQueued:
		build, err := h.caproverClient.GetCandidateBuild(logsCtx, attempt.AppSlug, attempt.ID.String())
		if err != nil {
			receipt["logs_unavailable"] = true
			return
		}
		if build.Log != "" {
			receipt["build_log"] = redactAppRuntimeLogs(build.Log, configs)
		}
	case deliveryPreviewRunning:
		if attempt.Status != model.AppStatusRuntimeReady {
			return
		}
		logs, err := h.caproverClient.CandidateLogs(logsCtx, attempt.AppSlug, attempt.ID.String())
		if err != nil {
			receipt["logs_unavailable"] = true
			return
		}
		receipt["runtime_logs"] = h.redactCandidateLogs(logs, attempt, configs)
	}
}

func appPublishReceipt(version model.AppDeploymentVersion) gin.H {
	return gin.H{
		"status": version.Status, "version": version.ReceiptVersion(), "published_version": version.Version,
		"attempt_id": version.ID, "request_id": version.RequestID, "is_active": version.IsActive,
		"app_tar_sha256": version.AppTarSHA256, "skill_zip_sha256": version.SkillZipSHA256,
		"delivery_plan_sha256": version.DeliveryPlanSHA256,
		"error_message":        version.ErrorMessage, "build_log": version.BuildLog, "retry_safe": false,
		"created_at":          version.CreatedAt,
		"image":               version.DeployedImageName,
		"delivery_step":       version.DeliveryStep,
		"delivery_assessment": version.DeliveryAssessment,
		"migration_rehearsal": version.MigrationRehearsal,
		"migration_execution": version.MigrationExecution,
	}
}

func configFingerprint(app model.DeployedApp, effective []model.AppConfigItem) string {
	value, _ := json.Marshal([]any{app.ContainerPort, app.AppConfigJSON, app.ResourceIDsJSON, app.StorageMountsJSON, effective})
	return fmt.Sprintf("%x", sha256.Sum256(value))
}
