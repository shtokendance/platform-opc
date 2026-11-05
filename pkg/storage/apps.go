package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// BootstrapDeployDB 在启动时幂等地将旧库（WeKnora 主库）中的 deployed_apps 迁移至 deploy-opc 库，
// 并将配置文件中的 online_resources 与默认 Dockerfile 模板初始化至 deploy-opc 库。
func (s *Storage) BootstrapDeployDB(ctx context.Context, seedResources []config.OnlineResourceConfig, fallbackDBCfg config.DatabaseConfig) error {
	deployDB := s.DeployDB()
	if deployDB == nil {
		return nil
	}
	_ = deployDB.AutoMigrate(&model.DeployedApp{}, &model.DeployResource{}, &model.DockerfileTemplate{}, &model.AppDeploymentVersion{})

	// 1. 幂等迁移主库（opc-weknora）中的历史 deployed_apps 记录到 deploy-opc
	if s.db != nil && s.deployDB != nil && s.db != s.deployDB {
		var legacyApps []model.DeployedApp
		if err := s.db.WithContext(ctx).Find(&legacyApps).Error; err == nil {
			for _, app := range legacyApps {
				var count int64
				if err := deployDB.WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", app.Slug).Count(&count).Error; err == nil && count == 0 {
					copyApp := app
					if copyApp.CurrentVersion <= 0 {
						copyApp.CurrentVersion = 1
					}
					_ = deployDB.WithContext(ctx).Create(&copyApp).Error
				}
			}
		}
	}

	// 1b. 为已存在但尚未生成版本记录的应用自动补齐 v1 基准版本记录
	var existingApps []model.DeployedApp
	if err := deployDB.WithContext(ctx).Find(&existingApps).Error; err == nil {
		for _, app := range existingApps {
			if app.CurrentVersion <= 0 {
				_ = deployDB.WithContext(ctx).Model(&model.DeployedApp{}).Where("id = ?", app.ID).Update("current_version", 1).Error
			}
			var verCount int64
			if err := deployDB.WithContext(ctx).Model(&model.AppDeploymentVersion{}).Where("app_slug = ?", app.Slug).Count(&verCount).Error; err == nil && verCount == 0 {
				v1 := model.AppDeploymentVersion{
					ID:                uuid.New(),
					AppSlug:           app.Slug,
					Version:           1,
					VersionNote:       "初始发布版本 (v1)",
					Status:            app.Status,
					IsActive:          true,
					CaproverVersion:   1,
					DeployedImageName: fmt.Sprintf("img-captain-%s:1", app.Slug),
					ContainerPort:     app.ContainerPort,
					Language:          app.Language,
					RuntimeVersion:    app.RuntimeVersion,
					DockerfileContent: app.DockerfileContent,
					AppConfigJSON:     app.AppConfigJSON,
					ResourceIDsJSON:   app.ResourceIDsJSON,
					StorageMountsJSON: app.StorageMountsJSON,
					CatalogSkillID:    app.CatalogSkillID,
					BuildLog:          app.BuildLog,
					ErrorMessage:      app.ErrorMessage,
					CreatedBy:         app.CreatedBy,
					CreatedAt:         app.CreatedAt,
					UpdatedAt:         app.UpdatedAt,
				}
				_ = deployDB.WithContext(ctx).Create(&v1).Error
			}
		}
	}

	// 2. 初始化基础服务资源表 deploy_resources
	for _, item := range seedResources {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		var existing model.DeployResource
		if err := deployDB.WithContext(ctx).Where("id = ?", item.ID).First(&existing).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			envJSON, _ := json.Marshal(item.EnvVars)
			host, port, user, pass, dbName := inferConnectionFieldsFromEnvVars(item.Type, item.EnvVars)
			res := model.DeployResource{
				ID:           item.ID,
				Name:         item.Name,
				Type:         strings.ToLower(strings.TrimSpace(item.Type)),
				IsDefault:    item.IsDefault,
				Host:         host,
				Port:         port,
				Username:     user,
				Password:     pass,
				DatabaseName: dbName,
				EnvVarsJSON:  string(envJSON),
				Description:  item.Description,
				Enabled:      true,
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			}
			_ = deployDB.WithContext(ctx).Create(&res).Error
		}
	}

	// 若未配置任何 postgres 资源且主数据库配置存在，自动注册服务端 PostgreSQL 基础资源（供 SQLite / 本地 PG 迁移使用）
	if fallbackDBCfg.Host != "" {
		var pgCount int64
		_ = deployDB.WithContext(ctx).Model(&model.DeployResource{}).Where("type = ?", "postgres").Count(&pgCount).Error
		if pgCount == 0 {
			port := fallbackDBCfg.Port
			if port == 0 {
				port = 5432
			}
			dbName := fallbackDBCfg.DBName
			if dbName == "" {
				dbName = "deploy-opc"
			}
			dsn := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=disable",
				url.PathEscape(fallbackDBCfg.User),
				url.PathEscape(fallbackDBCfg.Password),
				fallbackDBCfg.Host,
				port,
				dbName,
			)
			envMap := map[string]string{
				"DATABASE_URL": dsn,
				"PGHOST":       fallbackDBCfg.Host,
				"PGPORT":       strconv.Itoa(port),
				"PGUSER":       fallbackDBCfg.User,
				"PGPASSWORD":   fallbackDBCfg.Password,
				"PGDATABASE":   dbName,
				"DB_HOST":      fallbackDBCfg.Host,
				"DB_PORT":      strconv.Itoa(port),
				"DB_USER":      fallbackDBCfg.User,
				"DB_PASSWORD":  fallbackDBCfg.Password,
				"DB_NAME":      dbName,
			}
			envJSON, _ := json.Marshal(envMap)
			defaultPG := model.DeployResource{
				ID:           "pg-opc-server",
				Name:         "OPC 服务端 PostgreSQL 实例",
				Type:         "postgres",
				IsDefault:    true,
				Host:         fallbackDBCfg.Host,
				Port:         port,
				Username:     fallbackDBCfg.User,
				Password:     fallbackDBCfg.Password,
				DatabaseName: dbName,
				EnvVarsJSON:  string(envJSON),
				Description:  "平台内置 PostgreSQL 数据库实例，支持本地 SQLite 与本地 PostgreSQL 表结构及数据自动迁移（自动按应用创建独立库 app_<slug>）",
				Enabled:      true,
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			}
			_ = deployDB.WithContext(ctx).Create(&defaultPG).Error
		}
	}

	// 2b. 确保每种资源类型（postgres / mysql / redis / s3 / oss 等）都有且仅有一个默认共享资源 (is_default = true)
	ensureDefaultDeployResourcesPerType(ctx, deployDB)

	// 3. 初始化默认常用语言+版本 Dockerfile 模板
	var tplCount int64
	if err := deployDB.WithContext(ctx).Model(&model.DockerfileTemplate{}).Count(&tplCount).Error; err == nil && tplCount == 0 {
		seeds := defaultDockerfileTemplates()
		for _, tpl := range seeds {
			_ = deployDB.WithContext(ctx).Create(&tpl).Error
		}
	}

	return nil
}

