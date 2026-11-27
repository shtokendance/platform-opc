package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func TestGetAppMetrics(t *testing.T) {
	h, r := setupTestAppHandler(t)
	r.GET("/api/v1/apps/:slug/metrics", h.GetAppMetrics)

	fakeDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"description":"ok","data":{"token":"tok-1"}}`))
		case req.URL.Path == "/api/v2/user/apps/runtime/demo-app/metrics":
			_, _ = w.Write([]byte(`{"status":100,"description":"ok","data":{"available":true,"replicas_running":1,"replicas_desired":1,"cpu_percent":18.5,"online_cpus":4,"cpu_limit_cores":2,"memory_usage_bytes":268435456,"memory_limit_bytes":1073741824,"host_memory_bytes":8589934592,"memory_percent":25,"container_rw_bytes":10485760,"container_rootfs_bytes":524288000,"network_rx_bytes":1024,"network_tx_bytes":2048,"block_read_bytes":4096,"block_write_bytes":8192,"pids":16,"restart_count":0,"started_at":"2026-11-27T08:00:00Z","node_hostname":"swarm-node-1","collected_at":"2026-11-27T10:00:00Z"}}`))
		case strings.HasPrefix(req.URL.Path, "/api/v2/user/system/application-storage/"):
			_, _ = w.Write([]byte(`{"status":100,"description":"ok","data":{"disk":{"nodes":[{"nodeId":"node-1","hostname":"swarm-node-1","disks":[{"totalBytes":107374182400,"usedBytes":42949672960,"availableBytes":64424509440,"severity":"OK"}]}]},"volumes":[],"volume_roots":[],"services":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fakeDeploy.Close()

	h.caproverClient = deploy.NewCapRoverClient(fakeDeploy.URL, "pwd", "apps.opc.intra")

	app := &model.DeployedApp{
		ID:     uuid.New(),
		Name:   "Demo App",
		Slug:   "demo-app",
		Status: "running",
	}
	if err := h.storage.DeployDB().WithContext(t.Context()).Create(app).Error; err != nil {
		t.Fatalf("create app: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/demo-app/metrics", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp AppResourceOverview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Available {
		t.Fatalf("expected available=true, got false (message=%q)", resp.Message)
	}
	if resp.CPUPercent != 18.5 || resp.MemoryUsageBytes != 268435456 || resp.ContainerRwBytes != 10485760 {
		t.Fatalf("unexpected metrics payload: %+v", resp)
	}
	if resp.ServerTotalBytes != 107374182400 || resp.NodeHostname != "swarm-node-1" {
		t.Fatalf("unexpected host/disk payload: %+v", resp)
	}

	wNotFound := httptest.NewRecorder()
	r.ServeHTTP(wNotFound, httptest.NewRequest(http.MethodGet, "/api/v1/apps/missing-app/metrics", nil))
	if wNotFound.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", wNotFound.Code)
	}
}
