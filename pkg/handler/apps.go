package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	ID                uuid.UUID             `json:"id"`
	Name              string                `json:"name"`
	Slug              string                `json:"slug"`
	Description       string                `json:"description"`
	AppURL            string                `json:"app_url"`
	ContainerPort     int                   `json:"container_port"`
	Language          string                `json:"language"`
	RuntimeVersion    string                `json:"runtime_version"`
	DockerfileContent string                `json:"dockerfile_content"`
	CurrentVersion    int                   `json:"current_version"`
	Status            string                `json:"status"`
	AppConfig         []model.AppConfigItem `json:"app_config"`
	ResourceIDs       []string              `json:"resource_ids"`
	SkillName         string                `json:"skill_name"`
	CatalogSkillID    string                `json:"catalog_skill_id"`
	SkillSynced       bool                  `json:"skill_synced"`
	BuildLog          string                `json:"build_log"`
	ErrorMessage      string                `json:"error_message"`
	CreatedBy         string                `json:"created_by"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
}

// UpdateAppConfigRequest 修改配置请求体
type UpdateAppConfigRequest struct {
	ContainerPort int                   `json:"container_port"`
	AppConfig     []model.AppConfigItem `json:"app_config"`
	ResourceIDs   []string              `json:"resource_ids"`
}

// loadEffectiveDeployResources 优先从 deploy-opc 数据库读取启用的基础资源，若为空则回退至配置文件并自动入库
func (h *Handler) loadEffectiveDeployResources(ctx context.Context) []model.DeployResource {
	if h.storage != nil {
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
	for _, r := range h.cfg.AppDeploy.OnlineResources {
		out = append(out, model.DeployResource{
			ID:          r.ID,
			Name:        r.Name,
			Type:        r.Type,
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

// ListResources 返回脱敏后的线上现有可用资源列表（从 deploy-opc 数据库动态读取）
func (h *Handler) ListResources(c *gin.Context) {
	deployResources := h.loadEffectiveDeployResources(c.Request.Context())
	resources := make([]model.ResourceSummary, 0, len(deployResources))
	for _, res := range deployResources {
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
			Host:                  res.Host,
			Port:                  res.Port,
			DatabaseName:          res.DatabaseName,
			Description:           res.Description,
			EnvKeys:               keys,
			EnvPreview:            resourcePreview(res.EnvVars),
			SupportsAutoMigration: supportsAutoMigrate,
		})
	}

	c.JSON(http.StatusOK, gin.H{"resources": resources})
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

	if name == "" || slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and slug are required"})
		return
	}
	if !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid slug format"})
		return
	}

	port := 8080
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p <= 65535 {
			port = p
		} else {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid app_config json"})
		return
	}

	var parsedResourceIDs []string
	if err := json.Unmarshal([]byte(resourceIDsStr), &parsedResourceIDs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid resource_ids json"})
		return
	}

	effectiveResources := h.loadEffectiveOnlineResources(c.Request.Context())
	if _, err := mergeAppEnvironment(effectiveResources, parsedResourceIDs, parsedAppConfig); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 读取上传的 app.tar
	appTarFile, err := c.FormFile("app_tar")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "app_tar file is required"})
		return
	}
	tarReader, err := appTarFile.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open app_tar file failed"})
		return
	}
	defer tarReader.Close()
	tarBytes, err := io.ReadAll(tarReader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read app_tar file failed"})
		return
	}

	// 若未单独上传 dockerfile_content 字段，自动从 app.tar 提取 Dockerfile 并推断语言与版本
	if dockerfileContent == "" {
		dockerfileContent = extractDockerfileFromTar(tarBytes)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill_zip file is required"})
		return
	}
	zipReader, err := skillZipFile.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open skill_zip file failed"})
		return
	}
	defer zipReader.Close()
	zipBytes, err := io.ReadAll(zipReader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read skill_zip file failed"})
		return
	}

	intranetDomain := h.cfg.AppDeploy.IntranetRootDomain
	if intranetDomain == "" {
		intranetDomain = "apps.opc.intra"
	}
	appURL := fmt.Sprintf("http://%s.%s", slug, intranetDomain)
	skillName := fmt.Sprintf("app-%s", slug)

	ctx := c.Request.Context()
	nextVersion, verErr := h.storage.NextAppVersionNumber(ctx, slug)
	if verErr != nil || nextVersion <= 0 {
		nextVersion = 1
	}
	if versionNote == "" {
		if nextVersion == 1 {
			versionNote = "初始发布版本 (v1)"
		} else {
			versionNote = fmt.Sprintf("迭代升级版本 (v%d)", nextVersion)
		}
	}

	// 从 skill.zip 中提取本版本的 API 清单与页面清单摘要，并将物理文件（app.tar / skill.zip / Dockerfile / manifest.json）归档至 MinIO
	apisSummary, pagesSummary := extractSkillContractSummaries(zipBytes)
	apisSummaryJSON, _ := json.Marshal(apisSummary)
	pagesSummaryJSON, _ := json.Marshal(pagesSummary)
	bucket, tarKey, tarSHA, zipKey, zipSHA, _ := h.archiveVersionArtifacts(
		ctx,
		slug,
		nextVersion,
		tarBytes,
		zipBytes,
		dockerfileContent,
		apisSummary,
		pagesSummary,
		versionNote,
	)

	// 落库 deployed_apps (初始状态 building，记录 Dockerfile、语言及最新版本号)
	deployedApp := &model.DeployedApp{
		Name:              name,
		Slug:              slug,
		Description:       description,
		AppURL:            appURL,
		ContainerPort:     port,
		Language:          language,
		RuntimeVersion:    runtimeVersion,
		DockerfileContent: dockerfileContent,
		CurrentVersion:    nextVersion,
		Status:            model.AppStatusBuilding,
		AppConfigJSON:     appConfigStr,
		ResourceIDsJSON:   resourceIDsStr,
		SkillName:         skillName,
		BuildLog:          "Deployment job received. Initializing build...",
	}

	if err := h.storage.CreateOrUpdateApp(ctx, deployedApp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("save app record: %v", err)})
		return
	}

	// 记录本次发布的版本快照到 app_deployment_versions
	verRecord := &model.AppDeploymentVersion{
		AppSlug:           slug,
		Version:           nextVersion,
		VersionNote:       versionNote,
		Status:            model.AppStatusBuilding,
		IsActive:          false,
		ContainerPort:     port,
		Language:          language,
		RuntimeVersion:    runtimeVersion,
		DockerfileContent: dockerfileContent,
		AppConfigJSON:     appConfigStr,
		ResourceIDsJSON:   resourceIDsStr,
		StorageBucket:     bucket,
		AppTarObjectKey:   tarKey,
		AppTarSHA256:      tarSHA,
		AppTarSize:        int64(len(tarBytes)),
		SkillZipObjectKey: zipKey,
		SkillZipSHA256:    zipSHA,
		SkillZipSize:      int64(len(zipBytes)),
		APIsSummaryJSON:   string(apisSummaryJSON),
		PagesSummaryJSON:  string(pagesSummaryJSON),
		BuildLog:          "Deployment job received. Initializing build...",
	}
	_ = h.storage.CreateAppVersion(ctx, verRecord)

	// 启动部署流程（当未配置 CapRover 客户端时同步标记状态，避免测试或离线模式下产生异步竞态）
	if h.caproverClient == nil {
		h.runDeploymentAsync(slug, nextVersion, port, appURL, parsedAppConfig, parsedResourceIDs, tarBytes, zipBytes)
	} else {
		go h.runDeploymentAsync(slug, nextVersion, port, appURL, parsedAppConfig, parsedResourceIDs, tarBytes, zipBytes)
	}

	c.JSON(http.StatusAccepted, gin.H{
		"slug":            slug,
		"version":         nextVersion,
		"version_label":   fmt.Sprintf("v%d", nextVersion),
		"status":          model.AppStatusBuilding,
		"app_url":         appURL,
		"skill_name":      skillName,
		"language":        language,
		"runtime_version": runtimeVersion,
	})
}

// runDeploymentAsync 在后台执行部署至 deploy-opc 并同步技能至 knowledge-opc
func (h *Handler) runDeploymentAsync(slug string, version int, port int, appURL string, appConfigs []model.AppConfigItem, resourceIDs []string, tarBytes []byte, zipBytes []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	artifactReceipt := fmt.Sprintf("[Artifact] app.tar:%x\n[Artifact] skill.zip:%x\n", sha256.Sum256(tarBytes), sha256.Sum256(zipBytes))

	log.Printf("[DeployJob][%s][v%d] Starting background deployment...", slug, version)

	effectiveResources := h.loadEffectiveOnlineResources(ctx)
	mergedEnvList, err := mergeAppEnvironment(effectiveResources, resourceIDs, appConfigs)
	if err != nil {
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, err.Error(), err.Error(), "", false)
		_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusFailed, 0, "", "", err.Error(), err.Error())
		return
	}

	if h.caproverClient == nil {
		log.Printf("[DeployJob][%s] CapRover client not initialized. Marked as failed.", slug)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, "CapRover client not configured", "CapRover client not configured", "", false)
		_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusFailed, 0, "", "", "CapRover client not configured", "CapRover client not configured")
		return
	}

	// 2. 注册应用
	if err := h.caproverClient.EnsureAppRegistered(ctx, slug); err != nil {
		errMsg := fmt.Sprintf("EnsureAppRegistered failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
		_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusFailed, 0, "", "", errMsg, errMsg)
		return
	}

	// 3. 注入配置与端口（新建应用尚未拥有首个镜像时可能返回 1110，忽略并在构建完成后二次确认）
	if err := h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList); err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "1110") || strings.Contains(errLower, "imagename for deployed version is not available") {
			log.Printf("[DeployJob][%s] Pre-build UpdateAppConfigAndEnv returned 1110 (initial image not ready yet), will apply after build: %v", slug, err)
		} else {
			errMsg := fmt.Sprintf("UpdateAppConfigAndEnv failed: %v", err)
			log.Printf("[DeployJob][%s] %s", slug, errMsg)
			_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
			_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusFailed, 0, "", "", errMsg, errMsg)
			return
		}
	}

	// 4. 上传 app.tar
	if err := h.caproverClient.UploadAppTar(ctx, slug, tarBytes); err != nil {
		errMsg := fmt.Sprintf("UploadAppTar failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
		_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusFailed, 0, "", "", errMsg, errMsg)
		return
	}

	// 5. 等待构建完成并探测 /healthz (实时回调更新数据库日志)
	buildLog, err := h.caproverClient.WaitBuildConfigureAndHealthz(ctx, slug, appURL, func() error {
		return h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList)
	}, func(liveLog string) {
		_ = h.storage.UpdateAppLog(ctx, slug, artifactReceipt+liveLog)
		_ = h.storage.UpdateAppVersionLog(ctx, slug, version, artifactReceipt+liveLog)
	})
	if err != nil {
		errMsg := fmt.Sprintf("WaitBuildAndHealthz failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		logCtx, logCancel := context.WithTimeout(ctx, 10*time.Second)
		runtimeLog, logErr := h.caproverClient.RuntimeLogs(logCtx, slug)
		logCancel()
		if logErr == nil && strings.TrimSpace(runtimeLog) != "" {
			buildLog += "\n[Runtime logs]\n" + redactAppRuntimeLogs(runtimeLog, mergedEnvList)
		}
		finalLog := artifactReceipt + redactAppRuntimeLogs(buildLog+"\n"+errMsg, mergedEnvList)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, finalLog, errMsg, "", false)
		_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusFailed, 0, "", "", finalLog, errMsg)
		return
	}

	// 5b. 获取 CapRover 构建产出的镜像名称与内部版本号（供未来秒级回退使用）
	capVer, deployedImageName, _ := h.caproverClient.GetAppImageInfo(ctx, slug)
	if capVer <= 0 {
		capVer = version
	}
	if deployedImageName == "" {
		deployedImageName = fmt.Sprintf("img-captain-%s:%d", slug, capVer)
	}

	// 6. 同步技能至 knowledge-opc 技能池
	catalogSkillID := ""
	skillSynced := false
	if h.cfg.AppDeploy.AutoSyncSkill && h.knowledgeSyncer != nil {
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
	finalRunningLog := artifactReceipt + buildLog
	if err := h.storage.UpdateAppStatus(ctx, slug, model.AppStatusRunning, finalRunningLog, "", catalogSkillID, skillSynced); err != nil {
		log.Printf("[DeployJob][%s] Failed to set status to running: %v", slug, err)
		return
	}
	_ = h.storage.CompleteAppVersionDeploy(ctx, slug, version, model.AppStatusRunning, capVer, deployedImageName, catalogSkillID, finalRunningLog, "")

	// 8. 若该应用的语言+版本尚未沉淀 Dockerfile 模板，自动提取并存入 deploy-opc 数据库
	if savedApp, err := h.storage.GetAppBySlug(ctx, slug); err == nil && savedApp != nil {
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

	currentVer := app.CurrentVersion
	if currentVer <= 0 {
		currentVer = 1
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
		SkillName:         app.SkillName,
		CatalogSkillID:    app.CatalogSkillID,
		SkillSynced:       app.SkillSynced,
		BuildLog:          app.BuildLog,
		ErrorMessage:      app.ErrorMessage,
		CreatedBy:         app.CreatedBy,
		CreatedAt:         app.CreatedAt,
		UpdatedAt:         app.UpdatedAt,
	}
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
		result = append(result, h.toAppDetailResponse(ctx, app, onlineResources))
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
	c.JSON(http.StatusOK, h.toAppDetailResponse(ctx, *app, onlineResources))
}

// GetAppLogs GET /api/v1/apps/:slug/logs 获取应用在服务端的容器实时运行日志与构建日志
func (h *Handler) GetAppLogs(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
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
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
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

	configBytes, _ := json.Marshal(req.AppConfig)
	resourceBytes, _ := json.Marshal(req.ResourceIDs)

	effectiveResources := h.loadEffectiveOnlineResources(ctx)
	mergedEnvList, mergeErr := mergeAppEnvironment(effectiveResources, req.ResourceIDs, req.AppConfig)
	if mergeErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": mergeErr.Error()})
		return
	}
	if h.caproverClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment service unavailable"})
		return
	}

	// 先调用 CapRover 更新环境变量触发重启，成功后再落库，避免容器更新失败但数据库出现脏状态 (V09)
	if h.caproverClient != nil {
		if err := h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update caprover app env: %v", err)})
			return
		}
	}

	if err := h.storage.UpdateAppConfig(ctx, slug, port, string(configBytes), string(resourceBytes)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update app config in db: %v", err)})
		return
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
func (h *Handler) AdminDeleteDeployApp(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	if err := h.storage.DeleteAppBySlug(ctx, slug); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "应用记录已删除", "slug": slug})
}

// AdminListDeployResources GET /api/v1/apps/admin/resources
func (h *Handler) AdminListDeployResources(c *gin.Context) {
	ctx := c.Request.Context()
	if h.cfg != nil {
		_ = h.storage.BootstrapDeployDB(ctx, h.cfg.AppDeploy.OnlineResources, h.cfg.DeployDB)
	}
	list, err := h.storage.ListDeployResources(ctx, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list, "resources": list})
}

// AdminUpsertDeployResource POST/PUT /api/v1/apps/admin/resources
func (h *Handler) AdminUpsertDeployResource(c *gin.Context) {
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
	if err := h.storage.UpsertDeployResource(ctx, &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": req, "message": "基础服务资源已保存"})
}

// AdminDeleteDeployResource DELETE /api/v1/apps/admin/resources/:id
func (h *Handler) AdminDeleteDeployResource(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Param("id"))
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