func inferConnectionFieldsFromEnvVars(resType string, env map[string]string) (host string, port int, user, pass, dbName string) {
	if env == nil {
		return
	}
	for _, k := range []string{"DATABASE_URL", "REDIS_URL", "AMQP_URL", "MQ_URL"} {
		if raw := strings.TrimSpace(env[k]); raw != "" {
			if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
				host = u.Hostname()
				if p, err := strconv.Atoi(u.Port()); err == nil {
					port = p
				}
				if u.User != nil {
					user = u.User.Username()
					pass, _ = u.User.Password()
				}
				dbName = strings.TrimPrefix(u.Path, "/")
				return
			}
		}
	}
	for _, k := range []string{"PGHOST", "MYSQL_HOST", "DB_HOST", "REDIS_HOST"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			host = v
			break
		}
	}
	for _, k := range []string{"PGPORT", "MYSQL_PORT", "DB_PORT", "REDIS_PORT"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			if p, err := strconv.Atoi(v); err == nil {
				port = p
				break
			}
		}
	}
	for _, k := range []string{"PGUSER", "MYSQL_USER", "DB_USER"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			user = v
			break
		}
	}
	for _, k := range []string{"PGPASSWORD", "MYSQL_PASSWORD", "DB_PASSWORD", "REDIS_PASSWORD"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			pass = v
			break
		}
	}
	for _, k := range []string{"PGDATABASE", "MYSQL_DATABASE", "DB_NAME"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			dbName = v
			break
		}
	}
	return
}

func defaultDockerfileTemplates() []model.DockerfileTemplate {
	now := time.Now()
	return []model.DockerfileTemplate{
		{
			ID:             "tpl-python-3.11",
			Language:       "python",
			RuntimeVersion: "3.11",
			BaseImage:      "python:3.11-slim",
			Description:    "Python 3.11 标准 Web 服务模板（含阿里云/清华 pip 镜像加速与 0.0.0.0 监听）",
			SourceAppSlug:  "builtin",
			TemplateContent: `FROM python:3.11-slim
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DEFAULT_TIMEOUT=100 \
    PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/ \
    PIP_TRUSTED_HOST=mirrors.aliyun.com
COPY requirements*.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE 8080
CMD ["python", "main.py"]`,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:             "tpl-python-3.10",
			Language:       "python",
			RuntimeVersion: "3.10",
			BaseImage:      "python:3.10-slim",
			Description:    "Python 3.10 标准 Web 服务模板（含国内 pip 镜像加速）",
			SourceAppSlug:  "builtin",
			TemplateContent: `FROM python:3.10-slim
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DEFAULT_TIMEOUT=100 \
    PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/ \
    PIP_TRUSTED_HOST=mirrors.aliyun.com
COPY requirements*.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE 8080
CMD ["python", "app.py"]`,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:             "tpl-nodejs-20",
			Language:       "nodejs",
			RuntimeVersion: "20",
			BaseImage:      "node:20-alpine",
			Description:    "Node.js 20 服务端或全栈应用模板（含 npmmirror 国内源加速）",
			SourceAppSlug:  "builtin",
			TemplateContent: `FROM node:20-alpine
WORKDIR /app
RUN npm config set registry https://registry.npmmirror.com
COPY package*.json ./
RUN npm install --production=false
COPY . .
RUN if npm run | grep -q " build"; then npm run build; fi
EXPOSE 8080
ENV HOST=0.0.0.0 PORT=8080
CMD ["npm", "start"]`,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:             "tpl-go-1.22",
			Language:       "go",
			RuntimeVersion: "1.22",
			BaseImage:      "golang:1.22-alpine",
			Description:    "Go 1.22 多阶段构建模板（含 goproxy.cn 国内代理加速）",
			SourceAppSlug:  "builtin",
			TemplateContent: `FROM golang:1.22-alpine AS builder
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server .

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /out/server /app/server
COPY . .
EXPOSE 8080
CMD ["/app/server"]`,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
}

// CheckSlugAvailable 校验 slug 是否可用（未被占用）
func (s *Storage) CheckSlugAvailable(ctx context.Context, slug string) (bool, error) {
	var count int64
	err := s.DeployDB().WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Count(&count).Error
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// CreateOrUpdateApp 创建或保存应用记录（支持同 slug 重新发布更新）
func (s *Storage) CreateOrUpdateApp(ctx context.Context, app *model.DeployedApp) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DeployedApp{}, &model.AppDeploymentVersion{})
	var existing model.DeployedApp
	err := db.WithContext(ctx).Where("slug = ?", app.Slug).First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if app.ID == uuid.Nil {
				app.ID = uuid.New()
			}
			if app.CurrentVersion <= 0 {
				app.CurrentVersion = 1
			}
			now := time.Now()
			app.CreatedAt = now
			app.UpdatedAt = now
			return db.WithContext(ctx).Create(app).Error
		}
		return err
	}

	// 存在时更新关键字段（若本次未传 Dockerfile/Language 则保留已有值）
	app.ID = existing.ID
	app.CreatedAt = existing.CreatedAt
	app.UpdatedAt = time.Now()
	if app.Language == "" {
		app.Language = existing.Language
	}
	if app.RuntimeVersion == "" {
		app.RuntimeVersion = existing.RuntimeVersion
	}
	if app.DockerfileContent == "" {
		app.DockerfileContent = existing.DockerfileContent
	}
	if app.StorageMountsJSON == "" || app.StorageMountsJSON == "[]" {
		if existing.StorageMountsJSON != "" {
			app.StorageMountsJSON = existing.StorageMountsJSON
		} else {
			app.StorageMountsJSON = "[]"
		}
	}
	if app.CurrentVersion <= 0 {
		app.CurrentVersion = existing.CurrentVersion
		if app.CurrentVersion <= 0 {
			app.CurrentVersion = 1
		}
	}
	return db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
		"name":                app.Name,
		"description":         app.Description,
		"app_url":             app.AppURL,
		"container_port":      app.ContainerPort,
		"language":            app.Language,
		"runtime_version":     app.RuntimeVersion,
		"dockerfile_content":  app.DockerfileContent,
		"current_version":     app.CurrentVersion,
		"status":              app.Status,
		"app_config_json":     app.AppConfigJSON,
		"resource_ids_json":   app.ResourceIDsJSON,
		"storage_mounts_json": app.StorageMountsJSON,
		"skill_name":          app.SkillName,
		"catalog_skill_id":   app.CatalogSkillID,
		"skill_synced":       app.SkillSynced,
		"build_log":          app.BuildLog,
		"error_message":      app.ErrorMessage,
		"created_by":         app.CreatedBy,
		"updated_at":         app.UpdatedAt,
	}).Error
}

