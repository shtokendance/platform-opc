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
	_ = deployDB.AutoMigrate(&model.DeployedApp{}, &model.DeployResource{}, &model.DockerfileTemplate{})

	// 1. 幂等迁移主库（opc-weknora）中的历史 deployed_apps 记录到 deploy-opc
	if s.db != nil && s.deployDB != nil && s.db != s.deployDB {
		var legacyApps []model.DeployedApp
		if err := s.db.WithContext(ctx).Find(&legacyApps).Error; err == nil {
			for _, app := range legacyApps {
				var count int64
				if err := deployDB.WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", app.Slug).Count(&count).Error; err == nil && count == 0 {
					copyApp := app
					_ = deployDB.WithContext(ctx).Create(&copyApp).Error
				}
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
	var existing model.DeployedApp
	err := db.WithContext(ctx).Where("slug = ?", app.Slug).First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if app.ID == uuid.Nil {
				app.ID = uuid.New()
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
	return db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
		"name":               app.Name,
		"description":        app.Description,
		"app_url":            app.AppURL,
		"container_port":     app.ContainerPort,
		"language":           app.Language,
		"runtime_version":    app.RuntimeVersion,
		"dockerfile_content": app.DockerfileContent,
		"status":             app.Status,
		"app_config_json":    app.AppConfigJSON,
		"resource_ids_json":  app.ResourceIDsJSON,
		"skill_name":         app.SkillName,
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
	updates["updated_at"] = time.Now()
	res := s.DeployDB().WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteAppBySlug 删除应用记录
func (s *Storage) DeleteAppBySlug(ctx context.Context, slug string) error {
	res := s.DeployDB().WithContext(ctx).Where("slug = ?", slug).Delete(&model.DeployedApp{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
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
	var list []model.DeployResource
	q := db.WithContext(ctx).Order("created_at asc")
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

	var existing model.DeployResource
	err := db.WithContext(ctx).Where("id = ?", res.ID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if res.CreatedAt.IsZero() {
			res.CreatedAt = res.UpdatedAt
		}
		return db.WithContext(ctx).Create(res).Error
	}
	if err != nil {
		return err
	}
	res.CreatedAt = existing.CreatedAt
	return db.WithContext(ctx).Save(res).Error
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
