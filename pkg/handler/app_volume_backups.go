package handler

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	defaultVolumeBackupRetentionDays = 30
	defaultVolumeBackupMaxBytes      = int64(2) * 1024 * 1024 * 1024
)

func (h *Handler) enrichSwarmMountStats(ctx context.Context, details []model.AppStorageMountDetail) {
	if h.caproverClient == nil || len(details) == 0 {
		return
	}
	byNode := make(map[string][]string)
	for _, d := range details {
		if d.HostPath != "" && d.StorageNodeID != "" {
			byNode[d.StorageNodeID] = append(byNode[d.StorageNodeID], d.HostPath)
		}
	}
	if len(byNode) == 0 {
		return
	}
	statMap := make(map[string]deploy.ApplicationVolumePathStat)
	for nodeID, paths := range byNode {
		stats, err := h.caproverClient.InspectApplicationVolumePaths(ctx, nodeID, paths)
		if err != nil {
			continue
		}
		for _, st := range stats {
			statMap[st.HostPath] = st
		}
	}
	for i := range details {
		st, ok := statMap[details[i].HostPath]
		if !ok {
			continue
		}
		details[i].FileCount = st.FileCount
		details[i].SizeBytes = st.SizeBytes
		details[i].SizeHuman = deploy.FormatBytes(uint64(maxInt64(0, st.SizeBytes)))
		if len(st.SampleFiles) > 0 {
			details[i].SampleFiles = st.SampleFiles
		}
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func toVolumeBackupItem(b model.AppVolumeBackup) model.AppVolumeBackupItem {
	var mounts []model.AppVolumeBackupMount
	if b.MountsJSON != "" {
		_ = json.Unmarshal([]byte(b.MountsJSON), &mounts)
	}
	if mounts == nil {
		mounts = []model.AppVolumeBackupMount{}
	}
	for i := range mounts {
		if mounts[i].SizeHuman == "" {
			mounts[i].SizeHuman = deploy.FormatBytes(uint64(maxInt64(0, mounts[i].SizeBytes)))
		}
	}
	remainDays := int(math.Ceil(time.Until(b.ExpiresAt).Hours() / 24))
	if remainDays < 0 {
		remainDays = 0
	}
	return model.AppVolumeBackupItem{
		ID:           b.ID.String(),
		AppSlug:      b.AppSlug,
		AppName:      b.AppName,
		AppVersion:   b.AppVersion,
		OwnerID:      b.OwnerID,
		Filename:     b.Filename,
		SizeBytes:    b.SizeBytes,
		SizeHuman:    deploy.FormatBytes(uint64(maxInt64(0, b.SizeBytes))),
		FileCount:    b.FileCount,
		SHA256:       b.SHA256,
		Status:       b.Status,
		AppDeleted:   b.AppDeleted,
		Downloaded:   b.DownloadedAt != nil,
		DownloadedAt: b.DownloadedAt,
		ExpiresAt:    b.ExpiresAt,
		RemainDays:   remainDays,
		CreatedAt:    b.CreatedAt,
		Mounts:       mounts,
	}
}

type exportedTarManifest struct {
	Mounts []struct {
		ContainerPath string `json:"container_path"`
		ArchivePath   string `json:"archive_path"`
		FileCount     int    `json:"file_count"`
		SizeBytes     int64  `json:"size_bytes"`
	} `json:"mounts"`
}

func parseManifestFromTarGz(filePath string) ([]model.AppVolumeBackupMount, int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, 0, err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if path.Clean(hdr.Name) == "manifest.json" {
			data, err := io.ReadAll(io.LimitReader(tr, 2*1024*1024))
			if err != nil {
				return nil, 0, err
			}
			var parsed exportedTarManifest
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, 0, err
			}
			out := make([]model.AppVolumeBackupMount, 0, len(parsed.Mounts))
			totalFiles := 0
			for _, m := range parsed.Mounts {
				totalFiles += m.FileCount
				out = append(out, model.AppVolumeBackupMount{
					ContainerPath: m.ContainerPath,
					ArchivePath:   m.ArchivePath,
					FileCount:     m.FileCount,
					SizeBytes:     m.SizeBytes,
					SizeHuman:     deploy.FormatBytes(uint64(maxInt64(0, m.SizeBytes))),
				})
			}
			return out, totalFiles, nil
		}
	}
	return nil, 0, nil
}