// GetAppBySlug 根据唯一标识 slug 获取应用记录
func (s *Storage) GetAppBySlug(ctx context.Context, slug string) (*model.DeployedApp, error) {
	var app model.DeployedApp
	err := s.DeployDB().WithContext(ctx).Where("slug = ?", slug).First(&app).Error
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// ListApps 查询所有部署应用列表，按创建时间降序
func (s *Storage) ListApps(ctx context.Context) ([]model.DeployedApp, error) {
	var apps []model.DeployedApp
	err := s.DeployDB().WithContext(ctx).Order("created_at desc").Find(&apps).Error
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// UpdateAppStatus 更新应用的部署与技能同步状态
func (s *Storage) UpdateAppStatus(ctx context.Context, slug string, status string, buildLog string, errMsg string, catalogSkillID string, skillSynced bool) error {
	updates := map[string]interface{}{
		"status":        status,
		"build_log":     buildLog,
		"error_message": errMsg,
		"skill_synced":  skillSynced,
		"updated_at":    time.Now(),
	}
	if catalogSkillID != "" {
		updates["catalog_skill_id"] = catalogSkillID
	}
	return s.DeployDB().WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(updates).Error
}

// UpdateAppLog 实时更新应用的构建日志
func (s *Storage) UpdateAppLog(ctx context.Context, slug string, buildLog string) error {
	return s.DeployDB().WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(map[string]interface{}{
		"build_log":  buildLog,
		"updated_at": time.Now(),
	}).Error
}

// UpdateAppConfig 修改应用配置与绑定的线上资源，并更新时间戳
func (s *Storage) UpdateAppConfig(ctx context.Context, slug string, port int, appConfigJSON string, resourceIDsJSON string) error {
	return s.DeployDB().WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(map[string]interface{}{
		"container_port":    port,
		"app_config_json":   appConfigJSON,
		"resource_ids_json": resourceIDsJSON,
		"updated_at":        time.Now(),
	}).Error
}

// UpdateAppMetadata 管理后台修改应用基础信息、语言版本、Dockerfile、状态等
func (s *Storage) UpdateAppMetadata(ctx context.Context, slug string, updates map[string]interface{}) error {
	now := time.Now()
	updates["updated_at"] = now
	statusVal, _ := updates["status"].(string)
	if statusVal == model.AppStatusRunning {
		if _, hasErr := updates["error_message"]; !hasErr {
			updates["error_message"] = ""
		}
	}
	db := s.DeployDB().WithContext(ctx)
	res := db.Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	if statusVal == model.AppStatusRunning {
		_ = db.AutoMigrate(&model.AppDeploymentVersion{})
		var latest model.AppDeploymentVersion
		if err := db.Where("app_slug = ?", slug).Order("version desc").First(&latest).Error; err == nil {
			_ = db.Model(&model.AppDeploymentVersion{}).
				Where("app_slug = ? AND version <> ?", slug, latest.Version).
				Update("is_active", false).Error
			_ = db.Model(&model.AppDeploymentVersion{}).
				Where("id = ?", latest.ID).
				Updates(map[string]interface{}{
					"status":        model.AppStatusRunning,
					"is_active":     true,
					"error_message": "",
					"updated_at":    now,
				}).Error
			_ = db.Where("app_slug = ? AND version <> ? AND status = ?", slug, latest.Version, model.AppStatusFailed).
				Delete(&model.AppDeploymentVersion{}).Error
		}
	}
	return nil
}

// DeleteAppBySlug 删除应用记录并级联清理其所有的历史版本记录
func (s *Storage) DeleteAppBySlug(ctx context.Context, slug string) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DeployedApp{}, &model.AppDeploymentVersion{})

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 删除历史版本表 app_deployment_versions
		_ = tx.Where("app_slug = ?", slug).Delete(&model.AppDeploymentVersion{}).Error

		// 2. 删除已部署主表 deployed_apps
		res := tx.Where("slug = ?", slug).Delete(&model.DeployedApp{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

// ==================== AppDeploymentVersion CRUD (deploy-opc) ====================

// NextAppVersionNumber 计算指定应用的下一个版本号：
// - 若上一个版本尚未成功（status == failed），直接复用“已成功的最大版本号 + 1”（若从未成功过则始终复用 v1），避免反复调试失败产生多个废版本号；
// - 仅当上一个版本已成功运行（running）或正在构建时，才递增版本号。
func (s *Storage) NextAppVersionNumber(ctx context.Context, slug string) (int, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	var latest model.AppDeploymentVersion
	err := db.WithContext(ctx).
		Where("app_slug = ?", slug).
		Order("version desc").
		First(&latest).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 1, nil
		}
		return 0, err
	}
	if latest.Status == model.AppStatusFailed {
		var latestRunning model.AppDeploymentVersion
		runErr := db.WithContext(ctx).
			Where("app_slug = ? AND status = ?", slug, model.AppStatusRunning).
			Order("version desc").
			First(&latestRunning).Error
		if runErr != nil {
			if errors.Is(runErr, gorm.ErrRecordNotFound) {
				return 1, nil
			}
			return latest.Version, nil
		}
		return latestRunning.Version + 1, nil
	}
	return latest.Version + 1, nil
}

// CreateAppVersion 插入或覆盖更新一条应用版本发布记录（当同版本号因上次部署失败被复用时，原地更新该版本并清理更高版本的失败残留）
func (s *Storage) CreateAppVersion(ctx context.Context, ver *model.AppDeploymentVersion) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	now := time.Now()
	ver.UpdatedAt = now

	// 清理大于当前目标版本号的遗留失败记录
	_ = db.WithContext(ctx).
		Where("app_slug = ? AND version > ? AND status = ?", ver.AppSlug, ver.Version, model.AppStatusFailed).
		Delete(&model.AppDeploymentVersion{}).Error

	var existing model.AppDeploymentVersion
	err := db.WithContext(ctx).
		Where("app_slug = ? AND version = ?", ver.AppSlug, ver.Version).
		First(&existing).Error
	if err == nil {
		ver.ID = existing.ID
		ver.CreatedAt = existing.CreatedAt
		return db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
			"version_note":        ver.VersionNote,
			"status":              ver.Status,
			"is_active":           ver.IsActive,
			"container_port":      ver.ContainerPort,
			"language":            ver.Language,
			"runtime_version":     ver.RuntimeVersion,
			"dockerfile_content":  ver.DockerfileContent,
			"app_config_json":     ver.AppConfigJSON,
			"resource_ids_json":   ver.ResourceIDsJSON,
			"storage_mounts_json": ver.StorageMountsJSON,
			"storage_bucket":      ver.StorageBucket,
			"app_tar_object_key":  ver.AppTarObjectKey,
			"app_tar_sha256":      ver.AppTarSHA256,
			"app_tar_size":        ver.AppTarSize,
			"skill_zip_object_key": ver.SkillZipObjectKey,
			"skill_zip_sha256":    ver.SkillZipSHA256,
			"skill_zip_size":      ver.SkillZipSize,
			"apis_summary_json":   ver.APIsSummaryJSON,
			"pages_summary_json":  ver.PagesSummaryJSON,
			"build_log":           ver.BuildLog,
			"error_message":       "",
			"updated_at":          now,
		}).Error
	}

	if ver.ID == uuid.Nil {
		ver.ID = uuid.New()
	}
	if ver.CreatedAt.IsZero() {
		ver.CreatedAt = now
	}
	return db.WithContext(ctx).Create(ver).Error
}

