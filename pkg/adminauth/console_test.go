package adminauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestConsoleRequiresExistingAdminCredential(t *testing.T) {
	r := gin.New()
	cfg := MiddlewareConfig{Secret: testSecret, AllowedOrigins: []string{"https://platform.example"}}
	RegisterConsoleSession(r, cfg)
	r.GET("/admin/deploy", ConsolePage(cfg), func(c *gin.Context) { c.Status(200) })
	old, _ := IssueToken(testSecret, "admin-console", RoleSystemAdmin, 0, time.Hour)
	valid, _ := IssueToken(testSecret, "named-admin", RoleSystemAdmin, 0, time.Hour)
	for _, token := range []string{"", old} {
		req := httptest.NewRequest(http.MethodGet, "/admin/deploy", nil)
		req.AddCookie(&http.Cookie{Name: consoleCookie, Value: token})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 303 || w.Header().Get("Location") != "/admin/login" || len(w.Result().Cookies()) != 0 {
			t.Fatalf("anonymous or retired credential admitted: %d", w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/session", nil)
	req.Header.Set("Authorization", "Bearer "+valid)
	req.Header.Set("Origin", "https://platform.example")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	cookies := w.Result().Cookies()
	if w.Code != 204 || len(cookies) != 2 || cookies[0].Path != "/admin" || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("valid credential did not establish a protected browser session: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/deploy", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("valid admin session denied: %d", w.Code)
	}
}
