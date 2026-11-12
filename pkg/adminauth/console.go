package adminauth

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const consoleCookie = "opc_admin_token"

// ConsolePage redirects visitors without an administrator credential to the sign-in page.
func ConsolePage(cfg MiddlewareConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, _ := c.Cookie(consoleCookie)
		if header := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "); header != "" {
			raw = header
		}
		claims, err := ParseToken(cfg.Secret, raw)
		if err != nil || claims.Role != RoleSystemAdmin {
			c.Redirect(http.StatusSeeOther, "/admin/login")
			c.Abort()
			return
		}
		c.Next()
	}
}

// RegisterConsoleSession installs a browser session only for an existing administrator JWT.
func RegisterConsoleSession(r *gin.Engine, cfg MiddlewareConfig) {
	r.POST("/admin/session", RequireAdmin(cfg), func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.Status(http.StatusBadRequest)
			return
		}
		claims, ok := GetClaims(c)
		if !ok || claims.Role != RoleSystemAdmin {
			c.Status(http.StatusForbidden)
			return
		}
		ttl := int(time.Until(claims.ExpiresAt.Time).Seconds())
		if ttl <= 0 {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.SetSameSite(http.SameSiteStrictMode)
		secure := c.Request.TLS != nil || strings.HasPrefix(requestOrigin(c), "https://")
		c.SetCookie(consoleCookie, strings.TrimPrefix(header, "Bearer "), ttl, "/admin", "", secure, true)
		// Remove credentials that were previously written with the old path.
		c.SetCookie(consoleCookie, "", -1, "/", "", secure, true)
		c.Header("Cache-Control", "no-store")
		c.Status(http.StatusNoContent)
	})
}