// UpdateAppVersionLog 实时更新某版本的构建日志
func (s *Storage) UpdateAppVersionLog(ctx context.Context, slug string, version int, buildLog string) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	return db.WithContext(ctx).
		Model(&model.AppDeploymentVersion{}).
		Where("app_slug = ? AND version = ?", slug, version).
		Updates(map[string]interface{}{
			"build_log":  buildLog,
			"updated_at": time.Now(),
		}).Error
}

// CompleteAppVersionDeploy 更新某版本的构建结果；当 status == running 时将其标记为当前生效版本（is_active=true）、取消旧版本激活态并清理失败尝试记录
func (s *Storage) CompleteAppVersionDeploy(ctx context.Context, slug string, version int, status string, caproverVer int, imageName string, catalogSkillID string, buildLog string, errMsg string) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	now := time.Now()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if status == model.AppStatusRunning {
			if err := tx.Model(&model.AppDeploymentVersion{}).
				Where("app_slug = ? AND version <> ?", slug, version).
				Update("is_active", false).Error; err != nil {
				return err
			}
			// 清理同应用下历史的失败版本记录，保证版本列表只保留有效发布版本
			_ = tx.Where("app_slug = ? AND version <> ? AND status = ?", slug, version, model.AppStatusFailed).
				Delete(&model.AppDeploymentVersion{}).Error
		}
		updates := map[string]interface{}{
			"status":        status,
			"is_active":     status == model.AppStatusRunning,
			"build_log":     buildLog,
			"error_message": errMsg,
			"updated_at":    now,
		}
		if caproverVer > 0 {
			updates["caprover_version"] = caproverVer
		}
		if strings.TrimSpace(imageName) != "" {
			updates["deployed_image_name"] = strings.TrimSpace(imageName)
		}
		if strings.TrimSpace(catalogSkillID) != "" {
			updates["catalog_skill_id"] = strings.TrimSpace(catalogSkillID)
		}
		return tx.Model(&model.AppDeploymentVersion{}).
			Where("app_slug = ? AND version = ?", slug, version).
			Updates(updates).Error
	})
}

// ListAppVersions 查询某应用的有效历史版本，按 version 降序返回（保留所有历史版本及失败记录供审计追踪）
func (s *Storage) ListAppVersions(ctx context.Context, slug string) ([]model.AppDeploymentVersion, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	var list []model.AppDeploymentVersion
	if err := db.WithContext(ctx).
		Where("app_slug = ?", slug).
		Order("version desc").
		Find(&list).Error; err != nil {
		return nil, err
	}
	// 如果存量应用尚未生成任何版本记录，自动根据当前 deployed_apps 补齐一条 v1
	if len(list) == 0 {
		if app, err := s.GetAppBySlug(ctx, slug); err == nil && app != nil {
			v1 := model.AppDeploymentVersion{
				ID:                uuid.New(),
				AppSlug:           app.Slug,
				Version:           1,
				VersionNote:       "初始发布版本 (v1)",
				Status:            app.Status,
				IsActive:          true,
				CaproverVersion:   1,
				DeployedImageName: fmt.Sprintf("img-captain-%s:1", app.Slug),
				ContainerPort:     app.ContainerPort,
				Language:          app.Language,
				RuntimeVersion:    app.RuntimeVersion,
				DockerfileContent: app.DockerfileContent,
				AppConfigJSON:     app.AppConfigJSON,
				ResourceIDsJSON:   app.ResourceIDsJSON,
				CatalogSkillID:    app.CatalogSkillID,
				BuildLog:          app.BuildLog,
				ErrorMessage:      app.ErrorMessage,
				CreatedBy:         app.CreatedBy,
				CreatedAt:         app.CreatedAt,
				UpdatedAt:         app.UpdatedAt,
			}
			_ = db.WithContext(ctx).Create(&v1).Error
			list = append(list, v1)
		}
	}
	return list, nil
}

