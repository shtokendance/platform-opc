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

func TestConsoleAcceptsPlainAdminPassword(t *testing.T) {
	plainPassword := "Opc@2026!Admin"
	r := gin.New()
	cfg := MiddlewareConfig{Secret: []byte(plainPassword), AllowedOrigins: []string{"https://platform.example"}}
	RegisterConsoleSession(r, cfg)
	r.GET("/admin/deploy", ConsolePage(cfg), func(c *gin.Context) { c.Status(200) })

	// Wrong password rejected
	badReq := httptest.NewRequest(http.MethodPost, "/admin/session", nil)
	badReq.Header.Set("Authorization", "Bearer Wrong@Pass123")
	badReq.Header.Set("Origin", "https://platform.example")
	wBad := httptest.NewRecorder()
	r.ServeHTTP(wBad, badReq)
	if wBad.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", wBad.Code)
	}

	// Correct plain password establishes session and issues signed JWT cookie
	req := httptest.NewRequest(http.MethodPost, "/admin/session", nil)
	req.Header.Set("Authorization", "Bearer "+plainPassword)
	req.Header.Set("Origin", "https://platform.example")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	cookies := w.Result().Cookies()
	if w.Code != 204 || len(cookies) != 2 || cookies[0].Path != "/admin" || !cookies[0].HttpOnly {
		t.Fatalf("plain password did not establish a protected browser session: %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/deploy", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("plain password session denied on /admin/deploy: %d", w.Code)
	}
}
