package handler

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const appOwnerKey = "opc.app.owner"
const managedAppPrefix = "opc:"

func enterpriseAppOwner(organization, subject string) string {
	return fmt.Sprintf("%s%x", managedAppPrefix, sha256.Sum256([]byte(organization+"\n"+subject)))
}

func (h *Handler) EnterpriseAppsAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		session, ok := resolveAccessSession(h.cfg, h.storage, c)
		if !ok {
			c.Abort()
			return
		}
		if (session.claims["aud"] != audienceWork && session.claims["aud"] != audienceCodeBackend) || session.link.Status != "active" {
			writeErrorEnvelope(c, http.StatusForbidden, "APP_CLIENT_FORBIDDEN", "需要有效的企微登录")
			c.Abort()
			return
		}
		c.Set(appOwnerKey, enterpriseAppOwner(session.link.OrganizationID, session.link.ImmutableSubject))
		c.Next()
	}
}

func (h *Handler) requireAppOwner(c *gin.Context) {
	slug := strings.ToLower(strings.TrimSpace(c.Param("slug")))
	if !h.checkAppOwner(c, slug, false) {
		c.Abort()
	}
}

// Publish calls this inside the per-slug lock, including for an existing application.
func (h *Handler) checkAppOwner(c *gin.Context, slug string, creating bool) bool {
	app, err := h.storage.GetAppBySlug(c.Request.Context(), slug)
	if errors.Is(err, gorm.ErrRecordNotFound) && creating {
		return true
	}
	if err != nil {
		writeErrorEnvelope(c, 404, "APP_NOT_FOUND", "应用不存在")
		return false
	}
	owner := c.GetString(appOwnerKey)
	// 历史无主应用默认归属当前 work-opc 登录用户
	if app.CreatedBy == "" && owner != "" {
		app.CreatedBy = owner
		_ = h.storage.UpdateAppMetadata(c.Request.Context(), slug, map[string]interface{}{
			"created_by": owner,
		})
	}
	if strings.HasPrefix(app.CreatedBy, managedAppPrefix) && app.CreatedBy != owner {
		writeErrorEnvelope(c, 403, "APP_FORBIDDEN", "只有创建人或管理员可以管理此应用")
		return false
	}
	return true
}

// Both desktop route prefixes enforce the same access rules.
func (h *Handler) RegisterEnterpriseAppRoutes(apps *gin.RouterGroup) {
	apps.Use(h.EnterpriseAppsAuth())
	apps.GET("", h.ListApps)
	apps.POST("/check-slug", h.CheckSlug)
	apps.GET("/resources", h.ListResources)
	apps.POST("/publish", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<20)
		defer func() {
			if c.Request.MultipartForm != nil {
				_ = c.Request.MultipartForm.RemoveAll()
			}
		}()
		h.PublishApp(c)
	})
	apps.GET("/:slug", h.GetApp)
	apps.GET("/:slug/requests/:request_id", h.requireAppOwner, h.GetAppPublishRequest)
	apps.GET("/:slug/logs", h.requireAppOwner, h.GetAppLogs)
	apps.GET("/:slug/versions", h.requireAppOwner, h.ListAppVersions)
	apps.GET("/:slug/versions/:version/artifacts/:filename", h.requireAppOwner, h.DownloadAppVersionArtifact)
	apps.GET("/:slug/storage", h.requireAppOwner, h.GetAppStorageDetails)
	apps.GET("/:slug/storage/backup", h.requireAppOwner, h.DownloadAppStorageBackup)
	apps.POST("/:slug/recheck", h.requireAppOwner, h.RecheckAppHealth)
	apps.POST("/:slug/verify", h.requireAppOwner, h.RecheckAppHealth)
	apps.POST("/:slug/rollback", h.requireAppOwner, h.RollbackAppVersion)
	apps.POST("/:slug/migrate-data", h.requireAppOwner, h.MigrateAppData)
	apps.PUT("/:slug/config", h.requireAppOwner, h.UpdateAppConfig)
	apps.DELETE("/:slug", h.requireAppOwner, h.AdminDeleteDeployApp)
}

func (h *Handler) RegisterEnterpriseApps(v1 *gin.RouterGroup) {
	apps := v1.Group("/apps")
	apps.GET("/dockerfile-templates", h.EnterpriseAppsAuth(), h.GetDockerfileTemplates)
	h.RegisterEnterpriseAppRoutes(apps)
}