// GetActiveAppVersion 获取指定应用当前生效（is_active=true）的版本记录
func (s *Storage) GetActiveAppVersion(ctx context.Context, slug string) (*model.AppDeploymentVersion, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	var ver model.AppDeploymentVersion
	if err := db.WithContext(ctx).
		Where("app_slug = ? AND is_active = ?", slug, true).
		First(&ver).Error; err != nil {
		return nil, err
	}
	return &ver, nil
}

// GetAppVersion 获取指定应用的特定版本号记录
func (s *Storage) GetAppVersion(ctx context.Context, slug string, version int) (*model.AppDeploymentVersion, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppDeploymentVersion{})
	var ver model.AppDeploymentVersion
	if err := db.WithContext(ctx).
		Where("app_slug = ? AND version = ?", slug, version).
		First(&ver).Error; err != nil {
		return nil, err
	}
	return &ver, nil
}

// ActivateAppVersion 在版本回退成功后，将目标版本标记为当前激活版本，并同步更新 deployed_apps 主表快照
func (s *Storage) ActivateAppVersion(ctx context.Context, target *model.AppDeploymentVersion, buildLog string, catalogSkillID string, skillSynced bool) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DeployedApp{}, &model.AppDeploymentVersion{})
	now := time.Now()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.AppDeploymentVersion{}).
			Where("app_slug = ?", target.AppSlug).
			Update("is_active", false).Error; err != nil {
			return err
		}
		verUpdates := map[string]interface{}{
			"is_active":     true,
			"status":        model.AppStatusRunning,
			"error_message": "",
			"updated_at":    now,
		}
		if strings.TrimSpace(buildLog) != "" {
			verUpdates["build_log"] = buildLog
		}
		if strings.TrimSpace(catalogSkillID) != "" {
			verUpdates["catalog_skill_id"] = strings.TrimSpace(catalogSkillID)
		}
		if err := tx.Model(&model.AppDeploymentVersion{}).
			Where("app_slug = ? AND version = ?", target.AppSlug, target.Version).
			Updates(verUpdates).Error; err != nil {
			return err
		}

		appUpdates := map[string]interface{}{
			"current_version":    target.Version,
			"container_port":     target.ContainerPort,
			"language":           target.Language,
			"runtime_version":    target.RuntimeVersion,
			"dockerfile_content": target.DockerfileContent,
			"app_config_json":    target.AppConfigJSON,
			"resource_ids_json":  target.ResourceIDsJSON,
			"status":             model.AppStatusRunning,
			"error_message":      "",
			"skill_synced":       skillSynced,
			"updated_at":         now,
		}
		if strings.TrimSpace(buildLog) != "" {
			appUpdates["build_log"] = buildLog
		}
		if strings.TrimSpace(catalogSkillID) != "" {
			appUpdates["catalog_skill_id"] = strings.TrimSpace(catalogSkillID)
		}
		if strings.TrimSpace(target.StorageMountsJSON) != "" && strings.TrimSpace(target.StorageMountsJSON) != "[]" {
			appUpdates["storage_mounts_json"] = target.StorageMountsJSON
		}
		return tx.Model(&model.DeployedApp{}).
			Where("slug = ?", target.AppSlug).
			Updates(appUpdates).Error
	})
}

// ==================== DeployResource CRUD (deploy-opc) ====================

