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
	if err := s.DeployDB().AutoMigrate(&model.DeployedApp{}, &model.AppDeploymentVersion{}); err != nil {
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
		_ = writer.WriteField("dockerfile_content", "private source")
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
	if w := publish(aliceHeaders); w.Code != 202 {
		t.Fatalf("signed-in user without a space must be able to publish: %d %s", w.Code, w.Body)
	}
	if w := publish(bobHeaders); w.Code != 403 {
		t.Fatalf("another user must not overwrite the application: %d %s", w.Code, w.Body)
	}
	for _, suffix := range []string{"", "?tenant_id=arbitrary"} {
		w := request("GET", base+suffix, bobHeaders, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "owned-app") || !strings.Contains(w.Body.String(), "legacy-app") || strings.Contains(w.Body.String(), "private source") {
			t.Fatalf("catalog must share app metadata without private details: %d %s", w.Code, w.Body)
		}
	}
	if w := request("GET", base+"/owned-app", bobHeaders, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_manage":false`) || strings.Contains(w.Body.String(), "private source") {
		t.Fatalf("other users can read the public detail: %d %s", w.Code, w.Body)
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
	for _, route := range [][2]string{{"GET", "/owned-app/logs"}, {"PUT", "/owned-app/config"}, {"POST", "/owned-app/rollback"}, {"DELETE", "/owned-app"}, {"DELETE", "/legacy-app"}} {
		if w := request(route[0], base+route[1], bobHeaders, "{}"); w.Code != 403 {
			t.Fatalf("non-owner management %v: %d %s", route, w.Code, w.Body)
		}
	}
}
