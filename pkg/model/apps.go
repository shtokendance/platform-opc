package model

import (
	"time"

	"github.com/google/uuid"
)

const (
	AppStatusBuilding = "building"
	AppStatusRunning  = "running"
	AppStatusFailed   = "failed"
)

// AppConfigItem 表示单项环境变量应用配置
type AppConfigItem struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	HasValue    bool   `json:"has_value,omitempty"`
	ValueSource string `json:"value_source,omitempty"`
	SourceLabel string `json:"source_label,omitempty"`
	ResourceID  string `json:"resource_id,omitempty"`
}

// ResourceSummary 用于给前端展示线上可用的内网资源（敏感连接串脱敏，仅返回包含的环境变量 key）
type ResourceSummary struct {
	ID                    string            `json:"id"`
	Name                  string            `json:"name"`
	Type                  string            `json:"type"` // mysql, postgres, sqlserver, redis, mq, oss 等
	IsDefault             bool              `json:"is_default"`
	Host                  string            `json:"host,omitempty"`
	Port                  int               `json:"port,omitempty"`
	DatabaseName          string            `json:"database_name,omitempty"`
	Description           string            `json:"description"`
	EnvKeys               []string          `json:"env_keys"`
	EnvPreview            map[string]string `json:"env_preview"`
	SupportsAutoMigration bool              `json:"supports_auto_migration"`
}

// DeployedApp 对应 deployed_apps 数据库表，记录应用状态、配置、Dockerfile 与技能同步情况
type DeployedApp struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Name              string    `gorm:"type:varchar(128);not null" json:"name"`
	Slug              string    `gorm:"type:varchar(64);uniqueIndex;not null" json:"slug"`
	Description       string    `gorm:"type:text;not null;default:''" json:"description"`
	AppURL            string    `gorm:"type:varchar(255);not null" json:"app_url"`
	ContainerPort     int       `gorm:"type:integer;not null;default:8080" json:"container_port"`
	Language          string    `gorm:"type:varchar(64);not null;default:''" json:"language"`
	RuntimeVersion    string    `gorm:"type:varchar(64);not null;default:''" json:"runtime_version"`
	DockerfileContent string    `gorm:"type:text;not null;default:''" json:"dockerfile_content"`
	CurrentVersion    int       `gorm:"type:integer;not null;default:1" json:"current_version"`           // 当前激活版本号（v1, v2...）
	Status            string    `gorm:"type:varchar(32);index;not null;default:'building'" json:"status"` // building | running | failed
	AppConfigJSON     string    `gorm:"type:text;not null;default:'[]'" json:"app_config_json"`           // JSON 字符串: []AppConfigItem
	ResourceIDsJSON   string    `gorm:"type:text;not null;default:'[]'" json:"resource_ids_json"`         // JSON 字符串: []string
	SkillName         string    `gorm:"type:varchar(128);not null;default:''" json:"skill_name"`          // 技能名: app-<slug>
	CatalogSkillID    string    `gorm:"type:varchar(128);not null;default:''" json:"catalog_skill_id"`    // knowledge-opc 技能 Catalog ID
	SkillSynced       bool      `gorm:"type:boolean;not null;default:false" json:"skill_synced"`          // 是否已同步至 knowledge-opc
	BuildLog          string    `gorm:"type:text;not null;default:''" json:"build_log"`                   // 构建与部署日志
	ErrorMessage      string    `gorm:"type:text;not null;default:''" json:"error_message"`               // 失败原因
	CreatedBy         string    `gorm:"type:varchar(128);not null;default:''" json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (DeployedApp) TableName() string {
	return "deployed_apps"
}

// AppAPISummary 记录应用某版本包含的接口摘要
type AppAPISummary struct {
	ID      string `json:"id,omitempty"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Purpose string `json:"purpose,omitempty"`
	Effect  string `json:"effect,omitempty"`
}

// AppPageSummary 记录应用某版本包含的页面摘要
type AppPageSummary struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose,omitempty"`
}