func hydrateDeployResource(r *model.DeployResource) {
	r.EnvVars = make(map[string]string)
	if strings.TrimSpace(r.EnvVarsJSON) != "" {
		_ = json.Unmarshal([]byte(r.EnvVarsJSON), &r.EnvVars)
	}
	if r.EnvVars == nil {
		r.EnvVars = make(map[string]string)
	}
	// 当未显式指定自定义 env_vars 时，自动根据连接字段推导标准环境变量
	if r.Host != "" && len(r.EnvVars) == 0 {
		portStr := ""
		if r.Port > 0 {
			portStr = strconv.Itoa(r.Port)
		}
		switch strings.ToLower(r.Type) {
		case "postgres", "postgresql":
			if r.EnvVars["PGHOST"] == "" {
				r.EnvVars["PGHOST"] = r.Host
			}
			if portStr != "" && r.EnvVars["PGPORT"] == "" {
				r.EnvVars["PGPORT"] = portStr
			}
			if r.Username != "" && r.EnvVars["PGUSER"] == "" {
				r.EnvVars["PGUSER"] = r.Username
			}
			if r.Password != "" && r.EnvVars["PGPASSWORD"] == "" {
				r.EnvVars["PGPASSWORD"] = r.Password
			}
			if r.DatabaseName != "" && r.EnvVars["PGDATABASE"] == "" {
				r.EnvVars["PGDATABASE"] = r.DatabaseName
			}
			if r.EnvVars["DATABASE_URL"] == "" && r.Username != "" && r.DatabaseName != "" {
				p := r.Port
				if p == 0 {
					p = 5432
				}
				r.EnvVars["DATABASE_URL"] = fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=disable",
					url.PathEscape(r.Username), url.PathEscape(r.Password), r.Host, p, r.DatabaseName)
			}
		case "mysql":
			if r.EnvVars["MYSQL_HOST"] == "" {
				r.EnvVars["MYSQL_HOST"] = r.Host
			}
			if portStr != "" && r.EnvVars["MYSQL_PORT"] == "" {
				r.EnvVars["MYSQL_PORT"] = portStr
			}
			if r.Username != "" && r.EnvVars["MYSQL_USER"] == "" {
				r.EnvVars["MYSQL_USER"] = r.Username
			}
			if r.Password != "" && r.EnvVars["MYSQL_PASSWORD"] == "" {
				r.EnvVars["MYSQL_PASSWORD"] = r.Password
			}
			if r.DatabaseName != "" && r.EnvVars["MYSQL_DATABASE"] == "" {
				r.EnvVars["MYSQL_DATABASE"] = r.DatabaseName
			}
			if r.EnvVars["DATABASE_URL"] == "" && r.Username != "" && r.DatabaseName != "" {
				p := r.Port
				if p == 0 {
					p = 3306
				}
				r.EnvVars["DATABASE_URL"] = fmt.Sprintf("mysql://%s:%s@%s:%d/%s",
					url.PathEscape(r.Username), url.PathEscape(r.Password), r.Host, p, r.DatabaseName)
			}
		case "sqlserver", "mssql":
			if r.EnvVars["DB_HOST"] == "" {
				r.EnvVars["DB_HOST"] = r.Host
			}
			if portStr != "" && r.EnvVars["DB_PORT"] == "" {
				r.EnvVars["DB_PORT"] = portStr
			}
			if r.Username != "" && r.EnvVars["DB_USER"] == "" {
				r.EnvVars["DB_USER"] = r.Username
			}
			if r.Password != "" && r.EnvVars["DB_PASSWORD"] == "" {
				r.EnvVars["DB_PASSWORD"] = r.Password
			}
			if r.DatabaseName != "" && r.EnvVars["DB_NAME"] == "" {
				r.EnvVars["DB_NAME"] = r.DatabaseName
			}
		case "redis":
			if r.EnvVars["REDIS_HOST"] == "" {
				r.EnvVars["REDIS_HOST"] = r.Host
			}
			if portStr != "" && r.EnvVars["REDIS_PORT"] == "" {
				r.EnvVars["REDIS_PORT"] = portStr
			}
			if r.Password != "" && r.EnvVars["REDIS_PASSWORD"] == "" {
				r.EnvVars["REDIS_PASSWORD"] = r.Password
			}
			if r.EnvVars["REDIS_URL"] == "" {
				p := r.Port
				if p == 0 {
					p = 6379
				}
				if r.Password != "" {
					r.EnvVars["REDIS_URL"] = fmt.Sprintf("redis://:%s@%s:%d/0", url.PathEscape(r.Password), r.Host, p)
				} else {
					r.EnvVars["REDIS_URL"] = fmt.Sprintf("redis://%s:%d/0", r.Host, p)
				}
			}
		case "s3", "oss", "minio":
			endpoint := r.Host
			if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
				if r.Port > 0 {
					endpoint = fmt.Sprintf("http://%s:%d", r.Host, r.Port)
				} else {
					endpoint = "https://" + r.Host
				}
			}
			if r.EnvVars["S3_ENDPOINT"] == "" {
				r.EnvVars["S3_ENDPOINT"] = endpoint
			}
			if r.DatabaseName != "" && r.EnvVars["S3_BUCKET"] == "" {
				r.EnvVars["S3_BUCKET"] = r.DatabaseName
			}
			if r.Username != "" && r.EnvVars["S3_ACCESS_KEY"] == "" {
				r.EnvVars["S3_ACCESS_KEY"] = r.Username
			}
			if r.Password != "" && r.EnvVars["S3_SECRET_KEY"] == "" {
				r.EnvVars["S3_SECRET_KEY"] = r.Password
			}
		}
	}
	// 当资源为 s3 / oss / minio 时，自动同步补齐阿里云 OSS 标准变量别名，方便各类应用与同步引导层无缝读取
	if t := strings.ToLower(strings.TrimSpace(r.Type)); t == "s3" || t == "oss" || t == "minio" {
		if r.EnvVars["S3_ENDPOINT"] != "" && r.EnvVars["OSS_ENDPOINT"] == "" {
			r.EnvVars["OSS_ENDPOINT"] = r.EnvVars["S3_ENDPOINT"]
		}
		if r.EnvVars["S3_BUCKET"] != "" && r.EnvVars["OSS_BUCKET"] == "" {
			r.EnvVars["OSS_BUCKET"] = r.EnvVars["S3_BUCKET"]
		}
		if r.EnvVars["S3_ACCESS_KEY"] != "" && r.EnvVars["OSS_ACCESS_KEY_ID"] == "" {
			r.EnvVars["OSS_ACCESS_KEY_ID"] = r.EnvVars["S3_ACCESS_KEY"]
		}
		if r.EnvVars["S3_SECRET_KEY"] != "" && r.EnvVars["OSS_ACCESS_KEY_SECRET"] == "" {
			r.EnvVars["OSS_ACCESS_KEY_SECRET"] = r.EnvVars["S3_SECRET_KEY"]
		}
	}
}

func normalizeDeployResourceTypeGroup(rawType string) string {
	t := strings.ToLower(strings.TrimSpace(rawType))
	switch t {
	case "postgresql":
		return "postgres"
	case "mssql":
		return "sqlserver"
	default:
		return t
	}
}

func isPreferredDefaultResourceID(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	return strings.HasSuffix(lower, "-main") || strings.HasSuffix(lower, "-default") || strings.HasSuffix(lower, "-server")
}

// ensureDefaultDeployResourcesPerType 确保每种资源类型（排除应用专属迁移库 db-<slug>）有且仅有一个默认资源 (is_default = true)
func ensureDefaultDeployResourcesPerType(ctx context.Context, db *gorm.DB) {
	if db == nil {
		return
	}
	var all []model.DeployResource
	if err := db.WithContext(ctx).Order("created_at asc, id asc").Find(&all).Error; err != nil {
		return
	}

	byType := make(map[string][]model.DeployResource)
	for _, r := range all {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.ID)), "db-") {
			if r.IsDefault {
				_ = db.WithContext(ctx).Model(&model.DeployResource{}).Where("id = ?", r.ID).Update("is_default", false).Error
			}
			continue
		}
		if !r.Enabled {
			continue
		}
		g := normalizeDeployResourceTypeGroup(r.Type)
		if g == "" {
			continue
		}
		byType[g] = append(byType[g], r)
	}

	for _, group := range byType {
		if len(group) == 0 {
			continue
		}
		var defaults []model.DeployResource
		for _, r := range group {
			if r.IsDefault {
				defaults = append(defaults, r)
			}
		}
		if len(defaults) == 0 {
			chosen := group[0]
			for _, r := range group {
				if isPreferredDefaultResourceID(r.ID) {
					chosen = r
					break
				}
			}
			_ = db.WithContext(ctx).Model(&model.DeployResource{}).Where("id = ?", chosen.ID).Update("is_default", true).Error
		} else if len(defaults) > 1 {
			keep := defaults[0]
			for _, r := range defaults {
				if isPreferredDefaultResourceID(r.ID) {
					keep = r
					break
				}
			}
			for _, r := range defaults {
				if r.ID != keep.ID {
					_ = db.WithContext(ctx).Model(&model.DeployResource{}).Where("id = ?", r.ID).Update("is_default", false).Error
				}
			}
		}
	}
}

