package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestDeploymentControlWaitsForOriginalBuildToStop(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	const slug, owner = "fixture-app", "fixture-owner"
	app := model.DeployedApp{ID: uuid.New(), Slug: slug, Name: "Fixture", CreatedBy: owner, Status: model.AppStatusRunning, CurrentVersion: 2}
	attempt := model.AppDeploymentVersion{ID: uuid.New(), AppSlug: slug, RequestID: "fixture-attempt", CreatedBy: owner,
		DeliveryRevision: uuid.New(), Status: model.AppStatusBuilding, DeliveryStep: deliveryDomainBuildQueued, DeliveryBuildSubmitted: true}
	ctx := context.Background()
	if err := h.storage.SavePublishedAppAndVersion(ctx, &app, &attempt); err != nil {
		t.Fatal(err)
	}
	reachable, stopped, removed := false, false, false
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/login":
			fmt.Fprint(w, `{"status":100,"data":{"token":"fixture"}}`)
		case strings.HasSuffix(r.URL.Path, "/builds/"+attempt.ID.String()) && r.Method == http.MethodGet:
			if !reachable {
				http.Error(w, "temporarily unavailable", 503)
			} else {
				fmt.Fprint(w, `{"status":100,"data":{"status":"building"}}`)
			}
		case strings.HasSuffix(r.URL.Path, "/builds/"+attempt.ID.String()) && r.Method == http.MethodDelete:
			fmt.Fprintf(w, `{"status":100,"data":{"stopped":%t}}`, stopped)
		case strings.HasSuffix(r.URL.Path, "/candidates/"+attempt.ID.String()) && r.Method == http.MethodDelete:
			removed = true
			fmt.Fprint(w, `{"status":100}`)
		default:
			t.Errorf("unexpected deployment call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer cap.Close()
	h.SetCapRoverClient(deploy.NewCapRoverClient(cap.URL, "fixture", "apps.example.test"))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(appOwnerKey, c.GetHeader("X-Test-Owner")) })
	router.GET("/:slug/requests/:request_id", h.GetAppPublishRequest)
	router.POST("/:slug/requests/:request_id/cancel", h.CancelAppPublishRequest)
	read := func(method, suffix, who string, expected int) map[string]any {
		request := httptest.NewRequest(method, "/"+slug+"/requests/"+attempt.RequestID+suffix, nil)
		request.Header.Set("X-Test-Owner", who)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("got HTTP %d, want %d: %s", response.Code, expected, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	expectPhase := func(phase string, retry bool) {
		t.Helper()
		body := read(http.MethodGet, "", owner, 200)
		view, ok := body["progress"].(map[string]any)
		if !ok || view["phase"] != phase || view["retry_safe"] != retry {
			t.Fatalf("unexpected progress: %#v", body)
		}
	}
	h.executeAppDelivery(ctx, attempt)
	expectPhase("unknown", false)
	read(http.MethodPost, "/cancel", "another-owner", 404)
	read(http.MethodPost, "/cancel", owner, 409)
	reload := func() {
		if err := h.storage.DeployDB().First(&attempt, "id = ?", attempt.ID).Error; err != nil {
			t.Fatal(err)
		}
	}
	reachable = true
	reload()
	h.executeAppDelivery(ctx, attempt)
	expectPhase("building", false)
	read(http.MethodPost, "/cancel", owner, 202)
	expectPhase("cancelling", false)
	reload()
	h.executeAppDelivery(ctx, attempt)
	expectPhase("cancelling", false)
	if removed {
		t.Fatal("preview was cleaned before Deploy confirmed build stopped")
	}
	stopped = true
	reload()
	h.executeAppDelivery(ctx, attempt)
	expectPhase("cancelled", true)
	if !removed {
		t.Fatal("attempt-owned preview was not cleaned")
	}
	current, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil || current.CurrentVersion != 2 || current.Status != model.AppStatusRunning {
		t.Fatalf("online app changed: %#v %v", current, err)
	}
}
