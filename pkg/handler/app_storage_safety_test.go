package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestHistoricalVolumesRemainReferenced(t *testing.T) {
	_, s, _ := setupKnowledgeEnv(t, newFakeKnowledge())
	if err := s.DeployDB().AutoMigrate(&model.DeployedApp{}, &model.AppDeploymentVersion{}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, nil, nil, nil, nil, s)
	v := model.AppDeploymentVersion{AppSlug: "old-app", Version: 1, StorageMountsJSON: `[{"volume_name":"opc-vol-old-data"}]`}
	if err := s.DeployDB().Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	refs, err := h.referencedAppVolumes(context.Background())
	if err != nil || !refs["opc-vol-old-data"] {
		t.Fatalf("historical volume unprotected: %v %v", refs, err)
	}
}

func TestUpgradeFailureRecoveryAndFailureState(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	ctx := context.Background()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"recover-app"}`))
	}))
	defer appSrv.Close()

	// Seed v1 as active running version with image identity
	v1App := &model.DeployedApp{
		Name:           "Recover App",
		Slug:           "recover-app",
		AppURL:         appSrv.URL,
		ContainerPort:  8080,
		Status:         model.AppStatusRunning,
		CurrentVersion: 1,
		AppConfigJSON:  `[{"key":"MODE","value":"v1"}]`,
	}
	v1Ver := &model.AppDeploymentVersion{
		AppSlug:           "recover-app",
		Version:           1,
		Status:            model.AppStatusRunning,
		IsActive:          true,
		DeployedImageName: "img-captain-recover-app:1",
		ContainerPort:     8080,
		AppConfigJSON:     `[{"key":"MODE","value":"v1"}]`,
		AppTarSHA256:      "sha-v1-tar",
		SkillZipSHA256:    "sha-v1-zip",
	}
	if err := h.storage.SavePublishedAppAndVersion(ctx, v1App, v1Ver); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	// Prepare v2 upgrade record
	v2App := *v1App
	v2App.CurrentVersion = 2
	v2App.Status = model.AppStatusBuilding
	v2Ver := &model.AppDeploymentVersion{
		AppSlug:       "recover-app",
		Version:       2,
		Status:        model.AppStatusBuilding,
		IsActive:      false,
		ContainerPort: 8080,
		AppConfigJSON: `[{"key":"MODE","value":"v2"}]`,
	}
	if err := h.storage.SavePublishedAppAndVersion(ctx, &v2App, v2Ver); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	// Mock CapRover where UploadAppTar fails (simulating v2 build/upload failure), while DeployExistingImage succeeds
	capSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"data":{"token":"tok"}}`))
		case strings.HasPrefix(req.URL.Path, "/api/v2/user/apps/appData/"):
			if req.Method == "POST" && strings.Contains(req.Header.Get("Content-Type"), "multipart/form-data") {
				_, _ = w.Write([]byte(`{"status":1100,"description":"simulated v2 upload failure"}`))
				return
			}
			if req.Method == "GET" {
				_, _ = w.Write([]byte(`{"status":100,"data":{"isAppBuilding":false,"isBuildFailed":false,"logs":{"lines":["recovered v1"]}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":100,"data":{}}`))
		default:
			if writeRunningAppFixture(w, req, "recover-app", 1) {
				return
			}
			_, _ = w.Write([]byte(`{"status":100,"data":{}}`))
		}
	}))
	defer capSrv.Close()
	capClient := deploy.NewCapRoverClient(capSrv.URL, "pass", "apps.opc.intra")
	capClient.PollInterval = 10 * time.Millisecond
	capClient.ProbeInterval = 10 * time.Millisecond
	h.SetCapRoverClient(capClient)

	h.tryLockDeploySlug("recover-app")
	h.runDeploymentAsync("recover-app", 2, 8080, appSrv.URL, nil, nil, nil, []byte("v2-tar"), []byte("v2-zip"))

	afterApp, err := h.storage.GetAppBySlug(ctx, "recover-app")
	if err != nil {
		t.Fatalf("get app after recovery: %v", err)
	}
	if afterApp.Status != model.AppStatusRunning || afterApp.CurrentVersion != 1 {
		t.Fatalf("expected app restored to v1 running, got status=%s version=%d err=%s", afterApp.Status, afterApp.CurrentVersion, afterApp.ErrorMessage)
	}
	v2After, err := h.storage.GetAppVersion(ctx, "recover-app", 2)
	if err != nil || v2After.Status != model.AppStatusFailed || v2After.IsActive {
		t.Fatalf("expected v2 marked failed and inactive, got %+v", v2After)
	}

	// Now test when v1 has NO DeployedImageName: recovery must fail closed (status=failed, not fake running)
	_ = h.storage.DeployDB().Model(&model.AppDeploymentVersion{}).Where("app_slug = ? AND version = 1", "recover-app").Update("deployed_image_name", "").Error
	v3App := *afterApp
	v3App.CurrentVersion = 3
	v3App.Status = model.AppStatusBuilding
	v3Ver := &model.AppDeploymentVersion{
		AppSlug:       "recover-app",
		Version:       3,
		Status:        model.AppStatusBuilding,
		IsActive:      false,
		ContainerPort: 8080,
	}
	if err := h.storage.SavePublishedAppAndVersion(ctx, &v3App, v3Ver); err != nil {
		t.Fatalf("save v3: %v", err)
	}
	h.tryLockDeploySlug("recover-app")
	h.runDeploymentAsync("recover-app", 3, 8080, appSrv.URL, nil, nil, nil, []byte("v3-tar"), []byte("v3-zip"))

	afterFailedRecovery, err := h.storage.GetAppBySlug(ctx, "recover-app")
	if err != nil {
		t.Fatalf("get app after failed recovery: %v", err)
	}
	if afterFailedRecovery.Status != model.AppStatusFailed || !strings.Contains(afterFailedRecovery.ErrorMessage, "previous version recovery failed") {
		t.Fatalf("expected failed status when recovery lacks image identity, got status=%s err=%s", afterFailedRecovery.Status, afterFailedRecovery.ErrorMessage)
	}
}