// ListDeployResources 查询 deploy_resources 列表
func (s *Storage) ListDeployResources(ctx context.Context, onlyEnabled bool) ([]model.DeployResource, error) {
	db := s.DeployDB()
	if db == nil {
		return nil, nil
	}
	_ = db.AutoMigrate(&model.DeployResource{})
	ensureDefaultDeployResourcesPerType(ctx, db)
	var list []model.DeployResource
	q := db.WithContext(ctx).Order("is_default desc, created_at asc, id asc")
	if onlyEnabled {
		q = q.Where("enabled = ?", true)
	}
	if err := q.Find(&list).Error; err != nil {
		return nil, err
	}
	for i := range list {
		hydrateDeployResource(&list[i])
	}
	return list, nil
}

// GetDeployResourceByID 根据 ID 查询单个基础资源
func (s *Storage) GetDeployResourceByID(ctx context.Context, id string) (*model.DeployResource, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DeployResource{})
	var res model.DeployResource
	if err := db.WithContext(ctx).Where("id = ?", id).First(&res).Error; err != nil {
		return nil, err
	}
	hydrateDeployResource(&res)
	return &res, nil
}

// UpsertDeployResource 新增或更新基础服务资源
func (s *Storage) UpsertDeployResource(ctx context.Context, res *model.DeployResource) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DeployResource{})
	if res.EnvVars == nil {
		res.EnvVars = make(map[string]string)
	}
	hydrateDeployResource(res)
	envBytes, _ := json.Marshal(res.EnvVars)
	res.EnvVarsJSON = string(envBytes)
	res.UpdatedAt = time.Now()

	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(res.ID)), "db-") {
		res.IsDefault = false
	} else if res.IsDefault {
		typeGroup := normalizeDeployResourceTypeGroup(res.Type)
		var peers []model.DeployResource
		if err := db.WithContext(ctx).Find(&peers).Error; err == nil {
			for _, p := range peers {
				if p.ID != res.ID && normalizeDeployResourceTypeGroup(p.Type) == typeGroup && p.IsDefault {
					_ = db.WithContext(ctx).Model(&model.DeployResource{}).Where("id = ?", p.ID).Update("is_default", false).Error
				}
			}
		}
	}

	var existing model.DeployResource
	err := db.WithContext(ctx).Where("id = ?", res.ID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if res.CreatedAt.IsZero() {
			res.CreatedAt = res.UpdatedAt
		}
		if err := db.WithContext(ctx).Create(res).Error; err != nil {
			return err
		}
		ensureDefaultDeployResourcesPerType(ctx, db)
		return nil
	}
	if err != nil {
		return err
	}
	res.CreatedAt = existing.CreatedAt
	if err := db.WithContext(ctx).Save(res).Error; err != nil {
		return err
	}
	ensureDefaultDeployResourcesPerType(ctx, db)
	return nil
}

// DeleteDeployResource 删除基础服务资源
func (s *Storage) DeleteDeployResource(ctx context.Context, id string) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DeployResource{})
	tx := db.WithContext(ctx).Where("id = ?", id).Delete(&model.DeployResource{})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	ensureDefaultDeployResourcesPerType(ctx, db)
	return nil
}

// ==================== DockerfileTemplate CRUD & Auto-Learning (deploy-opc) ====================

// NormalizeLanguage 统一语言标识，例如 python3 -> python, node -> nodejs, golang -> go
func NormalizeLanguage(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	switch l {
	case "py", "python3", "fastapi", "flask", "django":
		return "python"
	case "node", "node.js", "js", "javascript", "ts", "typescript", "nextjs", "express", "nestjs":
		return "nodejs"
	case "golang", "gin":
		return "go"
	case "jdk", "openjdk", "spring", "springboot":
		return "java"
	default:
		return l
	}
}

// InferLanguageAndVersionFromDockerfile 从 Dockerfile 的 FROM 指令自动提取基础镜像、语言和版本
func InferLanguageAndVersionFromDockerfile(dockerfile string) (lang, version, baseImage string) {
	lines := strings.Split(dockerfile, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "FROM ") {
			fields := strings.Fields(line)
			for i := 1; i < len(fields); i++ {
				if strings.HasPrefix(fields[i], "--") {
					continue
				}
				baseImage = fields[i]
				break
			}
			if baseImage != "" {
				break
			}
		}
	}
	if baseImage == "" {
		return "", "", ""
	}
	img := strings.ToLower(baseImage)
	if idx := strings.LastIndex(img, "/"); idx >= 0 {
		img = img[idx+1:]
	}
	parts := strings.SplitN(img, ":", 2)
	imageName := parts[0]
	tag := ""
	if len(parts) > 1 {
		tag = parts[1]
	}

	verRe := regexp.MustCompile(`(\d+\.\d+|\d+)`)
	version = verRe.FindString(tag)

	switch {
	case strings.Contains(imageName, "python"):
		lang = "python"
	case strings.Contains(imageName, "node"):
		lang = "nodejs"
		// node 版本通常按主版本号如 18 / 20 / 22 匹配
		if strings.Contains(version, ".") {
			version = strings.Split(version, ".")[0]
		}
	case strings.Contains(imageName, "golang") || imageName == "go":
		lang = "go"
	case strings.Contains(imageName, "openjdk") || strings.Contains(imageName, "temurin") || strings.Contains(imageName, "java"):
		lang = "java"
	case strings.Contains(imageName, "rust"):
		lang = "rust"
	case strings.Contains(imageName, "nginx"):
		lang = "static"
	default:
		lang = imageName
	}
	return lang, version, baseImage
}