func (h *Handler) exportAppMountsToTempFile(ctx context.Context, app *model.DeployedApp, mounts []model.AppStorageMount) (string, int64, string, []model.AppVolumeBackupMount, int, error) {
	tmp, err := os.CreateTemp("", "opc-app-backup-*.tar.gz")
	if err != nil {
		return "", 0, "", nil, 0, err
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	var srcReader io.ReadCloser
	if h.usesRemoteStorage() {
		if h.caproverClient == nil {
			cleanupTmp()
			return "", 0, "", nil, 0, fmt.Errorf("未连接 Deploy，无法导出挂载卷")
		}
		nodeID := ""
		exportMounts := make([]deploy.ApplicationVolumeExportMount, 0, len(mounts))
		for _, m := range mounts {
			if m.HostPath == "" {
				continue
			}
			if nodeID == "" {
				nodeID = m.StorageNodeID
			}
			exportMounts = append(exportMounts, deploy.ApplicationVolumeExportMount{
				ContainerPath: m.ContainerPath,
				HostPath:      m.HostPath,
				Name:          m.Name,
			})
		}
		if len(exportMounts) == 0 || nodeID == "" {
			cleanupTmp()
			return "", 0, "", nil, 0, fmt.Errorf("应用挂载卷缺少有效宿主机路径，无法导出")
		}
		srcReader, err = h.caproverClient.ExportApplicationVolumes(ctx, deploy.ApplicationVolumeExportRequest{
			NodeID:   nodeID,
			AppSlug:  app.Slug,
			AppName:  app.Name,
			Mounts:   exportMounts,
			MaxBytes: defaultVolumeBackupMaxBytes,
		})
		if err != nil {
			cleanupTmp()
			return "", 0, "", nil, 0, err
		}
		defer srcReader.Close()

		hasher := sha256.New()
		mw := io.MultiWriter(tmp, hasher)
		written, copyErr := io.Copy(mw, io.LimitReader(srcReader, defaultVolumeBackupMaxBytes+1))
		if copyErr != nil {
			cleanupTmp()
			return "", 0, "", nil, 0, copyErr
		}
		if written > defaultVolumeBackupMaxBytes {
			cleanupTmp()
			return "", 0, "", nil, 0, fmt.Errorf("导出归档超过大小限制 (2 GB)")
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return "", 0, "", nil, 0, err
		}
		sum := hex.EncodeToString(hasher.Sum(nil))
		parsedMounts, totalFiles, _ := parseManifestFromTarGz(tmpPath)
		if len(parsedMounts) == 0 {
			for _, m := range mounts {
				cleanRel := strings.TrimLeft(path.Clean(m.ContainerPath), "/")
				parsedMounts = append(parsedMounts, model.AppVolumeBackupMount{
					ContainerPath: m.ContainerPath,
					ArchivePath:   "volumes/" + cleanRel + "/",
				})
			}
		}
		return tmpPath, written, sum, parsedMounts, totalFiles, nil
	}

	si := h.getStorageInspector()
	if _, err := si.FindAppContainerID(ctx, app.Slug); err != nil {
		cleanupTmp()
		return "", 0, "", nil, 0, fmt.Errorf("应用容器未在线运行，无法导出本地挂载卷: %w", err)
	}
	gw := gzip.NewWriter(tmp)
	if err := si.StreamMountBackup(ctx, app.Slug, mounts[0].ContainerPath, gw); err != nil {
		_ = gw.Close()
		cleanupTmp()
		return "", 0, "", nil, 0, err
	}
	if err := gw.Close(); err != nil {
		cleanupTmp()
		return "", 0, "", nil, 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, "", nil, 0, err
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, "", nil, 0, err
	}
	defer f.Close()
	hasher := sha256.New()
	written, err := io.Copy(hasher, f)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, "", nil, 0, err
	}
	backupMounts := make([]model.AppVolumeBackupMount, 0, len(mounts))
	for _, m := range mounts {
		cleanRel := strings.TrimLeft(path.Clean(m.ContainerPath), "/")
		backupMounts = append(backupMounts, model.AppVolumeBackupMount{
			ContainerPath: m.ContainerPath,
			ArchivePath:   "volumes/" + cleanRel + "/",
		})
	}
	return tmpPath, written, hex.EncodeToString(hasher.Sum(nil)), backupMounts, 0, nil
}

