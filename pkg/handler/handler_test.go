package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/syncer"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func setupCodeOAuthTestEnv(t *testing.T) (*gin.Engine, *fakeWecomIdentity) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	allDepts := map[string]accountcenter.SysDepartment{
		"T01": {ID: "101", DepartmentCode: "T01", DepartmentName: "技术中心", DepartmentLevel: 1, ThirdManagerID: "mgr_center"},
		"T02": {ID: "102", DepartmentCode: "T02", DepartmentName: "研发部", DepartmentLevel: 2, ParentCode: "T01", ThirdManagerID: "mgr_dept"},
		"T03": {ID: "103", DepartmentCode: "T03", DepartmentName: "架构组", DepartmentLevel: 3, ParentCode: "T02", ThirdManagerID: "mgr_group"},
		"T04": {ID: "104", DepartmentCode: "T04", DepartmentName: "云原生小组", DepartmentLevel: 4, ParentCode: "T03", ThirdManagerID: "mgr_team"},
		"UNMAPPED_99": {ID: "999", DepartmentCode: "UNMAPPED_99", DepartmentName: "未开通部门", DepartmentLevel: 1},
	}

	allUsers := map[string]accountcenter.SysUser{
		"wx_staff_l4": {
			ID:             "u_staff_l4",
			UserName:       "云原生员工",
			Email:          "staff_l4@example.com",
			DepartmentCode: "T04",
			UnionNo:        "wx_staff_l4",
			ThirdUserID:    "staff_l4",
		},
		"wx_mgr_group": {
			ID:             "u_mgr_group",
			UserName:       "架构组长",
			Email:          "mgr_group@example.com",
			DepartmentCode: "T03",
			UnionNo:        "wx_mgr_group",
			ThirdUserID:    "mgr_group",
		},
		"wx_mgr_center": {
			ID:             "u_mgr_center",
			UserName:       "技术中心总监",
			Email:          "mgr_center@example.com",
			DepartmentCode: "T01",
			UnionNo:        "wx_mgr_center",
			ThirdUserID:    "mgr_center",
		},
		"wx_staff_center": {
			ID:             "u_staff_center",
			UserName:       "普通员工",
			Email:          "staff_center@example.com",
			DepartmentCode: "T01",
			UnionNo:        "wx_staff_center",
			ThirdUserID:    "staff_regular",
		},
		"wx_unmapped": {
			ID:             "u_unmapped",
			UserName:       "未配置员工",
			Email:          "unmapped@example.com",
			DepartmentCode: "UNMAPPED_99",
			UnionNo:        "wx_unmapped",
			ThirdUserID:    "user_unmapped",
		},
	}

	acSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(req.Query, "GetUserByUnionNo") {
			unionNo, _ := req.Variables["unionNo"].(string)
			if u, ok := allUsers[unionNo]; ok {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"sysUsers": map[string]interface{}{
							"edges": []map[string]interface{}{
								{
									"node": map[string]interface{}{
										"id":             u.ID,
										"userName":       u.UserName,
										"email":          u.Email,
										"departmentCode": u.DepartmentCode,
										"unionNo":        u.UnionNo,
										"thirdUserId":    u.ThirdUserID,
									},
								},
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysUsers": map[string]interface{}{
						"edges": []map[string]interface{}{},
					},
				},
			})
			return
		}

		if strings.Contains(req.Query, "GetDepartmentByCode") {
			code, _ := req.Variables["code"].(string)
			if d, ok := allDepts[code]; ok {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"sysDepartments": map[string]interface{}{
							"edges": []map[string]interface{}{
								{
									"node": map[string]interface{}{
										"id":                d.ID,
										"departmentCode":    d.DepartmentCode,
										"departmentName":    d.DepartmentName,
										"thirdManagerId":    d.ThirdManagerID,
										"thirdDepartmentId": d.ThirdDepartmentID,
										"departmentLevel":   d.DepartmentLevel,
										"parentCode":        d.ParentCode,
									},
								},
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"sysDepartments": map[string]interface{}{
						"edges": []map[string]interface{}{},
					},
				},
			})
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(acSrv.Close)

	wk := &fakeWecomIdentity{}

	syn := syncer.NewSyncer(nil, nil, nil, nil)
	targetDepts := map[string]accountcenter.SysDepartment{
		"T01": allDepts["T01"],
	}
	syn.UpdateDeptCache(allDepts, targetDepts)

	h := &Handler{
		ac:     accountcenter.NewClient(acSrv.URL),
		wecom:  wk,
		syncer: syn,
	}

	r := gin.New()
	r.GET("/wecom/callback", h.OAuthCallback)

	return r, wk
}