// ListDockerfileTemplates 查询 Dockerfile 模板列表
func (s *Storage) ListDockerfileTemplates(ctx context.Context, language, version string) ([]model.DockerfileTemplate, error) {
	db := s.DeployDB()
	if db == nil {
		return nil, nil
	}
	_ = db.AutoMigrate(&model.DockerfileTemplate{})
	q := db.WithContext(ctx).Order("language asc, runtime_version desc, updated_at desc")
	if language != "" {
		q = q.Where("language = ?", NormalizeLanguage(language))
	}
	if version != "" {
		q = q.Where("runtime_version = ?", strings.TrimSpace(version))
	}
	var list []model.DockerfileTemplate
	if err := q.Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// MatchDockerfileTemplate 按语言+版本匹配最佳 Dockerfile 模板（精确匹配优先，其次同语言主版本或最新模板）
func (s *Storage) MatchDockerfileTemplate(ctx context.Context, language, version string) (*model.DockerfileTemplate, error) {
	db := s.DeployDB()
	if db == nil {
		return nil, gorm.ErrRecordNotFound
	}
	_ = db.AutoMigrate(&model.DockerfileTemplate{})
	normLang := NormalizeLanguage(language)
	normVer := strings.TrimSpace(version)
	if normLang == "" {
		return nil, gorm.ErrRecordNotFound
	}

	// 1. 精确匹配 language + runtime_version
	if normVer != "" {
		var exact model.DockerfileTemplate
		if err := db.WithContext(ctx).
			Where("language = ? AND runtime_version = ?", normLang, normVer).
			First(&exact).Error; err == nil {
			return &exact, nil
		}
		// 2. 前缀匹配（例如 3.11.8 -> 3.11 或 20.11 -> 20）
		parts := strings.Split(normVer, ".")
		if len(parts) >= 2 {
			majorMinor := parts[0] + "." + parts[1]
			if err := db.WithContext(ctx).
				Where("language = ? AND (runtime_version = ? OR runtime_version = ?)", normLang, majorMinor, parts[0]).
				Order("runtime_version desc").
				First(&exact).Error; err == nil {
				return &exact, nil
			}
		} else if len(parts) == 1 {
			if err := db.WithContext(ctx).
				Where("language = ? AND runtime_version LIKE ?", normLang, parts[0]+"%").
				Order("runtime_version desc").
				First(&exact).Error; err == nil {
				return &exact, nil
			}
		}
	}

	// 3. 同语言最新模板兜底
	var fallback model.DockerfileTemplate
	if err := db.WithContext(ctx).
		Where("language = ?", normLang).
		Order("updated_at desc").
		First(&fallback).Error; err != nil {
		return nil, err
	}
	return &fallback, nil
}

// GetDockerfileTemplateByID 根据 ID 获取模板
func (s *Storage) GetDockerfileTemplateByID(ctx context.Context, id string) (*model.DockerfileTemplate, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DockerfileTemplate{})
	var tpl model.DockerfileTemplate
	if err := db.WithContext(ctx).Where("id = ?", id).First(&tpl).Error; err != nil {
		return nil, err
	}
	return &tpl, nil
}

// UpsertDockerfileTemplate 创建或更新 Dockerfile 模板
func (s *Storage) UpsertDockerfileTemplate(ctx context.Context, tpl *model.DockerfileTemplate) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DockerfileTemplate{})
	tpl.Language = NormalizeLanguage(tpl.Language)
	tpl.RuntimeVersion = strings.TrimSpace(tpl.RuntimeVersion)
	if tpl.BaseImage == "" && tpl.TemplateContent != "" {
		_, _, baseImg := InferLanguageAndVersionFromDockerfile(tpl.TemplateContent)
		tpl.BaseImage = baseImg
	}
	if tpl.ID == "" {
		tpl.ID = fmt.Sprintf("tpl-%s-%s", tpl.Language, strings.ReplaceAll(tpl.RuntimeVersion, ".", "_"))
	}
	tpl.UpdatedAt = time.Now()

	var existing model.DockerfileTemplate
	err := db.WithContext(ctx).Where("id = ?", tpl.ID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if tpl.CreatedAt.IsZero() {
			tpl.CreatedAt = tpl.UpdatedAt
		}
		return db.WithContext(ctx).Create(tpl).Error
	}
	if err != nil {
		return err
	}
	tpl.CreatedAt = existing.CreatedAt
	return db.WithContext(ctx).Save(tpl).Error
}

// DeleteDockerfileTemplate 删除 Dockerfile 模板
func (s *Storage) DeleteDockerfileTemplate(ctx context.Context, id string) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.DockerfileTemplate{})
	tx := db.WithContext(ctx).Where("id = ?", id).Delete(&model.DockerfileTemplate{})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// AutoSaveDockerfileTemplateFromApp 当某个语言+版本的应用部署成功时，若 deploy-opc 中尚无该语言+版本的模板，自动提取保存
func (s *Storage) AutoSaveDockerfileTemplateFromApp(ctx context.Context, app *model.DeployedApp) error {
	if app == nil || strings.TrimSpace(app.DockerfileContent) == "" {
		return nil
	}
	db := s.DeployDB()
	if db == nil {
		return nil
	}
	_ = db.AutoMigrate(&model.DockerfileTemplate{})

	inferredLang, inferredVer, baseImg := InferLanguageAndVersionFromDockerfile(app.DockerfileContent)
	lang := NormalizeLanguage(app.Language)
	if lang == "" {
		lang = inferredLang
	}
	ver := strings.TrimSpace(app.RuntimeVersion)
	if ver == "" {
		ver = inferredVer
	}
	if lang == "" {
		return nil
	}
	if ver == "" {
		ver = "latest"
	}

	var count int64
	if err := db.WithContext(ctx).
		Model(&model.DockerfileTemplate{}).
		Where("language = ? AND runtime_version = ?", lang, ver).
		Count(&count).Error; err != nil || count > 0 {
		return err
	}

	now := time.Now()
	tpl := &model.DockerfileTemplate{
		ID:              fmt.Sprintf("tpl-%s-%s-%s", lang, strings.ReplaceAll(ver, ".", "_"), uuid.NewString()[:6]),
		Language:        lang,
		RuntimeVersion:  ver,
		BaseImage:       baseImg,
		TemplateContent: strings.TrimSpace(app.DockerfileContent),
		Description:     fmt.Sprintf("由成功部署应用 %s (%s) 自动沉淀的 %s %s Dockerfile 模板", app.Name, app.Slug, lang, ver),
		SourceAppSlug:   app.Slug,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	return db.WithContext(ctx).Create(tpl).Error
}
