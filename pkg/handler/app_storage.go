package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

// getStorageInspector 获取或初始化 StorageInspector
func (h *Handler) getStorageInspector() *deploy.StorageInspector {
	return deploy.NewStorageInspector("/var/run/docker.sock")
}

// AdminGetGlobalStorage GET /api/v1/apps/admin/storage
// 全局存储管理大盘：宿主机磁盘水位信息 + 全量应用持久化卷总览
func (h *Handler) AdminGetGlobalStorage(c *gin.Context) {
	ctx := c.Request.Context()
	si := h.getStorageInspector()

	diskInfo := si.GetServerDiskInfo("/")

	apps, err := h.storage.ListApps(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("list apps: %v", err)})
		return
	}

	var totalVols int
	var totalBytes int64
	appsStorage := make([]model.AppStorageOverview, 0, len(apps))

	for _, app := range apps {
		var mounts []model.AppStorageMount
		if app.StorageMountsJSON != "" {
			_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
		}
		if len(mounts) == 0 {
			continue
		}

		mountDetails := si.InspectAppMounts(ctx, app.Slug, mounts)
		var appBytes int64
		var appFiles int
		for _, m := range mountDetails {
			appBytes += m.SizeBytes
			appFiles += m.FileCount
		}

		totalVols += len(mountDetails)
		totalBytes += appBytes

		appsStorage = append(appsStorage, model.AppStorageOverview{
			AppSlug:     app.Slug,
			AppName:     app.Name,
			AppStatus:   app.Status,
			MountsCount: len(mountDetails),
			TotalBytes:  appBytes,
			TotalHuman:  deploy.FormatBytes(uint64(appBytes)),
			TotalFiles:  appFiles,
			Mounts:      mountDetails,
		})
	}

	c.JSON(http.StatusOK, model.GlobalStorageSummary{
		ServerDisk:   diskInfo,
		TotalVolumes: totalVols,
		TotalBytes:   totalBytes,
		TotalHuman:   deploy.FormatBytes(uint64(totalBytes)),
		AppsStorage:  appsStorage,
	})
}

// GetAppStorageDetails GET /api/v1/apps/:slug/storage
// 单个应用的持久化挂载卷实时详情（包含物理卷名、占用容量、文件列表样例、种子保护状态）
func (h *Handler) GetAppStorageDetails(c *gin.Context) {
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

	si := h.getStorageInspector()
	mountDetails := si.InspectAppMounts(ctx, slug, mounts)

	var appBytes int64
	var appFiles int
	for _, m := range mountDetails {
		appBytes += m.SizeBytes
		appFiles += m.FileCount
	}

	c.JSON(http.StatusOK, model.AppStorageOverview{
		AppSlug:     slug,
		AppName:     app.Name,
		AppStatus:   app.Status,
		MountsCount: len(mountDetails),
		TotalBytes:  appBytes,
		TotalHuman:  deploy.FormatBytes(uint64(appBytes)),
		TotalFiles:  appFiles,
		Mounts:      mountDetails,
	})
}

// DownloadAppStorageBackup GET /api/v1/apps/:slug/storage/backup
// 一键打包下载指定挂载点或应用持久化存储数据的 tar 归档流
func (cHandler *Handler) DownloadAppStorageBackup(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	targetPath := strings.TrimSpace(c.Query("path"))

	app, err := cHandler.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	var mounts []model.AppStorageMount
	if app.StorageMountsJSON != "" {
		_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
	}

	if targetPath == "" && len(mounts) > 0 {
		targetPath = mounts[0].ContainerPath
	}
	if targetPath == "" {
		targetPath = "/app/data"
	}

	cleanFileName := strings.ReplaceAll(strings.Trim(targetPath, "/"), "/", "-")
	if cleanFileName == "" {
		cleanFileName = "data"
	}
	filename := fmt.Sprintf("%s-storage-%s.tar", slug, cleanFileName)

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Content-Type", "application/x-tar")

	si := cHandler.getStorageInspector()
	if err := si.StreamMountBackup(ctx, slug, targetPath, c.Writer); err != nil {
		c.Status(http.StatusInternalServerError)
		_, _ = c.Writer.Write([]byte(fmt.Sprintf("backup failed: %v", err)))
	}
}
