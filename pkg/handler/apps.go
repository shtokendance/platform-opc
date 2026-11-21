package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var slugRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{2,39}$`)

// CheckSlugRequest 校验标识请求体
type CheckSlugRequest struct {
	Slug string `json:"slug" binding:"required"`
}

// CheckSlugResponse 校验标识响应体
type CheckSlugResponse struct {
	Available bool   `json:"available"`
	Slug      string `json:"slug"`
	AppURL    string `json:"app_url"`
	SkillName string `json:"skill_name"`
	Message   string `json:"message,omitempty"`
}

// AppDetailResponse 单个应用详情响应
type AppDetailResponse struct {
	ConfigFingerprint string                  `json:"config_fingerprint,omitempty"`
	CanManage         bool                    `json:"can_manage"`
	ID                uuid.UUID               `json:"id"`
	Name              string                  `json:"name"`
	Slug              string                  `json:"slug"`
	Description       string                  `json:"description"`
	AppURL            string                  `json:"app_url"`
	ContainerPort     int                     `json:"container_port"`
	Language          string                  `json:"language"`
	RuntimeVersion    string                  `json:"runtime_version"`
	DockerfileContent string                  `json:"dockerfile_content"`
	CurrentVersion    int                     `json:"current_version"`
	Status            string                  `json:"status"`
	AppConfig         []model.AppConfigItem   `json:"app_config"`
	ResourceIDs       []string                `json:"resource_ids"`
	StorageMounts     []model.AppStorageMount `json:"storage_mounts"`
	SkillName         string                  `json:"skill_name"`
	SkillZipSHA256    string                  `json:"skill_zip_sha256,omitempty"`
	CatalogSkillID    string                  `json:"catalog_skill_id"`
	SkillSynced       bool                    `json:"skill_synced"`
	BuildLog          string                  `json:"build_log"`
	ErrorMessage      string                  `json:"error_message"`
	CreatedBy         string                  `json:"created_by"`
	CreatedAt         time.Time               `json:"created_at"`
	UpdatedAt         time.Time               `json:"updated_at"`
}

// UpdateAppConfigRequest 修改配置请求体
type UpdateAppConfigRequest struct {
	ContainerPort int                     `json:"container_port"`
	AppConfig     []model.AppConfigItem   `json:"app_config"`
	ResourceIDs   []string                `json:"resource_ids"`
	StorageMounts []model.AppStorageMount `json:"storage_mounts"`
}

// loadEffectiveDeployResources 优先从 deploy-opc 数据库读取启用的基础资源，若为空则回退至配置文件并自动入库
func (h *Handler) loadEffectiveDeployResources(ctx context.Context) []model.DeployResource {
	if h.storage != nil {
		h.ensureLocalDirectoryVolumesFromDeploy(ctx)
		list, err := h.storage.ListDeployResources(ctx, true)
		if err == nil && len(list) > 0 {
			return list
		}
		if h.cfg != nil && len(h.cfg.AppDeploy.OnlineResources) > 0 {
			_ = h.storage.BootstrapDeployDB(ctx, h.cfg.AppDeploy.OnlineResources, h.cfg.DeployDB)
			if seeded, err2 := h.storage.ListDeployResources(ctx, true); err2 == nil && len(seeded) > 0 {
				return seeded
			}
		}
	}
	if h.cfg == nil {
		return nil
	}
	out := make([]model.DeployResource, 0, len(h.cfg.AppDeploy.OnlineResources))
	hasDefaultByType := make(map[string]bool)
	for _, r := range h.cfg.AppDeploy.OnlineResources {
		if r.IsDefault && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.ID)), "db-") {
			hasDefaultByType[strings.ToLower(strings.TrimSpace(r.Type))] = true
		}
	}
	for _, r := range h.cfg.AppDeploy.OnlineResources {
		tKey := strings.ToLower(strings.TrimSpace(r.Type))
		isDef := r.IsDefault
		if !isDef && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.ID)), "db-") && !hasDefaultByType[tKey] {
			isDef = true
			hasDefaultByType[tKey] = true
		}
		out = append(out, model.DeployResource{
			ID:          r.ID,
			Name:        r.Name,
			Type:        r.Type,
			IsDefault:   isDef,
			Description: r.Description,
			EnvVars:     r.EnvVars,
			Enabled:     true,
		})
	}
	return out
}

func (h *Handler) loadEffectiveOnlineResources(ctx context.Context) []config.OnlineResourceConfig {
	dbList := h.loadEffectiveDeployResources(ctx)
	out := make([]config.OnlineResourceConfig, 0, len(dbList))
	for _, r := range dbList {
		out = append(out, config.OnlineResourceConfig{
			ID:          r.ID,
			Name:        r.Name,
			Type:        r.Type,
			IsDefault:   r.IsDefault,
			Description: r.Description,
			EnvVars:     r.EnvVars,
		})
	}
	return out
}

// CheckSlug 校验 slug 唯一性并分配内网访问地址
func (h *Handler) CheckSlug(c *gin.Context) {
	var req CheckSlugRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request parameters"})
		return
	}

	slug := strings.TrimSpace(strings.ToLower(req.Slug))
	if !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, CheckSlugResponse{
			Available: false,
			Slug:      slug,
			Message:   "slug 必须以小写字母开头，只包含小写字母、数字和连字符，长度在 3 到 40 个字符之间",
		})
		return
	}

	available, err := h.storage.CheckSlugAvailable(c.Request.Context(), slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("check slug: %v", err)})
		return
	}

	intranetDomain := h.cfg.AppDeploy.IntranetRootDomain
	if intranetDomain == "" {
		intranetDomain = "apps.opc.intra"
	}

	appURL := fmt.Sprintf("http://%s.%s", slug, intranetDomain)
	skillName := fmt.Sprintf("app-%s", slug)

	c.JSON(http.StatusOK, CheckSlugResponse{
		Available: available,
		Slug:      slug,
		AppURL:    appURL,
		SkillName: skillName,
	})
}

func (h *Handler) isEnterpriseResourceAllowed(slug, resID string) bool {
	idLower := strings.ToLower(strings.TrimSpace(resID))
	if strings.HasPrefix(idLower, "appdb-") {
		return isDedicatedDeliveryResource(slug, idLower)
	}
	if strings.HasPrefix(idLower, "db-") && slug != "" && idLower == "db-"+slug {
		return true
	}
	if h.cfg != nil && len(h.cfg.AppDeploy.EnterpriseDeployResources) > 0 {
		return slices.Contains(h.cfg.AppDeploy.EnterpriseDeployResources, resID)
	}
	return idLower != "minio-knowledge"
}

// ListResources 返回脱敏后的线上现有可用资源列表（从 deploy-opc 数据库动态读取）
func (h *Handler) ListResources(c *gin.Context) {
	slug := strings.TrimSpace(strings.ToLower(c.Query("slug")))
	if slug != "" && c.GetString(appOwnerKey) != "" {
		app, err := h.storage.GetAppBySlug(c.Request.Context(), slug)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "application resource scope could not be read"})
			return
		}
		if app != nil && app.CreatedBy != "" && app.CreatedBy != c.GetString(appOwnerKey) {
			slug = ""
		}
	}
	deployResources := h.loadEffectiveDeployResources(c.Request.Context())
	resources := make([]model.ResourceSummary, 0, len(deployResources))
	for _, res := range deployResources {
		if c.GetString(appOwnerKey) != "" && !h.isEnterpriseResourceAllowed(slug, res.ID) {
			continue
		}
		idLower := strings.ToLower(strings.TrimSpace(res.ID))
		// 过滤其他应用迁移生成的专属数据库记录 (db-<slug>)，防止干扰新应用分析
		if strings.HasPrefix(idLower, "db-") && (slug == "" || idLower != "db-"+slug) {
			continue
		}
		if res.ID == "shared-oss-minio" {
			continue
		}
		if res.Type == objectStorageResourceType {
			resources = append(resources, model.ResourceSummary{ID: res.ID, Name: res.Name, Type: res.Type, IsDefault: res.IsDefault, Description: res.Description, EnvKeys: objectStorageConfigKeys})
			continue
		}
		if isVolumeResource(res.Type) {
			resources = append(resources, model.ResourceSummary{ID: res.ID, Name: res.Name, Type: res.Type, IsDefault: res.IsDefault, Description: res.Description})
			continue
		}
		keys := make([]string, 0, len(res.EnvVars))
		for k := range res.EnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		tLower := strings.ToLower(res.Type)
		supportsAutoMigrate := tLower == "postgres" || tLower == "postgresql" || tLower == "mysql" || tLower == "sqlserver" || tLower == "redis"

		resources = append(resources, model.ResourceSummary{
			ID:                    res.ID,
			Name:                  res.Name,
			Type:                  res.Type,
			IsDefault:             res.IsDefault,
			Host:                  res.Host,
			Port:                  res.Port,
			DatabaseName:          res.DatabaseName,
			Description:           res.Description,
			EnvKeys:               keys,
			EnvPreview:            resourcePreview(res.EnvVars),
			SupportsAutoMigration: supportsAutoMigrate,
		})
	}

	c.JSON(http.StatusOK, gin.H{"resources": resources, "delivery_format": 1})
}

// GetDockerfileTemplates GET /api/v1/apps/dockerfile-templates?language=&version=
// 为 AI 改造第二步提供同语言+同版本的 Dockerfile 模板与基础镜像
func (h *Handler) GetDockerfileTemplates(c *gin.Context) {
	ctx := c.Request.Context()
	if h.storage != nil && h.cfg != nil {
		_ = h.storage.BootstrapDeployDB(ctx, h.cfg.AppDeploy.OnlineResources, h.cfg.DeployDB)
	}
	lang := strings.TrimSpace(c.Query("language"))
	ver := strings.TrimSpace(c.Query("version"))

	var matched *model.DockerfileTemplate
	if lang != "" && h.storage != nil {
		if tpl, err := h.storage.MatchDockerfileTemplate(ctx, lang, ver); err == nil {
			matched = tpl
		}
	}

	var list []model.DockerfileTemplate
	if h.storage != nil {
		list, _ = h.storage.ListDockerfileTemplates(ctx, lang, "")
		if len(list) == 0 && lang != "" {
			list, _ = h.storage.ListDockerfileTemplates(ctx, "", "")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"matched_template": matched,
		"templates":        list,
	})
}

// PublishApp 接收应用包与配置并触发异步部署
func (h *Handler) PublishApp(c *gin.Context) {
	h.storageResourcesMu.RLock()
	defer h.storageResourcesMu.RUnlock()
	ctx := c.Request.Context()
	name := strings.TrimSpace(c.PostForm("name"))
	slug := strings.TrimSpace(strings.ToLower(c.PostForm("slug")))
	description := strings.TrimSpace(c.PostForm("description"))
	portStr := strings.TrimSpace(c.PostForm("container_port"))
	appConfigStr := strings.TrimSpace(c.PostForm("app_config"))
	resourceIDsStr := strings.TrimSpace(c.PostForm("resource_ids"))
	language := strings.TrimSpace(c.PostForm("language"))
	runtimeVersion := strings.TrimSpace(c.PostForm("runtime_version"))
	dockerfileContent := strings.TrimSpace(c.PostForm("dockerfile_content"))
	versionNote := strings.TrimSpace(c.PostForm("version_note"))
	requestID := strings.TrimSpace(c.PostForm("request_id"))
	if len(requestID) > 128 {
		c.JSON(400, gin.H{"error": "invalid request_id"})
		return
	}

	if name == "" || slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and slug are required"})
		return
	}
	if !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid slug format"})
		return
	}

	if !h.tryLockDeploySlug(slug) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("应用 '%s' 正在执行部署或回退中，请稍候再试", slug)})
		return
	}
	if !h.checkAppOwner(c, slug, true) {
		h.unlockDeploySlug(slug)
		return
	}

	attemptID := uuid.New()
	if requestID == "" {
		requestID = attemptID.String()
	} else {
		var prior model.AppDeploymentVersion
		if h.storage.DeployDB().WithContext(ctx).Where("app_slug = ? AND request_id = ?", slug, requestID).First(&prior).Error == nil {
			h.unlockDeploySlug(slug)
			domain := h.cfg.AppDeploy.IntranetRootDomain
			if domain == "" {
				domain = "apps.opc.intra"
			}
			receipt := appPublishReceipt(prior)
			receipt["slug"] = slug
			receipt["app_url"] = fmt.Sprintf("http://%s.%s", slug, domain)
			c.JSON(http.StatusOK, receipt)
			return
		}
	}

	port := 8080
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p <= 65535 {
			port = p
		} else {
			h.unlockDeploySlug(slug)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid container_port"})
			return
		}
	}

	if appConfigStr == "" {
		appConfigStr = "[]"
	}
	if resourceIDsStr == "" {
		resourceIDsStr = "[]"
	}

	var parsedAppConfig []model.AppConfigItem
	if err := json.Unmarshal([]byte(appConfigStr), &parsedAppConfig); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid app_config json"})
		return
	}

	var parsedResourceIDs []string
	if err := json.Unmarshal([]byte(resourceIDsStr), &parsedResourceIDs); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid resource_ids json"})
		return
	}

	if c.GetString(appOwnerKey) != "" {
		for _, id := range parsedResourceIDs {
			if !h.isEnterpriseResourceAllowed(slug, id) || (id == "shared-oss-minio" && !h.appUsesResource(ctx, slug, id)) {
				h.unlockDeploySlug(slug)
				writeErrorEnvelope(c, 403, "DEPLOY_RESOURCE_FORBIDDEN", "所选资源未对企业应用开放")
				return
			}
		}
	}
	effectiveResources := h.loadEffectiveOnlineResources(c.Request.Context())
	if existingApp, appErr := h.storage.GetAppBySlug(ctx, slug); appErr == nil && existingApp != nil {
		parsedAppConfig = preservePreviousSecrets(existingApp.AppConfigJSON, parsedAppConfig)
	}
	if err := h.checkObjectStorageChange(ctx, slug, parsedResourceIDs); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateObjectResourceBinding(effectiveResources, parsedResourceIDs, parsedAppConfig); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, err := mergeAppEnvironment(effectiveResources, parsedResourceIDs, parsedAppConfig); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 读取上传的 app.tar
	appTarFile, err := c.FormFile("app_tar")
	if err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": "app_tar file is required"})
		return
	}
	tarReader, err := appTarFile.Open()
	if err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open app_tar file failed"})
		return
	}
	defer tarReader.Close()
	tarBytes, err := io.ReadAll(tarReader)
	if err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read app_tar file failed"})
		return
	}

	// 优先从 app.tar 提取真实构建的 Dockerfile，确保种子保护校验与实际构建内容一致
	if tarDf := extractDockerfileFromTar(tarBytes); tarDf != "" {
		dockerfileContent = tarDf
	}
	if dockerfileContent != "" && (language == "" || runtimeVersion == "") {
		infLang, infVer, _ := storage.InferLanguageAndVersionFromDockerfile(dockerfileContent)
		if language == "" {
			language = infLang
		}
		if runtimeVersion == "" {
			runtimeVersion = infVer
		}
	}
	language = storage.NormalizeLanguage(language)

	// 读取上传的 skill.zip
	skillZipFile, err := c.FormFile("skill_zip")
	if err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill_zip file is required"})
		return
	}
	zipReader, err := skillZipFile.Open()
	if err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open skill_zip file failed"})
		return
	}
	defer zipReader.Close()
	zipBytes, err := io.ReadAll(zipReader)
	if err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read skill_zip file failed"})
		return
	}
	clientSkillSHA := strings.ToLower(strings.TrimSpace(c.PostForm("skill_zip_sha256")))
	actualSkillSHA := fmt.Sprintf("%x", sha256.Sum256(zipBytes))
	if clientSkillSHA != "" && clientSkillSHA != actualSkillSHA {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("skill_zip_sha256 mismatch: expected %s, got %s", clientSkillSHA, actualSkillSHA)})
		return
	}

	deliveryPlan := []byte(c.PostForm("delivery_plan"))
	deliveryRevisionID := uuid.Nil
	if len(deliveryPlan) > 0 {
		owner := c.GetString(appOwnerKey)
		if owner != "" {
			var err error
			deliveryRevisionID, err = uuid.Parse(c.PostForm("delivery_revision"))
			if err != nil || deliveryRevisionID == uuid.Nil {
				h.unlockDeploySlug(slug)
				c.JSON(http.StatusBadRequest, gin.H{"error": "an archived delivery revision is required"})
				return
			}
			db, err := h.deliveryRevisions(ctx)
			if err != nil {
				h.unlockDeploySlug(slug)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery revision storage unavailable"})
				return
			}
			var saved appDeliveryRevision
			if err := db.Where("owner = ? AND slug = ? AND id = ?", owner, slug, deliveryRevisionID).First(&saved).Error; err != nil ||
				saved.AppSHA != fmt.Sprintf("%x", sha256.Sum256(tarBytes)) || saved.SkillSHA != actualSkillSHA ||
				saved.PlanSHA != fmt.Sprintf("%x", sha256.Sum256(deliveryPlan)) {
				h.unlockDeploySlug(slug)
				c.JSON(http.StatusConflict, gin.H{"error": "deployment artifacts do not match the archived delivery revision"})
				return
			}
			dataPlan, err := readDeliveryDataPlan(deliveryPlan)
			if err == nil && dataPlan.Strategy == "postgres_copy" && c.PostForm("migration_approved") != "true" {
				err = fmt.Errorf("the PostgreSQL migration and write-hold window require explicit approval")
			}
			if err == nil {
				err = h.validateDataTargets(ctx, slug, dataPlan, parsedResourceIDs)
			}
			if err != nil {
				h.unlockDeploySlug(slug)
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		var currentVersion int
		if app, err := h.storage.GetAppBySlug(ctx, slug); err == nil {
			currentVersion = app.CurrentVersion
			if active, err := h.storage.GetActiveAppVersion(ctx, slug); err == nil {
				currentVersion = active.Version
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				h.unlockDeploySlug(slug)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "active application version cannot be read"})
				return
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			h.unlockDeploySlug(slug)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "existing application state cannot be read"})
			return
		}
		if err := validateDeliveryPlan(deliveryPlan, slug, port, currentVersion, tarBytes); err != nil {
			h.unlockDeploySlug(slug)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if deliveryRevisionID == uuid.Nil && !h.allowChangeAfterMigration(c, slug) {
		h.unlockDeploySlug(slug)
		return
	}

	intranetDomain := h.cfg.AppDeploy.IntranetRootDomain
	if intranetDomain == "" {
		intranetDomain = "apps.opc.intra"
	}
	appURL := fmt.Sprintf("http://%s.%s", slug, intranetDomain)
	skillName := fmt.Sprintf("app-%s", slug)

	nextVersion, verErr := h.storage.NextAppVersionNumber(ctx, slug)
	if verErr != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "next application version could not be read"})
		return
	}

	if !h.allowAppChange(c, slug) {
		h.unlockDeploySlug(slug)
		return
	}

	// 从 skill.zip 中提取 API 清单、页面清单与持久化存储挂载目录
	apisSummary, pagesSummary := extractSkillContractSummaries(zipBytes)
	apisSummaryJSON, _ := json.Marshal(apisSummary)
	pagesSummaryJSON, _ := json.Marshal(pagesSummary)

	var storageMounts []model.AppStorageMount
	rawMounts := strings.TrimSpace(c.PostForm("storage_mounts"))
	explicitStorageMounts := rawMounts != ""
	if len(deliveryPlan) > 0 && !strings.HasPrefix(rawMounts, "[") {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": "delivery_plan requires explicit storage_mounts, use [] for no mounts"})
		return
	}
	if explicitStorageMounts {
		if err := json.Unmarshal([]byte(rawMounts), &storageMounts); err != nil {
			h.unlockDeploySlug(slug)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid storage_mounts json"})
			return
		}
	}
	if len(deliveryPlan) == 0 && len(storageMounts) == 0 {
		storageMounts = extractSkillStorageMounts(zipBytes)
	}
	if c.GetString(appOwnerKey) != "" {
		for _, mount := range storageMounts {
			if mount.HostPath != "" {
				h.unlockDeploySlug(slug)
				writeErrorEnvelope(c, 400, "HOST_MOUNT_FORBIDDEN", "enterprise applications cannot choose host paths")
				return
			}
		}
	}
	if len(deliveryPlan) == 0 && len(storageMounts) == 0 {
		storageMounts = deploy.InferStorageMountsFromTar(slug, dockerfileContent, tarBytes)
	}
	mountCount := len(storageMounts)
	storageMounts = deploy.NormalizeStorageMounts(slug, storageMounts)
	if len(deliveryPlan) > 0 && len(storageMounts) != mountCount {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": "storage_mounts contains an invalid or duplicate target"})
		return
	}
	var previousMounts []model.AppStorageMount
	var prevCatalogSkillID string
	var prevSkillSynced bool
	appID := uuid.New()
	if previous, err := h.storage.GetAppBySlug(ctx, slug); err == nil {
		appID = previous.ID
		prevCatalogSkillID = previous.CatalogSkillID
		prevSkillSynced = previous.SkillSynced
		if previous.StorageMountsJSON != "" {
			if err := json.Unmarshal([]byte(previous.StorageMountsJSON), &previousMounts); err != nil {
				h.unlockDeploySlug(slug)
				c.JSON(http.StatusConflict, gin.H{"error": "existing storage bindings cannot be read"})
				return
			}
		}
		if !explicitStorageMounts && len(previousMounts) > 0 {
			seenPaths := make(map[string]bool, len(storageMounts))
			for _, m := range storageMounts {
				seenPaths[m.ContainerPath] = true
			}
			for _, old := range previousMounts {
				if !seenPaths[old.ContainerPath] {
					storageMounts = append(storageMounts, old)
					seenPaths[old.ContainerPath] = true
				}
			}
			storageMounts = deploy.NormalizeStorageMounts(slug, storageMounts)
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "existing application state cannot be read"})
		return
	}
	if err := h.resolveVolumeResources(ctx, slug, appID, c.GetString(appOwnerKey) != "", storageMounts, previousMounts, effectiveResources); err != nil {
		h.unlockDeploySlug(slug)
		writeErrorEnvelope(c, http.StatusBadRequest, "STORAGE_RESOURCE_UNAVAILABLE", err.Error())
		return
	}
	storageMounts, mountErr := deploy.ReconcileStorageMounts(slug, previousMounts, storageMounts)
	if mountErr != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusBadRequest, gin.H{"error": mountErr.Error()})
		return
	}
	if len(deliveryPlan) == 0 {
		if err := deploy.ValidateSeedProtection(dockerfileContent, storageMounts); err != nil {
			h.unlockDeploySlug(slug)
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
	}
	if h.caproverClient != nil && deliveryRevisionID == uuid.Nil {
		if err := h.caproverClient.CreateVolumePaths(ctx, storageMounts, previousMounts); err != nil {
			h.unlockDeploySlug(slug)
			writeErrorEnvelope(c, http.StatusBadRequest, "STORAGE_RESOURCE_UNAVAILABLE", err.Error())
			return
		}
	}
	storageMountsJSONBytes, _ := json.Marshal(storageMounts)
	storageMountsJSON := string(storageMountsJSONBytes)

	deployedApp := &model.DeployedApp{
		ID:                appID,
		CreatedBy:         c.GetString(appOwnerKey),
		Name:              name,
		Slug:              slug,
		Description:       description,
		AppURL:            appURL,
		ContainerPort:     port,
		Language:          language,
		RuntimeVersion:    runtimeVersion,
		DockerfileContent: dockerfileContent,
		Status:            model.AppStatusBuilding,
		AppConfigJSON:     appConfigStr,
		ResourceIDsJSON:   resourceIDsStr,
		StorageMountsJSON: storageMountsJSON,
		SkillName:         skillName,
		CatalogSkillID:    prevCatalogSkillID,
		SkillSynced:       prevSkillSynced,
		BuildLog:          "Deployment job received. Initializing build...",
	}

	// 记录本次发布的版本快照到 app_deployment_versions 并与主表 deployed_apps 原子落库
	verRecord := &model.AppDeploymentVersion{
		ID:                  attemptID,
		RequestID:           requestID,
		DeliveryRevision:    deliveryRevisionID,
		PreparedName:        name,
		PreparedDescription: description,
		CreatedBy:           c.GetString(appOwnerKey),
		AppSlug:             slug,
		ProposedVersion:     nextVersion,
		VersionNote:         versionNote,
		Status:              model.AppStatusBuilding,
		IsActive:            false,
		ContainerPort:       port,
		Language:            language,
		RuntimeVersion:      runtimeVersion,
		DockerfileContent:   dockerfileContent,
		AppConfigJSON:       appConfigStr,
		ResourceIDsJSON:     resourceIDsStr,
		StorageMountsJSON:   storageMountsJSON,
		AppTarSize:          int64(len(tarBytes)),
		SkillZipSize:        int64(len(zipBytes)),
		APIsSummaryJSON:     string(apisSummaryJSON),
		PagesSummaryJSON:    string(pagesSummaryJSON),
		BuildLog:            "Deployment job received. Initializing build...",
	}
	attemptPrefix := fmt.Sprintf("apps/%s/attempts/%s", slug, attemptID)
	if err := archivePublishArtifacts(ctx, h.resolveArtifactStore(ctx), verRecord, attemptPrefix, tarBytes, zipBytes, deliveryPlan); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment artifacts could not be archived"})
		return
	}
	if err := h.storage.SavePublishedAppAndVersion(ctx, deployedApp, verRecord); err != nil {
		h.unlockDeploySlug(slug)
		c.JSON(503, gin.H{"error": fmt.Sprintf("version record could not be saved: %v", err)})
		return
	}

	// 启动部署流程（当未配置 CapRover 客户端时同步标记状态，避免测试或离线模式下产生异步竞态）
	if h.caproverClient == nil {
		h.runDeploymentAsync(verRecord, port, appURL, parsedAppConfig, parsedResourceIDs, storageMounts, tarBytes, zipBytes, deliveryPlan)
	} else {
		go h.runDeploymentAsync(verRecord, port, appURL, parsedAppConfig, parsedResourceIDs, storageMounts, tarBytes, zipBytes, deliveryPlan)
	}

	c.JSON(http.StatusAccepted, gin.H{
		"slug":                 slug,
		"version":              nextVersion,
		"published_version":    0,
		"attempt_id":           attemptID,
		"request_id":           requestID,
		"status":               model.AppStatusBuilding,
		"app_url":              appURL,
		"skill_name":           skillName,
		"app_tar_sha256":       verRecord.AppTarSHA256,
		"skill_zip_sha256":     verRecord.SkillZipSHA256,
		"delivery_plan_sha256": verRecord.DeliveryPlanSHA256,
		"language":             language,
		"runtime_version":      runtimeVersion,
		"storage_mounts":       storageMounts,
	})
}

// runDeploymentAsync 在后台执行部署至 deploy-opc 并同步技能至 knowledge-opc
func (h *Handler) runDeploymentAsync(attempt *model.AppDeploymentVersion, port int, appURL string, appConfigs []model.AppConfigItem, resourceIDs []string, storageMounts []model.AppStorageMount, tarBytes []byte, zipBytes []byte, deliveryPlan ...[]byte) {
	slug := attempt.AppSlug
	defer h.unlockDeploySlug(slug)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if attempt.DeliveryRevision != uuid.Nil {
		h.buildArchivedDelivery(ctx, attempt, tarBytes)
		return
	}
	buildTarBytes := buildDeploymentArchive(attempt, tarBytes, storageMounts)
	artifactReceipt := fmt.Sprintf("[Artifact] app.tar:%x\n[Artifact] skill.zip:%x\n", sha256.Sum256(tarBytes), sha256.Sum256(zipBytes))
	if attempt.DeliveryPlanSHA256 != "" {
		artifactReceipt += fmt.Sprintf("[Artifact] delivery.json:%s\n", attempt.DeliveryPlanSHA256)
	}
	if !bytes.Equal(buildTarBytes, tarBytes) {
		artifactReceipt += fmt.Sprintf("[Artifact-Build] build_app.tar:%x (server_seed_protection=rewritten)\n", sha256.Sum256(buildTarBytes))
	}

	handleDeployFailure := func(finalLog, errMsg string) {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer recoveryCancel()
		activeVer, _ := h.storage.GetActiveAppVersion(recoveryCtx, slug)
		isUpgrade := activeVer != nil && activeVer.ID != attempt.ID
		if err := h.storage.FailAppDeployment(recoveryCtx, attempt.ID, finalLog, errMsg); err != nil {
			log.Printf("[DeployJob][%s][%s] Failed to record deployment failure: %v", slug, attempt.ID, err)
		}
		if isUpgrade && h.caproverClient != nil {
			var oldConfig []model.AppConfigItem
			var oldResources []string
			var oldMounts []model.AppStorageMount
			_ = json.Unmarshal([]byte(activeVer.AppConfigJSON), &oldConfig)
			_ = json.Unmarshal([]byte(activeVer.ResourceIDsJSON), &oldResources)
			_ = json.Unmarshal([]byte(activeVer.StorageMountsJSON), &oldMounts)
			resources := h.loadEffectiveOnlineResources(recoveryCtx)
			env, restoreErr := h.applicationEnvironment(recoveryCtx, slug, resources, oldResources, oldConfig, activeVer.DeliveryPlanSHA256 != "")
			if activeVer.DeliveryPlanSHA256 == "" {
				env = appendStorageMountEnvVars(slug, env, oldMounts, resources)
			}
			if restoreErr == nil && activeVer.DeployedImageName == "" && activeVer.AppTarObjectKey == "" {
				restoreErr = fmt.Errorf("previous image identity unavailable")
			}
			recovered := false
			if restoreErr == nil && activeVer.DeployedImageName != "" {
				preStaged := false
				if err := h.caproverClient.PrestageAppConfigAndEnv(recoveryCtx, slug, activeVer.ContainerPort, env, oldMounts); err == nil {
					preStaged = true
				} else {
					errLower := strings.ToLower(err.Error())
					if !strings.Contains(errLower, "1110") && !strings.Contains(errLower, "imagename for deployed version is not available") {
						restoreErr = err
					}
				}
				if restoreErr == nil {
					if err := h.caproverClient.DeployExistingImage(recoveryCtx, slug, activeVer.DeployedImageName); err != nil {
						restoreErr = err
					} else {
						var configureFn func() error
						if !preStaged {
							configureFn = func() error {
								return h.caproverClient.UpdateAppConfigAndEnv(recoveryCtx, slug, activeVer.ContainerPort, env, oldMounts)
							}
						}
						if _, err := h.caproverClient.WaitBuildConfigureAndAppReady(recoveryCtx, slug, appURL, configureFn, nil); err != nil {
							restoreErr = err
						} else if err := h.verifyAllocatedVolumes(recoveryCtx, slug, oldMounts); err != nil {
							restoreErr = err
						} else {
							recovered = true
						}
					}
				}
			}
			if !recovered && activeVer.AppTarObjectKey != "" {
				store := h.resolveArtifactStore(recoveryCtx)
				oldTar, _, tarErr := store.GetObject(recoveryCtx, activeVer.AppTarObjectKey)
				if tarErr == nil && len(oldTar) > 0 {
					restoreErr = nil
					protectedTar := buildDeploymentArchive(activeVer, oldTar, oldMounts)
					preStaged := false
					if err := h.caproverClient.PrestageAppConfigAndEnv(recoveryCtx, slug, activeVer.ContainerPort, env, oldMounts); err == nil {
						preStaged = true
					} else {
						errLower := strings.ToLower(err.Error())
						if !strings.Contains(errLower, "1110") && !strings.Contains(errLower, "imagename for deployed version is not available") {
							restoreErr = err
						}
					}
					if restoreErr == nil {
						if err := h.caproverClient.UploadAppTar(recoveryCtx, slug, protectedTar); err != nil {
							restoreErr = err
						} else {
							var configureFn func() error
							if !preStaged {
								configureFn = func() error {
									return h.caproverClient.UpdateAppConfigAndEnv(recoveryCtx, slug, activeVer.ContainerPort, env, oldMounts)
								}
							}
							if _, err := h.caproverClient.WaitBuildConfigureAndAppReady(recoveryCtx, slug, appURL, configureFn, nil); err != nil {
								restoreErr = err
							} else if err := h.verifyAllocatedVolumes(recoveryCtx, slug, oldMounts); err != nil {
								restoreErr = err
							} else {
								recovered = true
							}
						}
					}
				} else if restoreErr == nil && tarErr != nil {
					restoreErr = tarErr
				}
			}
			if recovered && restoreErr == nil {
				receipt := fmt.Sprintf("[Artifact] app.tar:%s\n[Artifact] skill.zip:%s\n", activeVer.AppTarSHA256, activeVer.SkillZipSHA256)
				restoreErr = h.storage.ActivateAppVersion(recoveryCtx, activeVer, receipt+"Previous version restored and health checked", activeVer.CatalogSkillID, false)
				if restoreErr == nil {
					return
				}
			}
			errMsg += "; previous version recovery failed: " + restoreErr.Error()
		}
		_ = h.storage.UpdateAppStatus(recoveryCtx, slug, model.AppStatusFailed, finalLog, errMsg, "", false)
	}

	log.Printf("[DeployJob][%s][%s] Starting background deployment (storage_mounts=%d)...", slug, attempt.ID, len(storageMounts))

	effectiveResources := h.loadEffectiveOnlineResources(ctx)
	mergedEnvList, err := h.applicationEnvironment(ctx, slug, effectiveResources, resourceIDs, appConfigs, attempt.DeliveryPlanSHA256 != "")
	if err != nil {
		handleDeployFailure(err.Error(), err.Error())
		return
	}
	if attempt.DeliveryPlanSHA256 == "" {
		mergedEnvList = appendStorageMountEnvVars(slug, mergedEnvList, storageMounts, effectiveResources)
	}

	if h.caproverClient == nil {
		log.Printf("[DeployJob][%s] CapRover client not initialized. Marked as failed.", slug)
		handleDeployFailure("CapRover client not configured", "CapRover client not configured")
		return
	}

	// 2. 注册应用
	if err := h.caproverClient.EnsureAppRegistered(ctx, slug); err != nil {
		errMsg := fmt.Sprintf("EnsureAppRegistered failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		handleDeployFailure(errMsg, errMsg)
		return
	}

	// 3. 首次部署时直接注入配置与挂载点；若已有活跃旧版本运行中，则使用 Prestage 仅落库预置配置与挂载（不提前重启旧容器），构建完成后由 Deploy-OPC 一次原子切换
	activeVerPreBuild, _ := h.storage.GetActiveAppVersion(ctx, slug)
	hasRunningVersion := activeVerPreBuild != nil && activeVerPreBuild.ID != attempt.ID
	preStaged := false
	if hasRunningVersion {
		if err := h.caproverClient.CreateVolumePaths(ctx, storageMounts, nil); err != nil {
			errMsg := fmt.Sprintf("CreateVolumePaths failed: %v", err)
			log.Printf("[DeployJob][%s] %s", slug, errMsg)
			handleDeployFailure(errMsg, errMsg)
			return
		}
		if err := h.caproverClient.PrestageAppConfigAndEnv(ctx, slug, port, mergedEnvList, storageMounts); err != nil {
			errLower := strings.ToLower(err.Error())
			if strings.Contains(errLower, "1110") || strings.Contains(errLower, "imagename for deployed version is not available") {
				log.Printf("[DeployJob][%s] PrestageAppConfigAndEnv returned 1110, will apply after build: %v", slug, err)
			} else {
				errMsg := fmt.Sprintf("PrestageAppConfigAndEnv failed: %v", err)
				log.Printf("[DeployJob][%s] %s", slug, errMsg)
				handleDeployFailure(errMsg, errMsg)
				return
			}
		} else {
			preStaged = true
		}
	} else if err := h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList, storageMounts); err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "1110") || strings.Contains(errLower, "imagename for deployed version is not available") {
			log.Printf("[DeployJob][%s] Pre-build UpdateAppConfigAndEnv returned 1110 (initial image not ready yet), will apply after build: %v", slug, err)
		} else {
			errMsg := fmt.Sprintf("UpdateAppConfigAndEnv failed: %v", err)
			log.Printf("[DeployJob][%s] %s", slug, errMsg)
			handleDeployFailure(errMsg, errMsg)
			return
		}
	} else {
		preStaged = true
	}

	// 4. 上传 app.tar（对包含持久化卷的应用确保具备 .opc-seed 种子无损保护）
	if err := h.caproverClient.UploadAppTar(ctx, slug, buildTarBytes); err != nil {
		errMsg := fmt.Sprintf("UploadAppTar failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		handleDeployFailure(errMsg, errMsg)
		return
	}

	// 5. 等待构建完成、目标容器启动及应用入口就绪 (实时回调更新数据库日志)
	var postBuildConfigure func() error
	if !preStaged {
		postBuildConfigure = func() error {
			return h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList, storageMounts)
		}
	}
	buildLog, err := h.caproverClient.WaitBuildConfigureAndAppReady(ctx, slug, appURL, postBuildConfigure, func(liveLog string) {
		_ = h.storage.UpdateAppLog(ctx, slug, artifactReceipt+redactAppRuntimeLogs(liveLog, mergedEnvList))
		_ = h.storage.UpdateAppVersionLog(ctx, attempt.ID, artifactReceipt+redactAppRuntimeLogs(liveLog, mergedEnvList))
	})
	if err != nil {
		errMsg := fmt.Sprintf("WaitBuildAndAppReady failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		logCtx, logCancel := context.WithTimeout(ctx, 10*time.Second)
		runtimeLog, logErr := h.caproverClient.RuntimeLogs(logCtx, slug)
		logCancel()
		if logErr == nil && strings.TrimSpace(runtimeLog) != "" {
			buildLog += "\n[Runtime logs]\n" + redactAppRuntimeLogs(runtimeLog, mergedEnvList)
		}
		finalLog := artifactReceipt + redactAppRuntimeLogs(buildLog+"\n"+errMsg, mergedEnvList)
		handleDeployFailure(finalLog, errMsg)
		return
	}

	if err := h.verifyAllocatedVolumes(ctx, slug, storageMounts); err != nil {
		handleDeployFailure(artifactReceipt+redactAppRuntimeLogs(buildLog, mergedEnvList), err.Error())
		return
	}

	// 5b. 获取 CapRover 构建产出的镜像名称与内部版本号（供未来秒级回退使用）
	capVer, deployedImageName, _ := h.caproverClient.GetAppImageInfo(ctx, slug)

	// 6. 同步技能至 knowledge-opc 技能池
	catalogSkillID := ""
	skillSynced := false
	saved, ownerErr := h.storage.GetAppBySlug(ctx, slug)
	if ownerErr == nil && saved != nil {
		catalogSkillID = saved.CatalogSkillID
		skillSynced = saved.SkillSynced
	}
	managed := ownerErr != nil || saved == nil || strings.HasPrefix(saved.CreatedBy, managedAppPrefix)
	// Enterprise app skills are enabled locally only after business verification.
	// The legacy synchronizer targets a shared catalog, not the caller's tenant.
	if !managed && h.cfg.AppDeploy.AutoSyncSkill && h.knowledgeSyncer != nil {
		catID, err := h.knowledgeSyncer.PushAndEnableSkill(ctx, zipBytes)
		if err != nil {
			log.Printf("[DeployJob][%s] PushAndEnableSkill warning: %v", slug, err)
			buildLog += fmt.Sprintf("\n[Warning] Push to knowledge-opc failed: %v", err)
		} else {
			catalogSkillID = catID
			skillSynced = true
			buildLog += fmt.Sprintf("\n[Success] Pushed to knowledge-opc Catalog ID: %s", catalogSkillID)
		}
	}

	// 7. 更新状态为 running 并激活当前版本记录
	finalRunningLog := artifactReceipt + redactAppRuntimeLogs(buildLog, mergedEnvList)
	store := h.resolveArtifactStore(ctx)
	version, err := h.storage.PromoteAppDeployment(ctx, attempt.ID, slug, capVer, deployedImageName, catalogSkillID, finalRunningLog, skillSynced, func(release *model.AppDeploymentVersion) error {
		if release.VersionNote == "" {
			release.VersionNote = fmt.Sprintf("发布版本 (v%d)", release.Version)
		}
		prefix := fmt.Sprintf("apps/%s/v%d", slug, release.Version)
		return archivePublishArtifacts(ctx, store, release, prefix, tarBytes, zipBytes, deliveryPlan...)
	})
	if err != nil {
		message := fmt.Sprintf("deployment release could not be recorded: %v", err)
		handleDeployFailure(finalRunningLog+"\n"+message, message)
		return
	}

	// 8. 若该应用的语言+版本尚未沉淀 Dockerfile 模板，自动提取并存入 deploy-opc 数据库
	if savedApp, err := h.storage.GetAppBySlug(ctx, slug); !managed && err == nil && savedApp != nil {
		if tplErr := h.storage.AutoSaveDockerfileTemplateFromApp(ctx, savedApp); tplErr != nil {
			log.Printf("[DeployJob][%s] AutoSaveDockerfileTemplateFromApp warning: %v", slug, tplErr)
		}
	}

	log.Printf("[DeployJob][%s][v%d] App deployed successfully! Running at %s (image=%s)", slug, version, appURL, deployedImageName)
}

func (h *Handler) toAppDetailResponse(ctx context.Context, app model.DeployedApp, onlineResources []config.OnlineResourceConfig) AppDetailResponse {
	var configs []model.AppConfigItem
	_ = json.Unmarshal([]byte(app.AppConfigJSON), &configs)

	var resIDs []string
	_ = json.Unmarshal([]byte(app.ResourceIDsJSON), &resIDs)

	var storageMounts []model.AppStorageMount
	_ = json.Unmarshal([]byte(app.StorageMountsJSON), &storageMounts)
	if storageMounts == nil {
		storageMounts = []model.AppStorageMount{}
	}

	currentVer := app.CurrentVersion
	var skillZipSHA string
	// 若当前标记的 current_version 是失败版本且已有成功运行的活跃版本，在详情中展示实际运行的当前版本
	if app.Status == model.AppStatusRunning && currentVer > 1 {
		if activeVer, err := h.storage.GetActiveAppVersion(ctx, app.Slug); err == nil && activeVer != nil && activeVer.Version > 0 {
			currentVer = activeVer.Version
			skillZipSHA = activeVer.SkillZipSHA256
		}
	}
	if currentVer > 0 && skillZipSHA == "" && h.storage != nil {
		if verRec, err := h.storage.GetAppVersion(ctx, app.Slug, currentVer); err == nil && verRec != nil {
			skillZipSHA = verRec.SkillZipSHA256
		}
	}

	return AppDetailResponse{
		ID:                app.ID,
		Name:              app.Name,
		Slug:              app.Slug,
		Description:       app.Description,
		AppURL:            app.AppURL,
		ContainerPort:     app.ContainerPort,
		Language:          app.Language,
		RuntimeVersion:    app.RuntimeVersion,
		DockerfileContent: app.DockerfileContent,
		CurrentVersion:    currentVer,
		Status:            app.Status,
		AppConfig:         describeAppConfig(onlineResources, resIDs, configs),
		ResourceIDs:       resIDs,
		StorageMounts:     storageMounts,
		SkillName:         app.SkillName,
		SkillZipSHA256:    skillZipSHA,
		CatalogSkillID:    app.CatalogSkillID,
		SkillSynced:       app.SkillSynced,
		BuildLog:          app.BuildLog,
		ErrorMessage:      app.ErrorMessage,
		CreatedBy:         app.CreatedBy,
		CreatedAt:         app.CreatedAt,
		UpdatedAt:         app.UpdatedAt,
	}
}

func (h *Handler) appView(c *gin.Context, app model.DeployedApp, resources []config.OnlineResourceConfig) AppDetailResponse {
	view := h.toAppDetailResponse(c.Request.Context(), app, resources)
	var configs []model.AppConfigItem
	var resourceIDs []string
	_ = json.Unmarshal([]byte(app.AppConfigJSON), &configs)
	_ = json.Unmarshal([]byte(app.ResourceIDsJSON), &resourceIDs)
	effective, _ := mergeAppEnvironment(resources, resourceIDs, configs)
	view.ConfigFingerprint = configFingerprint(app, effective)
	owner := c.GetString(appOwnerKey)
	isHistorical := app.CreatedBy == ""
	view.CanManage = owner == "" || owner == app.CreatedBy || isHistorical
	if isHistorical && owner != "" {
		view.CreatedBy = owner
	}
	if !view.CanManage {
		view.DockerfileContent = ""
		view.AppConfig = nil
		view.ResourceIDs = nil
		view.StorageMounts = nil
		view.BuildLog = ""
		view.ErrorMessage = ""
		view.CreatedBy = ""
	}
	return view
}

// ListApps 查询所有部署应用列表
func (h *Handler) ListApps(c *gin.Context) {
	ctx := c.Request.Context()
	apps, err := h.storage.ListApps(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("list apps: %v", err)})
		return
	}

	onlineResources := h.loadEffectiveOnlineResources(ctx)
	result := make([]AppDetailResponse, 0, len(apps))
	for _, app := range apps {
		result = append(result, h.appView(c, app, onlineResources))
	}

	c.JSON(http.StatusOK, gin.H{"apps": result})
}

// GetApp 查询单个应用详情及部署状态
func (h *Handler) GetApp(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	onlineResources := h.loadEffectiveOnlineResources(ctx)
	c.JSON(http.StatusOK, h.appView(c, *app, onlineResources))
}

// GetAppLogs GET /api/v1/apps/:slug/logs 获取应用在服务端的容器实时运行日志与构建日志
func (h *Handler) GetAppLogs(c *gin.Context) {
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

	var configs []model.AppConfigItem
	_ = json.Unmarshal([]byte(app.AppConfigJSON), &configs)
	var resIDs []string
	_ = json.Unmarshal([]byte(app.ResourceIDsJSON), &resIDs)
	onlineResources := h.loadEffectiveOnlineResources(ctx)
	mergedEnv, _ := mergeAppEnvironment(onlineResources, resIDs, configs)
	if app.Status == model.AppStatusActivating || app.Status == model.AppStatusFailed {
		var candidate model.AppDeploymentVersion
		query := h.storage.DeployDB().WithContext(ctx).Where("app_slug = ? AND version = 0 AND status IN ?", slug,
			[]string{model.AppStatusActivating, model.AppStatusRuntimeReady, model.AppStatusFailed})
		if err := query.Order("created_at DESC").First(&candidate).Error; err == nil && candidate.DeliveryRevision != uuid.Nil {
			var candidateConfig []model.AppConfigItem
			var candidateIDs []string
			if json.Unmarshal([]byte(candidate.AppConfigJSON), &candidateConfig) == nil && json.Unmarshal([]byte(candidate.ResourceIDsJSON), &candidateIDs) == nil {
				candidateEnv, _ := mergeAppEnvironment(onlineResources, candidateIDs, candidateConfig)
				mergedEnv = append(mergedEnv, candidateEnv...)
			}
		}
	}

	runtimeLogs := ""
	runtimeErrStr := ""
	if h.caproverClient != nil {
		logCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		rawLogs, rErr := h.caproverClient.RuntimeLogs(logCtx, slug)
		cancel()
		if rErr != nil {
			runtimeErrStr = rErr.Error()
		} else {
			runtimeLogs = redactAppRuntimeLogs(rawLogs, mergedEnv)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"slug":          app.Slug,
		"name":          app.Name,
		"status":        app.Status,
		"app_url":       app.AppURL,
		"build_log":     redactAppRuntimeLogs(app.BuildLog, mergedEnv),
		"runtime_logs":  runtimeLogs,
		"runtime_error": runtimeErrStr,
		"updated_at":    app.UpdatedAt,
	})
}

// UpdateAppConfig 修改应用配置与绑定的线上资源，并触发容器秒级重启
func (h *Handler) UpdateAppConfig(c *gin.Context) {
	h.storageResourcesMu.RLock()
	defer h.storageResourcesMu.RUnlock()
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	if !h.tryLockDeploySlug(slug) {
		writeErrorEnvelope(c, 409, "APP_BUSY", "应用正在执行部署或管理操作")
		return
	}
	defer h.unlockDeploySlug(slug)
	if c.GetString(appOwnerKey) != "" && !h.checkAppOwner(c, slug, false) {
		return
	}
	if !h.allowAppChange(c, slug) || !h.allowChangeAfterMigration(c, slug) {
		return
	}
	var req UpdateAppConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	port := req.ContainerPort
	if port <= 0 {
		port = app.ContainerPort
	}
	if port <= 0 {
		port = 8080
	}

	if c.GetString(appOwnerKey) != "" {
		for _, id := range req.ResourceIDs {
			if !h.isEnterpriseResourceAllowed(slug, id) || (id == "shared-oss-minio" && !h.appUsesResource(ctx, slug, id)) {
				writeErrorEnvelope(c, 403, "DEPLOY_RESOURCE_FORBIDDEN", "所选资源未对企业应用开放")
				return
			}
		}
		for _, mount := range req.StorageMounts {
			if mount.HostPath != "" {
				writeErrorEnvelope(c, 400, "HOST_MOUNT_FORBIDDEN", "enterprise applications cannot choose host paths")
				return
			}
		}
	}
	req.AppConfig = preservePreviousSecrets(app.AppConfigJSON, req.AppConfig)
	configBytes, _ := json.Marshal(req.AppConfig)
	resourceBytes, _ := json.Marshal(req.ResourceIDs)

	var existingMounts []model.AppStorageMount
	if len(req.StorageMounts) > 0 {
		existingMounts = deploy.NormalizeStorageMounts(slug, req.StorageMounts)
	} else {
		_ = json.Unmarshal([]byte(app.StorageMountsJSON), &existingMounts)
		existingMounts = deploy.NormalizeStorageMounts(slug, existingMounts)
	}

	var previousMounts []model.AppStorageMount
	if app.StorageMountsJSON != "" {
		if err := json.Unmarshal([]byte(app.StorageMountsJSON), &previousMounts); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "existing storage bindings cannot be read"})
			return
		}
	}
	effectiveResources := h.loadEffectiveOnlineResources(ctx)
	if err := h.resolveVolumeResources(ctx, slug, app.ID, c.GetString(appOwnerKey) != "", existingMounts, previousMounts, effectiveResources); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, "STORAGE_RESOURCE_UNAVAILABLE", err.Error())
		return
	}
	var mountErr error
	existingMounts, mountErr = deploy.ReconcileStorageMounts(slug, previousMounts, existingMounts)
	if mountErr != nil {
		c.JSON(400, gin.H{"error": mountErr.Error()})
		return
	}
	if err := h.checkObjectStorageChange(ctx, slug, req.ResourceIDs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	active, activeErr := h.storage.GetActiveAppVersion(ctx, slug)
	if activeErr != nil && !errors.Is(activeErr, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "active application version cannot be read"})
		return
	}
	onlyDeclared := active != nil && active.DeliveryPlanSHA256 != ""
	mergedEnvList, mergeErr := h.applicationEnvironment(ctx, slug, effectiveResources, req.ResourceIDs, req.AppConfig, onlyDeclared)
	if mergeErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": mergeErr.Error()})
		return
	}
	if !onlyDeclared {
		mergedEnvList = appendStorageMountEnvVars(slug, mergedEnvList, existingMounts, effectiveResources)
	}
	if h.caproverClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment service unavailable"})
		return
	}
	if err := h.caproverClient.CreateVolumePaths(ctx, existingMounts, previousMounts); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, "STORAGE_RESOURCE_UNAVAILABLE", err.Error())
		return
	}

	// 先调用 CapRover 更新环境变量与持久化挂载配置触发重启，成功后再落库，避免容器更新失败但数据库出现脏状态 (V09)
	if h.caproverClient != nil {
		if err := h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList, existingMounts); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update caprover app env: %v", err)})
			return
		}
	}

	if err := h.storage.UpdateAppConfig(ctx, slug, port, string(configBytes), string(resourceBytes)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update app config in db: %v", err)})
		return
	}
	verUpdates := map[string]interface{}{
		"container_port":    port,
		"app_config_json":   string(configBytes),
		"resource_ids_json": string(resourceBytes),
		"updated_at":        time.Now(),
	}
	if len(req.StorageMounts) > 0 {
		mountsBytes, _ := json.Marshal(existingMounts)
		_ = h.storage.UpdateAppMetadata(ctx, slug, map[string]interface{}{
			"storage_mounts_json": string(mountsBytes),
		})
		verUpdates["storage_mounts_json"] = string(mountsBytes)
	}
	if db := h.storage.DeployDB(); db != nil {
		_ = db.WithContext(ctx).Model(&model.AppDeploymentVersion{}).
			Where("app_slug = ? AND is_active = ?", slug, true).
			Updates(verUpdates).Error
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "App configuration updated and restarted successfully",
		"slug":    slug,
	})
}

// ==================== Deploy-OPC 管理面 CRUD 接口 ====================

// AdminUpdateDeployAppRequest 管理后台修改应用记录请求体
type AdminUpdateDeployAppRequest struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	AppURL            string `json:"app_url"`
	ContainerPort     int    `json:"container_port"`
	Language          string `json:"language"`
	RuntimeVersion    string `json:"runtime_version"`
	DockerfileContent string `json:"dockerfile_content"`
	Status            string `json:"status"`
}

// AdminUpdateDeployApp PUT /api/v1/apps/:slug 或 /admin/api/deploy/apps/:slug
func (h *Handler) AdminUpdateDeployApp(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	var req AdminUpdateDeployAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("invalid body: %v", err)})
		return
	}
	updates := map[string]interface{}{}
	if strings.TrimSpace(req.Name) != "" {
		updates["name"] = strings.TrimSpace(req.Name)
	}
	if req.Description != "" {
		updates["description"] = req.Description
	}
	if strings.TrimSpace(req.AppURL) != "" {
		updates["app_url"] = strings.TrimSpace(req.AppURL)
	}
	if req.ContainerPort > 0 {
		updates["container_port"] = req.ContainerPort
	}
	if req.Language != "" {
		updates["language"] = storage.NormalizeLanguage(req.Language)
	}
	if req.RuntimeVersion != "" {
		updates["runtime_version"] = strings.TrimSpace(req.RuntimeVersion)
	}
	if req.DockerfileContent != "" {
		updates["dockerfile_content"] = req.DockerfileContent
	}
	if req.Status != "" {
		updates["status"] = strings.TrimSpace(req.Status)
	}
	if err := h.storage.UpdateAppMetadata(ctx, slug, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	app, _ := h.storage.GetAppBySlug(ctx, slug)
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app, "message": "应用信息已更新"})
}

// AdminDeleteDeployApp DELETE /api/v1/apps/:slug 或 /admin/api/deploy/apps/:slug
// 级联删除：停止并销毁 CapRover 容器服务、清理 MinIO 历史物理制品归档、删除所有版本快照及主表记录
func (h *Handler) AdminDeleteDeployApp(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	if !h.tryLockDeploySlug(slug) {
		writeErrorEnvelope(c, 409, "APP_BUSY", "应用正在执行部署或管理操作")
		return
	}
	defer h.unlockDeploySlug(slug)
	if c.GetString(appOwnerKey) != "" && !h.checkAppOwner(c, slug, false) {
		return
	}
	if !h.allowAppChange(c, slug) {
		return
	}

	// 1. 优先从数据库中级联删除 deployed_apps、app_deployment_versions 及专属迁移资源 (db-<slug>)
	// 具备幂等性：如果已被删除或不存在，视为已清理成功，不阻塞外部资源清理
	_ = h.storage.DeleteDeployResource(ctx, "db-"+slug)
	if err := h.storage.DeleteAppBySlug(ctx, slug); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	// 2. 并行清理外部基础设施资源（CapRover 容器服务、MinIO 历史制品包、持久化 Docker 卷）
	// 为防止上游网络挂起阻塞 HTTP 连接，使用独立 15s 超时 Context
	cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// 2a. 若配置了 CapRover 客户端，向 CapRover 发出销毁容器服务指令
	if h.caproverClient != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.caproverClient.DeleteApp(cleanCtx, slug); err != nil {
				log.Printf("[AdminDeleteDeployApp][%s] CapRover DeleteApp warning: %v", slug, err)
			}
		}()
	}

	// 2b. 清理 MinIO 对象存储与本地缓存中的归档包 (apps/<slug>/*)
	store := h.resolveArtifactStore(ctx)
	if store != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.DeleteAppPrefix(cleanCtx, slug); err != nil {
				log.Printf("[AdminDeleteDeployApp][%s] ArtifactStore DeleteAppPrefix warning: %v", slug, err)
			}
		}()
	}

	// 2c. 若请求指定了 purge_volumes=true，并发清理该应用的所有持久化 Docker 卷 (opc-vol-<slug>-*)
	if c.Query("purge_volumes") == "true" {
		si := h.getStorageInspector()
		vols, _ := si.QueryDockerVolumesRaw(cleanCtx)
		volPrefix := fmt.Sprintf("opc-vol-%s-", slug)
		for _, v := range vols {
			if v != nil && strings.HasPrefix(v.Name, volPrefix) {
				volName := v.Name
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					if err := si.RemoveDockerVolume(cleanCtx, name, true); err != nil {
						log.Printf("[AdminDeleteDeployApp][%s] remove volume %s warning: %v", slug, name, err)
					} else {
						log.Printf("[AdminDeleteDeployApp][%s] removed volume %s", slug, name)
					}
				}(volName)
			}
		}
	}

	wg.Wait()

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": fmt.Sprintf("应用 %s 及其容器服务、版本历史与制品归档已彻底删除", slug),
		"slug":    slug,
	})
}

// RecheckAppHealth POST /api/v1/apps/:slug/recheck or /:slug/verify
// 服务端执行双重健康检查（Docker 容器运行态 + HTTP 端点就绪态）并同步刷新数据库状态
func (h *Handler) RecheckAppHealth(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	if !h.tryLockDeploySlug(slug) {
		writeErrorEnvelope(c, 409, "APP_BUSY", "应用正在执行部署或管理操作")
		return
	}
	defer h.unlockDeploySlug(slug)
	if c.GetString(appOwnerKey) != "" && !h.checkAppOwner(c, slug, false) {
		return
	}

	var verifyReq struct {
		Path           string `json:"path"`
		ExpectedStatus int    `json:"expected_status"`
	}
	_ = c.ShouldBindJSON(&verifyReq)
	customPath := strings.TrimSpace(verifyReq.Path)
	expectedStatus := verifyReq.ExpectedStatus
	if expectedStatus <= 0 {
		expectedStatus = http.StatusOK
	}

	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	si := h.getStorageInspector()
	containerID, cErr := si.FindAppContainerID(ctx, slug)
	containerRunning := cErr == nil && containerID != ""

	httpPassed := false
	httpStatus := 0
	checkedURL := strings.TrimRight(app.AppURL, "/") + "/"
	if customPath != "" {
		if !strings.HasPrefix(customPath, "/") {
			customPath = "/" + customPath
		}
		checkedURL = strings.TrimRight(app.AppURL, "/") + customPath
	}
	httpMsg := "CapRover client not configured"
	if app.AppURL != "" {
		if customPath != "" {
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			probeClient := &http.Client{Timeout: 5 * time.Second}
			req, reqErr := http.NewRequestWithContext(probeCtx, "GET", checkedURL, nil)
			if reqErr == nil {
				if resp, doErr := probeClient.Do(req); doErr == nil {
					httpStatus = resp.StatusCode
					_, _ = io.ReadAll(io.LimitReader(resp.Body, 8192))
					_ = resp.Body.Close()
					httpPassed = resp.StatusCode == expectedStatus
					httpMsg = fmt.Sprintf("HTTP %d (%s)", resp.StatusCode, checkedURL)
				} else {
					httpMsg = doErr.Error()
				}
			} else {
				httpMsg = reqErr.Error()
			}
		} else if h.caproverClient != nil {
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			passed, msg, pErr := h.caproverClient.ProbeAppEndpoint(probeCtx, app.AppURL)
			httpPassed = passed
			if passed {
				httpStatus = http.StatusOK
			}
			if pErr != nil {
				httpMsg = pErr.Error()
			} else {
				httpMsg = msg
			}
		}
	}

	newStatus := model.AppStatusRunning
	if !containerRunning {
		newStatus = model.AppStatusFailed
	} else if !httpPassed {
		newStatus = "unhealthy"
	}

	_ = h.storage.UpdateAppStatus(ctx, slug, newStatus, app.BuildLog, httpMsg, app.CatalogSkillID, app.SkillSynced)

	healthy := newStatus == model.AppStatusRunning
	c.JSON(http.StatusOK, gin.H{
		"ok":                healthy,
		"healthy":           healthy,
		"status":            newStatus,
		"http_status":       httpStatus,
		"checked_url":       checkedURL,
		"container_id":      containerID,
		"container_running": containerRunning,
		"http_passed":       httpPassed,
		"message":           httpMsg,
		"app_url":           app.AppURL,
	})
}

// AdminListDeployResources GET /api/v1/apps/admin/resources
func (h *Handler) AdminListDeployResources(c *gin.Context) {
	ctx := c.Request.Context()
	if h.cfg != nil {
		_ = h.storage.BootstrapDeployDB(ctx, h.cfg.AppDeploy.OnlineResources, h.cfg.DeployDB)
	}
	h.ensureLocalDirectoryVolumesFromDeploy(ctx)
	list, err := h.storage.ListDeployResources(ctx, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	for i := range list {
		if list[i].Type == objectStorageResourceType {
			list[i].Password = ""
			list[i].EnvVars = nil
		}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list, "resources": list})
}

// AdminUpsertDeployResource POST/PUT /api/v1/apps/admin/resources
func (h *Handler) AdminUpsertDeployResource(c *gin.Context) {
	h.storageResourcesMu.Lock()
	defer h.storageResourcesMu.Unlock()
	ctx := c.Request.Context()
	var req model.DeployResource
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("参数错误: %v", err)})
		return
	}
	if idParam := strings.TrimSpace(c.Param("id")); idParam != "" {
		req.ID = idParam
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Name = strings.TrimSpace(req.Name)
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.ID == "" || req.Name == "" || req.Type == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "id、name、type 均不能为空"})
		return
	}
	if isManagedStorage(req.Type) {
		if !storageResourceID.MatchString(req.ID) || strings.HasPrefix(strings.ToLower(req.ID), "db-") {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "存储资源 ID 需为 1–64 位字母、数字、点、连字符或下划线，并避开 db- 前缀"})
			return
		}
		if claims, ok := adminauth.GetClaims(c); ok && claims.Role != adminauth.RoleSystemAdmin {
			c.JSON(http.StatusForbidden, gin.H{"code": -1, "message": "只有系统管理员可以配置应用存储"})
			return
		}
	}
	old, _ := h.storage.GetDeployResourceByID(ctx, req.ID)
	if old != nil && (isManagedStorage(old.Type) || isManagedStorage(req.Type)) && req.Type != old.Type {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "存储资源类型固定，请使用新资源 ID"})
		return
	}
	if deploy.IsDirectoryVolume(req.Type) {
		if err := h.configureDirectoryVolume(ctx, &req, old); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": err.Error()})
			return
		}
	}
	if req.Type == objectStorageResourceType {
		if old == nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "MinIO 自动分桶仅保留已有资源，请配置本地磁盘卷或阿里云 OSS 卷"})
			return
		}
		if req.Password == "" {
			req.Password = old.Password
		}
		if old.Host != req.Host || old.Username != req.Username || old.Password != req.Password || old.DatabaseName != req.DatabaseName || (old.Enabled && !req.Enabled) {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "存储连接和分配规则固定。请使用新资源 ID，并为已有应用执行数据迁移"})
			return
		}
		if _, err := managedObjectResourceConfig(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": err.Error()})
			return
		}
		req.EnvVars = objectResourcePlaceholders()
	}
	if req.Type == volumeResourceType {
		node := strings.TrimSpace(req.Host)
		if !swarmStorageNodeID.MatchString(node) {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "请填写 Swarm 存储节点 ID"})
			return
		}
		if req.Enabled {
			if h.caproverClient == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"code": -1, "message": "Deploy 未配置"})
				return
			}
			snapshot, err := h.caproverClient.ApplicationStorage(ctx)
			if err != nil || snapshot.Disk.Nodes[0].NodeID != node {
				c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无法确认此 ID 是 Deploy 所在的 Swarm 节点"})
				return
			}
		}
		req.EnvVars = map[string]string{"OPC_STORAGE_NODE_ID": node}
		req.Host, req.Username, req.Password, req.DatabaseName, req.Port = node, "", "", "", 0
	}
	if err := h.storage.UpsertDeployResource(ctx, &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	if req.Type == objectStorageResourceType {
		req.Password = ""
		req.EnvVars = nil
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": req, "message": "基础服务资源已保存"})
}

// AdminDeleteDeployResource DELETE /api/v1/apps/admin/resources/:id
func (h *Handler) AdminDeleteDeployResource(c *gin.Context) {
	h.storageResourcesMu.Lock()
	defer h.storageResourcesMu.Unlock()
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Param("id"))
	if res, err := h.storage.GetDeployResourceByID(ctx, id); err == nil && isManagedStorage(res.Type) {
		if claims, ok := adminauth.GetClaims(c); ok && claims.Role != adminauth.RoleSystemAdmin {
			c.JSON(http.StatusForbidden, gin.H{"code": -1, "message": "只有系统管理员可以配置应用存储"})
			return
		}
		inUse, err := h.storageResourceInUse(ctx, id, res.Type)
		if err != nil || inUse {
			c.JSON(http.StatusConflict, gin.H{"code": -1, "message": "该存储仍关联应用或历史版本，无法删除"})
			return
		}
	}
	if err := h.storage.DeleteDeployResource(ctx, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "基础服务资源已删除", "id": id})
}

// AdminListDockerfileTemplates GET /api/v1/apps/admin/dockerfile-templates
func (h *Handler) AdminListDockerfileTemplates(c *gin.Context) {
	ctx := c.Request.Context()
	if h.cfg != nil {
		_ = h.storage.BootstrapDeployDB(ctx, h.cfg.AppDeploy.OnlineResources, h.cfg.DeployDB)
	}
	lang := strings.TrimSpace(c.Query("language"))
	ver := strings.TrimSpace(c.Query("version"))
	list, err := h.storage.ListDockerfileTemplates(ctx, lang, ver)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list, "templates": list})
}

// AdminUpsertDockerfileTemplate POST/PUT /api/v1/apps/admin/dockerfile-templates
func (h *Handler) AdminUpsertDockerfileTemplate(c *gin.Context) {
	ctx := c.Request.Context()
	var req model.DockerfileTemplate
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("参数错误: %v", err)})
		return
	}
	if idParam := strings.TrimSpace(c.Param("id")); idParam != "" {
		req.ID = idParam
	}
	req.Language = storage.NormalizeLanguage(req.Language)
	req.RuntimeVersion = strings.TrimSpace(req.RuntimeVersion)
	if req.Language == "" || strings.TrimSpace(req.TemplateContent) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "language 和 template_content 不能为空"})
		return
	}
	if req.RuntimeVersion == "" {
		req.RuntimeVersion = "latest"
	}
	if err := h.storage.UpsertDockerfileTemplate(ctx, &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": req, "message": "Dockerfile 模板已保存"})
}

// AdminDeleteDockerfileTemplate DELETE /api/v1/apps/admin/dockerfile-templates/:id
func (h *Handler) AdminDeleteDockerfileTemplate(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Param("id"))
	if err := h.storage.DeleteDockerfileTemplate(ctx, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "Dockerfile 模板已删除", "id": id})
}
