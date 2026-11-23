package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestPreviewLaunchUsesTemporaryResources(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	h.cfg.Auth.Key = "fixture-signing-key"
	attempt := model.AppDeploymentVersion{
		ID: uuid.New(), AppSlug: "preview-app", Status: model.AppStatusActivating,
		DeliveryStep: deliveryPreviewQueued, ContainerPort: 5432,
		AppConfigJSON:     `[{"key":"SERVICE_DATABASE","resource_id":"pg-test","resource_key":"DATABASE_URL"}]`,
		StorageMountsJSON: `[{"container_path":"/data/attachments","host_path":"/production/data","volume_name":"formal-volume"}]`,
	}
	requests := make(chan map[string]json.RawMessage, 1)
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/login":
			fmt.Fprint(w, `{"status":100,"data":{"token":"fixture"}}`)
		case r.URL.Path == "/api/v2/user/system/application-storage/":
			fmt.Fprint(w, `{"status":100,"data":{"volumeRoots":[]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/user/apps/runtime/preview-app/candidates/"+attempt.ID.String()+"/sandbox":
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode launch request: %v", err)
			}
			requests <- payload
			fmt.Fprint(w, `{"status":100}`)
		default:
			t.Errorf("unexpected Deploy operation: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer cap.Close()
	h.SetCapRoverClient(deploy.NewCapRoverClient(cap.URL, "fixture", "apps.example.test"))
	if err := h.storage.CreateAppVersion(t.Context(), &attempt); err != nil {
		t.Fatal(err)
	}
	h.executeAppDelivery(t.Context(), attempt)
	var saved model.AppDeploymentVersion
	if err := h.storage.DeployDB().First(&saved, "id = ?", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.DeliveryStep != deliveryPreviewRunning || saved.Status != model.AppStatusRuntimeReady || saved.Version != 0 {
		t.Fatalf("unexpected candidate status %s/%s: %s", saved.Status, saved.DeliveryStep, saved.ErrorMessage)
	}
	payload := <-requests
	if got := string(payload["mounts"]); got != `["/data/attachments"]` {
		t.Fatalf("expected only the container mount path: %s", got)
	}
	if env := string(payload["env"]); !strings.Contains(env, `"key":"SERVICE_DATABASE"`) || !strings.Contains(env, "@127.0.0.1:15432/opc_preview") || strings.Contains(env, "10.0.0.1") {
		t.Fatal("candidate database environment did not use a separate local database and port")
	}
	if services := string(payload["services"]); !strings.Contains(services, `"kind":"postgres","port":15432`) {
		t.Fatal("candidate database service was not requested")
	}
}

func TestPreviewResourcesKeepBindingsScopedByResourceAndReceipt(t *testing.T) {
	configs := []model.AppConfigItem{
		{Key: "PRIMARY", ResourceID: "pg1", ResourceKey: "DATABASE_URL"},
		{Key: "REPORTING", ResourceID: "pg2", ResourceKey: "DATABASE_URL"},
		{Key: "CACHE_HOST", ResourceID: "redis", ResourceKey: "REDIS_HOST"},
		{Key: "CACHE_PORT", ResourceID: "redis", ResourceKey: "REDIS_PORT"},
		{Key: "EXTERNAL_URL", ResourceID: "external", Value: "https://production.example.test"},
	}
	types := map[string]string{"pg1": "postgresql", "pg2": "postgres", "redis": "redis", "external": "oss"}
	attempt := model.AppDeploymentVersion{ID: uuid.New(), ContainerPort: 8080}
	env, services, skipped, err := previewEnvironment(attempt, "fixture-key", configs, types)
	if err != nil || len(env) != 4 || len(services) != 3 || len(skipped) != 1 || skipped[0] != "EXTERNAL_URL" {
		t.Fatalf("unexpected candidate bindings: %d env, %d services, %v skipped, %v", len(env), len(services), skipped, err)
	}
	if services[0].Port != 5432 || services[1].Port != 15432 || services[0].Password == services[1].Password || services[2].Password != "" || env[2].Value != "127.0.0.1" || env[3].Value != "6379" {
		t.Fatal("candidate resources were not separated or the unauthenticated cache gained a password")
	}
	previous := services[0].Password
	attempt.ID = uuid.New()
	_, services, _, err = previewEnvironment(attempt, "fixture-key", configs, types)
	if err != nil || services[0].Password == previous {
		t.Fatal("preview database credentials must change with the delivery receipt")
	}
}
