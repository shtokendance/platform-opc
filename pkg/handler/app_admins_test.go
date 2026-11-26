package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestAppAdminsAuthorizationLifecycle(t *testing.T) {
	_, s, _ := setupKnowledgeEnv(t, newFakeKnowledge())
	if err := s.DeployDB().AutoMigrate(
		&model.AppRuntimeOperation{},
		&model.DeployedApp{},
		&model.AppDeploymentVersion{},
		&model.AppDeliveryRevision{},
		&model.AppVolumeBackup{},
		&model.AppAdmin{},
	); err != nil {
		t.Fatal(err)
	}

	alice := insertIdentityLink(t, s, "app-admin-alice", "ku-alice")
	bob := insertIdentityLink(t, s, "app-admin-bob", "ku-bob")
	now := time.Now()
	for _, u := range []model.WeKnoraUser{
		{ID: "ku-alice", Username: "Alice", Email: "alice@example.com", PasswordHash: "x", TenantID: 10001, IsActive: true, Preferences: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "ku-bob", Username: "Bob", Email: "bob@example.com", PasswordHash: "x", TenantID: 10001, IsActive: true, Preferences: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "ku-unlinked", Username: "Unlinked", Email: "unlinked@example.com", PasswordHash: "x", TenantID: 10001, IsActive: true, Preferences: "{}", CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.DB().Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}

	aliceOwner := enterpriseAppOwner(alice.OrganizationID, alice.ImmutableSubject)
	bobOwner := enterpriseAppOwner(bob.OrganizationID, bob.ImmutableSubject)
	appID := uuid.New()
	cfgJSON, _ := json.Marshal([]model.AppConfigItem{{Key: "PORT", Value: "8080", HasValue: true, ValueSource: "manual"}})
	app := &model.DeployedApp{
		ID:                appID,
		Slug:              "owned-app",
		Name:              "Owned App",
		AppURL:            "http://owned-app.apps.opc.intra",
		ContainerPort:     8080,
		Language:          "go",
		RuntimeVersion:    "1.22",
		DockerfileContent: "FROM golang:1.22\nEXPOSE 8080\n",
		CurrentVersion:    1,
		Status:            model.AppStatusRunning,
		AppConfigJSON:     string(cfgJSON),
		ResourceIDsJSON:   "[]",
		StorageMountsJSON: "[]",
		CreatedBy:         aliceOwner,
	}
	v1 := &model.AppDeploymentVersion{
		ID:                uuid.New(),
		RequestID:         "req-v1",
		AppSlug:           "owned-app",
		Version:           1,
		ProposedVersion:   1,
		Status:            model.AppStatusRunning,
		IsActive:          true,
		ContainerPort:     8080,
		Language:          "go",
		RuntimeVersion:    "1.22",
		DockerfileContent: "FROM golang:1.22\nEXPOSE 8080\n",
		AppConfigJSON:     string(cfgJSON),
		ResourceIDsJSON:   "[]",
		StorageMountsJSON: "[]",
		CreatedBy:         aliceOwner,
	}
	if err := s.SavePublishedAppAndVersion(context.Background(), app, v1); err != nil {
		t.Fatal(err)
	}

	capSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"data":{"token":"tok"}}`))
		default:
			_, _ = w.Write([]byte(`{"status":100,"data":{}}`))
		}
	}))
	defer capSrv.Close()

	h := NewHandler(&config.Config{
		Auth:  config.AuthConfig{Key: testAuthKey},
		Admin: config.AdminConfig{Key: string(testAdminSecret)},
	}, nil, nil, nil, nil, s)
	h.SetCapRoverClient(deploy.NewCapRoverClient(capSrv.URL, "pass", "apps.opc.intra"))
	h.SetArtifactStore(deploy.NewArtifactStore(deploy.ArtifactStoreConfig{
		Bucket:   "opc-apps",
		LocalDir: t.TempDir(),
	}))

	r := gin.New()
	h.RegisterEnterpriseApps(r.Group("/integration/v1"))
	adminAPI := r.Group("/admin/api", adminauth.RequireAdmin(adminauth.MiddlewareConfig{
		Secret:         testAdminSecret,
		AllowedOrigins: []string{"https://admin.example.com"},
		Audit:          s.InsertAuditEvent,
	}))
	{
		adminAPI.GET("/deploy/apps", h.ListApps)
		adminAPI.POST("/deploy/apps/:slug/admins", h.AdminUpdateAppAdmin)
		adminAPI.DELETE("/deploy/apps/:slug", h.AdminDeleteDeployApp)
	}

	aliceHeaders := platformAccessHeaders(t, s, alice.ID)
	bobHeaders := platformAccessHeaders(t, s, bob.ID)
	adminToken := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)

	doUserReq := func(method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := doUserReq("GET", "/integration/v1/apps/owned-app", bobHeaders, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"can_manage":false`) {
		t.Fatalf("expected can_manage:false before grant, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("PUT", "/integration/v1/apps/owned-app/config", bobHeaders, `{"container_port":8080,"app_config":[],"resource_ids":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 before grant on PUT config, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("GET", "/integration/v1/apps/owned-app/runtime-management", bobHeaders, ""); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 before grant on GET runtime-management, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("POST", "/integration/v1/apps/owned-app/recheck", bobHeaders, "{}"); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 before grant on POST recheck, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("POST", "/integration/v1/apps/owned-app/config-values", bobHeaders, `{"bindings":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 before grant on POST config-values, got %d %s", w.Code, w.Body.String())
	}

	if w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/deploy/apps/owned-app/admins", adminToken, "", `{"user_id":"ku-unlinked","action":"add"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for user without active identity link, got %d %s", w.Code, w.Body.String())
	}

	wGrant := doAuthzRequest(t, r, http.MethodPost, "/admin/api/deploy/apps/owned-app/admins", adminToken, "", `{"user_id":"ku-bob","action":"add"}`)
	if wGrant.Code != http.StatusOK {
		t.Fatalf("expected 200 on grant admin, got %d %s", wGrant.Code, wGrant.Body.String())
	}
	var grantResp struct {
		Status string                  `json:"status"`
		Admins []model.AppAdminSummary `json:"admins"`
	}
	if err := json.Unmarshal(wGrant.Body.Bytes(), &grantResp); err != nil {
		t.Fatal(err)
	}
	if len(grantResp.Admins) != 2 || !grantResp.Admins[0].IsCreator || grantResp.Admins[0].UserID != "ku-alice" || grantResp.Admins[1].IsCreator || grantResp.Admins[1].UserID != "ku-bob" {
		t.Fatalf("unexpected admins list after grant: %+v", grantResp.Admins)
	}

	wAdminList := doAuthzRequest(t, r, http.MethodGet, "/admin/api/deploy/apps", adminToken, "", "")
	if wAdminList.Code != http.StatusOK || !strings.Contains(wAdminList.Body.String(), `"username":"Bob"`) {
		t.Fatalf("expected admin list to contain Bob in admins: %d %s", wAdminList.Code, wAdminList.Body.String())
	}

	if w := doUserReq("GET", "/integration/v1/apps/owned-app", bobHeaders, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"can_manage":true`) {
		t.Fatalf("expected can_manage:true after grant, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("GET", "/integration/v1/apps", bobHeaders, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"can_manage":true`) {
		t.Fatalf("expected catalog can_manage:true after grant, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("PUT", "/integration/v1/apps/owned-app/config", bobHeaders, `{"container_port":8080,"app_config":[{"key":"PORT","value":"8080"}],"resource_ids":[]}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delegated admin PUT config, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("GET", "/integration/v1/apps/owned-app/runtime-management", bobHeaders, ""); w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delegated admin GET runtime-management, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("POST", "/integration/v1/apps/owned-app/recheck", bobHeaders, "{}"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delegated admin POST recheck, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("POST", "/integration/v1/apps/owned-app/config-values", bobHeaders, `{"bindings":[]}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delegated admin POST config-values, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("GET", "/integration/v1/apps/owned-app/versions", bobHeaders, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"created_by"`) {
		t.Fatalf("expected 200 for delegated admin GET versions, got %d %s", w.Code, w.Body.String())
	}

	v2 := &model.AppDeploymentVersion{
		ID:                uuid.New(),
		RequestID:         "req-v2",
		AppSlug:           "owned-app",
		Version:           2,
		ProposedVersion:   2,
		Status:            model.AppStatusRunning,
		IsActive:          true,
		ContainerPort:     8080,
		Language:          "go",
		RuntimeVersion:    "1.22",
		DockerfileContent: "FROM golang:1.22\nEXPOSE 8080\n",
		AppConfigJSON:     string(cfgJSON),
		ResourceIDsJSON:   "[]",
		StorageMountsJSON: "[]",
		CreatedBy:         bobOwner,
	}
	upgradeApp := &model.DeployedApp{
		ID:                appID,
		Slug:              "owned-app",
		Name:              "Owned App",
		AppURL:            "http://owned-app.apps.opc.intra",
		ContainerPort:     8080,
		Language:          "go",
		RuntimeVersion:    "1.22",
		DockerfileContent: "FROM golang:1.22\nEXPOSE 8080\n",
		CurrentVersion:    2,
		Status:            model.AppStatusRunning,
		AppConfigJSON:     string(cfgJSON),
		ResourceIDsJSON:   "[]",
		StorageMountsJSON: "[]",
		CreatedBy:         bobOwner,
	}
	if err := s.SavePublishedAppAndVersion(context.Background(), upgradeApp, v2); err != nil {
		t.Fatal(err)
	}
	storedApp, err := s.GetAppBySlug(context.Background(), "owned-app")
	if err != nil {
		t.Fatal(err)
	}
	if storedApp.CreatedBy != aliceOwner {
		t.Fatalf("expected app created_by to stay %s after delegated admin upgrade, got %s", aliceOwner, storedApp.CreatedBy)
	}
	if v2.CreatedBy != bobOwner {
		t.Fatalf("expected version created_by to record %s, got %s", bobOwner, v2.CreatedBy)
	}

	if w := doUserReq("DELETE", "/integration/v1/apps/owned-app", bobHeaders, ""); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when delegated admin tries to delete app, got %d %s", w.Code, w.Body.String())
	}

	if w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/deploy/apps/owned-app/admins", adminToken, "", `{"user_id":"ku-alice","action":"remove"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when removing creator, got %d %s", w.Code, w.Body.String())
	}

	wRevoke := doAuthzRequest(t, r, http.MethodPost, "/admin/api/deploy/apps/owned-app/admins", adminToken, "", `{"user_id":"ku-bob","action":"remove"}`)
	if wRevoke.Code != http.StatusOK {
		t.Fatalf("expected 200 on revoke admin, got %d %s", wRevoke.Code, wRevoke.Body.String())
	}
	if w := doUserReq("PUT", "/integration/v1/apps/owned-app/config", bobHeaders, `{"container_port":8080,"app_config":[],"resource_ids":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 after revoke on PUT config, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("GET", "/integration/v1/apps/owned-app/runtime-management", bobHeaders, ""); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 after revoke on GET runtime-management, got %d %s", w.Code, w.Body.String())
	}

	if w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/deploy/apps/owned-app/admins", adminToken, "", `{"user_id":"ku-bob","action":"add"}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200 re-adding admin before delete, got %d %s", w.Code, w.Body.String())
	}
	if w := doUserReq("DELETE", "/integration/v1/apps/owned-app", aliceHeaders, ""); w.Code != http.StatusOK {
		t.Fatalf("expected 200 when creator deletes app, got %d %s", w.Code, w.Body.String())
	}
	remaining, err := s.ListAppAdmins(context.Background(), "owned-app")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected app_admins cascade deleted with app, got %d rows", len(remaining))
	}
}
