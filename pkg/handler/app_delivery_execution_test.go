package handler

import (
	"archive/tar"
	"bytes"
	"context"
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

func TestDeliveryLogsFollowTheRequestedCandidate(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	h.cfg.SystemAESKey = strings.Repeat("k", 32)
	const slug, owner = "demo-app", "fixture-owner"
	configs, err := h.protectAppConfig([]model.AppConfigItem{{Key: "APP_KEY", Value: "fixture-secret-value", Secret: true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	attempt := model.AppDeploymentVersion{
		ID: uuid.New(), AppSlug: slug, RequestID: "fixture-request", CreatedBy: owner,
		DeliveryRevision: uuid.New(), ProposedVersion: 2, Status: model.AppStatusBuilding,
		DeliveryStep: deliveryBuildQueued, BuildLog: "Waiting for Docker", AppConfigJSON: string(encoded),
	}
	app := model.DeployedApp{ID: uuid.New(), Slug: slug, Name: "Demo", CreatedBy: owner, CurrentVersion: 1, Status: model.AppStatusRunning, BuildLog: "Previous version output"}
	if err := h.storage.SavePublishedAppAndVersion(context.Background(), &app, &attempt); err != nil {
		t.Fatal(err)
	}
	var buildLookups, runtimeLookups, formalRuntimeLookups atomic.Int32
	var unavailable atomic.Bool
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/login":
			fmt.Fprint(w, `{"status":100,"data":{"token":"fixture"}}`)
		case "/api/v2/user/system/application-storage/":
			fmt.Fprint(w, `{"status":100,"data":{"volumeRoots":[]}}`)
		case "/api/v2/user/apps/appData/" + slug + "/builds/" + attempt.ID.String():
			buildLookups.Add(1)
			if unavailable.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"status":1000}`)
				return
			}
			fmt.Fprint(w, `{"status":100,"data":{"status":"building","log":"Step 2: downloading dependencies, fixture-secret-value"}}`)
		case "/api/v2/user/apps/runtime/" + slug + "/candidates/" + attempt.ID.String() + "/logs":
			runtimeLookups.Add(1)
			fmt.Fprint(w, `{"status":100,"data":{"logs":"Candidate server started"}}`)
		case "/api/v2/user/apps/appData/" + slug + "/logs":
			formalRuntimeLookups.Add(1)
			fmt.Fprint(w, `{"status":100,"data":{"logs":"Formal server running"}}`)
		default:
			t.Errorf("unexpected Deploy path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer cap.Close()
	h.SetCapRoverClient(deploy.NewCapRoverClient(cap.URL, "fixture", "apps.example.test"))
	router := gin.New()
	router.GET("/admin/api/deploy/apps/:slug/logs", h.GetAppLogs)
	router.GET("/:slug/requests/:request_id", func(c *gin.Context) {
		c.Set(appOwnerKey, c.GetHeader("X-Test-Owner"))
		h.GetAppPublishRequest(c)
	})
	read := func(owner, query string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/"+slug+"/requests/"+attempt.RequestID+query, nil)
		req.Header.Set("X-Test-Owner", owner)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res.Code, res.Body.String()
	}
	const live = "?live_logs=1"
	if status, body := read(owner, ""); status != 200 || !strings.Contains(body, "Waiting for Docker") || buildLookups.Load() != 0 {
		t.Fatalf("ordinary status lookup: %d %s", status, body)
	}
	if status, body := read("another-owner", live); status != 404 || strings.Contains(body, "Waiting for Docker") || buildLookups.Load() != 0 {
		t.Fatalf("unauthorized log lookup: %d %s", status, body)
	}
	if status, body := read(owner, live); status != 200 || !strings.Contains(body, "Step 2: downloading dependencies, [REDACTED]") || strings.Contains(body, "Previous version output") || buildLookups.Load() != 1 {
		t.Fatalf("live candidate lookup: %d %s", status, body)
	}
	readAdmin := func() (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/deploy/apps/"+slug+"/logs", nil)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res.Code, res.Body.String()
	}
	if status, body := readAdmin(); status != 200 || !strings.Contains(body, "downloading dependencies") || !strings.Contains(body, "Formal server running") || strings.Contains(body, "Previous version output") {
		t.Fatalf("admin logs during an upgrade: %d %s", status, body)
	}
	if err := h.storage.DeployDB().Model(&app).Updates(map[string]any{"current_version": 0, "status": model.AppStatusBuilding}).Error; err != nil {
		t.Fatal(err)
	}
	if status, body := readAdmin(); status != 200 || !strings.Contains(body, "downloading dependencies") || !strings.Contains(body, `"runtime_source":"pending"`) || formalRuntimeLookups.Load() != 1 {
		t.Fatalf("admin logs during a first build: %d %s", status, body)
	}
	unavailable.Store(true)
	if status, body := read(owner, live); status != 200 || !strings.Contains(body, "Waiting for Docker") || !strings.Contains(body, `"logs_unavailable":true`) {
		t.Fatalf("transient failure must retain the receipt: %d %s", status, body)
	}
	if err := h.storage.DeployDB().Model(&attempt).Updates(map[string]any{"status": model.AppStatusRuntimeReady, "delivery_step": deliveryPreviewRunning, "build_log": "Build finished"}).Error; err != nil {
		t.Fatal(err)
	}
	if status, body := read(owner, live); status != 200 || !strings.Contains(body, `"build_log":"Build finished"`) || !strings.Contains(body, `"runtime_logs":"Candidate server started"`) || runtimeLookups.Load() != 1 {
		t.Fatalf("candidate runtime lookup: %d %s", status, body)
	}
	if status, body := readAdmin(); status != 200 || !strings.Contains(body, `"build_log":"Build finished"`) || !strings.Contains(body, `"runtime_logs":"Candidate server started"`) || !strings.Contains(body, `"runtime_source":"candidate"`) || formalRuntimeLookups.Load() != 1 {
		t.Fatalf("admin logs during candidate verification: %d %s", status, body)
	}
}

func TestArchivedDeliveryBuildAndRuntimeAssessments(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	h.cfg.SystemAESKey = strings.Repeat("k", 32)
	h.cfg.Auth.Key = "fixture-owner-probe-signing-key"
	const fixtureValue = "fixture-direct-secret-4042"
	const slug = "fresh-app"
	const image = "img-captain-fresh-app:1"
	const storageRoot = "/data-apps"
	const nodeID = "012345678901234567890abcd"
	const mounts = `[{"name":"uploads","kind":"uploads","container_path":"/app/uploads","resource_id":"fixture-storage"}]`
	if err := h.storage.UpsertDeployResource(t.Context(), &model.DeployResource{
		ID: "fixture-storage", Name: "Fixture storage", Type: deploy.LocalDirectoryVolume, Host: storageRoot, Enabled: true,
		EnvVars: map[string]string{"OPC_STORAGE_NODE_ID": nodeID},
	}); err != nil {
		t.Fatal(err)
	}
	var starts, candidateStarts atomic.Int32
	var injected atomic.Bool
	var allocatedPath atomic.Value
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, _ := io.ReadAll(r.Body)
		if bytes.Contains(payload, []byte(fixtureValue)) {
			injected.Store(true)
		}
		switch {
		case r.URL.Path == "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"data":{"token":"fixture"}}`))
		case r.URL.Path == "/api/v2/user/system/application-storage/paths/":
			var request struct {
				Root      string `json:"root"`
				NodeID    string `json:"node_id"`
				Directory string `json:"directory"`
				Create    bool   `json:"create"`
			}
			if err := json.Unmarshal(payload, &request); err != nil || request.Root != storageRoot || request.NodeID != nodeID || !strings.HasSuffix(request.Directory, "/uploads") {
				t.Errorf("invalid formal storage path request: %+v (%v)", request, err)
				_, _ = w.Write([]byte(`{"status":1100}`))
				return
			}
			requestedPath := request.Root + "/" + request.Directory
			if request.Create {
				allocatedPath.Store(requestedPath)
			} else if allocatedPath.Load() != requestedPath {
				_, _ = w.Write([]byte(`{"status":1100,"description":"path missing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":100}`))
		case r.URL.Path == "/api/v2/user/system/application-storage/paths/"+slug:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":[{"node_id":%q,"mounts":[{"type":"bind","source":%q,"target":"/app/uploads"}]}]}`, nodeID, allocatedPath.Load())
		case r.URL.Path == "/api/v2/user/apps/appDefinitions" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"appDefinitions":[{"appName":%q,"deployedVersion":1,"versions":[{"version":1,"deployedImageName":%q}]}]}}`, slug, image)
		case strings.Contains(r.URL.Path, "/builds/") && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"status":"built","image":%q}}`, image)
		case strings.Contains(r.URL.Path, "/builds/"):
			_, _ = w.Write([]byte(`{"status":101}`))
		case strings.Contains(r.URL.Path, "/candidates/") && strings.HasSuffix(r.URL.Path, "/probe"):
			_, _ = w.Write([]byte(`{"status":100,"data":{"status":200,"headers":{"content-type":["text/plain"]},"body":"isolated fixture"}}`))
		case strings.Contains(r.URL.Path, "/candidates/") && r.Method == http.MethodPost:
			candidateStarts.Add(1)
			_, _ = w.Write([]byte(`{"status":100}`))
		case strings.Contains(r.URL.Path, "/candidates/"):
			_, _ = w.Write([]byte(`{"status":100}`))
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
	router.Use(func(c *gin.Context) {
		c.Set(appOwnerKey, enterpriseAppOwner("fixture", "owner"+c.GetHeader("X-Test-Owner")))
	})
	router.POST("/deliveries", h.SaveAppDeliveryRevision)
	router.POST("/publish", h.PublishApp)
	router.GET("/:slug/requests/:request_id", h.GetAppPublishRequest)
	router.POST("/:slug/requests/:request_id/assessment", h.AssessAppDelivery)
	router.POST("/:slug/requests/:request_id/probe", h.ProbeAppDelivery)
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
	plan := fmt.Sprintf(`{"effective_configs":[{"key":"DATA_CIPHER","value":%q}],"format":1,"slug":"fresh-app","base_version":0,"config_format":1,"summary":"fixture","contract":{"slug":"fresh-app","configs":[{"key":"DATA_CIPHER","secret":true,"required":true,"preserve_on_upgrade":true,"value_plan":{"kind":"input"}}],"runtime":{"port":8080},"verification":{"targets":[{"id":"root-page","kind":"http","path":"/","method":"GET"}]},"storage_mounts":%s,"data_plan":{"strategy":"initialize","summary":"empty fixture","recovery":"nothing to restore"}},"files":[]}`, fixtureValue, mounts)
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
		"storage_mounts": mounts, "delivery_revision": rev, "delivery_plan": plan,
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
	runtimeProof := "fabricated proof"
	assess := func(phase string, status int) {
		t.Helper()
		evidence := `{"kind":"state","request_id":"test-request","result":{"status":"built"}}`
		if phase == "runtime" {
			evidence = fmt.Sprintf(`{"kind":"http","target":"root-page","request_id":"test-request","result":{"status":200,"proof":%q}}`, runtimeProof)
		}
		content, _ := json.Marshal(deliveryAssessment{Phase: phase, Outcome: "passed", Report: "AI confirmed from recorded facts", Evidence: []string{evidence}})
		req := httptest.NewRequest(http.MethodPost, requestPath+"/assessment", bytes.NewReader(content))
		req.Header.Set("Content-Type", "application/json")
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, req)
		if reply.Code != status {
			t.Fatalf("%s assessment: %d %s", phase, reply.Code, reply.Body)
		}
	}
	wait("built")
	if starts.Load() != 0 || candidateStarts.Load() != 0 {
		t.Fatal("candidate ran before build assessment")
	}
	assess("runtime", http.StatusConflict)
	assess("build", http.StatusAccepted)
	wait("runtime_ready")
	if starts.Load() != 0 || candidateStarts.Load() != 1 {
		t.Fatal("candidate did not start once in isolation before formal deployment")
	}
	probe := func(owner, request string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, requestPath+"/probe", strings.NewReader(request))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Owner", owner)
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, req)
		return reply
	}
	if wrong := probe("-other", `{"target":"root-page","method":"GET","path":"/"}`); wrong.Code != http.StatusNotFound {
		t.Fatalf("another owner obtained an isolated response: %d", wrong.Code)
	}
	if invalid := probe("", `{"target":"root-page","method":"GET","path":"//example.test"}`); invalid.Code != http.StatusBadRequest {
		t.Fatalf("candidate accepted a non-local path: %d", invalid.Code)
	}
	assess("runtime", http.StatusBadRequest)
	observed := probe("", `{"target":"root-page","method":"GET","path":"/"}`)
	var actual struct {
		Proof string `json:"proof"`
	}
	if observed.Code != http.StatusOK || !strings.Contains(observed.Body.String(), "isolated fixture") ||
		json.Unmarshal(observed.Body.Bytes(), &actual) != nil || actual.Proof == "" {
		t.Fatalf("owner could not obtain candidate evidence: %d %s", observed.Code, observed.Body)
	}
	runtimeProof = actual.Proof
	before, err := h.storage.GetAppBySlug(t.Context(), slug)
	if err != nil || before.CurrentVersion != 0 {
		t.Fatalf("unassessed release was promoted: %+v %v", before, err)
	}
	if allocatedPath.Load() != nil {
		t.Fatal("formal storage was allocated during isolated candidate verification")
	}
	assess("runtime", http.StatusAccepted)
	wait("running")
	if starts.Load() != 1 || allocatedPath.Load() == nil {
		t.Fatal("formal deployment did not allocate storage and start exactly once after AI approval")
	}
	assess("runtime", http.StatusOK)
	release, err := h.storage.GetActiveAppVersion(t.Context(), slug)
	if err != nil || release.Version != 1 || !strings.Contains(release.DeliveryAssessment, "root-page") {
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
	if out.Code != http.StatusConflict {
		t.Fatalf("direct configuration changed outside skills: %d %s", out.Code, out.Body)
	}
	unchanged, err := h.storage.GetActiveAppVersion(t.Context(), slug)
	if err != nil || unchanged.ConfigRevision != release.ConfigRevision || unchanged.AppConfigJSON != release.AppConfigJSON {
		t.Fatal("direct configuration changed the published version")
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
