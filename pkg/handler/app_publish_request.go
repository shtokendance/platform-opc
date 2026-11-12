package handler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"weknora-tokendance/pkg/model"
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
			canManage = owner == app.CreatedBy
		}
	}
	buildLog := version.BuildLog
	errMsg := version.ErrorMessage
	if !canManage {
		buildLog = ""
		errMsg = ""
	}
	c.JSON(200, gin.H{
		"status":           version.Status,
		"version":          version.Version,
		"request_id":       version.RequestID,
		"is_active":        version.IsActive,
		"app_tar_sha256":   version.AppTarSHA256,
		"skill_zip_sha256": version.SkillZipSHA256,
		"error_message":    errMsg,
		"build_log":        buildLog,
		"retry_safe":       false,
	})
}
func configFingerprint(app model.DeployedApp, effective []model.AppConfigItem) string {
	value, _ := json.Marshal([]any{app.ContainerPort, app.AppConfigJSON, app.ResourceIDsJSON, app.StorageMountsJSON, effective})
	return fmt.Sprintf("%x", sha256.Sum256(value))
}
