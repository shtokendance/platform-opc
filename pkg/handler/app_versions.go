package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AppVersionDetailResponse 单个应用版本详情响应（包含 MinIO 物理文件状态、接口/页面清单与下载入口）
type AppVersionDetailResponse struct {
	ID                uuid.UUID              `json:"id"`
	AppSlug           string                 `json:"app_slug"`
	Version           int                    `json:"version"`
	VersionLabel      string                 `json:"version_label"`
	VersionNote       string                 `json:"version_note"`
	Status            string                 `json:"status"`
	IsActive          bool                   `json:"is_active"`
	CaproverVersion   int                    `json:"caprover_version"`
	DeployedImageName string                 `json:"deployed_image_name"`
	ContainerPort     int                    `json:"container_port"`
	Language          string                 `json:"language"`
	RuntimeVersion    string                 `json:"runtime_version"`
	DockerfileContent string                 `json:"dockerfile_content"`
	AppConfig         []model.AppConfigItem   `json:"app_config"`
	ResourceIDs       []string                `json:"resource_ids"`
	StorageMounts     []model.AppStorageMount `json:"storage_mounts"`
	StorageBucket     string                  `json:"storage_bucket"`
	AppTarObjectKey   string                 `json:"app_tar_object_key"`
	AppTarSHA256      string                 `json:"app_tar_sha256"`
	AppTarSize        int64                  `json:"app_tar_size"`
	HasAppTar         bool                   `json:"has_app_tar"`
	SkillZipObjectKey string                 `json:"skill_zip_object_key"`
	SkillZipSHA256    string                 `json:"skill_zip_sha256"`
	SkillZipSize      int64                  `json:"skill_zip_size"`
	HasSkillZip       bool                   `json:"has_skill_zip"`
	CatalogSkillID    string                 `json:"catalog_skill_id"`
	APIsSummary       []model.AppAPISummary  `json:"apis_summary"`
	PagesSummary      []model.AppPageSummary `json:"pages_summary"`
	DownloadURLs      map[string]string      `json:"download_urls"`
	ErrorMessage      string                 `json:"error_message,omitempty"`
	CreatedBy         string                 `json:"created_by"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
}

// RollbackAppVersionRequest 触发应用回退到历史版本的请求体
type RollbackAppVersionRequest struct {
	TargetVersion int    `json:"target_version" binding:"required"`
	Reason        string `json:"reason"`
}

// resolveArtifactStore 动态解析 MinIO / S3 存储客户端（优先使用测试注入实例，其次从 deploy_resources 的 minio/oss 资源或环境变量读取）
func (h *Handler) resolveArtifactStore(ctx context.Context) *deploy.ArtifactStore {
	h.artifactStoreMu.Lock()
	defer h.artifactStoreMu.Unlock()
	if h.artifactStore != nil {
		return h.artifactStore
	}

	endpoint := strings.TrimSpace(os.Getenv("S3_ENDPOINT"))
	accessKey := strings.TrimSpace(os.Getenv("S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("S3_SECRET_KEY"))
	bucket := strings.TrimSpace(os.Getenv("S3_BUCKET"))

	resources := h.loadEffectiveDeployResources(ctx)
	for _, r := range resources {
		tLower := strings.ToLower(strings.TrimSpace(r.Type))
		if tLower == "oss" || tLower == "minio" || tLower == "s3" || strings.Contains(strings.ToLower(r.ID), "minio") || r.EnvVars["S3_ENDPOINT"] != "" {
			if endpoint == "" && strings.TrimSpace(r.EnvVars["S3_ENDPOINT"]) != "" {
				endpoint = strings.TrimSpace(r.EnvVars["S3_ENDPOINT"])
			}
			if endpoint == "" && r.Host != "" {
				port := r.Port
				if port == 0 {
					port = 9000
				}
				endpoint = fmt.Sprintf("http://%s:%d", r.Host, port)
			}
			if accessKey == "" {
				if v := strings.TrimSpace(r.EnvVars["S3_ACCESS_KEY"]); v != "" {
					accessKey = v
				} else if r.Username != "" {
					accessKey = r.Username
				}
			}
			if secretKey == "" {
				if v := strings.TrimSpace(r.EnvVars["S3_SECRET_KEY"]); v != "" {
					secretKey = v
				} else if r.Password != "" {
					secretKey = r.Password
				}
			}
			if bucket == "" {
				if v := strings.TrimSpace(r.EnvVars["S3_BUCKET"]); v != "" {
					bucket = v
				} else if r.DatabaseName != "" {
					bucket = r.DatabaseName
				}
			}
			if endpoint != "" && accessKey != "" && secretKey != "" {
				break
			}
		}
	}

	if bucket == "" {
		bucket = "opc-apps"
	}

	h.artifactStore = deploy.NewArtifactStore(deploy.ArtifactStoreConfig{
		Endpoint:  endpoint,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Bucket:    bucket,
	})
	return h.artifactStore
}

// extractSkillContractSummaries 从上传的 skill.zip 中解析 references/operations.json，提取该版本的 API 列表与页面列表摘要
func extractSkillContractSummaries(zipBytes []byte) ([]model.AppAPISummary, []model.AppPageSummary) {
	apis := make([]model.AppAPISummary, 0)
	pages := make([]model.AppPageSummary, 0)
	if len(zipBytes) == 0 {
		return apis, pages
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return apis, pages
	}
	for _, f := range zr.File {
		cleanName := strings.TrimPrefix(strings.TrimPrefix(f.Name, "./"), "/")
		if cleanName == "references/operations.json" || strings.HasSuffix(cleanName, "/operations.json") {
			rc, openErr := f.Open()
			if openErr != nil {
				continue
			}
			raw, readErr := io.ReadAll(io.LimitReader(rc, 50*1024*1024))
			_ = rc.Close()
			if readErr != nil {
				continue
			}
			var contract struct {
				APIs []struct {
					ID      string `json:"id"`
					Method  string `json:"method"`
					Path    string `json:"path"`
					Purpose string `json:"purpose"`
					Effect  string `json:"effect"`
				} `json:"apis"`
				Pages []struct {
					Path    string `json:"path"`
					Purpose string `json:"purpose"`
				} `json:"pages"`
			}
			if json.Unmarshal(raw, &contract) == nil {
				for _, a := range contract.APIs {
					apis = append(apis, model.AppAPISummary{
						ID:      a.ID,
						Method:  a.Method,
						Path:    a.Path,
						Purpose: a.Purpose,
						Effect:  a.Effect,
					})
				}
				for _, p := range contract.Pages {
					pages = append(pages, model.AppPageSummary{
						Path:    p.Path,
						Purpose: p.Purpose,
					})
				}
				return apis, pages
			}
		}
	}
	return apis, pages
}

// extractSkillStorageMounts 从上传的 skill.zip (references/operations.json) 中提取应用声明的持久化存储挂载目录
func extractSkillStorageMounts(zipBytes []byte) []model.AppStorageMount {
	mounts := make([]model.AppStorageMount, 0)
	if len(zipBytes) == 0 {
		return mounts
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return mounts
	}
	for _, f := range zr.File {
		cleanName := strings.TrimPrefix(strings.TrimPrefix(f.Name, "./"), "/")
		if cleanName == "references/operations.json" || strings.HasSuffix(cleanName, "/operations.json") {
			rc, openErr := f.Open()
			if openErr != nil {
				continue
			}
			raw, readErr := io.ReadAll(io.LimitReader(rc, 50*1024*1024))
			_ = rc.Close()
			if readErr != nil {
				continue
			}
			var contract struct {
				StorageMounts []model.AppStorageMount `json:"storage_mounts"`
			}
			if json.Unmarshal(raw, &contract) == nil && len(contract.StorageMounts) > 0 {
				return contract.StorageMounts
			}
		}
	}
	return mounts
}

// archiveVersionArtifacts 将指定版本的物理文件（app.tar、skill.zip、Dockerfile、manifest.json）归档至 MinIO（含本地缓存）
func (h *Handler) archiveVersionArtifacts(
	ctx context.Context,
	slug string,
	version int,
	tarBytes []byte,
	zipBytes []byte,
	dockerfileContent string,
	apis []model.AppAPISummary,
	pages []model.AppPageSummary,
	versionNote string,
) (bucket, tarKey, tarSHA, zipKey, zipSHA string, err error) {
	store := h.resolveArtifactStore(ctx)
	bucket = store.Bucket()

	tarSum := sha256.Sum256(tarBytes)
	tarSHA = hex.EncodeToString(tarSum[:])
	tarKey = deploy.BuildVersionObjectKey(slug, version, "app.tar")

	zipSum := sha256.Sum256(zipBytes)
	zipSHA = hex.EncodeToString(zipSum[:])
	zipKey = deploy.BuildVersionObjectKey(slug, version, "skill.zip")

	if putErr := store.PutObject(ctx, tarKey, tarBytes, "application/x-tar"); putErr != nil {
		log.Printf("[ArtifactStore][%s][v%d] PutObject app.tar warning: %v", slug, version, putErr)
	}
	if putErr := store.PutObject(ctx, zipKey, zipBytes, "application/zip"); putErr != nil {
		log.Printf("[ArtifactStore][%s][v%d] PutObject skill.zip warning: %v", slug, version, putErr)
	}
	if strings.TrimSpace(dockerfileContent) != "" {
		dfKey := deploy.BuildVersionObjectKey(slug, version, "Dockerfile")
		_ = store.PutObject(ctx, dfKey, []byte(dockerfileContent), "text/plain; charset=utf-8")
	}

	manifestObj := map[string]interface{}{
		"slug":           slug,
		"version":        version,
		"version_note":   versionNote,
		"app_tar_sha256": tarSHA,
		"app_tar_size":   len(tarBytes),
		"skill_zip_sha":  zipSHA,
		"skill_zip_size": len(zipBytes),
		"apis_summary":   apis,
		"pages_summary":  pages,
		"archived_at":    time.Now().UTC().Format(time.RFC3339),
	}
	if manifestBytes, mErr := json.MarshalIndent(manifestObj, "", "  "); mErr == nil {
		manifestKey := deploy.BuildVersionObjectKey(slug, version, "manifest.json")
		_ = store.PutObject(ctx, manifestKey, manifestBytes, "application/json; charset=utf-8")
	}

	return bucket, tarKey, tarSHA, zipKey, zipSHA, nil
}

func (h *Handler) toAppVersionDetailResponse(ctx context.Context, ver model.AppDeploymentVersion, onlineResources []config.OnlineResourceConfig) AppVersionDetailResponse {
	var configs []model.AppConfigItem
	_ = json.Unmarshal([]byte(ver.AppConfigJSON), &configs)
	var resIDs []string
	_ = json.Unmarshal([]byte(ver.ResourceIDsJSON), &resIDs)
	var storageMounts []model.AppStorageMount
	_ = json.Unmarshal([]byte(ver.StorageMountsJSON), &storageMounts)
	if storageMounts == nil {
		storageMounts = []model.AppStorageMount{}
	}
	var apis []model.AppAPISummary
	_ = json.Unmarshal([]byte(ver.APIsSummaryJSON), &apis)
	if apis == nil {
		apis = []model.AppAPISummary{}
	}
	var pages []model.AppPageSummary
	_ = json.Unmarshal([]byte(ver.PagesSummaryJSON), &pages)
	if pages == nil {
		pages = []model.AppPageSummary{}
	}

	store := h.resolveArtifactStore(ctx)
	hasAppTar := ver.AppTarObjectKey != "" && (ver.AppTarSize > 0 || store.ObjectExists(ctx, ver.AppTarObjectKey))
	hasSkillZip := ver.SkillZipObjectKey != "" && (ver.SkillZipSize > 0 || store.ObjectExists(ctx, ver.SkillZipObjectKey))

	basePrefix := fmt.Sprintf("/api/v1/apps/%s/versions/%d/artifacts", ver.AppSlug, ver.Version)
	downloads := map[string]string{
		"app.tar":       basePrefix + "/app.tar",
		"skill.zip":     basePrefix + "/skill.zip",
		"Dockerfile":    basePrefix + "/Dockerfile",
		"manifest.json": basePrefix + "/manifest.json",
	}

	bucket := ver.StorageBucket
	if bucket == "" {
		bucket = store.Bucket()
	}

	return AppVersionDetailResponse{
		ID:                ver.ID,
		AppSlug:           ver.AppSlug,
		Version:           ver.Version,
		VersionLabel:      fmt.Sprintf("v%d", ver.Version),
		VersionNote:       ver.VersionNote,
		Status:            ver.Status,
		IsActive:          ver.IsActive,
		CaproverVersion:   ver.CaproverVersion,
		DeployedImageName: ver.DeployedImageName,
		ContainerPort:     ver.ContainerPort,
		Language:          ver.Language,
		RuntimeVersion:    ver.RuntimeVersion,
		DockerfileContent: ver.DockerfileContent,
		AppConfig:         describeAppConfig(onlineResources, resIDs, configs),
		ResourceIDs:       resIDs,
		StorageMounts:     storageMounts,
		StorageBucket:     bucket,
		AppTarObjectKey:   ver.AppTarObjectKey,
		AppTarSHA256:      ver.AppTarSHA256,
		AppTarSize:        ver.AppTarSize,
		HasAppTar:         hasAppTar,
		SkillZipObjectKey: ver.SkillZipObjectKey,
		SkillZipSHA256:    ver.SkillZipSHA256,
		SkillZipSize:      ver.SkillZipSize,
		HasSkillZip:       hasSkillZip,
		CatalogSkillID:    ver.CatalogSkillID,
		APIsSummary:       apis,
		PagesSummary:      pages,
		DownloadURLs:      downloads,
		ErrorMessage:      ver.ErrorMessage,
		CreatedBy:         ver.CreatedBy,
		CreatedAt:         ver.CreatedAt,
		UpdatedAt:         ver.UpdatedAt,
	}
}

// ListAppVersions GET /api/v1/apps/:slug/versions 查询应用的所有历史发布版本
func (h *Handler) ListAppVersions(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	versions, err := h.storage.ListAppVersions(ctx, slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("list app versions: %v", err)})
		return
	}

	onlineResources := h.loadEffectiveOnlineResources(ctx)
	items := make([]AppVersionDetailResponse, 0, len(versions))
	for _, v := range versions {
		items = append(items, h.toAppVersionDetailResponse(ctx, v, onlineResources))
	}

	currentVer := app.CurrentVersion
	// 若当前标记的 version 不在有效发布列表中，优先寻找当前生效（is_active）或最新运行（running）版本，保证前后端版本展示一致
	foundCurrent := false
	var activeVer int
	for _, it := range items {
		if it.Version == currentVer {
			foundCurrent = true
		}
		if it.IsActive && activeVer == 0 {
			activeVer = it.Version
		}
	}
	if !foundCurrent {
		if activeVer > 0 {
			currentVer = activeVer
		} else if len(items) > 0 {
			currentVer = items[0].Version
		}
	}
	if currentVer <= 0 && len(items) > 0 {
		currentVer = items[0].Version
	}

	c.JSON(http.StatusOK, gin.H{
		"slug":            app.Slug,
		"name":            app.Name,
		"app_url":         app.AppURL,
		"current_version": currentVer,
		"versions":        items,
	})
}

// DownloadAppVersionArtifact GET /api/v1/apps/:slug/versions/:version/artifacts/:filename
// 通过 platform-opc 网关从底层 MinIO 流式读取并下载指定版本的物理文件（app.tar / skill.zip / Dockerfile / manifest.json）
func (h *Handler) DownloadAppVersionArtifact(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	verStr := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(c.Param("version")), "v"))
	verNum, err := strconv.Atoi(verStr)
	if err != nil || verNum <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version number"})
		return
	}

	filename := strings.TrimSpace(c.Param("filename"))
	allowed := map[string]bool{
		"app.tar":       true,
		"skill.zip":     true,
		"Dockerfile":    true,
		"manifest.json": true,
	}
	if !allowed[filename] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported artifact filename, must be app.tar, skill.zip, Dockerfile, or manifest.json"})
		return
	}

	verRecord, err := h.storage.GetAppVersion(ctx, slug, verNum)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("version v%d not found for app %s", verNum, slug)})
		return
	}

	store := h.resolveArtifactStore(ctx)
	objectKey := deploy.BuildVersionObjectKey(slug, verNum, filename)
	switch filename {
	case "app.tar":
		if verRecord.AppTarObjectKey != "" {
			objectKey = verRecord.AppTarObjectKey
		}
	case "skill.zip":
		if verRecord.SkillZipObjectKey != "" {
			objectKey = verRecord.SkillZipObjectKey
		}
	}

	data, contentType, getErr := store.GetObject(ctx, objectKey)
	if getErr != nil {
		// 若请求的是 Dockerfile 且数据库中保存了 DockerfileContent，直接返回数据库快照
		if filename == "Dockerfile" && strings.TrimSpace(verRecord.DockerfileContent) != "" {
			data = []byte(verRecord.DockerfileContent)
			contentType = "text/plain; charset=utf-8"
		} else if filename == "manifest.json" {
			var apis []model.AppAPISummary
			_ = json.Unmarshal([]byte(verRecord.APIsSummaryJSON), &apis)
			var pages []model.AppPageSummary
			_ = json.Unmarshal([]byte(verRecord.PagesSummaryJSON), &pages)
			fallbackManifest, _ := json.MarshalIndent(map[string]interface{}{
				"slug":           verRecord.AppSlug,
				"version":        verRecord.Version,
				"version_note":   verRecord.VersionNote,
				"app_tar_sha256": verRecord.AppTarSHA256,
				"skill_zip_sha":  verRecord.SkillZipSHA256,
				"apis_summary":   apis,
				"pages_summary":  pages,
			}, "", "  ")
			data = fallbackManifest
			contentType = "application/json; charset=utf-8"
		} else {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("artifact %s for %s v%d not found in MinIO storage (%v)", filename, slug, verNum, getErr),
			})
			return
		}
	}

	sum := sha256.Sum256(data)
	c.Header("X-Artifact-SHA256", hex.EncodeToString(sum[:]))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("%s-v%d-%s", slug, verNum, filename)))
	c.Data(http.StatusOK, contentType, data)
}

// RollbackAppVersion POST /api/v1/apps/:slug/rollback
// 原子回退应用至指定的历史版本：
// 1. 恢复目标版本的环境变量与端口配置；
// 2. 优先使用 CapRover 历史镜像秒级切换，若镜像已被清理则自动从 MinIO 拉取目标版本 app.tar 重新构建，并等待 /healthz 探针就绪；
// 3. 从 MinIO 读取目标版本 skill.zip 推送到 knowledge-opc 技能池覆盖恢复远端技能；
// 4. 更新数据库当前激活版本与日志回执，返回目标版本详情及 skill.zip 下载地址供客户端同步覆盖本地技能。
func (h *Handler) RollbackAppVersion(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))

	var req RollbackAppVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.TargetVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_version (positive integer) is required"})
		return
	}

	if !h.tryLockDeploySlug(slug) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("应用 '%s' 正在执行部署或回退中，请稍候再试", slug)})
		return
	}
	defer h.unlockDeploySlug(slug)

	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	targetVer, err := h.storage.GetAppVersion(ctx, slug, req.TargetVersion)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("version v%d not found for app %s", req.TargetVersion, slug)})
		return
	}
	if targetVer.Status == model.AppStatusFailed {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("版本 v%d 部署未成功 (status=%s)，不支持回退到失败版本", req.TargetVersion, targetVer.Status)})
		return
	}

	var targetConfigs []model.AppConfigItem
	_ = json.Unmarshal([]byte(targetVer.AppConfigJSON), &targetConfigs)
	var targetResIDs []string
	_ = json.Unmarshal([]byte(targetVer.ResourceIDsJSON), &targetResIDs)
	var targetMounts []model.AppStorageMount
	_ = json.Unmarshal([]byte(targetVer.StorageMountsJSON), &targetMounts)
	if len(targetMounts) == 0 && app.StorageMountsJSON != "" {
		_ = json.Unmarshal([]byte(app.StorageMountsJSON), &targetMounts)
	}
	targetMounts = deploy.NormalizeStorageMounts(slug, targetMounts)

	effectiveResources := h.loadEffectiveOnlineResources(ctx)
	mergedEnvList, mergeErr := mergeAppEnvironment(effectiveResources, targetResIDs, targetConfigs)
	if mergeErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("merge target version environment failed: %v", mergeErr)})
		return
	}
	mergedEnvList = appendStorageMountEnvVars(slug, mergedEnvList, targetMounts, effectiveResources)

	port := targetVer.ContainerPort
	if port <= 0 {
		port = app.ContainerPort
	}
	if port <= 0 {
		port = 8080
	}

	store := h.resolveArtifactStore(ctx)
	rollbackMode := "metadata_only"
	var rollbackLog strings.Builder
	if targetVer.AppTarSHA256 != "" || targetVer.SkillZipSHA256 != "" {
		rollbackLog.WriteString(fmt.Sprintf("[Artifact] app.tar:%s\n[Artifact] skill.zip:%s\n", targetVer.AppTarSHA256, targetVer.SkillZipSHA256))
	}
	rollbackLog.WriteString(fmt.Sprintf("[Rollback] Rolling back %s from v%d to v%d (reason: %s)...\n", slug, app.CurrentVersion, targetVer.Version, strings.TrimSpace(req.Reason)))

	if h.caproverClient != nil {
		rollbackCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		if err := h.caproverClient.EnsureAppRegistered(rollbackCtx, slug); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("EnsureAppRegistered during rollback failed: %v", err)})
			return
		}

		if err := h.caproverClient.UpdateAppConfigAndEnv(rollbackCtx, slug, port, mergedEnvList, targetMounts); err != nil {
			errLower := strings.ToLower(err.Error())
			if !strings.Contains(errLower, "1110") && !strings.Contains(errLower, "imagename for deployed version is not available") {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("UpdateAppConfigAndEnv during rollback failed: %v", err)})
				return
			}
		}

		// 优先尝试使用 CapRover 历史镜像秒级回退
		candidateImage := strings.TrimSpace(targetVer.DeployedImageName)
		if candidateImage == "" && targetVer.CaproverVersion > 0 {
			candidateImage = fmt.Sprintf("img-captain-%s:%d", slug, targetVer.CaproverVersion)
		}

		switched := false
		if candidateImage != "" {
			if imgErr := h.caproverClient.DeployExistingImage(rollbackCtx, slug, candidateImage); imgErr == nil {
				waitLog, waitErr := h.caproverClient.WaitBuildConfigureAndHealthz(rollbackCtx, slug, app.AppURL, func() error {
					return h.caproverClient.UpdateAppConfigAndEnv(rollbackCtx, slug, port, mergedEnvList, targetMounts)
				}, nil)
				if waitErr == nil {
					switched = true
					rollbackMode = "image_switch"
					rollbackLog.WriteString(fmt.Sprintf("[Rollback] Fast image switch to %s succeeded.\n%s\n", candidateImage, waitLog))
				} else {
					log.Printf("[Rollback][%s] Fast image switch health check failed (%v), falling back to MinIO app.tar rebuild", slug, waitErr)
				}
			} else {
				log.Printf("[Rollback][%s] DeployExistingImage(%s) failed (%v), falling back to MinIO app.tar rebuild", slug, candidateImage, imgErr)
			}
		}

		// 若历史镜像不存在或秒切失败，自动从 MinIO 拉取该版本的 app.tar 重新构建
		if !switched {
			tarKey := targetVer.AppTarObjectKey
			if tarKey == "" {
				tarKey = deploy.BuildVersionObjectKey(slug, targetVer.Version, "app.tar")
			}
			tarBytes, _, tarErr := store.GetObject(rollbackCtx, tarKey)
			if tarErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": fmt.Sprintf("rollback failed: Docker image %q unavailable and app.tar could not be fetched from MinIO (%v)", candidateImage, tarErr),
				})
				return
			}
			buildTarBytes := deploy.EnsureTarSeedProtection(slug, tarBytes, targetMounts)
			if upErr := h.caproverClient.UploadAppTar(rollbackCtx, slug, buildTarBytes); upErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("UploadAppTar from MinIO archive failed: %v", upErr)})
				return
			}
			waitLog, waitErr := h.caproverClient.WaitBuildConfigureAndHealthz(rollbackCtx, slug, app.AppURL, func() error {
				return h.caproverClient.UpdateAppConfigAndEnv(rollbackCtx, slug, port, mergedEnvList, targetMounts)
			}, nil)
			if waitErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("WaitBuildAndHealthz after MinIO app.tar rebuild failed: %v", waitErr)})
				return
			}
			rollbackMode = "minio_tar_rebuild"
			rollbackLog.WriteString(fmt.Sprintf("[Rollback] Rebuilt from MinIO object %s succeeded.\n%s\n", tarKey, waitLog))
		}
	}

	// 从 MinIO 读取目标版本的 skill.zip 并重新同步覆盖至 knowledge-opc 技能池
	catalogSkillID := targetVer.CatalogSkillID
	skillSynced := app.SkillSynced
	zipKey := targetVer.SkillZipObjectKey
	if zipKey == "" {
		zipKey = deploy.BuildVersionObjectKey(slug, targetVer.Version, "skill.zip")
	}
	if zipBytes, _, zipErr := store.GetObject(ctx, zipKey); zipErr == nil && len(zipBytes) > 0 {
		if h.cfg != nil && h.cfg.AppDeploy.AutoSyncSkill && h.knowledgeSyncer != nil {
			if catID, syncErr := h.knowledgeSyncer.PushAndEnableSkill(ctx, zipBytes); syncErr == nil {
				catalogSkillID = catID
				skillSynced = true
				rollbackLog.WriteString(fmt.Sprintf("[Rollback] Re-pushed v%d skill.zip to knowledge-opc (Catalog ID: %s)\n", targetVer.Version, catID))
			} else {
				rollbackLog.WriteString(fmt.Sprintf("[Rollback][Warning] Push v%d skill.zip to knowledge-opc failed: %v\n", targetVer.Version, syncErr))
			}
		}
	}

	if err := h.storage.ActivateAppVersion(ctx, targetVer, rollbackLog.String(), catalogSkillID, skillSynced); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("activate target version in db: %v", err)})
		return
	}

	updatedVer, _ := h.storage.GetAppVersion(ctx, slug, targetVer.Version)
	if updatedVer == nil {
		updatedVer = targetVer
	}
	onlineResources := h.loadEffectiveOnlineResources(ctx)

	c.JSON(http.StatusOK, gin.H{
		"ok":                 true,
		"slug":               slug,
		"previous_version":   app.CurrentVersion,
		"current_version":    targetVer.Version,
		"rollback_mode":      rollbackMode,
		"skill_download_url": fmt.Sprintf("/api/v1/apps/%s/versions/%d/artifacts/skill.zip", slug, targetVer.Version),
		"app_tar_url":        fmt.Sprintf("/api/v1/apps/%s/versions/%d/artifacts/app.tar", slug, targetVer.Version),
		"version":            h.toAppVersionDetailResponse(ctx, *updatedVer, onlineResources),
		"message":            fmt.Sprintf("已成功回退应用 %s 至 v%d", slug, targetVer.Version),
	})
}
