package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"weknora-tokendance/config"
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
		apps.PUT("/:slug", h.AdminUpdateDeployApp)
		apps.DELETE("/:slug", h.AdminDeleteDeployApp)
		apps.PUT("/:slug/config", h.UpdateAppConfig)
		apps.POST("/:slug/migrate-data", h.MigrateAppData)
		apps.GET("/admin/resources", h.AdminListDeployResources)
		apps.POST("/admin/resources", h.AdminUpsertDeployResource)
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
}
