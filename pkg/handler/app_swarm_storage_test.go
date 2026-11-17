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
	if err := h.resolveVolumeResources(ctx, "example", true, mounts, nil, []config.OnlineResourceConfig{resource}); err != nil {
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
	if err := h.resolveVolumeResources(ctx, "example", true, next, previous, nil); err != nil {
		t.Fatal(err)
	}
	reused, err := deploy.ReconcileStorageMounts("example", previous, next)
	if err != nil || reused[0].VolumeName != assignedName {
		t.Fatalf("upgrade changed volume: %+v %v", reused, err)
	}
	next[0].ResourceID = "new-default"
	if err := h.resolveVolumeResources(ctx, "example", true, next, previous, nil); err == nil {
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