// AppDeploymentVersion 对应 deploy-opc 库中的 app_deployment_versions 表，
// 记录每次应用发布/升级的版本快照、MinIO 物理文件路径与校验和、CapRover 镜像标识及对应的技能版本。
type AppDeploymentVersion struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	AppSlug           string    `gorm:"type:varchar(64);index;not null" json:"app_slug"`
	Version           int       `gorm:"type:integer;index;not null" json:"version"`                       // 递增版本号: 1, 2, 3...
	VersionNote       string    `gorm:"type:text;not null;default:''" json:"version_note"`                // 版本说明（如 新增接口 / 页面调整）
	Status            string    `gorm:"type:varchar(32);index;not null;default:'building'" json:"status"` // building | running | failed
	IsActive          bool      `gorm:"type:boolean;index;not null;default:false" json:"is_active"`       // 是否为当前线上生效版本
	CaproverVersion   int       `gorm:"type:integer;not null;default:0" json:"caprover_version"`          // CapRover 内部构建版本号
	DeployedImageName string    `gorm:"type:varchar(255);not null;default:''" json:"deployed_image_name"` // 例如 img-captain-<slug>:1
	ContainerPort     int       `gorm:"type:integer;not null;default:8080" json:"container_port"`
	Language          string    `gorm:"type:varchar(64);not null;default:''" json:"language"`
	RuntimeVersion    string    `gorm:"type:varchar(64);not null;default:''" json:"runtime_version"`
	DockerfileContent string    `gorm:"type:text;not null;default:''" json:"dockerfile_content"`
	AppConfigJSON     string    `gorm:"type:text;not null;default:'[]'" json:"app_config_json"`
	ResourceIDsJSON   string    `gorm:"type:text;not null;default:'[]'" json:"resource_ids_json"`
	StorageBucket     string    `gorm:"type:varchar(128);not null;default:'opc-apps'" json:"storage_bucket"`
	AppTarObjectKey   string    `gorm:"type:varchar(255);not null;default:''" json:"app_tar_object_key"`
	AppTarSHA256      string    `gorm:"type:varchar(64);not null;default:''" json:"app_tar_sha256"`
	AppTarSize        int64     `gorm:"type:bigint;not null;default:0" json:"app_tar_size"`
	SkillZipObjectKey string    `gorm:"type:varchar(255);not null;default:''" json:"skill_zip_object_key"`
	SkillZipSHA256    string    `gorm:"type:varchar(64);not null;default:''" json:"skill_zip_sha256"`
	SkillZipSize      int64     `gorm:"type:bigint;not null;default:0" json:"skill_zip_size"`
	CatalogSkillID    string    `gorm:"type:varchar(128);not null;default:''" json:"catalog_skill_id"`
	APIsSummaryJSON   string    `gorm:"type:text;not null;default:'[]'" json:"apis_summary_json"`
	PagesSummaryJSON  string    `gorm:"type:text;not null;default:'[]'" json:"pages_summary_json"`
	BuildLog          string    `gorm:"type:text;not null;default:''" json:"build_log"`
	ErrorMessage      string    `gorm:"type:text;not null;default:''" json:"error_message"`
	CreatedBy         string    `gorm:"type:varchar(128);not null;default:''" json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (AppDeploymentVersion) TableName() string {
	return "app_deployment_versions"
}

// DeployResource 对应 deploy-opc 库中的 deploy_resources 表，管理平台向应用提供的线上基础服务资源（PG/MySQL/SQLServer/Redis/MQ 等）
type DeployResource struct {
	ID           string            `gorm:"type:varchar(64);primaryKey" json:"id"`
	Name         string            `gorm:"type:varchar(128);not null" json:"name"`
	Type         string            `gorm:"type:varchar(64);index;not null" json:"type"` // postgres | mysql | sqlserver | redis | mq | oss | other
	IsDefault    bool              `gorm:"type:boolean;not null;default:false" json:"is_default"`
	Host         string            `gorm:"type:varchar(255);not null;default:''" json:"host"`
	Port         int               `gorm:"type:integer;not null;default:0" json:"port"`
	Username     string            `gorm:"type:varchar(128);not null;default:''" json:"username"`
	Password     string            `gorm:"type:varchar(255);not null;default:''" json:"password"`
	DatabaseName string            `gorm:"type:varchar(128);not null;default:''" json:"database_name"`
	EnvVarsJSON  string            `gorm:"type:text;not null;default:'{}'" json:"-"`
	EnvVars      map[string]string `gorm:"-" json:"env_vars"`
	Description  string            `gorm:"type:text;not null;default:''" json:"description"`
	Enabled      bool              `gorm:"type:boolean;not null;default:true" json:"enabled"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func (DeployResource) TableName() string {
	return "deploy_resources"
}

// DockerfileTemplate 对应 deploy-opc 库中的 dockerfile_templates 表，按语言+版本沉淀成功部署的 Dockerfile 模板与基础镜像
type DockerfileTemplate struct {
	ID              string    `gorm:"type:varchar(64);primaryKey" json:"id"`
	Language        string    `gorm:"type:varchar(64);index;not null" json:"language"`         // python | nodejs | go | java | rust | static 等
	RuntimeVersion  string    `gorm:"type:varchar(64);index;not null" json:"runtime_version"`  // 3.11 | 20 | 1.22 | 17 等
	BaseImage       string    `gorm:"type:varchar(255);not null;default:''" json:"base_image"` // python:3.11-slim 等
	TemplateContent string    `gorm:"type:text;not null;default:''" json:"template_content"`
	Description     string    `gorm:"type:text;not null;default:''" json:"description"`
	SourceAppSlug   string    `gorm:"type:varchar(64);not null;default:''" json:"source_app_slug"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (DockerfileTemplate) TableName() string {
	return "dockerfile_templates"
}
