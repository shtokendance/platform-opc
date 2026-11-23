package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestConfiguredVolumeUsesActualSwarmMount(t *testing.T) {
	const node = "012345678901234567890abcd"
	const originalName = "opc-vol-example-data"
	var volume atomic.Value
	volume.Store(originalName)
	var storageStatus atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/login":
			w.Write([]byte(`{"status":100,"data":{"token":"fixture"}}`))
		case "/api/v2/user/system/application-storage/":
			if r.Header.Get("x-captain-auth") != "fixture" {
				t.Errorf("missing Deploy auth")
			}
			fmt.Fprintf(w, `{"status":100,"data":{"disk":{"nodes":[{"nodeId":%q,"disks":[{"totalBytes":100,"availableBytes":80}]}]},
				"services":[{"name":"example","running_tasks":%d,"mounts":[{"type":"volume","source":%q,"target":"/app/data"}]}],
				"volumes":[{"name":%q,"size_bytes":512,"ref_count":1}]}}`, node, storageStatus.Load(), volume.Load(), volume.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	h := &Handler{caproverClient: deploy.NewCapRoverClient(srv.URL, "password", "apps.example.test")}
	resource := config.OnlineResourceConfig{ID: "disk", Type: "docker-volume", EnvVars: map[string]string{"OPC_STORAGE_NODE_ID": node}}
	mounts := deploy.NormalizeStorageMounts("example", []model.AppStorageMount{{Name: "data", ContainerPath: "/app/data", ResourceID: "disk", StorageNodeID: "client-supplied"}})
	ctx := context.Background()
	if err := h.resolveVolumeResources(ctx, "example", uuid.Nil, true, mounts, nil, []config.OnlineResourceConfig{resource}); err != nil {
		t.Fatal(err)
	}
	if mounts[0].StorageNodeID != node || mounts[0].VolumeName == originalName {
		t.Fatalf("unsafe or non-unique mount: %+v", mounts[0])
	}
	assignedName := mounts[0].VolumeName
	volume.Store(assignedName)
	normalized, err := deploy.ReconcileStorageMounts("example", nil, mounts)
	if err != nil {
		t.Fatal(err)
	}
	override := deploy.BuildServiceUpdateOverrideJSON("example", normalized)
	if !strings.Contains(override, "node.id == "+node) || !strings.Contains(override, assignedName) {
		t.Fatalf("missing assigned node or volume: %s", override)
	}
	if err := h.verifyAllocatedVolumes(ctx, "example", normalized); err == nil {
		t.Fatal("service spec with no running tasks was accepted")
	}
	storageStatus.Store(1)
	if err := h.verifyAllocatedVolumes(ctx, "example", normalized); err != nil {
		t.Fatal(err)
	}
	previous := normalized
	next := []model.AppStorageMount{{Name: "new-name", ContainerPath: "/app/data"}}
	if err := h.resolveVolumeResources(ctx, "example", uuid.Nil, true, next, previous, nil); err != nil {
		t.Fatal(err)
	}
	reused, err := deploy.ReconcileStorageMounts("example", previous, next)
	if err != nil || reused[0].VolumeName != assignedName {
		t.Fatalf("upgrade changed volume: %+v %v", reused, err)
	}
	next[0].ResourceID = "new-default"
	if err := h.resolveVolumeResources(ctx, "example", uuid.Nil, true, next, previous, nil); err == nil {
		t.Fatal("upgrade replaced bound storage without migration")
	}

	var snapshot deploy.SwarmStorage
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"services":[{"name":"example","running_tasks":1,"mounts":[{"type":"volume","source":"mismatch","target":"/app/data"}]}],"volumes":[{"name":%q,"size_bytes":512}]}`, assignedName)), &snapshot); err != nil {
		t.Fatal(err)
	}
	details := describeSwarmMounts(snapshot, "example", previous)
	if details[0].Status != "mismatch" {
		t.Fatalf("unexpected state: %+v", details)
	}
}

func TestAdminGetSwarmStorageExposesDeployVolumeRoots(t *testing.T) {
	const node = "012345678901234567890abcd"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/login":
			w.Write([]byte(`{"status":100,"data":{"token":"fixture"}}`))
		case "/api/v2/user/system/application-storage/":
			fmt.Fprintf(w, `{"status":100,"data":{"disk":{"nodes":[{"nodeId":%q,"hostname":"swarm-manager","disks":[{"totalBytes":1000,"usedBytes":200,"availableBytes":800,"severity":"normal"}]}]},
				"services":[],
				"volumes":[],
				"volume_roots":[{"root":"/data-vdc","node_id":%q,"backend":"local-directory","filesystem_type":"ext4","writable":true,"available":true,"total_bytes":107374182400,"used_bytes":10737418240,"free_bytes":96636764160,"usage_ratio":10}]}}`, node, node)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	h, _ := setupTestAppHandler(t)
	_ = h.storage.BootstrapDeployDB(context.Background(), nil, config.DatabaseConfig{})
	h.SetCapRoverClient(deploy.NewCapRoverClient(srv.URL, "password", "apps.example.test"))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/api/deploy/storage", nil)
	h.adminGetSwarmStorage(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp model.GlobalStorageSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.VolumeRoots) != 1 || resp.VolumeRoots[0].Root != "/data-vdc" || resp.VolumeRoots[0].Backend != deploy.LocalDirectoryVolume || !resp.VolumeRoots[0].Available {
		t.Fatalf("unexpected volume_roots in summary: %+v", resp.VolumeRoots)
	}
	if resp.VolumeRoots[0].TotalHuman != "100.00 GB" || resp.VolumeRoots[0].UsedHuman != "10.00 GB" || resp.VolumeRoots[0].FreeHuman != "90.00 GB" || resp.VolumeRoots[0].UsageRatio != 10 {
		t.Fatalf("unexpected volume_roots capacity formatting: %+v", resp.VolumeRoots[0])
	}

	resources, err := h.storage.ListDeployResources(context.Background(), false)
	if err != nil {
		t.Fatalf("list deploy resources: %v", err)
	}
	var found *model.DeployResource
	for i := range resources {
		if resources[i].Host == "/data-vdc" && resources[i].Type == deploy.LocalDirectoryVolume {
			found = &resources[i]
		}
	}
	if found == nil || !found.IsDefault || !found.Enabled || found.EnvVars["OPC_STORAGE_NODE_ID"] != node {
		t.Fatalf("expected /data-vdc auto-registered as enabled default local-directory, got %+v", resources)
	}

	// Syncing a second mounted root keeps the first default intact and adds the second as non-default.
	if changed := h.syncLocalDirectoryVolumeRoots(context.Background(), []deploy.SwarmVolumeRoot{
		{Root: "/data-vdc", NodeID: node, Backend: deploy.LocalDirectoryVolume, Available: true},
		{Root: "/data-ssd", NodeID: node, Backend: deploy.LocalDirectoryVolume, Available: true},
	}); !changed {
		t.Fatalf("expected syncing second volume root /data-ssd to record a new resource")
	}
	resources, err = h.storage.ListDeployResources(context.Background(), false)
	if err != nil {
		t.Fatalf("list deploy resources after second sync: %v", err)
	}
	var first, second *model.DeployResource
	for i := range resources {
		if resources[i].Host == "/data-vdc" {
			first = &resources[i]
		} else if resources[i].Host == "/data-ssd" {
			second = &resources[i]
		}
	}
	if first == nil || !first.IsDefault || second == nil || second.IsDefault || !second.Enabled {
		t.Fatalf("expected /data-vdc default and /data-ssd enabled non-default, got first=%+v second=%+v", first, second)
	}
}
