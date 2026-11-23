package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"weknora-tokendance/pkg/deploy"

	"weknora-tokendance/pkg/model"
)

func TestEditRuntimeConfigOnlyChangesEditableDeclaredValues(t *testing.T) {
	current := []model.AppConfigItem{
		{Key: "FEATURE_FLAG", Value: "false", Required: true, Description: "业务开关"},
		{Key: "PORT", Value: "8080"},
		{Key: "DB_HOST", Value: "db"},
		{Key: "BOUND", ResourceID: "redis"},
		{Key: "FIELD_KEY", Value: "old", PreserveOnUpgrade: true},
	}
	next, changes, err := editRuntimeConfig(current, map[string]string{"FEATURE_FLAG": "true"})
	if err != nil || len(changes) != 1 || changes["FEATURE_FLAG"] != "true" || next[0].Description != current[0].Description || current[0].Value != "false" {
		t.Fatalf("editable change was not preserved: %v", err)
	}
	for _, key := range []string{"PORT", "DB_HOST", "BOUND", "FIELD_KEY", "UNDECLARED"} {
		if _, _, err := editRuntimeConfig(current, map[string]string{key: "new"}); err == nil {
			t.Errorf("accepted protected configuration %s", key)
		}
	}
	if _, _, err := editRuntimeConfig(current, map[string]string{"FEATURE_FLAG": ""}); err == nil {
		t.Error("accepted empty required value")
	}
	if _, changes, err := editRuntimeConfig(current, map[string]string{"FEATURE_FLAG": "false"}); err != nil || len(changes) != 0 {
		t.Error("unchanged configuration was treated as an edit")
	}
}

func TestRuntimeManagementAppliesAndRecoversConfiguration(t *testing.T) {
	h, router := setupTestAppHandler(t)
	ctx := context.Background()
	slug := "runtime-app"
	var mutex sync.Mutex
	var rollout string
	currentValue := "original"
	restartCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		switch r.URL.Path {
		case "/api/v2/login":
			fmt.Fprint(w, `{"status":100,"data":{"token":"fixture"}}`)
		case "/api/v2/user/apps/runtime/" + slug + "/restart":
			var command struct {
				OperationID string
				Changes     []model.AppConfigItem
			}
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil || len(command.Changes) != 1 {
				http.Error(w, "invalid restart", http.StatusBadRequest)
				return
			}
			restartCount++
			rollout, currentValue = command.OperationID, command.Changes[0].Value
			fmt.Fprint(w, `{"status":100}`)
		case "/api/v2/user/apps/runtime/" + slug:
			running := 1
			if currentValue == "broken" {
				running = 0
			}
			fmt.Fprintf(w, `{"status":100,"data":{"DesiredInstances":1,"RolloutId":%q,"RolloutRunning":%d,"PreviousRunning":0}}`, rollout, running)
		case "/":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := deploy.NewCapRoverClient(server.URL, "fixture", "apps.example.test")
	client.AppReadyTimeout = 50 * time.Millisecond
	client.ProbeInterval = 5 * time.Millisecond
	h.SetCapRoverClient(client)
	db := h.storage.DeployDB()
	if err := db.AutoMigrate(&model.AppDeploymentVersion{}); err != nil {
		t.Fatal(err)
	}
	owner := "subject:test-owner"
	app := model.DeployedApp{ID: uuid.New(), Slug: slug, Name: "测试", AppURL: server.URL,
		CurrentVersion: 5, Status: "running", CreatedBy: owner, AppConfigJSON: `[{"key":"FEATURE_FLAG","value":"original"}]`}
	if err := db.Create(&app).Error; err != nil {
		t.Fatal(err)
	}
	release := model.AppDeploymentVersion{ID: uuid.New(), AppSlug: slug, IsActive: true,
		Version: 5, Status: "running", ConfigRevision: "initial", AppConfigJSON: app.AppConfigJSON}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	router.POST("/runtime/:slug", func(c *gin.Context) {
		c.Set(appOwnerKey, c.GetHeader("X-Test-Subject"))
		h.StartAppRuntimeManagement(c)
	})
	run := func(body, subject string, expected int) model.AppRuntimeOperation {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/runtime/"+slug, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Subject", subject)
		router.ServeHTTP(rr, req)
		if rr.Code != expected {
			t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
		}
		var operation model.AppRuntimeOperation
		if expected != http.StatusAccepted {
			return operation
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &operation); err != nil {
			t.Fatal(err)
		}
		if err := db.First(&operation, "id = ?", operation.ID).Error; err != nil {
			t.Fatal(err)
		}
		h.runAppRuntimeOperation(ctx, operation)
		if err := db.First(&operation, "id = ?", operation.ID).Error; err != nil {
			t.Fatal(err)
		}
		return operation
	}
	body := `{"kind":"config","base_version":5,"base_config_revision":"initial","changes":{"FEATURE_FLAG":"new"}}`
	run(body, "other", http.StatusForbidden)
	first := run(body, owner, http.StatusAccepted)
	if first.State != "succeeded" || first.ConfigNumber != 2 {
		t.Fatalf("successful first revision not recorded: %+v", first)
	}
	run(body, owner, http.StatusConflict)
	broken := run(fmt.Sprintf(`{"kind":"config","base_version":5,"base_config_revision":%q,"changes":{"FEATURE_FLAG":"broken"}}`, first.ConfigRevision), owner, http.StatusAccepted)
	if broken.State != "restored" || broken.ConfigNumber != 3 {
		t.Fatalf("failed configuration was not restored: %+v", broken)
	}
	active, err := h.storage.GetActiveAppVersion(ctx, slug)
	if err != nil || active.ConfigRevision != first.ConfigRevision || !strings.Contains(active.AppConfigJSON, `"value":"new"`) {
		t.Fatalf("active configuration was not preserved: %+v, %v", active, err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if currentValue != "new" || restartCount != 3 {
		t.Fatalf("unexpected runtime after recovery: %s, %d", currentValue, restartCount)
	}
}