func extractClaimsFromRedirect(t *testing.T, loc string) jwt.MapClaims {
	t.Helper()
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
	return claims
}

func TestOAuthCallback_Code_TargetDeptLevel1(t *testing.T) {
	r, wk := setupCodeOAuthTestEnv(t)
	wk.set("wx_staff_l4", nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state=code-desktop", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d, body=%s", w.Code, w.Body.String())
	}

	claims := extractClaimsFromRedirect(t, w.Header().Get("Location"))
	if claims["dept_code"] != "T01" {
		t.Errorf("expected dept_code T01, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "技术中心" {
		t.Errorf("expected dept_name 技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != false {
		t.Errorf("expected is_dept_manager false, got %v", claims["is_dept_manager"])
	}
}

func TestOAuthCallback_Code_SubDeptManagerInherit(t *testing.T) {
	r, wk := setupCodeOAuthTestEnv(t)
	wk.set("wx_mgr_group", nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state=code-desktop", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d, body=%s", w.Code, w.Body.String())
	}

	claims := extractClaimsFromRedirect(t, w.Header().Get("Location"))
	if claims["dept_code"] != "T01" {
		t.Errorf("expected dept_code T01, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "技术中心" {
		t.Errorf("expected dept_name 技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != true {
		t.Errorf("expected is_dept_manager true, got %v", claims["is_dept_manager"])
	}
}

func TestOAuthCallback_Code_CenterManager(t *testing.T) {
	r, wk := setupCodeOAuthTestEnv(t)
	wk.set("wx_mgr_center", nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state=code-desktop", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d, body=%s", w.Code, w.Body.String())
	}

	claims := extractClaimsFromRedirect(t, w.Header().Get("Location"))
	if claims["dept_code"] != "T01" {
		t.Errorf("expected dept_code T01, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "技术中心" {
		t.Errorf("expected dept_name 技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != true {
		t.Errorf("expected is_dept_manager true, got %v", claims["is_dept_manager"])
	}
}

func TestOAuthCallback_Code_RegularStaff(t *testing.T) {
	r, wk := setupCodeOAuthTestEnv(t)
	wk.set("wx_staff_center", nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state=code-desktop", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d, body=%s", w.Code, w.Body.String())
	}

	claims := extractClaimsFromRedirect(t, w.Header().Get("Location"))
	if claims["dept_code"] != "T01" {
		t.Errorf("expected dept_code T01, got %v", claims["dept_code"])
	}
	if claims["dept_name"] != "技术中心" {
		t.Errorf("expected dept_name 技术中心, got %v", claims["dept_name"])
	}
	if claims["is_dept_manager"] != false {
		t.Errorf("expected is_dept_manager false, got %v", claims["is_dept_manager"])
	}
}

func TestOAuthCallback_Code_NotProvisioned(t *testing.T) {
	r, wk := setupCodeOAuthTestEnv(t)
	wk.set("wx_unmapped", nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wecom/callback?code=mock_code&state=code-desktop", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response error: %v", err)
	}

	if resp.Error.Code != "SUBJECT_NOT_PROVISIONED" {
		t.Errorf("expected error code SUBJECT_NOT_PROVISIONED, got %s", resp.Error.Code)
	}
	expectedMsg := "department space not provisioned, please contact administrator to run a sync"
	if resp.Error.Message != expectedMsg {
		t.Errorf("expected error message %q, got %q", expectedMsg, resp.Error.Message)
	}
}
