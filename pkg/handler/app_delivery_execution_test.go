package handler

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestArchivedDeliveryBuildAndRuntimeAssessments(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	h.cfg.SystemAESKey = strings.Repeat("k", 32)
	const fixtureValue = "fixture-direct-secret-4042"
	const slug = "fresh-app"
	const image = "img-captain-fresh-app:1"
	var starts atomic.Int32
	var injected atomic.Bool
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, _ := io.ReadAll(r.Body)
		if bytes.Contains(payload, []byte(fixtureValue)) {
			injected.Store(true)
		}
		switch {
		case r.URL.Path == "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"data":{"token":"fixture"}}`))
		case r.URL.Path == "/api/v2/user/apps/appDefinitions" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"appDefinitions":[{"appName":%q,"deployedVersion":1,"versions":[{"version":1,"deployedImageName":%q}]}]}}`, slug, image)
		case strings.Contains(r.URL.Path, "/builds/") && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"status":"built","image":%q}}`, image)
		case strings.Contains(r.URL.Path, "/builds/"):
			_, _ = w.Write([]byte(`{"status":101}`))
		case strings.Contains(r.URL.Path, "/runtime/"):
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"desiredInstances":1,"currentTasks":[{"image":%q,"state":"running"}]}}`, image)
		case strings.Contains(r.URL.Path, "/appData/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"status":100,"data":{"isAppBuilding":false,"isBuildFailed":false}}`))
		case strings.Contains(r.URL.Path, "/appData/"):
			starts.Add(1)
			_, _ = w.Write([]byte(`{"status":101}`))
		default:
			_, _ = w.Write([]byte(`{"status":100}`))
		}
	}))
	defer cap.Close()
	client := deploy.NewCapRoverClient(cap.URL, "cap-password", "apps.opc.intra")
	client.PollInterval = time.Millisecond
	h.SetCapRoverClient(client)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(appOwnerKey, enterpriseAppOwner("fixture", "owner")) })
	router.POST("/deliveries", h.SaveAppDeliveryRevision)
	router.POST("/publish", h.PublishApp)
	router.GET("/:slug/requests/:request_id", h.GetAppPublishRequest)
	router.POST("/:slug/requests/:request_id/assessment", h.AssessAppDelivery)
	router.GET("/:slug/deliveries/:revision/artifacts/:filename", h.DownloadAppDeliveryRevision)
	router.GET("/:slug/versions/:version/artifacts/:filename", h.DownloadAppVersionArtifact)
	router.GET("/:slug/versions", h.ListAppVersions)
	router.PUT("/:slug/config", h.UpdateAppConfig)

	var archive bytes.Buffer
	files := tar.NewWriter(&archive)
	body := []byte("FROM scratch\n")
	if err := files.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	plan := fmt.Sprintf(`{"effective_configs":[{"key":"DATA_CIPHER","value":%q}],"format":1,"slug":"fresh-app","base_version":0,"config_format":1,"summary":"fixture","contract":{"slug":"fresh-app","configs":[{"key":"DATA_CIPHER","secret":true,"required":true,"preserve_on_upgrade":true,"value_plan":{"kind":"input"}}],"runtime":{"port":8080},"storage_mounts":[],"data_plan":{"strategy":"initialize","summary":"empty fixture","recovery":"nothing to restore"}},"files":[]}`, fixtureValue)
	rev := uuid.NewString()
	submit := func(endpoint string, fields map[string]string, saving bool) *httptest.ResponseRecorder {
		t.Helper()
		var requestBody bytes.Buffer
		multipartWriter := multipart.NewWriter(&requestBody)
		for k, v := range fields {
			if err := multipartWriter.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		artifacts := map[string][]byte{"app_tar": archive.Bytes(), "skill_zip": []byte("skill fixture")}
		if saving {
			artifacts["delivery_plan"] = []byte(plan)
		}
		for field, artifact := range artifacts {
			part, err := multipartWriter.CreateFormFile(field, field)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(artifact); err != nil {
				t.Fatal(err)
			}
		}
		if err := multipartWriter.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, endpoint, &requestBody)
		request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, request)
		return reply
	}
	save := submit("/deliveries", map[string]string{"slug": slug, "revision": rev}, true)
	if save.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", save.Code, save.Body)
	}
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", "/"+slug+"/deliveries/"+rev+"/artifacts/delivery.json", nil))
	if out.Code != http.StatusOK || out.Body.String() != plan {
		t.Fatalf("archived plan: %d %s", out.Code, out.Body)
	}

	pub := submit("/publish", map[string]string{"slug": slug, "name": "Fresh", "request_id": "test-request",
		"storage_mounts": "[]", "delivery_revision": rev, "delivery_plan": plan,
		"app_config": fmt.Sprintf(`[{"key":"DATA_CIPHER","value":%q}]`, fixtureValue)}, false)
	if pub.Code != http.StatusAccepted {
		t.Fatalf("publish: %d %s", pub.Code, pub.Body)
	}
	requestPath := "/" + slug + "/requests/test-request"
	wait := func(expected string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			reply := httptest.NewRecorder()
			router.ServeHTTP(reply, httptest.NewRequest("GET", requestPath, nil))
			var receipt map[string]any
			_ = json.Unmarshal(reply.Body.Bytes(), &receipt)
			if receipt["status"] == expected {
				return
			}
			if receipt["status"] == "failed" {
				t.Fatalf("delivery failed: %s", reply.Body)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", expected)
	}
	assess := func(phase string, status int) {
		t.Helper()
		content := fmt.Sprintf(`{"phase":%q,"outcome":"passed","report":"AI confirmed from recorded facts","evidence":["observed fixture"]}`, phase)
		req := httptest.NewRequest(http.MethodPost, requestPath+"/assessment", strings.NewReader(content))
		req.Header.Set("Content-Type", "application/json")
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, req)
		if reply.Code != status {
			t.Fatalf("%s assessment: %d %s", phase, reply.Code, reply.Body)
		}
	}
	wait("built")
	if starts.Load() != 0 {
		t.Fatal("candidate ran before build assessment")
	}
	assess("runtime", http.StatusConflict)
	assess("build", http.StatusAccepted)
	wait("runtime_ready")
	if starts.Load() != 1 {
		t.Fatal("approved image was not started exactly once")
	}
	before, err := h.storage.GetAppBySlug(t.Context(), slug)
	if err != nil || before.CurrentVersion != 0 {
		t.Fatalf("unassessed release was promoted: %+v %v", before, err)
	}
	assess("runtime", http.StatusOK)
	wait("running")
	assess("runtime", http.StatusOK)
	release, err := h.storage.GetActiveAppVersion(t.Context(), slug)
	if err != nil || release.Version != 1 || !strings.Contains(release.DeliveryAssessment, "observed fixture") {
		t.Fatalf("accepted release not persisted: %+v %v", release, err)
	}
	if !injected.Load() || release.ConfigRevision == "" || release.DeploymentSnapshotSHA == "" {
		t.Fatal("confirmed config did not reach runtime or durable version metadata")
	}
	if strings.Contains(release.AppConfigJSON, fixtureValue) || !strings.Contains(release.AppConfigJSON, "enc:v1:") {
		t.Fatal("protected app config was not encrypted in storage")
	}
	out = httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", "/"+slug+"/versions/1/artifacts/confirmed-deployment.json", nil))
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), release.ConfigRevision) || !strings.Contains(out.Body.String(), fixtureValue) ||
		out.Header().Get("X-Artifact-SHA256") != release.DeploymentSnapshotSHA {
		t.Fatal("downloaded configuration snapshot is missing, stale or drops the actual value")
	}
	out = httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", "/"+slug+"/versions", nil))
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"has_value":true`) || !strings.Contains(out.Body.String(), fixtureValue) || strings.Contains(out.Body.String(), "enc:v1:") {
		t.Fatal("version listing did not report the owner’s actual configuration value")
	}
	change := httptest.NewRequest("PUT", "/"+slug+"/config", strings.NewReader(`{"app_config":[{"key":"DATA_CIPHER","value":"","has_value":true}],"resource_ids":[]}`))
	change.Header.Set("Content-Type", "application/json")
	out = httptest.NewRecorder()
	router.ServeHTTP(out, change)
	if out.Code != http.StatusOK {
		t.Fatalf("could not preserve and revise protected runtime configuration: %d %s", out.Code, out.Body)
	}
	updated, err := h.storage.GetActiveAppVersion(t.Context(), slug)
	if err != nil || updated.ConfigRevision == release.ConfigRevision || updated.Version != release.Version {
		t.Fatal("runtime configuration update did not preserve the code version and allocate a new config revision")
	}
	var initialConfig, updatedConfig []model.AppConfigItem
	if json.Unmarshal([]byte(release.AppConfigJSON), &initialConfig) != nil ||
		json.Unmarshal([]byte(updated.AppConfigJSON), &updatedConfig) != nil ||
		len(initialConfig) != 1 || len(updatedConfig) != 1 || initialConfig[0].Value != updatedConfig[0].Value {
		t.Fatal("runtime configuration revision did not preserve the encrypted value")
	}
	previousSnapshot := deploy.BuildVersionObjectKey(slug, release.Version, confirmedSnapshotFilename(release.ConfigRevision))
	if !h.resolveArtifactStore(t.Context()).ObjectExists(t.Context(), previousSnapshot) {
		t.Fatal("the old confirmed deployment snapshot was overwritten")
	}
}

func TestConfirmedUpgradeGuardsDataBindingsAndPersistentKey(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	h.cfg.SystemAESKey = strings.Repeat("k", 32)
	previous := []model.AppConfigItem{
		{Key: "DB_NAME", ResourceID: "pg-old", ResourceKey: "PGDATABASE"},
		{Key: "REDIS_DB", ResourceID: "redis-old", ResourceKey: "REDIS_DB"},
		{Key: "CIPHER", Value: "fixture-data-cipher", Secret: true, PreserveOnUpgrade: true},
	}
	stored, err := h.protectAppConfig(previous, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	resources := []config.OnlineResourceConfig{{ID: "pg-old", Type: "postgres"}, {ID: "redis-old", Type: "redis"}}
	next := slices.Clone(previous)
	next[2].PreserveOnUpgrade = false
	if err := h.protectExistingDataConfig(string(data), next, resources, false); err != nil || !next[2].PreserveOnUpgrade {
		t.Fatal("an unchanged key must stay protected across upgrades")
	}
	next[0].ResourceID = "pg-new"
	if err := h.protectExistingDataConfig(string(data), next, resources, false); err == nil {
		t.Fatal("reuse allowed an application to switch databases")
	}
	if err := h.protectExistingDataConfig(string(data), next, resources, true); err != nil {
		t.Fatal("the dedicated PostgreSQL migration path could not change its binding")
	}
	next[1].ResourceID = "redis-new"
	if err := h.protectExistingDataConfig(string(data), next, resources, true); err == nil {
		t.Fatal("PostgreSQL migration allowed non-PostgreSQL state to switch")
	}
	next[1].ResourceID = "redis-old"
	next[2].Value = "wrong-cipher"
	if err := h.protectExistingDataConfig(string(data), next, resources, false); err == nil {
		t.Fatal("a key used to read existing data could be silently replaced")
	}
	resources[0].EnvVars = map[string]string{"PGDATABASE": "old"}
	pinned := &model.AppDeploymentVersion{DataBindingDigestsJSON: dataResourceDigests(previous, resources)}
	resources[0].EnvVars["PGDATABASE"] = "new"
	if checkDataResourceDigests(pinned, previous, resources) == nil {
		t.Fatal("the same resource ID was allowed to point to a different database")
	}
}

func TestDeploymentValuesAreVisibleOnlyInAppContext(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	owner := enterpriseAppOwner("fixture", "owner")
	const slug = "visible-app"
	if err := h.storage.CreateOrUpdateApp(t.Context(), &model.DeployedApp{
		ID: uuid.New(), Slug: slug, Name: "Visible app", CreatedBy: owner, AppConfigJSON: `[{"key":"API_SECRET","value":"fixture-owner-value","secret":true}]`,
	}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(appOwnerKey, c.GetHeader("X-Test-Owner")) })
	router.GET("/:slug", h.GetApp)
	router.POST("/:slug/config-values", h.PreviewAppConfigValues)
	for _, requester := range []string{owner, enterpriseAppOwner("fixture", "other")} {
		req := httptest.NewRequest(http.MethodGet, "/"+slug, nil)
		req.Header.Set("X-Test-Owner", requester)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if out.Code != http.StatusOK || strings.Contains(out.Body.String(), "fixture-owner-value") != (requester == owner) {
			t.Fatal("plain application config did not follow ownership")
		}

		req = httptest.NewRequest(http.MethodPost, "/"+slug+"/config-values", strings.NewReader(
			`{"bindings":[{"key":"DB_DSN","resource_id":"pg-test","resource_key":"DATABASE_URL"}]}`))
		req.Header.Set("X-Test-Owner", requester)
		req.Header.Set("Content-Type", "application/json")
		out = httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if requester == owner {
			if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "postgresql://usr:pwd@") {
				t.Fatal("bound source did not return its actual application value")
			}
		} else if out.Code != http.StatusForbidden {
			t.Fatal("another app operator could preview these values")
		}
	}
}
