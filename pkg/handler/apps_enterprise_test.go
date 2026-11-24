package handler

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
)

func TestEnterpriseAppsSharedCatalogAndOwnerManagement(t *testing.T) {
	_, s, _ := setupKnowledgeEnv(t, newFakeKnowledge())
	if err := s.DeployDB().AutoMigrate(&model.AppRuntimeOperation{}, &model.DeployedApp{}, &model.AppDeploymentVersion{}); err != nil {
		t.Fatal(err)
	}
	// Alice and Bob have valid enterprise sessions and no space entitlements.
	alice := insertIdentityLink(t, s, "app-alice", "app-ku-a")
	bob := insertIdentityLink(t, s, "app-bob", "app-ku-b")
	legacy := &model.DeployedApp{Slug: "legacy-app", Name: "Legacy", DockerfileContent: "private source", AppConfigJSON: "[]", ResourceIDsJSON: "[]", StorageMountsJSON: "[]"}
	if err := s.CreateOrUpdateApp(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{Auth: config.AuthConfig{Key: testAuthKey}}, nil, nil, nil, nil, s)
	r := gin.New()
	h.RegisterEnterpriseApps(r.Group("/integration/v1"))
	h.RegisterEnterpriseAppRoutes(r.Group("/api/v1/apps"))
	aliceHeaders := platformAccessHeaders(t, s, alice.ID)
	bobHeaders := platformAccessHeaders(t, s, bob.ID)
	request := func(method, path string, headers map[string]string, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	const base = "/integration/v1/apps"
	if w := request("GET", base, nil, ""); w.Code != 401 {
		t.Fatalf("anonymous catalog: %d", w.Code)
	}
	publish := func(headers map[string]string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		_ = writer.WriteField("slug", "owned-app")
		_ = writer.WriteField("name", "Owned")
		_ = writer.WriteField("dockerfile_content", "alice private source")
		for _, entry := range [][2]string{{"app_tar", "app.tar"}, {"skill_zip", "skill.zip"}} {
			part, _ := writer.CreateFormFile(entry[0], entry[1])
			_, _ = part.Write([]byte("fixture"))
		}
		_ = writer.Close()
		req := httptest.NewRequest(http.MethodPost, base+"/publish", &buf)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := publish(aliceHeaders); w.Code != 400 || !strings.Contains(w.Body.String(), "DELIVERY_PLAN_REQUIRED") {
		t.Fatalf("authorized desktop publish must require an AI delivery plan: %d %s", w.Code, w.Body)
	}
	if err := s.CreateOrUpdateApp(t.Context(), &model.DeployedApp{
		Slug: "owned-app", Name: "Owned", CreatedBy: enterpriseAppOwner(alice.OrganizationID, alice.ImmutableSubject),
		DockerfileContent: "alice private source", AppConfigJSON: "[]", ResourceIDsJSON: "[]", StorageMountsJSON: "[]",
	}); err != nil {
		t.Fatal(err)
	}
	if w := publish(bobHeaders); w.Code != 403 {
		t.Fatalf("another user must not overwrite the application: %d %s", w.Code, w.Body)
	}
	for _, suffix := range []string{"", "?tenant_id=arbitrary"} {
		w := request("GET", base+suffix, bobHeaders, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "owned-app") || !strings.Contains(w.Body.String(), "legacy-app") {
			t.Fatalf("catalog must share app metadata: %d %s", w.Code, w.Body)
		}
	}
	if w := request("GET", base+"/owned-app", bobHeaders, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_manage":false`) {
		t.Fatalf("other users can read the full read-only detail: %d %s", w.Code, w.Body)
	}
	for _, readRoute := range []string{"/owned-app/logs", "/owned-app/versions", "/owned-app/storage"} {
		if w := request("GET", base+readRoute, bobHeaders, ""); w.Code == 403 {
			t.Fatalf("non-owner read-only detail route %s must not be forbidden: %d %s", readRoute, w.Code, w.Body)
		}
	}
	if w := request("GET", base+"/legacy-app", bobHeaders, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_manage":true`) {
		t.Fatalf("historical unassigned apps must default to current logged-in user: %d %s", w.Code, w.Body)
	}
	if w := request("GET", base+"/owned-app?tenant_id=different", aliceHeaders, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_manage":true`) {
		t.Fatalf("space selection must not change ownership: %d %s", w.Code, w.Body)
	}
	if w := request("GET", "/api/v1/apps", nil, ""); w.Code != 401 {
		t.Fatalf("old app catalog must also require an enterprise login: %d", w.Code)
	}
	if w := request("DELETE", "/api/v1/apps/owned-app", bobHeaders, ""); w.Code != 403 {
		t.Fatalf("old route bypassed owner protection: %d", w.Code)
	}
	for _, route := range [][2]string{{"PUT", "/owned-app/config"}, {"POST", "/owned-app/rollback"}, {"POST", "/owned-app/sync-skill"}, {"DELETE", "/owned-app"}} {
		if w := request(route[0], base+route[1], bobHeaders, "{}"); w.Code != 403 {
			t.Fatalf("non-owner management %v: %d %s", route, w.Code, w.Body)
		}
	}
	// 历史无主应用由当前登录用户默认接管，允许查询日志与管理
	if w := request("GET", base+"/legacy-app/logs", bobHeaders, ""); w.Code == 403 {
		t.Fatalf("historical app logs must not be forbidden for logged-in user: %d %s", w.Code, w.Body)
	}
}
