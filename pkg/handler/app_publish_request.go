package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	if c.Query("live_logs") == "1" && version.DeliveryRevision != uuid.Nil {
		if owner := c.GetString(appOwnerKey); !canManage || (owner != "" && owner != version.CreatedBy) {
			c.JSON(http.StatusNotFound, gin.H{"error": "delivery attempt not found"})
			return
		}
		c.Header("Cache-Control", "no-store")
		h.addDeliveryLogs(c.Request.Context(), version, receipt)
	}
	c.JSON(http.StatusOK, receipt)
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
	case deliveryBuildQueued:
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
		receipt["runtime_logs"] = redactAppRuntimeLogs(logs, configs)
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
