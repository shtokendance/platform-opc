package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

var volumeNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// getStorageInspector 获取或初始化 StorageInspector
func (h *Handler) getStorageInspector() *deploy.StorageInspector {
	if strings.TrimSpace(h.dockerSockPath) != "" {
		return deploy.NewStorageInspector(h.dockerSockPath)
	}
	return deploy.NewStorageInspector("/var/run/docker.sock")
}

// AdminGetGlobalStorage GET /api/v1/apps/admin/storage
// 全局存储管理大盘：宿主机磁盘水位信息 + 全量应用持久化卷总览 + 孤儿卷检测
func (h *Handler) AdminGetGlobalStorage(c *gin.Context) {
	ctx := c.Request.Context()
	si := h.getStorageInspector()

	diskInfo := si.GetServerDiskInfo("/")

	apps, err := h.storage.ListApps(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("list apps: %v", err)})
		return
	}

	// 1. 批量获取 Docker 卷元数据与容器运行态缓存
	rawVolumes, inspectErr := si.QueryDockerVolumesRaw(ctx)
	if inspectErr != nil {
		c.JSON(503, gin.H{"error": inspectErr.Error()})
		return
	}
	volSizes := make(map[string]int64)
	for _, v := range rawVolumes {
		if v != nil {
			sz := v.UsageData.Size
			if sz < 0 {
				sz = 0
			}
			volSizes[v.Name] = sz
		}
	}
	appContainers, _ := si.ListAppContainers(ctx)

	var totalVols int
	var totalBytes int64
	appsStorage := make([]model.AppStorageOverview, 0, len(apps))
	knownVolumeNames, refErr := h.referencedAppVolumes(ctx)
	if refErr != nil {
		c.JSON(503, gin.H{"error": refErr.Error()})
		return
	}

	for _, app := range apps {
		var mounts []model.AppStorageMount
		if app.StorageMountsJSON != "" {
			_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
		}
		if len(mounts) == 0 {
			continue
		}

		for _, m := range mounts {
			if m.VolumeName != "" {
				knownVolumeNames[m.VolumeName] = true
			}
		}

		containerID := ""
		if appContainers != nil {
			containerID = appContainers[app.Slug]
		}

		mountDetails := si.InspectAppMountsWithCache(ctx, app.Slug, mounts, volSizes, containerID)
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

	// 2. 识别孤儿卷 (以 opc-vol- 开头，但当前没有任何已登记应用引用的 Docker 卷)
	var orphanVolumes []model.OrphanVolumeInfo
	var orphanTotalBytes int64
	for _, v := range rawVolumes {
		if v == nil || !strings.HasPrefix(v.Name, "opc-vol-") {
			continue
		}
		if !knownVolumeNames[v.Name] {
			sz := v.UsageData.Size
			if sz < 0 {
				sz = 0
			}
			orphanTotalBytes += sz

			parts := strings.Split(strings.TrimPrefix(v.Name, "opc-vol-"), "-")
			parsedSlug := ""
			parsedMount := ""
			if len(parts) >= 2 {
				parsedSlug = parts[0]
				parsedMount = strings.Join(parts[1:], "-")
			} else if len(parts) == 1 {
				parsedSlug = parts[0]
			}

			orphanVolumes = append(orphanVolumes, model.OrphanVolumeInfo{
				VolumeName: v.Name,
				AppSlug:    parsedSlug,
				MountName:  parsedMount,
				Driver:     v.Driver,
				SizeBytes:  sz,
				SizeHuman:  deploy.FormatBytes(uint64(sz)),
				CreatedAt:  v.CreatedAt,
			})
		}
	}

	c.JSON(http.StatusOK, model.GlobalStorageSummary{
		ServerDisk:         diskInfo,
		TotalVolumes:       totalVols,
		TotalBytes:         totalBytes,
		TotalHuman:         deploy.FormatBytes(uint64(totalBytes)),
		AppsStorage:        appsStorage,
		OrphanVolumes:      orphanVolumes,
		OrphanVolumesCount: len(orphanVolumes),
		OrphanTotalBytes:   orphanTotalBytes,
		OrphanTotalHuman:   deploy.FormatBytes(uint64(orphanTotalBytes)),
	})
}

