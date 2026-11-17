package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func describeSwarmMounts(snapshot deploy.SwarmStorage, slug string, mounts []model.AppStorageMount) []model.AppStorageMountDetail {
	attached := make(map[string]deploy.SwarmMount)
	running := false
	for _, service := range snapshot.Services {
		if service.Name == slug || service.Name == "srv-captain--"+slug {
			for _, m := range service.Mounts {
				attached[m.Target] = m
			}
			running = service.RunningTasks > 0
			break
		}
	}
	sizes := make(map[string]int64, len(snapshot.Volumes))
	for _, volume := range snapshot.Volumes {
		sizes[volume.Name] = volume.SizeBytes
	}
	details := make([]model.AppStorageMountDetail, 0, len(mounts))
	for _, m := range mounts {
		detail := model.AppStorageMountDetail{AppStorageMount: m, Status: "missing", SizeHuman: "未知", SeedProtected: m.SeedFilesCount > 0}
		if live, ok := attached[m.ContainerPath]; ok {
			if (m.HostPath == "" && live.Type == "volume" && live.Source == m.VolumeName) ||
				(live.Type == "bind" && live.Source == m.HostPath) {
				detail.Status = "pending"
				sz, present := sizes[live.Source]
				if running && (live.Type == "bind" || present) {
					detail.Status = "mounted"
				}
				if live.Type == "volume" && present && sz >= 0 {
					detail.SizeBytes = sz
					detail.SizeHuman = deploy.FormatBytes(uint64(sz))
				}
			} else {
				detail.Status = "mismatch"
			}
		}
		details = append(details, detail)
	}
	return details
}

func (h *Handler) verifyAllocatedVolumes(ctx context.Context, slug string, mounts []model.AppStorageMount) error {
	var allocated []model.AppStorageMount
	for _, mount := range mounts {
		if mount.ResourceID != "" {
			allocated = append(allocated, mount)
		}
	}
	if len(allocated) == 0 {
		return nil
	}
	snapshot, err := h.caproverClient.ApplicationStorage(ctx)
	if err != nil {
		return fmt.Errorf("verify Swarm application mounts: %w", err)
	}
	for _, detail := range describeSwarmMounts(snapshot, slug, allocated) {
		if detail.Status != "mounted" || snapshot.Disk.Nodes[0].NodeID != detail.StorageNodeID {
			return fmt.Errorf("Swarm did not mount the assigned volume at %s", detail.ContainerPath)
		}
	}
	return nil
}

func (h *Handler) adminGetSwarmStorage(c *gin.Context) {
	ctx := c.Request.Context()
	snapshot, err := h.caproverClient.ApplicationStorage(ctx)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Swarm 存储信息不可用，请检查 Deploy 版本和管理连接"})
		return
	}
	apps, err := h.storage.ListApps(ctx)
	if err != nil {
		c.JSON(500, gin.H{"error": "无法读取应用存储绑定"})
		return
	}
	node := snapshot.Disk.Nodes[0]
	disk := node.Disks[0]
	summary := model.GlobalStorageSummary{
		Source: "swarm", NodeID: node.NodeID, NodeName: node.Hostname,
		ServerDisk: model.StorageServerDiskInfo{
			Path: "Deploy 数据目录所在文件系统", TotalBytes: disk.Total, UsedBytes: disk.Used, FreeBytes: disk.Available,
			TotalHuman: deploy.FormatBytes(disk.Total), UsedHuman: deploy.FormatBytes(disk.Used), FreeHuman: deploy.FormatBytes(disk.Available),
			UsageRatio: float64(disk.Used) * 100 / float64(disk.Total), IsLowSpace: disk.Severity != "normal",
		},
		AppsStorage: make([]model.AppStorageOverview, 0, len(apps)),
	}
	refs, err := h.referencedAppVolumes(ctx)
	if err != nil {
		c.JSON(500, gin.H{"error": "无法校验应用和历史版本的卷引用"})
		return
	}
	for _, service := range snapshot.Services {
		for _, mount := range service.Mounts {
			if mount.Type == "volume" {
				refs[mount.Source] = true
			}
		}
	}
	objectResources, err := h.managedObjectResources(ctx)
	if err != nil {
		c.JSON(500, gin.H{"error": "无法读取对象存储绑定"})
		return
	}
	for _, app := range apps {
		var mounts []model.AppStorageMount
		if app.StorageMountsJSON != "" && json.Unmarshal([]byte(app.StorageMountsJSON), &mounts) != nil {
			c.JSON(500, gin.H{"error": "应用的卷配置无效"})
			return
		}
		object := appObjectStorage(app, objectResources)
		if len(mounts) == 0 && object == nil {
			continue
		}
		details := describeSwarmMounts(snapshot, app.Slug, mounts)
		storage := model.AppStorageOverview{ObjectStorage: object, AppSlug: app.Slug, AppName: app.Name, AppStatus: app.Status, Mounts: details, MountsCount: len(details)}
		for _, item := range details {
			if item.Status == "mounted" {
				summary.TotalVolumes++
			}
			storage.TotalBytes += item.SizeBytes
		}
		storage.TotalHuman = deploy.FormatBytes(uint64(storage.TotalBytes))
		summary.TotalBytes += storage.TotalBytes
		summary.AppsStorage = append(summary.AppsStorage, storage)
	}
	for _, vol := range snapshot.Volumes {
		if !strings.HasPrefix(vol.Name, "opc-vol-") || refs[vol.Name] || vol.RefCount != 0 {
			continue
		}
		size := max(0, vol.SizeBytes)
		human := "未知"
		if vol.SizeBytes >= 0 {
			human = deploy.FormatBytes(uint64(size))
		}
		summary.OrphanVolumes = append(summary.OrphanVolumes, model.OrphanVolumeInfo{VolumeName: vol.Name, Driver: vol.Driver, SizeBytes: size, SizeHuman: human})
		summary.OrphanTotalBytes += size
	}
	summary.TotalHuman = deploy.FormatBytes(uint64(summary.TotalBytes))
	summary.OrphanVolumesCount = len(summary.OrphanVolumes)
	summary.OrphanTotalHuman = deploy.FormatBytes(uint64(summary.OrphanTotalBytes))
	c.JSON(http.StatusOK, summary)
}
