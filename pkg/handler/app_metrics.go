package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

type AppResourceOverview struct {
	AppSlug              string  `json:"app_slug"`
	AppName              string  `json:"app_name"`
	AppStatus            string  `json:"app_status"`
	Available            bool    `json:"available"`
	Message              string  `json:"message,omitempty"`
	ReplicasRunning      int     `json:"replicas_running"`
	ReplicasDesired      int     `json:"replicas_desired"`
	CPUPercent           float64 `json:"cpu_percent"`
	OnlineCPUs           int     `json:"online_cpus"`
	CPULimitCores        float64 `json:"cpu_limit_cores"`
	MemoryUsageBytes     int64   `json:"memory_usage_bytes"`
	MemoryLimitBytes     int64   `json:"memory_limit_bytes"`
	HostMemoryBytes      int64   `json:"host_memory_bytes"`
	MemoryPercent        float64 `json:"memory_percent"`
	ContainerRwBytes     int64   `json:"container_rw_bytes"`
	ContainerRootfsBytes int64   `json:"container_rootfs_bytes"`
	PersistentBytes      int64   `json:"persistent_bytes"`
	PersistentMounts     int     `json:"persistent_mounts"`
	PersistentFiles      int     `json:"persistent_files"`
	ServerTotalBytes     uint64  `json:"server_total_bytes"`
	ServerUsedBytes      uint64  `json:"server_used_bytes"`
	ServerAvailableBytes uint64  `json:"server_available_bytes"`
	NetworkRxBytes       int64   `json:"network_rx_bytes"`
	NetworkTxBytes       int64   `json:"network_tx_bytes"`
	BlockReadBytes       int64   `json:"block_read_bytes"`
	BlockWriteBytes      int64   `json:"block_write_bytes"`
	PIDs                 int     `json:"pids"`
	RestartCount         int     `json:"restart_count"`
	StartedAt            string  `json:"started_at,omitempty"`
	NodeHostname         string  `json:"node_hostname,omitempty"`
	CollectedAt          string  `json:"collected_at"`
}

func (h *Handler) GetAppMetrics(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))

	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	var mounts []model.AppStorageMount
	if app.StorageMountsJSON != "" {
		_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
	}

	var metrics deploy.AppResourceMetrics
	var metricsErr error
	if h.usesRemoteStorage() {
		metrics, metricsErr = h.caproverClient.GetAppResourceMetrics(ctx, slug)
	} else {
		metrics, metricsErr = h.getStorageInspector().InspectAppMetrics(ctx, slug)
	}
	if metricsErr != nil {
		metrics = deploy.AppResourceMetrics{
			Available:   false,
			Message:     metricsErr.Error(),
			CollectedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	var persistentBytes int64
	var persistentFiles int
	var persistentMounts int
	var serverTotal, serverUsed, serverAvail uint64

	if h.usesRemoteStorage() {
		if snapshot, err := h.caproverClient.ApplicationStorage(ctx); err == nil {
			if len(snapshot.Disk.Nodes) > 0 && len(snapshot.Disk.Nodes[0].Disks) > 0 {
				d := snapshot.Disk.Nodes[0].Disks[0]
				serverTotal = d.Total
				serverUsed = d.Used
				serverAvail = d.Available
			}
			if len(mounts) > 0 {
				details := describeSwarmMounts(snapshot, slug, mounts)
				h.enrichSwarmMountStats(ctx, details)
				persistentMounts = len(details)
				for _, m := range details {
					persistentBytes += m.SizeBytes
					persistentFiles += m.FileCount
				}
			}
		}
	} else {
		si := h.getStorageInspector()
		diskInfo := si.GetServerDiskInfo("/")
		serverTotal = diskInfo.TotalBytes
		serverUsed = diskInfo.UsedBytes
		serverAvail = diskInfo.FreeBytes
		if len(mounts) > 0 {
			if details, err := si.InspectAppMountsChecked(ctx, slug, mounts); err == nil {
				persistentMounts = len(details)
				for _, m := range details {
					persistentBytes += m.SizeBytes
					persistentFiles += m.FileCount
				}
			}
		}
	}

	collectedAt := metrics.CollectedAt
	if collectedAt == "" {
		collectedAt = time.Now().UTC().Format(time.RFC3339)
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, AppResourceOverview{
		AppSlug:              slug,
		AppName:              app.Name,
		AppStatus:            app.Status,
		Available:            metrics.Available,
		Message:              metrics.Message,
		ReplicasRunning:      metrics.ReplicasRunning,
		ReplicasDesired:      metrics.ReplicasDesired,
		CPUPercent:           metrics.CPUPercent,
		OnlineCPUs:           metrics.OnlineCPUs,
		CPULimitCores:        metrics.CPULimitCores,
		MemoryUsageBytes:     metrics.MemoryUsageBytes,
		MemoryLimitBytes:     metrics.MemoryLimitBytes,
		HostMemoryBytes:      metrics.HostMemoryBytes,
		MemoryPercent:        metrics.MemoryPercent,
		ContainerRwBytes:     metrics.ContainerRwBytes,
		ContainerRootfsBytes: metrics.ContainerRootfsBytes,
		PersistentBytes:      persistentBytes,
		PersistentMounts:     persistentMounts,
		PersistentFiles:      persistentFiles,
		ServerTotalBytes:     serverTotal,
		ServerUsedBytes:      serverUsed,
		ServerAvailableBytes: serverAvail,
		NetworkRxBytes:       metrics.NetworkRxBytes,
		NetworkTxBytes:       metrics.NetworkTxBytes,
		BlockReadBytes:       metrics.BlockReadBytes,
		BlockWriteBytes:      metrics.BlockWriteBytes,
		PIDs:                 metrics.PIDs,
		RestartCount:         metrics.RestartCount,
		StartedAt:            metrics.StartedAt,
		NodeHostname:         metrics.NodeHostname,
		CollectedAt:          collectedAt,
	})
}