func TestUpgradeSkipsPreBuildRestartAndRollbackPreservesAddedMounts(t *testing.T) {
	h, r := setupTestAppHandler(t)
	ctx := context.Background()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"up-app"}`))
	}))
	defer appSrv.Close()

	// Seed v1 running with 1 mount (/app/data) and v2 running with 2 mounts (/app/data, /app/uploads)
	v1Mounts := `[{"name":"data","container_path":"/app/data","volume_name":"opc-vol-up-app-data"}]`
	v2Mounts := `[{"name":"data","container_path":"/app/data","volume_name":"opc-vol-up-app-data"},{"name":"uploads","container_path":"/app/uploads","volume_name":"opc-vol-up-app-uploads"}]`

	_ = h.storage.SavePublishedAppAndVersion(ctx, &model.DeployedApp{
		Name:              "Up App",
		Slug:              "up-app",
		AppURL:            appSrv.URL,
		ContainerPort:     8080,
		Status:            model.AppStatusRunning,
		CurrentVersion:    1,
		StorageMountsJSON: v1Mounts,
	}, &model.AppDeploymentVersion{
		AppSlug:           "up-app",
		Version:           1,
		Status:            model.AppStatusRunning,
		IsActive:          true,
		DeployedImageName: "img-captain-up-app:1",
		ContainerPort:     8080,
		StorageMountsJSON: v1Mounts,
	})

	_ = h.storage.SavePublishedAppAndVersion(ctx, &model.DeployedApp{
		Name:              "Up App",
		Slug:              "up-app",
		AppURL:            appSrv.URL,
		ContainerPort:     8080,
		Status:            model.AppStatusRunning,
		CurrentVersion:    2,
		StorageMountsJSON: v2Mounts,
	}, &model.AppDeploymentVersion{
		AppSlug:           "up-app",
		Version:           2,
		Status:            model.AppStatusRunning,
		IsActive:          true,
		DeployedImageName: "img-captain-up-app:2",
		ContainerPort:     8080,
		StorageMountsJSON: v2Mounts,
	})
	// Mark v1 inactive since v2 became active
	_ = h.storage.DeployDB().Model(&model.AppDeploymentVersion{}).Where("app_slug = ? AND version = 1", "up-app").Update("is_active", false).Error

	var uploaded atomic.Bool
	var runtimeVersion atomic.Int32
	runtimeVersion.Store(3)
	var nonPrestageUpdateBeforeUpload atomic.Bool
	var prestageUpdateBeforeUpload atomic.Bool
	var prestageAttempts atomic.Int32
	var redundantUpdateAfterUpload atomic.Bool
	capSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"data":{"token":"tok"}}`))
		case req.URL.Path == "/api/v2/user/apps/appDefinitions/update":
			var body map[string]interface{}
			_ = json.NewDecoder(req.Body).Decode(&body)
			skipUpdate, _ := body["skipServiceUpdate"].(bool)
			if !uploaded.Load() {
				if skipUpdate {
					if prestageAttempts.Add(1) == 1 {
						_, _ = w.Write([]byte(`{"status":1108,"description":"Build in-progress for up-app. Please wait..."}`))
						return
					}
					prestageUpdateBeforeUpload.Store(true)
				} else {
					nonPrestageUpdateBeforeUpload.Store(true)
				}
			} else if skipUpdate == false {
				// Rollback below intentionally uses Prestage before DeployExistingImage too;
				// during v3 upgrade, post-build update must not be called once pre-staged.
				redundantUpdateAfterUpload.Store(true)
			}
			_, _ = w.Write([]byte(`{"status":100,"data":{}}`))
		case strings.HasPrefix(req.URL.Path, "/api/v2/user/apps/appData/"):
			if req.Method == http.MethodPost && strings.Contains(req.Header.Get("Content-Type"), "multipart/form-data") {
				uploaded.Store(true)
			}
			if req.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"status":100,"data":{"isAppBuilding":false,"isBuildFailed":false,"logs":{"lines":["ok"]}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":100,"data":{}}`))
		default:
			if writeRunningAppFixture(w, req, "up-app", int(runtimeVersion.Load())) {
				return
			}
			_, _ = w.Write([]byte(`{"status":100,"data":{}}`))
		}
	}))
	defer capSrv.Close()
	capClient := deploy.NewCapRoverClient(capSrv.URL, "pass", "apps.opc.intra")
	capClient.PollInterval = 5 * time.Millisecond
	capClient.ProbeInterval = 5 * time.Millisecond
	h.SetCapRoverClient(capClient)

	// 1. Verify v3 upgrade pre-stages config with skipServiceUpdate=true before UploadAppTar and performs single atomic switch
	_ = h.storage.SavePublishedAppAndVersion(ctx, &model.DeployedApp{
		Name:              "Up App",
		Slug:              "up-app",
		AppURL:            appSrv.URL,
		ContainerPort:     8080,
		Status:            model.AppStatusBuilding,
		CurrentVersion:    3,
		StorageMountsJSON: v2Mounts,
	}, &model.AppDeploymentVersion{
		AppSlug:           "up-app",
		Version:           3,
		Status:            model.AppStatusBuilding,
		IsActive:          false,
		ContainerPort:     8080,
		StorageMountsJSON: v2Mounts,
	})
	h.tryLockDeploySlug("up-app")
	h.runDeploymentAsync("up-app", 3, 8080, appSrv.URL, nil, nil, []model.AppStorageMount{
		{Name: "data", ContainerPath: "/app/data"},
		{Name: "uploads", ContainerPath: "/app/uploads"},
	}, []byte("v3-tar"), []byte("v3-zip"))
	if nonPrestageUpdateBeforeUpload.Load() {
		t.Fatalf("expected upgrade deployment not to restart active container before UploadAppTar")
	}
	if !prestageUpdateBeforeUpload.Load() {
		t.Fatalf("expected upgrade deployment to pre-stage config with skipServiceUpdate=true before UploadAppTar")
	}
	if !uploaded.Load() || prestageAttempts.Load() != 2 {
		t.Fatalf("expected the same deployment to retry pre-stage then upload (attempts=%d, uploaded=%v)", prestageAttempts.Load(), uploaded.Load())
	}
	if redundantUpdateAfterUpload.Load() {
		t.Fatalf("expected no duplicate UpdateAppConfigAndEnv after UploadAppTar when pre-staged")
	}

	// 2. Simulate RecheckAppHealth marking deployed_apps.status = "unhealthy" (while v3.Status is still "running")
	// and verify default Rollback (target_version=0) rolls back to v2, and rolling back to v1 (fewer mounts) succeeds!
	_ = h.storage.UpdateAppStatus(ctx, "up-app", "unhealthy", "down", "down", "", false)

	runtimeVersion.Store(2)
	rbAutoReq := httptest.NewRequest(http.MethodPost, "/api/v1/apps/up-app/rollback", bytes.NewReader([]byte(`{"target_version":0}`)))
	rbAutoReq.Header.Set("Content-Type", "application/json")
	wAuto := httptest.NewRecorder()
	r.ServeHTTP(wAuto, rbAutoReq)
	if wAuto.Code != http.StatusOK {
		t.Fatalf("auto rollback failed (%d): %s", wAuto.Code, wAuto.Body.String())
	}
	var autoResp struct {
		CurrentVersion int `json:"current_version"`
	}
	_ = json.Unmarshal(wAuto.Body.Bytes(), &autoResp)
	if autoResp.CurrentVersion != 2 {
		t.Fatalf("expected auto rollback after v3 unhealthy to target v2, got v%d", autoResp.CurrentVersion)
	}

	// Now explicitly rollback from v2 (2 mounts) back to v1 (1 mount) - must succeed without 400!
	runtimeVersion.Store(1)
	rbV1Req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/up-app/rollback", bytes.NewReader([]byte(`{"target_version":1}`)))
	rbV1Req.Header.Set("Content-Type", "application/json")
	wV1 := httptest.NewRecorder()
	r.ServeHTTP(wV1, rbV1Req)
	if wV1.Code != http.StatusOK {
		t.Fatalf("rollback from v2 (2 mounts) to v1 (1 mount) must succeed, got %d: %s", wV1.Code, wV1.Body.String())
	}
}

