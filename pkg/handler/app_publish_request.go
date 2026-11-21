package handler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
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
	c.JSON(http.StatusOK, receipt)
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
		"delivery_assessment": version.DeliveryAssessment,
		"migration_rehearsal": version.MigrationRehearsal,
		"migration_execution": version.MigrationExecution,
	}
}

func configFingerprint(app model.DeployedApp, effective []model.AppConfigItem) string {
	value, _ := json.Marshal([]any{app.ContainerPort, app.AppConfigJSON, app.ResourceIDsJSON, app.StorageMountsJSON, effective})
	return fmt.Sprintf("%x", sha256.Sum256(value))
}
