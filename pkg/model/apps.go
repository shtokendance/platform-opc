package model

import (
	"time"

	"github.com/google/uuid"
)

const (
	AppStatusBuilding     = "building"
	AppStatusBuilt        = "built"
	AppStatusActivating   = "activating"
	AppStatusRuntimeReady = "runtime_ready"
	AppStatusPromoting    = "promoting"
	AppStatusRunning      = "running"
	AppStatusFailed       = "failed"
	AppStatusCancelled    = "cancelled"

	DeliveryCleanupStep          = "cleanup_preview"
	DeliveryRecoveryRequiredStep = "recovery_required"
)

// AppConfigItem 表示单项环境变量应用配置
type AppConfigItem struct {
	EffectiveValue    string `json:"effective_value,omitempty"`
	Secret            bool   `json:"secret,omitempty"`
	PreserveOnUpgrade bool   `json:"preserve_on_upgrade,omitempty"`
	Key               string `json:"key"`
	Value             string `json:"value"`
	Description       string `json:"description,omitempty"`
	Required          bool   `json:"required,omitempty"`
	HasValue          bool   `json:"has_value,omitempty"`
	ValueSource       string `json:"value_source,omitempty"`
	SourceLabel       string `json:"source_label,omitempty"`
	ResourceID        string `json:"resource_id,omitempty"`
	ResourceKey       string `json:"resource_key,omitempty"`
}

// AppDeliveryBase identifies the active release and configuration approved as an update baseline.
type AppDeliveryBase struct {
	Version        int    `json:"base_version"`
	ConfigRevision string `json:"base_config_revision"`
}

