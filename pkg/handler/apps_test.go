package handler

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestAppHandler(t *testing.T) (*Handler, *gin.Engine) {
	gin.SetMode(gin.TestMode)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&model.DeployedApp{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	cfg := &config.Config{
		AppDeploy: config.AppDeployConfig{
			IntranetRootDomain: "apps.opc.intra",
			OnlineResources: []config.OnlineResourceConfig{
				{
					ID:          "pg-test",
					Name:        "Test PostgreSQL",
					Type:        "postgres",
					Description: "Test DB",
					EnvVars: map[string]string{
						"DATABASE_URL": "postgresql://usr:pwd@10.0.0.1:5432/db",
						"PGHOST":       "10.0.0.1",
					},
				},
			},
		},
	}

	storageInstance := storage.NewStorageWithDB(db)
	h := NewHandler(cfg, nil, nil, nil, nil, storageInstance)
	h.SetArtifactStore(deploy.NewArtifactStore(deploy.ArtifactStoreConfig{
		Bucket:   "opc-apps",
		LocalDir: t.TempDir(),
	}))

	r := gin.New()
	apps := r.Group("/api/v1/apps")
	{
		apps.POST("/check-slug", h.CheckSlug)
		apps.GET("/resources", h.ListResources)
		apps.GET("/dockerfile-templates", h.GetDockerfileTemplates)
		apps.POST("/publish", h.PublishApp)
		apps.GET("", h.ListApps)
		apps.GET("/:slug", h.GetApp)
		apps.GET("/:slug/logs", h.GetAppLogs)
		apps.GET("/:slug/versions", h.ListAppVersions)
		apps.GET("/:slug/versions/:version/artifacts/:filename", h.DownloadAppVersionArtifact)
		apps.POST("/:slug/rollback", h.RollbackAppVersion)
		apps.PUT("/:slug", h.AdminUpdateDeployApp)
		apps.DELETE("/:slug", h.AdminDeleteDeployApp)
		apps.PUT("/:slug/config", h.UpdateAppConfig)
		apps.POST("/:slug/migrate-data", h.MigrateAppData)
		apps.GET("/admin/resources", h.AdminListDeployResources)
		apps.POST("/admin/resources", h.AdminUpsertDeployResource)
		apps.GET("/admin/storage", h.AdminGetGlobalStorage)
		apps.GET("/:slug/storage", h.GetAppStorageDetails)
		apps.GET("/:slug/storage/backup", h.DownloadAppStorageBackup)
		apps.POST("/:slug/recheck", h.RecheckAppHealth)
		apps.POST("/admin/storage/orphan-volumes/purge", h.AdminPurgeOrphanVolumes)
		apps.DELETE("/admin/storage/volumes/:name", h.AdminDeleteVolume)
		apps.DELETE("/admin/resources/:id", h.AdminDeleteDeployResource)
		apps.GET("/admin/dockerfile-templates", h.AdminListDockerfileTemplates)
		apps.POST("/admin/dockerfile-templates", h.AdminUpsertDockerfileTemplate)
		apps.DELETE("/admin/dockerfile-templates/:id", h.AdminDeleteDockerfileTemplate)
	}

	return h, r
}

func TestHandler_CheckSlug(t *testing.T) {
	_, r := setupTestAppHandler(t)

	// 1. Valid and available
	body, _ := json.Marshal(map[string]string{"slug": "test-app"})
	req := httptest.NewRequest("POST", "/api/v1/apps/check-slug", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp CheckSlugResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Available || resp.AppURL != "http://test-app.apps.opc.intra" || resp.SkillName != "app-test-app" {
		t.Errorf("unexpected check-slug resp: %+v", resp)
	}

	// 2. Invalid format
	body, _ = json.Marshal(map[string]string{"slug": "INVALID_SLUG!"})
	req = httptest.NewRequest("POST", "/api/v1/apps/check-slug", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid slug, got %d", w.Code)
	}
}

func TestHandler_ListResources(t *testing.T) {
	_, r := setupTestAppHandler(t)

	req := httptest.NewRequest("GET", "/api/v1/apps/resources", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result struct {
		Resources []model.ResourceSummary `json:"resources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(result.Resources) != 1 || result.Resources[0].ID != "pg-test" {
		t.Fatalf("unexpected resources: %+v", result)
	}
	if len(result.Resources[0].EnvKeys) != 2 {
		t.Errorf("expected 2 env keys, got %v", result.Resources[0].EnvKeys)
	}
}

func TestHandler_PublishAndList(t *testing.T) {
	_, r := setupTestAppHandler(t)

	// Publish multipart
	buf := &bytes.Buffer{}
	writer := multipart.NewWriter(buf)
	_ = writer.WriteField("name", "报销系统")
	_ = writer.WriteField("slug", "expense-app")
	_ = writer.WriteField("description", "报销管理系统")
	_ = writer.WriteField("container_port", "8080")
	_ = writer.WriteField("app_config", `[{"key":"MAX_SIZE","value":"100"}]`)
	_ = writer.WriteField("resource_ids", `["pg-test"]`)

	part1, _ := writer.CreateFormFile("app_tar", "app.tar")
	part1.Write([]byte("mock-tar"))
	part2, _ := writer.CreateFormFile("skill_zip", "skill.zip")
	part2.Write([]byte("mock-zip"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/apps/publish", buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", w.Code, w.Body.String())
	}

	// GET /api/v1/apps
	listReq := httptest.NewRequest("GET", "/api/v1/apps", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, listReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var listResp struct {
		Apps []AppDetailResponse `json:"apps"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Apps) != 1 || listResp.Apps[0].Slug != "expense-app" {
		t.Fatalf("expected 1 app, got %+v", listResp.Apps)
	}

	// GET /api/v1/apps/expense-app
	getReq := httptest.NewRequest("GET", "/api/v1/apps/expense-app", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, getReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// PUT /api/v1/apps/expense-app/config
	updatePayload, _ := json.Marshal(UpdateAppConfigRequest{
		ContainerPort: 9000,
		AppConfig: []model.AppConfigItem{
			{Key: "MAX_SIZE", Value: "200"},
		},
		ResourceIDs: []string{"pg-test"},
	})
	putReq := httptest.NewRequest("PUT", "/api/v1/apps/expense-app/config", bytes.NewReader(updatePayload))
	putReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putReq)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected deployment service unavailable without a controller, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_DockerfileTemplatesAndAutoLearn(t *testing.T) {
	h, r := setupTestAppHandler(t)

	// 1. Query matched template for python 3.11
	req := httptest.NewRequest("GET", "/api/v1/apps/dockerfile-templates?language=python&version=3.11", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		MatchedTemplate *model.DockerfileTemplate  `json:"matched_template"`
		Templates       []model.DockerfileTemplate `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.MatchedTemplate == nil || resp.MatchedTemplate.BaseImage != "python:3.11-slim" {
		t.Fatalf("expected matched template python:3.11-slim, got %+v", resp.MatchedTemplate)
	}

	// 2. Test AutoSaveDockerfileTemplateFromApp for a new language+version (ruby 3.3)
	app := &model.DeployedApp{
		Name:              "Ruby App",
		Slug:              "ruby-demo",
		Language:          "ruby",
		RuntimeVersion:    "3.3",
		DockerfileContent: "FROM ruby:3.3-slim\nWORKDIR /app\nCOPY . .\nCMD [\"ruby\", \"app.rb\"]",
	}
	if err := h.storage.AutoSaveDockerfileTemplateFromApp(req.Context(), app); err != nil {
		t.Fatalf("AutoSaveDockerfileTemplateFromApp failed: %v", err)
	}

	req2 := httptest.NewRequest("GET", "/api/v1/apps/dockerfile-templates?language=ruby&version=3.3", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal ruby template: %v", err)
	}
	if resp.MatchedTemplate == nil || resp.MatchedTemplate.BaseImage != "ruby:3.3-slim" || resp.MatchedTemplate.SourceAppSlug != "ruby-demo" {
		t.Fatalf("expected auto-learned ruby 3.3 template, got %+v", resp.MatchedTemplate)
	}
}

func TestHandler_MigrateAppDataAndAdminCRUD(t *testing.T) {
	_, r := setupTestAppHandler(t)

	// 1. Migrate SQLite table & rows to server DB
	migReq := MigrateAppDataRequest{
		SourceType: "sqlite",
		TargetType: "postgres",
		Tables: []MigratedTableSpec{
			{
				Name: "users",
				Columns: []MigratedColumnSpec{
					{Name: "id", Type: "INTEGER", PrimaryKey: true},
					{Name: "username", Type: "TEXT", NotNull: true},
				},
				Rows: []map[string]interface{}{
					{"id": 1, "username": "admin"},
					{"id": 2, "username": "operator"},
				},
			},
		},
	}
	body, _ := json.Marshal(migReq)
	req := httptest.NewRequest("POST", "/api/v1/apps/crm-portal/migrate-data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var migResp struct {
		Code int                    `json:"code"`
		Data MigrateAppDataResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &migResp); err != nil {
		t.Fatalf("unmarshal migrate resp: %v", err)
	}
	if migResp.Data.TablesMigrated != 1 || migResp.Data.RowsMigrated != 2 || migResp.Data.DatabaseName != "app_crm_portal" {
		t.Fatalf("unexpected migrate result: %+v", migResp.Data)
	}
	if migResp.Data.EnvVars["PGDATABASE"] != "app_crm_portal" {
		t.Fatalf("expected PGDATABASE=app_crm_portal, got %+v", migResp.Data.EnvVars)
	}

	// 2. Admin CRUD for resources
	newRes := model.DeployResource{
		ID:          "redis-local-test",
		Name:        "Test Redis",
		Type:        "redis",
		Host:        "10.0.0.2",
		Port:        6379,
		Description: "Redis cache",
		Enabled:     true,
	}
	resBody, _ := json.Marshal(newRes)
	resReq := httptest.NewRequest("POST", "/api/v1/apps/admin/resources", bytes.NewReader(resBody))
	resReq.Header.Set("Content-Type", "application/json")
	wRes := httptest.NewRecorder()
	r.ServeHTTP(wRes, resReq)
	if wRes.Code != http.StatusOK {
		t.Fatalf("expected 200 creating resource, got %d: %s", wRes.Code, wRes.Body.String())
	}

	// 3. Verify GET /api/v1/apps/resources filters out db-crm-portal by default and sets is_default per type
	wListRes := httptest.NewRecorder()
	r.ServeHTTP(wListRes, httptest.NewRequest("GET", "/api/v1/apps/resources", nil))
	if wListRes.Code != http.StatusOK {
		t.Fatalf("expected 200 listing resources, got %d", wListRes.Code)
	}
	var listResResp struct {
		Resources []model.ResourceSummary `json:"resources"`
	}
	if err := json.Unmarshal(wListRes.Body.Bytes(), &listResResp); err != nil {
		t.Fatalf("unmarshal resources: %v", err)
	}
	for _, item := range listResResp.Resources {
		if item.ID == "db-crm-portal" {
			t.Fatalf("expected app-specific db-crm-portal to be hidden from global /api/v1/apps/resources")
		}
		if (item.ID == "pg-test" || item.ID == "redis-local-test") && !item.IsDefault {
			t.Fatalf("expected %s to be marked is_default=true, got %+v", item.ID, item)
		}
	}

	// 4. Verify adding another redis resource with is_default=true switches the default for redis
	newRes2 := model.DeployResource{
		ID:          "redis-main",
		Name:        "Main Redis",
		Type:        "redis",
		IsDefault:   true,
		Host:        "10.0.0.3",
		Port:        6379,
		Description: "Primary Redis",
		Enabled:     true,
	}
	resBody2, _ := json.Marshal(newRes2)
	resReq2 := httptest.NewRequest("POST", "/api/v1/apps/admin/resources", bytes.NewReader(resBody2))
	resReq2.Header.Set("Content-Type", "application/json")
	wRes2 := httptest.NewRecorder()
	r.ServeHTTP(wRes2, resReq2)
	if wRes2.Code != http.StatusOK {
		t.Fatalf("expected 200 creating second redis resource, got %d", wRes2.Code)
	}

	wListRes2 := httptest.NewRecorder()
	r.ServeHTTP(wListRes2, httptest.NewRequest("GET", "/api/v1/apps/resources?slug=crm-portal", nil))
	if err := json.Unmarshal(wListRes2.Body.Bytes(), &listResResp); err != nil {
		t.Fatalf("unmarshal resources with slug: %v", err)
	}
	foundOwnDB := false
	for _, item := range listResResp.Resources {
		if item.ID == "db-crm-portal" {
			foundOwnDB = true
			if item.IsDefault {
				t.Fatalf("expected db-crm-portal to have is_default=false")
			}
		}
		if item.ID == "redis-main" && !item.IsDefault {
			t.Fatalf("expected redis-main to be default")
		}
		if item.ID == "redis-local-test" && item.IsDefault {
			t.Fatalf("expected redis-local-test to no longer be default")
		}
	}
	if !foundOwnDB {
		t.Fatalf("expected ?slug=crm-portal to include db-crm-portal")
	}
}

func makeTestSkillZip(t *testing.T, slug string, apiPaths []string, pagePaths []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	fSkill, _ := zw.Create("SKILL.md")
	_, _ = fSkill.Write([]byte(fmt.Sprintf("---\nname: app-%s\n---\n# %s\n", slug, slug)))

	apis := make([]map[string]interface{}, 0, len(apiPaths))
	for i, p := range apiPaths {
		apis = append(apis, map[string]interface{}{
			"id":      fmt.Sprintf("op-%d", i+1),
			"method":  "GET",
			"path":    p,
			"purpose": "Query " + p,
			"effect":  "read",
		})
	}
	pages := make([]map[string]interface{}, 0, len(pagePaths))
	for _, p := range pagePaths {
		pages = append(pages, map[string]interface{}{
			"path":    p,
			"purpose": "View " + p,
		})
	}
	opsJSON, _ := json.Marshal(map[string]interface{}{
		"slug":  slug,
		"apis":  apis,
		"pages": pages,
	})
	fOps, _ := zw.Create("references/operations.json")
	_, _ = fOps.Write(opsJSON)
	_ = zw.Close()
	return buf.Bytes()
}

func TestHandler_AppVersionsUpgradeDownloadAndRollback(t *testing.T) {
	h, r := setupTestAppHandler(t)

	publishVersion := func(tarContent string, zipBytes []byte, note string) int {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		_ = writer.WriteField("name", "Order Portal")
		_ = writer.WriteField("slug", "order-portal")
		_ = writer.WriteField("description", "Order Management")
		_ = writer.WriteField("container_port", "8080")
		_ = writer.WriteField("language", "python")
		_ = writer.WriteField("runtime_version", "3.11")
		_ = writer.WriteField("dockerfile_content", "FROM python:3.11-slim\nCMD [\"python\", \"app.py\"]")
		_ = writer.WriteField("version_note", note)
		_ = writer.WriteField("app_config", `[{"key":"APP_ENV","value":"prod"}]`)
		_ = writer.WriteField("resource_ids", `["pg-test"]`)

		partTar, _ := writer.CreateFormFile("app_tar", "app.tar")
		_, _ = partTar.Write([]byte(tarContent))
		partZip, _ := writer.CreateFormFile("skill_zip", "skill.zip")
		_, _ = partZip.Write(zipBytes)
		_ = writer.Close()

		req := httptest.NewRequest("POST", "/api/v1/apps/publish", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("publish failed (%d): %s", w.Code, w.Body.String())
		}
		var res struct {
			Version int `json:"version"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return res.Version
	}

	markRunning := func(slug string) {
		putBody, _ := json.Marshal(map[string]string{"status": model.AppStatusRunning})
		reqPut := httptest.NewRequest("PUT", "/api/v1/apps/"+slug, bytes.NewReader(putBody))
		reqPut.Header.Set("Content-Type", "application/json")
		wPut := httptest.NewRecorder()
		r.ServeHTTP(wPut, reqPut)
		if wPut.Code != http.StatusOK {
			t.Fatalf("mark running failed (%d): %s", wPut.Code, wPut.Body.String())
		}
	}

	// 1. Publish v1 attempt 1 (fails in background because caproverClient is nil)
	zipV1Draft := makeTestSkillZip(t, "order-portal", []string{"/api/orders"}, []string{"/orders"})
	v1Try1 := publishVersion("tar-bytes-v1-broken", zipV1Draft, "首次发布尝试 1")
	if v1Try1 != 1 {
		t.Fatalf("expected version 1, got %d", v1Try1)
	}
	// Ensure background goroutine marked v1 as failed
	_ = h.storage.CompleteAppVersionDeploy(t.Context(), "order-portal", 1, model.AppStatusFailed, 0, "", "", "build failed", "healthz failed")

	// Attempting to rollback to a failed version must be rejected with 400
	rbFailedBody, _ := json.Marshal(RollbackAppVersionRequest{TargetVersion: 1, Reason: "try rollback failed"})
	reqRBFailed := httptest.NewRequest("POST", "/api/v1/apps/order-portal/rollback", bytes.NewReader(rbFailedBody))
	reqRBFailed.Header.Set("Content-Type", "application/json")
	wRBFailed := httptest.NewRecorder()
	r.ServeHTTP(wRBFailed, reqRBFailed)
	if wRBFailed.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when rolling back to failed version, got %d: %s", wRBFailed.Code, wRBFailed.Body.String())
	}

	// 1b. Retry publishing after failure -> must reuse v1 (NOT increment to v2)
	zipV1 := makeTestSkillZip(t, "order-portal", []string{"/api/orders"}, []string{"/orders"})
	v1 := publishVersion("tar-bytes-v1", zipV1, "首次发布 v1")
	if v1 != 1 {
		t.Fatalf("expected retry of failed v1 to reuse version 1, got %d", v1)
	}
	markRunning("order-portal")

	// 2. Publish v2 after v1 is running (2 APIs, 2 Pages) -> increments to v2
	zipV2 := makeTestSkillZip(t, "order-portal", []string{"/api/orders", "/api/orders/export"}, []string{"/orders", "/orders/analytics"})
	v2 := publishVersion("tar-bytes-v2", zipV2, "新增导出接口与分析页 v2")
	if v2 != 2 {
		t.Fatalf("expected version 2, got %d", v2)
	}
	markRunning("order-portal")

	// 3. List versions
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, httptest.NewRequest("GET", "/api/v1/apps/order-portal/versions", nil))
	if wList.Code != http.StatusOK {
		t.Fatalf("list versions failed (%d): %s", wList.Code, wList.Body.String())
	}
	var listResp struct {
		CurrentVersion int                        `json:"current_version"`
		Versions       []AppVersionDetailResponse `json:"versions"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal versions: %v", err)
	}
	if listResp.CurrentVersion != 2 || len(listResp.Versions) != 2 {
		t.Fatalf("expected current_version=2 and 2 versions, got %+v", listResp)
	}
	if len(listResp.Versions[0].APIsSummary) != 2 || len(listResp.Versions[1].APIsSummary) != 1 {
		t.Fatalf("unexpected APIsSummary counts: v2=%d, v1=%d", len(listResp.Versions[0].APIsSummary), len(listResp.Versions[1].APIsSummary))
	}

	// 4. Download v1's app.tar and skill.zip via platform-opc artifact endpoint
	wTar := httptest.NewRecorder()
	r.ServeHTTP(wTar, httptest.NewRequest("GET", "/api/v1/apps/order-portal/versions/1/artifacts/app.tar", nil))
	if wTar.Code != http.StatusOK || wTar.Body.String() != "tar-bytes-v1" {
		t.Fatalf("download v1 app.tar mismatch: code=%d body=%q", wTar.Code, wTar.Body.String())
	}

	wZip := httptest.NewRecorder()
	r.ServeHTTP(wZip, httptest.NewRequest("GET", "/api/v1/apps/order-portal/versions/1/artifacts/skill.zip", nil))
	if wZip.Code != http.StatusOK || !bytes.Equal(wZip.Body.Bytes(), zipV1) {
		t.Fatalf("download v1 skill.zip mismatch: code=%d len=%d", wZip.Code, wZip.Body.Len())
	}

	// 5. Rollback from v2 to v1
	rbBody, _ := json.Marshal(RollbackAppVersionRequest{
		TargetVersion: 1,
		Reason:        "v2 新增导出接口有问题，回退到 v1",
	})
	reqRB := httptest.NewRequest("POST", "/api/v1/apps/order-portal/rollback", bytes.NewReader(rbBody))
	reqRB.Header.Set("Content-Type", "application/json")
	wRB := httptest.NewRecorder()
	r.ServeHTTP(wRB, reqRB)
	if wRB.Code != http.StatusOK {
		t.Fatalf("rollback failed (%d): %s", wRB.Code, wRB.Body.String())
	}
	var rbResp struct {
		OK              bool                     `json:"ok"`
		PreviousVersion int                      `json:"previous_version"`
		CurrentVersion  int                      `json:"current_version"`
		Version         AppVersionDetailResponse `json:"version"`
	}
	if err := json.Unmarshal(wRB.Body.Bytes(), &rbResp); err != nil {
		t.Fatalf("unmarshal rollback response: %v", err)
	}
	if !rbResp.OK || rbResp.PreviousVersion != 2 || rbResp.CurrentVersion != 1 || !rbResp.Version.IsActive {
		t.Fatalf("unexpected rollback response: %+v", rbResp)
	}

	// 6. Test Storage Endpoints
	wAdminStorage := httptest.NewRecorder()
	r.ServeHTTP(wAdminStorage, httptest.NewRequest("GET", "/api/v1/apps/admin/storage", nil))
	if wAdminStorage.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/apps/admin/storage failed (%d): %s", wAdminStorage.Code, wAdminStorage.Body.String())
	}
	var globalStorage model.GlobalStorageSummary
	if err := json.Unmarshal(wAdminStorage.Body.Bytes(), &globalStorage); err != nil {
		t.Fatalf("unmarshal global storage summary: %v", err)
	}
	if globalStorage.ServerDisk.TotalHuman == "" {
		t.Fatalf("expected non-empty ServerDisk.TotalHuman, got %+v", globalStorage.ServerDisk)
	}

	wAppStorage := httptest.NewRecorder()
	r.ServeHTTP(wAppStorage, httptest.NewRequest("GET", "/api/v1/apps/order-portal/storage", nil))
	if wAppStorage.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/apps/order-portal/storage failed (%d): %s", wAppStorage.Code, wAppStorage.Body.String())
	}
	var appStorage model.AppStorageOverview
	if err := json.Unmarshal(wAppStorage.Body.Bytes(), &appStorage); err != nil {
		t.Fatalf("unmarshal app storage detail: %v", err)
	}
	if appStorage.AppSlug != "order-portal" {
		t.Fatalf("expected app_slug order-portal, got %s", appStorage.AppSlug)
	}

	// 7. Test RecheckAppHealth
	wRecheck := httptest.NewRecorder()
	r.ServeHTTP(wRecheck, httptest.NewRequest("POST", "/api/v1/apps/order-portal/recheck", nil))
	if wRecheck.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/apps/order-portal/recheck failed (%d): %s", wRecheck.Code, wRecheck.Body.String())
	}
	var recheckResp map[string]interface{}
	if err := json.Unmarshal(wRecheck.Body.Bytes(), &recheckResp); err != nil {
		t.Fatalf("unmarshal recheck response: %v", err)
	}
	if recheckResp["status"] == "" {
		t.Fatalf("expected status field in recheck response, got %+v", recheckResp)
	}

	// 8. Test DownloadAppStorageBackup Whitelist Validation
	wBadBackup := httptest.NewRecorder()
	r.ServeHTTP(wBadBackup, httptest.NewRequest("GET", "/api/v1/apps/order-portal/storage/backup?path=/etc/shadow", nil))
	if wBadBackup.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unauthorized backup path /etc/shadow, got %d: %s", wBadBackup.Code, wBadBackup.Body.String())
	}

	// 9. Test Purge Orphan Volumes
	wPurge := httptest.NewRecorder()
	purgeBody, _ := json.Marshal(PurgeOrphanVolumesRequest{VolumeNames: []string{"bad-name"}})
	reqPurge := httptest.NewRequest("POST", "/api/v1/apps/admin/storage/orphan-volumes/purge", bytes.NewReader(purgeBody))
	reqPurge.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPurge, reqPurge)
	if wPurge.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/apps/admin/storage/orphan-volumes/purge failed (%d): %s", wPurge.Code, wPurge.Body.String())
	}
}