// PurgeOrphanVolumesRequest 批量清理孤儿卷请求体（留空则清理全部识别出的孤儿卷）
type PurgeOrphanVolumesRequest struct {
	VolumeNames []string `json:"volume_names"`
}

// AdminPurgeOrphanVolumes POST /api/v1/apps/admin/storage/orphan-volumes/purge
// 批量清理孤儿持久化存储卷
func (h *Handler) AdminPurgeOrphanVolumes(c *gin.Context) {
	ctx := c.Request.Context()
	var req PurgeOrphanVolumesRequest
	_ = c.ShouldBindJSON(&req)
	if h.hasActiveDeploys() {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot purge volumes while a deployment or rollback is in progress"})
		return
	}

	si := h.getStorageInspector()
	toDelete := req.VolumeNames
	refs, refErr := h.referencedAppVolumes(ctx)
	if refErr != nil {
		c.JSON(503, gin.H{"error": refErr.Error()})
		return
	}

	// 若未指定具体卷名，自动扫描全量孤儿卷
	if len(toDelete) == 0 {
		apps, err := h.storage.ListApps(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("list apps: %v", err)})
			return
		}
		knownVols := make(map[string]bool)
		for _, app := range apps {
			var mounts []model.AppStorageMount
			if app.StorageMountsJSON != "" {
				_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
			}
			for _, m := range mounts {
				if m.VolumeName != "" {
					knownVols[m.VolumeName] = true
				}
			}
		}

		rawVols, err := si.QueryDockerVolumesRaw(ctx)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("query volumes: %v", err)})
			return
		}
		for _, v := range rawVols {
			if v != nil && strings.HasPrefix(v.Name, "opc-vol-") && !knownVols[v.Name] && !refs[v.Name] {
				toDelete = append(toDelete, v.Name)
			}
		}
	}

	purged := make([]string, 0, len(toDelete))
	failed := make(map[string]string)
	for _, vName := range toDelete {
		if refs[vName] {
			failed[vName] = "volume is referenced by an application or historical version"
			continue
		}
		if !strings.HasPrefix(vName, "opc-vol-") {
			failed[vName] = "volume name must start with opc-vol-"
			continue
		}
		if err := si.RemoveDockerVolume(ctx, vName, false); err != nil {
			failed[vName] = err.Error()
		} else {
			purged = append(purged, vName)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": fmt.Sprintf("成功清理 %d 个孤儿卷，失败 %d 个", len(purged), len(failed)),
		"purged":  purged,
		"failed":  failed,
	})
}

// AdminDeleteVolume DELETE /api/v1/apps/admin/storage/volumes/:name
// 删除指定的持久化存储卷
func (h *Handler) AdminDeleteVolume(c *gin.Context) {
	ctx := c.Request.Context()
	vName := strings.TrimSpace(c.Param("name"))
	if !volumeNameRegex.MatchString(vName) || !strings.HasPrefix(vName, "opc-vol-") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid volume name (must start with opc-vol-)"})
		return
	}
	if h.hasActiveDeploys() {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot delete volume while a deployment or rollback is in progress"})
		return
	}

	refs, refErr := h.referencedAppVolumes(ctx)
	if refErr != nil {
		c.JSON(503, gin.H{"error": refErr.Error()})
		return
	}
	if refs[vName] {
		c.JSON(409, gin.H{"error": "volume is referenced by an application or historical version"})
		return
	}
	si := h.getStorageInspector()
	if err := si.RemoveDockerVolume(ctx, vName, false); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("remove volume %s: %v", vName, err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":        0,
		"message":     fmt.Sprintf("卷 %s 已成功删除", vName),
		"volume_name": vName,
	})
}