// AppDeliveryRevision records the immutable artifacts submitted before publishing.
type AppDeliveryRevision struct {
	Owner       string    `gorm:"primaryKey;size:68" json:"-"`
	Slug        string    `gorm:"primaryKey;size:40" json:"slug"`
	ID          uuid.UUID `gorm:"primaryKey;type:uuid" json:"revision"`
	BaseVersion int       `json:"base_version"`
	AppSHA      string    `gorm:"size:64" json:"app_tar_sha256"`
	SkillSHA    string    `gorm:"size:64" json:"skill_zip_sha256"`
	PlanSHA     string    `gorm:"size:64" json:"delivery_plan_sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

// AppStorageMount 描述应用容器持久化存储挂载点配置（支持 Docker Volume、宿主机/NAS 目录挂载及 S3/OSS 对象存储协同同步）
type AppStorageMount struct {
	Name            string `json:"name"`                       // 逻辑挂载名，如 data, uploads, reports, backups
	ContainerPath   string `json:"container_path"`             // 容器内绝对挂载路径，如 /app/data
	RelativePath    string `json:"relative_path,omitempty"`    // 源码相对目录，如 data, uploads
	Purpose         string `json:"purpose,omitempty"`          // 用途说明
	Kind            string `json:"kind,omitempty"`             // state-data | uploads | exports | backups | sqlite
	VolumeName      string `json:"volume_name,omitempty"`      // 分配的持久化命名卷，如 opc-vol-supplier-data
	HostPath        string `json:"host_path,omitempty"`        // 可选宿主机/NAS 绝对路径
	StorageBackend  string `json:"storage_backend,omitempty"`  // docker-volume | local-directory | aliyun-oss-volume | legacy backends
	StorageLocation string `json:"storage_location,omitempty"` // allocated OSS bucket and prefix, for display
	ResourceID      string `json:"resource_id,omitempty"`      // selected Platform volume resource
	StorageNodeID   string `json:"storage_node_id,omitempty"`  // resolved Swarm node, assigned by Platform
	SeedFilesCount  int    `json:"seed_files_count,omitempty"`
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
	CurrentVersion    int       `gorm:"type:integer;not null;default:0" json:"current_version"`           // 当前激活版本号（v1, v2...）
	Status            string    `gorm:"type:varchar(32);index;not null;default:'building'" json:"status"` // building | running | failed
	AppConfigJSON     string    `gorm:"type:text;not null;default:'[]'" json:"app_config_json"`           // JSON 字符串: []AppConfigItem
	ResourceIDsJSON   string    `gorm:"type:text;not null;default:'[]'" json:"resource_ids_json"`         // JSON 字符串: []string
	StorageMountsJSON string    `gorm:"type:text;not null;default:'[]'" json:"storage_mounts_json"`       // JSON 字符串: []AppStorageMount
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
	RequestID         string    `gorm:"type:varchar(128);index" json:"request_id,omitempty"`
	ID                uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	AppSlug           string    `gorm:"type:varchar(64);index;not null" json:"app_slug"`
	Version           int       `gorm:"type:integer;index;not null" json:"version"`                       // 部署成功后分配的正式版本号，0 表示尚未发布
	ProposedVersion   int       `gorm:"type:integer;not null;default:0" json:"-"`                         // 兼容旧客户端发布回执的候选版本号
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
	StorageMountsJSON string    `gorm:"type:text;not null;default:'[]'" json:"storage_mounts_json"`
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

	ConfigRevision         string     `gorm:"size:36;not null;default:''" json:"config_revision,omitempty"`
	DataBindingDigestsJSON string     `gorm:"type:text;not null;default:''" json:"-"`
	DeploymentSnapshotSHA  string     `gorm:"size:64;not null;default:''" json:"deployment_snapshot_sha256,omitempty"`
	DeliveryPlanSHA256     string     `gorm:"type:varchar(64);not null;default:''" json:"delivery_plan_sha256"`
	DeliveryRevision       uuid.UUID  `gorm:"type:uuid" json:"delivery_revision"`
	DeliveryAssessment     string     `gorm:"type:text;not null;default:''" json:"delivery_assessment,omitempty"`
	DeliveryStep           string     `gorm:"size:32;not null;default:''" json:"-"`
	DeliveryPhaseStartedAt *time.Time `json:"-"`
	DeliveryBuildSubmitted bool       `gorm:"not null;default:false" json:"-"`
	DeliveryBuildState     string     `gorm:"size:16;not null;default:''" json:"-"`
	DeliveryFailureKind    string     `gorm:"size:16;not null;default:''" json:"-"`
	DeliveryLease          string     `gorm:"size:36;not null;default:''" json:"-"`
	DeliveryLeaseUntil     *time.Time `json:"-"`
	MigrationRehearsal     string     `gorm:"type:text;not null;default:''" json:"migration_rehearsal,omitempty"`
	MigrationExecution     string     `gorm:"type:text;not null;default:''" json:"migration_execution,omitempty"`
	PreparedName           string     `gorm:"size:255" json:"-"`
	PreparedDescription    string     `gorm:"type:text" json:"-"`
}

// ReceiptVersion preserves the positive version expected by older clients while a request is pending or failed.
func (v AppDeploymentVersion) ReceiptVersion() int {
	if v.Version > 0 {
		return v.Version
	}
	return v.ProposedVersion
}

func (AppDeploymentVersion) TableName() string {
	return "app_deployment_versions"
}

var BlockingAppRuntimeStates = []string{"pending", "restarting", "checking", "restoring", "unknown", "recovery_unknown"}

type AppRuntimeOperation struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	AppID          uuid.UUID  `gorm:"type:uuid;index;not null" json:"-"`
	Slug           string     `gorm:"index;not null" json:"-"`
	ReleaseID      uuid.UUID  `gorm:"type:uuid;index;not null" json:"-"`
	Version        int        `json:"version"`
	Kind           string     `json:"kind"`
	State          string     `gorm:"index" json:"state"`
	Message        string     `json:"message"`
	ConfigNumber   int        `json:"config_number,omitempty"`
	ConfigRevision string     `json:"config_revision,omitempty"`
	BaseRevision   string     `json:"-"`
	SnapshotSHA    string     `json:"-"`
	ConfigJSON     string     `gorm:"type:text" json:"-"`
	ChangedKeys    string     `json:"changed_keys,omitempty"`
	RestorationID  uuid.UUID  `gorm:"type:uuid" json:"-"`
	Submitted      bool       `json:"-"`
	LeaseUntil     *time.Time `json:"-"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// DeployResource 对应 deploy-opc 库中的 deploy_resources 表，管理平台向应用提供的线上基础服务资源（PG/MySQL/SQLServer/Redis/MQ 等）
type DeployResource struct {
	ID           string            `gorm:"type:varchar(96);primaryKey" json:"id"`
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