func (h *Handler) CreateAppVolumeBackup(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	if !h.tryLockDeploySlug(slug) {
		writeErrorEnvelope(c, http.StatusConflict, "APP_BUSY", "应用正在执行部署或管理操作")
		return
	}
	defer h.unlockDeploySlug(slug)

	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		writeErrorEnvelope(c, http.StatusNotFound, "APP_NOT_FOUND", "应用不存在")
		return
	}

	var mounts []model.AppStorageMount
	if app.StorageMountsJSON != "" {
		_ = json.Unmarshal([]byte(app.StorageMountsJSON), &mounts)
	}
	if len(mounts) == 0 {
		writeErrorEnvelope(c, http.StatusBadRequest, "NO_STORAGE_MOUNTS", "该应用未配置持久化挂载卷，无需导出")
		return
	}

	store := h.resolveArtifactStore(ctx)
	if store == nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "BACKUP_STORE_UNAVAILABLE", "对象存储未就绪，无法归档挂载卷")
		return
	}

	tmpPath, sizeBytes, shaHex, backupMounts, totalFiles, err := h.exportAppMountsToTempFile(ctx, app, mounts)
	if err != nil {
		writeErrorEnvelope(c, http.StatusBadGateway, "EXPORT_FAILED", fmt.Sprintf("挂载卷导出失败: %v", err))
		return
	}
	defer os.Remove(tmpPath)

	now := time.Now().UTC()
	backupID := uuid.New()
	ownerID := strings.TrimSpace(c.GetString(appOwnerKey))
	if ownerID == "" {
		ownerID = strings.TrimSpace(app.CreatedBy)
	}
	if ownerID == "" {
		ownerID = "system"
	}
	filename := fmt.Sprintf("%s-volumes-%s.tar.gz", slug, now.Format("20060102-150405"))
	objectKey := deploy.BuildVolumeBackupObjectKey(ownerID, backupID.String(), filename)

	if err := store.PutObjectFile(ctx, objectKey, tmpPath, sizeBytes, "application/gzip"); err != nil {
		writeErrorEnvelope(c, http.StatusBadGateway, "BACKUP_UPLOAD_FAILED", fmt.Sprintf("归档上传对象存储失败: %v", err))
		return
	}

	mountsBytes, _ := json.Marshal(backupMounts)
	record := &model.AppVolumeBackup{
		ID:         backupID,
		AppSlug:    app.Slug,
		AppName:    app.Name,
		AppVersion: app.CurrentVersion,
		OwnerID:    ownerID,
		ObjectKey:  objectKey,
		Filename:   filename,
		SizeBytes:  sizeBytes,
		FileCount:  totalFiles,
		SHA256:     shaHex,
		MountsJSON: string(mountsBytes),
		Status:     "ready",
		AppDeleted: false,
		ExpiresAt:  now.Add(defaultVolumeBackupRetentionDays * 24 * time.Hour),
		CreatedAt:  now,
	}
	if err := h.storage.CreateAppVolumeBackup(ctx, record); err != nil {
		_ = store.DeleteObject(ctx, objectKey)
		writeErrorEnvelope(c, http.StatusInternalServerError, "BACKUP_SAVE_FAILED", fmt.Sprintf("保存备份记录失败: %v", err))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"backup": toVolumeBackupItem(*record),
	})
}

func (h *Handler) ListAppVolumeBackups(c *gin.Context) {
	ctx := c.Request.Context()
	owner := strings.TrimSpace(c.GetString(appOwnerKey))
	onlyDeleted := c.Query("all") != "true"
	rows, err := h.storage.ListAppVolumeBackups(ctx, owner, onlyDeleted)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, "BACKUP_LIST_FAILED", err.Error())
		return
	}
	items := make([]model.AppVolumeBackupItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toVolumeBackupItem(row))
	}
	c.JSON(http.StatusOK, gin.H{
		"backups":        items,
		"retention_days": defaultVolumeBackupRetentionDays,
	})
}

func (h *Handler) loadOwnedVolumeBackup(c *gin.Context) (*model.AppVolumeBackup, bool) {
	id, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, "INVALID_BACKUP_ID", "无效的备份 ID")
		return nil, false
	}
	backup, err := h.storage.GetAppVolumeBackupByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErrorEnvelope(c, http.StatusNotFound, "BACKUP_NOT_FOUND", "备份记录不存在或已删除")
		} else {
			writeErrorEnvelope(c, http.StatusInternalServerError, "BACKUP_LOAD_FAILED", err.Error())
		}
		return nil, false
	}
	owner := strings.TrimSpace(c.GetString(appOwnerKey))
	if owner != "" && backup.OwnerID != owner {
		allowed := false
		if backup.AppSlug != "" {
			if app, appErr := h.storage.GetAppBySlug(c.Request.Context(), backup.AppSlug); appErr == nil && h.canManageApp(c.Request.Context(), app, owner) {
				allowed = true
			}
		}
		if !allowed {
			writeErrorEnvelope(c, http.StatusForbidden, "BACKUP_FORBIDDEN", "无权访问该备份记录")
			return nil, false
		}
	}
	return backup, true
}