// GetAppStorageDetails GET /api/v1/apps/:slug/storage
// 单个应用的持久化挂载卷实时详情（包含物理卷名、占用容量、文件列表样例、种子保护状态）
func (h *Handler) GetAppStorageDetails(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	if c.GetString(appOwnerKey) != "" && !h.checkAppOwner(c, slug, false) {
		return
	}

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
	mountDetails, inspectErr := si.InspectAppMountsChecked(ctx, slug, mounts)
	if inspectErr != nil {
		c.JSON(503, gin.H{"error": inspectErr.Error()})
		return
	}

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
	if c.GetString(appOwnerKey) != "" && !cHandler.checkAppOwner(c, slug, false) {
		return
	}
	targetPath := strings.TrimSpace(c.Query("path"))
	targetMount := strings.TrimSpace(c.Query("mount"))

	app, err := cHandler.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	var mounts []model.AppStorageMount
	if app.StorageMountsJSON != "" {
		_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
	}

	if len(mounts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该应用未配置持久化挂载卷，无存储数据可导出"})
		return
	}

	// 白名单校验：匹配指定的挂载目录或挂载点名称
	var matchedMount *model.AppStorageMount
	if targetMount != "" {
		for i := range mounts {
			if mounts[i].Name == targetMount {
				matchedMount = &mounts[i]
				break
			}
		}
	} else if targetPath != "" {
		for i := range mounts {
			if mounts[i].ContainerPath == targetPath {
				matchedMount = &mounts[i]
				break
			}
		}
	} else {
		// 默认取第一个挂载卷
		matchedMount = &mounts[0]
	}

	if matchedMount == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("指定的挂载点或路径不在应用合法持久化配置清单中 (path=%s, mount=%s)", targetPath, targetMount),
		})
		return
	}

	// 预先检查容器是否在线，防止写 HTTP 头后再报 500
	si := cHandler.getStorageInspector()
	if _, err := si.FindAppContainerID(ctx, slug); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": fmt.Sprintf("应用容器未在线运行，无法导出挂载卷数据: %v", err),
		})
		return
	}

	cleanFileName := strings.ReplaceAll(strings.Trim(matchedMount.ContainerPath, "/"), "/", "-")
	if cleanFileName == "" {
		cleanFileName = matchedMount.Name
	}
	filename := fmt.Sprintf("%s-storage-%s.tar", slug, cleanFileName)

	tmp, err := os.CreateTemp("", "opc-volume-backup-*.tar")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := si.StreamMountBackup(ctx, slug, matchedMount.ContainerPath, tmp); err != nil {
		c.JSON(502, gin.H{"error": fmt.Sprintf("backup failed: %v", err)})
		return
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Content-Type", "application/x-tar")
	_, _ = io.Copy(c.Writer, tmp)

}

// referencedAppVolumes protects both current and rollback snapshots, including failed attempts.
func (h *Handler) referencedAppVolumes(ctx context.Context) (map[string]bool, error) {
	refs := map[string]bool{}
	var apps []model.DeployedApp
	if err := h.storage.DeployDB().WithContext(ctx).Find(&apps).Error; err != nil {
		return nil, err
	}
	var versions []model.AppDeploymentVersion
	if err := h.storage.DeployDB().WithContext(ctx).Find(&versions).Error; err != nil {
		return nil, err
	}
	collect := func(raw string) error {
		var mounts []model.AppStorageMount
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
				return err
			}
		}
		for _, m := range mounts {
			if m.VolumeName != "" {
				refs[m.VolumeName] = true
			}
		}
		return nil
	}
	for _, a := range apps {
		if err := collect(a.StorageMountsJSON); err != nil {
			return nil, err
		}
	}
	for _, v := range versions {
		if err := collect(v.StorageMountsJSON); err != nil {
			return nil, err
		}
	}
	return refs, nil
}
