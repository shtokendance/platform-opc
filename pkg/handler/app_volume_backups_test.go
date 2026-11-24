package handler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func buildTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		data := []byte(content)
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setupVolumeBackupRouter(t *testing.T, owner string) (*Handler, *gin.Engine) {
	t.Helper()
	h, _ := setupTestAppHandler(t)
	_ = h.storage.DeployDB().AutoMigrate(&model.AppVolumeBackup{}, &model.DeployResource{})
	r := gin.New()
	apps := r.Group("/api/v1/apps", func(c *gin.Context) {
		if owner != "" {
			c.Set(appOwnerKey, owner)
		}
		c.Next()
	})
	apps.GET("/backups", h.ListAppVolumeBackups)
	apps.GET("/backups/:id/download", h.DownloadAppVolumeBackupFile)
	apps.DELETE("/backups/:id", h.DeleteAppVolumeBackup)
	apps.GET("/:slug/storage", h.requireAppOwner, h.GetAppStorageDetails)
	apps.GET("/:slug/storage/backup", h.requireAppOwner, h.DownloadAppStorageBackup)
	apps.POST("/:slug/storage/backups", h.requireAppOwner, h.CreateAppVolumeBackup)
	apps.DELETE("/:slug", h.requireAppOwner, h.AdminDeleteDeployApp)
	return h, r
}