func (h *Handler) DownloadAppVolumeBackupFile(c *gin.Context) {
	backup, ok := h.loadOwnedVolumeBackup(c)
	if !ok {
		return
	}
	if backup.Status != "ready" || time.Now().UTC().After(backup.ExpiresAt) {
		writeErrorEnvelope(c, http.StatusGone, "BACKUP_EXPIRED", "该备份已过期或不可用")
		return
	}
	ctx := c.Request.Context()
	store := h.resolveArtifactStore(ctx)
	if store == nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "BACKUP_STORE_UNAVAILABLE", "对象存储未就绪")
		return
	}
	reader, contentLen, contentType, err := store.OpenObjectStream(ctx, backup.ObjectKey)
	if err != nil {
		writeErrorEnvelope(c, http.StatusNotFound, "BACKUP_OBJECT_MISSING", fmt.Sprintf("读取备份文件失败: %v", err))
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, backup.Filename))
	c.Header("Content-Type", contentType)
	if contentLen > 0 {
		c.Header("Content-Length", fmt.Sprintf("%d", contentLen))
	} else if backup.SizeBytes > 0 {
		c.Header("Content-Length", fmt.Sprintf("%d", backup.SizeBytes))
	}
	written, copyErr := io.Copy(c.Writer, reader)
	if copyErr != nil || (backup.SizeBytes > 0 && written != backup.SizeBytes) {
		return
	}
	_ = h.storage.MarkAppVolumeBackupDownloaded(context.Background(), backup.ID, time.Now().UTC())
}

func (h *Handler) DeleteAppVolumeBackup(c *gin.Context) {
	backup, ok := h.loadOwnedVolumeBackup(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if store := h.resolveArtifactStore(ctx); store != nil {
		if err := store.DeleteObject(ctx, backup.ObjectKey); err != nil {
			writeErrorEnvelope(c, http.StatusBadGateway, "BACKUP_DELETE_FAILED", fmt.Sprintf("删除对象存储文件失败: %v", err))
			return
		}
	}
	if err := h.storage.SoftDeleteAppVolumeBackup(ctx, backup.ID, time.Now().UTC()); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		writeErrorEnvelope(c, http.StatusInternalServerError, "BACKUP_DELETE_FAILED", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true, "id": backup.ID.String()})
}

func (h *Handler) validateDeleteBackupGate(ctx context.Context, slug, owner, exportMode, backupIDRaw string) error {
	if exportMode != "export_and_delete" {
		return nil
	}
	backupID, err := uuid.Parse(strings.TrimSpace(backupIDRaw))
	if err != nil {
		return fmt.Errorf("missing_or_invalid_backup_id")
	}
	backup, err := h.storage.GetAppVolumeBackupByID(ctx, backupID)
	if err != nil {
		return fmt.Errorf("backup_not_found")
	}
	if backup.AppSlug != slug {
		return fmt.Errorf("backup_slug_mismatch")
	}
	if owner != "" && backup.OwnerID != owner {
		return fmt.Errorf("backup_owner_mismatch")
	}
	if backup.Status != "ready" || time.Now().UTC().After(backup.ExpiresAt) {
		return fmt.Errorf("backup_not_ready")
	}
	if backup.DownloadedAt == nil {
		return fmt.Errorf("backup_not_downloaded")
	}
	if store := h.resolveArtifactStore(ctx); store != nil && !store.ObjectExists(ctx, backup.ObjectKey) {
		return fmt.Errorf("backup_object_missing")
	}
	return nil
}

func (h *Handler) CleanupExpiredAppVolumeBackups(ctx context.Context) {
	if h.storage == nil {
		return
	}
	now := time.Now().UTC()
	expired, err := h.storage.ListExpiredAppVolumeBackups(ctx, now, 100)
	if err != nil {
		return
	}
	store := h.resolveArtifactStore(ctx)
	for _, item := range expired {
		if store != nil {
			if err := store.DeleteObject(ctx, item.ObjectKey); err != nil {
				log.Printf("[CleanupExpiredAppVolumeBackups] delete object %s failed: %v", item.ObjectKey, err)
				continue
			}
		}
		if err := h.storage.MarkAppVolumeBackupExpired(ctx, item.ID, now); err != nil {
			log.Printf("[CleanupExpiredAppVolumeBackups] mark expired %s failed: %v", item.ID, err)
		}
	}
}
