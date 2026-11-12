package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		Name:          "Recover App",
		Slug:          "recover-app",
		AppURL:        appSrv.URL,
		ContainerPort: 8080,
		Status:        model.AppStatusRunning,
		CurrentVersion: 1,
		AppConfigJSON: `[{"key":"MODE","value":"v1"}]`,
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
