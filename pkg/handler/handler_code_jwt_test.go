package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func TestCodeDesktopLoginIncludesDepartmentClaims(t *testing.T) {
	fakeAC := newSyncFakeAccountCenter(t)
	fakeAC.seed() // T01 技术中心, u1 员工 (dept: T01)

	h := &Handler{
		ac: fakeAC.client(),
	}

	claims := h.buildCodeJWTClaims("u1", "u1@corp.test", "员工一", "T01", "技术中心", true)
	if claims["dept_code"] != "T01" {
		t.Fatalf("expected dept_code T01, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "技术中心" {
		t.Fatalf("expected dept_name 技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != true {
		t.Fatalf("expected is_dept_manager true, got %v", claims["is_dept_manager"])
	}
}

func TestOAuthCallback_CodeDesktop_InjectsDepartmentAndManagerClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	acSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "GetUserByUnionNo") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysUsers": map[string]interface{}{
						"edges": []map[string]interface{}{
							{
								"node": map[string]interface{}{
									"id":             "u1",
									"userName":       "张经理",
									"email":          "manager@example.com",
									"departmentCode": "D001",
									"unionNo":        "wx_u1",
									"thirdUserId":    "third_mgr_1",
								},
							},
						},
					},
				},
			})
			return
		}
		if strings.Contains(req.Query, "GetDepartmentByCode") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysDepartments": map[string]interface{}{
						"edges": []map[string]interface{}{
							{
								"node": map[string]interface{}{
									"id":                "dept-1",
									"departmentCode":    "D001",
									"departmentName":    "产品技术中心",
									"thirdManagerId":    "third_mgr_1",
									"thirdDepartmentId": "third_dept_1",
								},
							},
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(acSrv.Close)

	wk := &fakeWecomIdentity{}
	wk.set("wx_u1", nil)

	h := &Handler{
		ac:    accountcenter.NewClient(acSrv.URL),
		wecom: wk,
	}

	r := gin.New()
	r.GET("/wecom/callback", h.OAuthCallback)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state=code-desktop", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d, body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "multica://auth/callback?token=") {
		t.Fatalf("unexpected location: %s", loc)
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location url: %v", err)
	}
	tokenStr := u.Query().Get("token")
	if tokenStr == "" {
		t.Fatal("missing token query in redirect location")
	}

	secret := "dfb4baad1fe70dcf3b18a79739a50341ae6aa040ce1a8663ce7a7ac61c170f79"
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		t.Fatalf("invalid token: %v", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("claims is not MapClaims")
	}

	if claims["dept_code"] != "D001" {
		t.Errorf("expected dept_code D001, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "产品技术中心" {
		t.Errorf("expected dept_name 产品技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != true {
		t.Errorf("expected is_dept_manager true, got %v", claims["is_dept_manager"])
	}
	if claims["email"] != "manager@example.com" {
		t.Errorf("expected email manager@example.com, got %v", claims["email"])
	}
	if claims["name"] != "张经理" {
		t.Errorf("expected name 张经理, got %v", claims["name"])
	}
}

func TestOAuthCallback_CodeWeb_NonManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	acSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "GetUserByUnionNo") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysUsers": map[string]interface{}{
						"edges": []map[string]interface{}{
							{
								"node": map[string]interface{}{
									"id":             "u2",
									"userName":       "普通员工",
									"email":          "staff@example.com",
									"departmentCode": "D001",
									"unionNo":        "wx_u2",
									"thirdUserId":    "third_staff_2",
								},
							},
						},
					},
				},
			})
			return
		}
		if strings.Contains(req.Query, "GetDepartmentByCode") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysDepartments": map[string]interface{}{
						"edges": []map[string]interface{}{
							{
								"node": map[string]interface{}{
									"id":                "dept-1",
									"departmentCode":    "D001",
									"departmentName":    "产品技术中心",
									"thirdManagerId":    "third_mgr_1", // different from third_staff_2
									"thirdDepartmentId": "third_dept_1",
								},
							},
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(acSrv.Close)

	wk := &fakeWecomIdentity{}
	wk.set("wx_u2", nil)

	h := &Handler{
		ac:    accountcenter.NewClient(acSrv.URL),
		wecom: wk,
	}

	r := gin.New()
	r.GET("/wecom/callback", h.OAuthCallback)

	targetURI := "http://localhost:3000/auth/callback"
	state := "code-web:" + base64.RawURLEncoding.EncodeToString([]byte(targetURI))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state="+state, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d, body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, targetURI+"?token=") {
		t.Fatalf("unexpected location: %s", loc)
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location url: %v", err)
	}
	tokenStr := u.Query().Get("token")
	secret := "dfb4baad1fe70dcf3b18a79739a50341ae6aa040ce1a8663ce7a7ac61c170f79"
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		t.Fatalf("invalid token: %v", err)
	}
	claims := token.Claims.(jwt.MapClaims)

	if claims["dept_code"] != "D001" {
		t.Errorf("expected dept_code D001, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "产品技术中心" {
		t.Errorf("expected dept_name 产品技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != false {
		t.Errorf("expected is_dept_manager false, got %v", claims["is_dept_manager"])
	}
}

func TestOAuthCallback_CodeDesktop_LoopbackRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	acSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "GetUserByUnionNo") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysUsers": map[string]interface{}{
						"edges": []map[string]interface{}{
							{
								"node": map[string]interface{}{
									"id":             "u101",
									"userName":       "alice",
									"email":          "alice@example.com",
									"departmentCode": "D001",
									"unionNo":        "wx_alice",
									"thirdUserId":    "third_alice",
								},
							},
						},
					},
				},
			})
			return
		}
		if strings.Contains(req.Query, "GetDepartmentByCode") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysDepartments": map[string]interface{}{
						"edges": []map[string]interface{}{
							{
								"node": map[string]interface{}{
									"id":                "dept-1",
									"departmentCode":    "D001",
									"departmentName":    "产品技术中心",
									"thirdManagerId":    "third_bob",
									"thirdDepartmentId": "third_dept_1",
									"departmentLevel":   1,
								},
							},
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(acSrv.Close)

	wk := &fakeWecomIdentity{}
	wk.set("wx_alice", nil)

	h := &Handler{
		ac:    accountcenter.NewClient(acSrv.URL),
		wecom: wk,
	}

	r := gin.New()
	r.GET("/wecom/callback", h.OAuthCallback)

	loopbackURI := "http://127.0.0.1:41920/callback"
	encodedState := "code-desktop:" + base64.RawURLEncoding.EncodeToString([]byte(loopbackURI))
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state="+url.QueryEscape(encodedState), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d, body: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, loopbackURI+"?token=") {
		t.Fatalf("unexpected location: %s", loc)
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location url: %v", err)
	}
	tokenStr := u.Query().Get("token")
	if tokenStr == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestResolveCodeJWTSecret_FallbackOrder(t *testing.T) {
	t.Setenv("CODE_JWT_SECRET", "")
	t.Setenv("JWT_SECRET", "")
	t.Setenv("WEKNORA_JWT_SECRET", "")

	cfg := &config.Config{
		WeKnora: config.WeKnoraConfig{
			JWTSecret: "cfg-weknora-secret",
		},
	}
	if got := resolveCodeJWTSecret(cfg); got != "cfg-weknora-secret" {
		t.Fatalf("expected cfg.WeKnora.JWTSecret fallback, got %q", got)
	}

	t.Setenv("WEKNORA_JWT_SECRET", "env-weknora-secret")
	if got := resolveCodeJWTSecret(cfg); got != "env-weknora-secret" {
		t.Fatalf("expected WEKNORA_JWT_SECRET fallback, got %q", got)
	}

	t.Setenv("JWT_SECRET", "env-jwt-secret")
	if got := resolveCodeJWTSecret(cfg); got != "env-jwt-secret" {
		t.Fatalf("expected JWT_SECRET precedence, got %q", got)
	}

	t.Setenv("CODE_JWT_SECRET", "env-code-jwt-secret")
	if got := resolveCodeJWTSecret(cfg); got != "env-code-jwt-secret" {
		t.Fatalf("expected CODE_JWT_SECRET precedence, got %q", got)
	}

	t.Setenv("CODE_JWT_SECRET", "")
	t.Setenv("JWT_SECRET", "")
	t.Setenv("WEKNORA_JWT_SECRET", "")
	if got := resolveCodeJWTSecret(nil); got != "dfb4baad1fe70dcf3b18a79739a50341ae6aa040ce1a8663ce7a7ac61c170f79" {
		t.Fatalf("expected hardcoded default fallback, got %q", got)
	}
}