func TestVolumeTrailingSlashDefaultResolutionAndSecretPreservation(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	h.SetCapRoverClient(deploy.NewCapRoverClient("http://127.0.0.1:1", "pwd", "apps.opc.intra"))
	ctx := context.Background()
	const node = "012345678901234567890abcd"

	// 1. configureDirectoryVolume trims trailing slash on Host
	res := &model.DeployResource{
		ID:   "data-vdc",
		Name: "默认存储",
		Type: deploy.LocalDirectoryVolume,
		Host: "/data-vdc/",
		EnvVars: map[string]string{
			"OPC_STORAGE_NODE_ID": node,
		},
		Enabled: false,
	}
	if err := h.configureDirectoryVolume(ctx, res, nil); err != nil {
		t.Fatalf("configureDirectoryVolume should accept trailing slash, got: %v", err)
	}
	if res.Host != "/data-vdc" {
		t.Fatalf("expected trimmed Host /data-vdc, got %q", res.Host)
	}

	// 2. resolveVolumeResources picks IsDefault=true compatible volume even if another volume appears earlier
	_ = h.storage.UpsertDeployResource(ctx, &model.DeployResource{
		ID:        "vol-secondary",
		Name:      "Secondary",
		Type:      deploy.LocalDirectoryVolume,
		Host:      "/data/secondary",
		EnvVars:   map[string]string{"OPC_STORAGE_NODE_ID": node},
		IsDefault: false,
		Enabled:   true,
	})
	_ = h.storage.UpsertDeployResource(ctx, &model.DeployResource{
		ID:        "vol-primary",
		Name:      "Primary",
		Type:      deploy.LocalDirectoryVolume,
		Host:      "/data/primary",
		EnvVars:   map[string]string{"OPC_STORAGE_NODE_ID": node},
		IsDefault: true,
		Enabled:   true,
	})
	appID := uuid.New()
	mounts := []model.AppStorageMount{{Name: "data", ContainerPath: "/app/data", Kind: "sqlite"}}
	if err := h.resolveVolumeResources(ctx, "def-app", appID, true, mounts, nil, []config.OnlineResourceConfig{
		{ID: "vol-secondary", Type: deploy.LocalDirectoryVolume, IsDefault: false, EnvVars: map[string]string{"OPC_STORAGE_NODE_ID": node}},
		{ID: "vol-primary", Type: deploy.LocalDirectoryVolume, IsDefault: true, EnvVars: map[string]string{"OPC_STORAGE_NODE_ID": node}},
	}); err != nil {
		t.Fatalf("resolveVolumeResources failed: %v", err)
	}
	if mounts[0].ResourceID != "vol-primary" {
		t.Fatalf("expected default resource vol-primary, got %q", mounts[0].ResourceID)
	}

	// 3. preservePreviousSecrets keeps stored secret when upgrade/update leaves it empty with has_value=true
	prevJSON := `[{"key":"API_SECRET","value":"prod-secret-123"},{"key":"PUBLIC_MSG","value":"old-msg"}]`
	merged := preservePreviousSecrets(prevJSON, []model.AppConfigItem{
		{Key: "API_SECRET", Value: "", HasValue: true, ValueSource: "manual"},
		{Key: "PUBLIC_MSG", Value: "", ValueSource: "manual"},
	})
	if merged[0].Value != "prod-secret-123" {
		t.Fatalf("expected API_SECRET preserved, got %q", merged[0].Value)
	}
	if merged[1].Value != "" {
		t.Fatalf("expected explicitly cleared non-secret PUBLIC_MSG to stay empty, got %q", merged[1].Value)
	}
}

func writeRunningAppFixture(w http.ResponseWriter, req *http.Request, slug string, version int) bool {
	image := fmt.Sprintf("img-captain-%s:%d", slug, version)
	var data map[string]any
	switch req.URL.Path {
	case "/api/v2/user/apps/appDefinitions":
		data = map[string]any{"appDefinitions": []map[string]any{
			{"appName": slug, "deployedVersion": version,
				"versions": []map[string]any{{"version": version, "deployedImageName": image}}},
		}}
	case "/api/v2/user/apps/runtime/" + slug:
		data = map[string]any{"desiredInstances": 1,
			"currentTasks": []map[string]any{{"image": image, "state": "running"}}}
	default:
		return false
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": 100, "data": data})
	return true
}