func TestSwarmStorageInspectAndExportBackupFlow(t *testing.T) {
	const slug = "audit-app"
	const owner = "user-alice"
	const nodeID = "012345678901234567890abcd"
	appID := uuid.New()
	mounts := []model.AppStorageMount{
		{
			Name:           "data",
			ContainerPath:  "/app/data",
			HostPath:       "/data-vdc/" + slug + "-" + appID.String() + "/data",
			StorageBackend: deploy.LocalDirectoryVolume,
			StorageNodeID:  nodeID,
			ResourceID:     "data-vdc",
			Purpose:        "业务数据",
		},
		{
			Name:           "uploads",
			ContainerPath:  "/app/uploads",
			HostPath:       "/data-vdc/" + slug + "-" + appID.String() + "/uploads",
			StorageBackend: deploy.LocalDirectoryVolume,
			StorageNodeID:  nodeID,
			ResourceID:     "data-vdc",
			Purpose:        "上传附件",
		},
	}
	mountsJSON, _ := json.Marshal(mounts)
	archiveBytes := buildTestTarGz(t, map[string]string{
		"manifest.json":                `{"app_slug":"audit-app","mounts":[{"container_path":"/app/data","archive_path":"volumes/app/data/","file_count":1,"size_bytes":12},{"container_path":"/app/uploads","archive_path":"volumes/app/uploads/","file_count":2,"size_bytes":100}]}`,
		"volumes/app/data/state.json":  `{"ok":true}`,
		"volumes/app/uploads/a.txt":    `hello`,
	})

	exportShouldFail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v2/login":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":100,"data":{"token":"tok"}}`)
		case "/api/v2/user/system/application-storage/":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":100,"data":{"disk":{"nodes":[{"nodeId":"%s","hostname":"swarm-1","disks":[{"totalBytes":1000000,"usedBytes":1000,"availableBytes":999000,"severity":"ok"}]}]},"volume_roots":[{"root":"/data-vdc","node_id":"%s","backend":"local-directory","available":true,"writable":true}],"volumes":[],"services":[{"name":"%s","running_tasks":1,"mounts":[{"type":"bind","source":"%s","target":"/app/data"},{"type":"bind","source":"%s","target":"/app/uploads"}]}]}}`,
				nodeID, nodeID, slug, mounts[0].HostPath, mounts[1].HostPath)
		case "/api/v2/user/system/application-storage/inspect/":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":100,"data":{"paths":[{"host_path":"%s","exists":true,"file_count":1,"size_bytes":12,"sample_files":["state.json"]},{"host_path":"%s","exists":true,"file_count":2,"size_bytes":100,"sample_files":["a.txt"]}]}}`,
				mounts[0].HostPath, mounts[1].HostPath)
		case "/api/v2/user/system/application-storage/export/":
			if exportShouldFail {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"status":1000,"description":"disk read error"}`)
				return
			}
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archiveBytes)
		case "/api/v2/user/apps/appDefinitions/delete":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":100}`)
		case "/api/v2/user/system/application-storage/paths/":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":100}`)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	h, r := setupVolumeBackupRouter(t, owner)
	h.SetCapRoverClient(deploy.NewCapRoverClient(server.URL, "pw", ""))
	ctx := context.Background()
	if err := h.storage.CreateOrUpdateApp(ctx, &model.DeployedApp{
		ID:                appID,
		Name:              "Audit App",
		Slug:              slug,
		Status:            model.AppStatusRunning,
		CurrentVersion:    2,
		CreatedBy:         owner,
		StorageMountsJSON: string(mountsJSON),
		AppConfigJSON:     "[]",
		ResourceIDsJSON:   "[]",
	}); err != nil {
		t.Fatal(err)
	}

	wStorage := httptest.NewRecorder()
	r.ServeHTTP(wStorage, httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+slug+"/storage", nil))
	if wStorage.Code != http.StatusOK {
		t.Fatalf("GET storage failed (%d): %s", wStorage.Code, wStorage.Body.String())
	}
	var storageResp model.AppStorageOverview
	if err := json.Unmarshal(wStorage.Body.Bytes(), &storageResp); err != nil {
		t.Fatal(err)
	}
	if storageResp.TotalFiles != 3 || storageResp.TotalBytes != 112 {
		t.Fatalf("expected swarm total_files=3 total_bytes=112, got files=%d bytes=%d", storageResp.TotalFiles, storageResp.TotalBytes)
	}
	if len(storageResp.Mounts) != 2 || storageResp.Mounts[0].SizeBytes != 12 || storageResp.Mounts[1].FileCount != 2 {
		t.Fatalf("unexpected mount stats: %+v", storageResp.Mounts)
	}

	exportShouldFail = true
	wFail := httptest.NewRecorder()
	r.ServeHTTP(wFail, httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+slug+"/storage/backups", nil))
	if wFail.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on failed export, got %d: %s", wFail.Code, wFail.Body.String())
	}
	exportShouldFail = false

	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+slug+"/storage/backups", nil))
	if wCreate.Code != http.StatusOK {
		t.Fatalf("POST backups failed (%d): %s", wCreate.Code, wCreate.Body.String())
	}
	var createPayload struct {
		Backup model.AppVolumeBackupItem `json:"backup"`
	}
	if err := json.Unmarshal(wCreate.Body.Bytes(), &createPayload); err != nil {
		t.Fatal(err)
	}
	backupID := createPayload.Backup.ID
	if backupID == "" || createPayload.Backup.Downloaded || createPayload.Backup.FileCount != 3 {
		t.Fatalf("unexpected backup item: %+v", createPayload.Backup)
	}

	wBlocked := httptest.NewRecorder()
	r.ServeHTTP(wBlocked, httptest.NewRequest(http.MethodDelete, "/api/v1/apps/"+slug+"?export_mode=export_and_delete&backup_id="+backupID+"&purge_volumes=true", nil))
	if wBlocked.Code != http.StatusConflict {
		t.Fatalf("expected 409 before download, got %d: %s", wBlocked.Code, wBlocked.Body.String())
	}
	if _, err := h.storage.GetAppBySlug(ctx, slug); err != nil {
		t.Fatalf("app must still exist when delete is blocked: %v", err)
	}

	wDl := httptest.NewRecorder()
	r.ServeHTTP(wDl, httptest.NewRequest(http.MethodGet, "/api/v1/apps/backups/"+backupID+"/download", nil))
	if wDl.Code != http.StatusOK || !bytes.Equal(wDl.Body.Bytes(), archiveBytes) {
		t.Fatalf("download failed (%d), len=%d", wDl.Code, wDl.Body.Len())
	}

	wDel := httptest.NewRecorder()
	r.ServeHTTP(wDel, httptest.NewRequest(http.MethodDelete, "/api/v1/apps/"+slug+"?export_mode=export_and_delete&backup_id="+backupID+"&purge_volumes=true", nil))
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete after download failed (%d): %s", wDel.Code, wDel.Body.String())
	}
	if _, err := h.storage.GetAppBySlug(ctx, slug); err == nil {
		t.Fatalf("expected app to be deleted")
	}

	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, httptest.NewRequest(http.MethodGet, "/api/v1/apps/backups", nil))
	if wList.Code != http.StatusOK {
		t.Fatalf("list backups failed (%d): %s", wList.Code, wList.Body.String())
	}
	var listPayload struct {
		Backups []model.AppVolumeBackupItem `json:"backups"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listPayload); err != nil {
		t.Fatal(err)
	}
	if len(listPayload.Backups) != 1 || !listPayload.Backups[0].AppDeleted || !listPayload.Backups[0].Downloaded {
		t.Fatalf("unexpected listed backups: %+v", listPayload.Backups)
	}

	wDelBk := httptest.NewRecorder()
	r.ServeHTTP(wDelBk, httptest.NewRequest(http.MethodDelete, "/api/v1/apps/backups/"+backupID, nil))
	if wDelBk.Code != http.StatusOK {
		t.Fatalf("delete backup failed (%d): %s", wDelBk.Code, wDelBk.Body.String())
	}
	wListAfter := httptest.NewRecorder()
	r.ServeHTTP(wListAfter, httptest.NewRequest(http.MethodGet, "/api/v1/apps/backups", nil))
	if !strings.Contains(wListAfter.Body.String(), `"backups":[]`) {
		t.Fatalf("expected empty backups list after delete, got %s", wListAfter.Body.String())
	}
}
