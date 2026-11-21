package handler

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
)

// PreviewAppConfigValues resolves the exact fields selected for this application.
func (h *Handler) PreviewAppConfigValues(c *gin.Context) {
	slug := strings.TrimSpace(c.Param("slug"))
	if !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid application slug"})
		return
	}
	app, err := h.storage.GetAppBySlug(c.Request.Context(), slug)
	owner := c.GetString(appOwnerKey)
	if owner == "" || err == nil && app.CreatedBy != owner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the application owner can read deployment values"})
		return
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not verify application ownership"})
		return
	}
	var body struct {
		Bindings []model.AppConfigItem `json:"bindings"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Bindings) > 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid configuration bindings"})
		return
	}
	c.Header("Cache-Control", "no-store")
	resources := h.loadEffectiveOnlineResources(c.Request.Context())
	values := make(map[string]string)
	for _, binding := range body.Bindings {
		if !appEnvName.MatchString(binding.Key) || !appEnvName.MatchString(resourceEnvKey(binding)) ||
			!h.isEnterpriseResourceAllowed(slug, binding.ResourceID) || binding.ResourceID == "shared-oss-minio" || binding.ResourceID == "minio-knowledge" {
			c.JSON(http.StatusForbidden, gin.H{"error": "configuration binding is not available for this application"})
			return
		}
		i := slices.IndexFunc(resources, func(r config.OnlineResourceConfig) bool { return r.ID == binding.ResourceID })
		if i < 0 || resources[i].Type == objectStorageResourceType || isVolumeResource(resources[i].Type) {
			continue
		}
		if value, ok := resources[i].EnvVars[resourceEnvKey(binding)]; ok {
			values[binding.Key] = value
		}
	}
	c.JSON(http.StatusOK, gin.H{"values": values})
}

func (h *Handler) appConfigValues(resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem) ([]model.AppConfigItem, error) {
	opened, err := h.openAppConfig(configs)
	if err != nil {
		return nil, err
	}
	out := describeAppConfig(resources, ids, opened)
	for i := range out {
		item := &out[i]
		if item.ResourceID == "" {
			continue
		}
		index := slices.IndexFunc(resources, func(r config.OnlineResourceConfig) bool {
			return r.ID == item.ResourceID && r.Type != objectStorageResourceType && r.ID != "shared-oss-minio" && r.ID != "minio-knowledge"
		})
		if index >= 0 {
			item.EffectiveValue = resources[index].EnvVars[resourceEnvKey(*item)]
		}
	}
	return out, nil
}
