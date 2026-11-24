package model

import (
	"time"

	"github.com/google/uuid"
)

// AppStorageMountDetail 包含持久化挂载点的运行时统计信息（容量、文件数、种子保护状态）
type AppStorageMountDetail struct {
	AppStorageMount
	SizeBytes     int64    `json:"size_bytes"`
	SizeHuman     string   `json:"size_human"`
	FileCount     int      `json:"file_count"`
	SampleFiles   []string `json:"sample_files,omitempty"`
	Status        string   `json:"status"` // healthy | missing | empty
	SeedProtected bool     `json:"seed_protected"`
}

// StorageServerDiskInfo 服务器底层磁盘水位看板信息
type StorageServerDiskInfo struct {
	Path       string  `json:"path"`
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	FreeBytes  uint64  `json:"free_bytes"`
	TotalHuman string  `json:"total_human"`
	UsedHuman  string  `json:"used_human"`
	FreeHuman  string  `json:"free_human"`
	UsageRatio float64 `json:"usage_ratio"`  // 百分比，如 41.2
	IsLowSpace bool    `json:"is_low_space"` // 剩余空间少于 10%
}

// OrphanVolumeInfo 孤儿卷信息（Docker 卷存在但数据库中应用已删除或已不再声明该挂载点）
type OrphanVolumeInfo struct {
	VolumeName string `json:"volume_name"`
	AppSlug    string `json:"app_slug,omitempty"`
	MountName  string `json:"mount_name,omitempty"`
	Driver     string `json:"driver"`
	SizeBytes  int64  `json:"size_bytes"`
	SizeHuman  string `json:"size_human"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// GlobalStorageSummary 全局存储管理大盘摘要响应
type DeployVolumeRoot struct {
	Root           string  `json:"root"`
	NodeID         string  `json:"node_id"`
	Backend        string  `json:"backend"`
	FilesystemType string  `json:"filesystem_type"`
	Writable       bool    `json:"writable"`
	Available      bool    `json:"available"`
	TotalBytes     uint64  `json:"total_bytes,omitempty"`
	UsedBytes      uint64  `json:"used_bytes,omitempty"`
	FreeBytes      uint64  `json:"free_bytes,omitempty"`
	TotalHuman     string  `json:"total_human,omitempty"`
	UsedHuman      string  `json:"used_human,omitempty"`
	FreeHuman      string  `json:"free_human,omitempty"`
	UsageRatio     float64 `json:"usage_ratio,omitempty"`
	Error          string  `json:"error,omitempty"`
}

type AppVolumeBackup struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	AppSlug      string     `gorm:"size:120;index;not null" json:"app_slug"`
	AppName      string     `gorm:"size:200;not null" json:"app_name"`
	AppVersion   int        `gorm:"not null;default:0" json:"app_version"`
	OwnerID      string     `gorm:"size:160;index;not null" json:"owner_id"`
	ObjectKey    string     `gorm:"size:512;not null" json:"object_key"`
	Filename     string     `gorm:"size:255;not null" json:"filename"`
	SizeBytes    int64      `gorm:"not null;default:0" json:"size_bytes"`
	FileCount    int        `gorm:"not null;default:0" json:"file_count"`
	SHA256       string     `gorm:"size:64;not null;default:''" json:"sha256"`
	MountsJSON   string     `gorm:"type:text;not null;default:'[]'" json:"-"`
	Status       string     `gorm:"size:32;index;not null" json:"status"`
	ErrorMessage string     `gorm:"type:text;not null;default:''" json:"error_message,omitempty"`
	AppDeleted   bool       `gorm:"not null;default:false;index" json:"app_deleted"`
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	ExpiresAt    time.Time  `gorm:"index;not null" json:"expires_at"`
	CreatedAt    time.Time  `gorm:"index;not null" json:"created_at"`
	DeletedAt    *time.Time `gorm:"index" json:"deleted_at,omitempty"`
}

type AppVolumeBackupMount struct {
	ContainerPath string `json:"container_path"`
	ArchivePath   string `json:"archive_path"`
	FileCount     int    `json:"file_count"`
	SizeBytes     int64  `json:"size_bytes"`
	SizeHuman     string `json:"size_human"`
}

type AppVolumeBackupItem struct {
	ID           string                 `json:"id"`
	AppSlug      string                 `json:"app_slug"`
	AppName      string                 `json:"app_name"`
	AppVersion   int                    `json:"app_version"`
	OwnerID      string                 `json:"owner_id"`
	Filename     string                 `json:"filename"`
	SizeBytes    int64                  `json:"size_bytes"`
	SizeHuman    string                 `json:"size_human"`
	FileCount    int                    `json:"file_count"`
	SHA256       string                 `json:"sha256"`
	Status       string                 `json:"status"`
	AppDeleted   bool                   `json:"app_deleted"`
	Downloaded   bool                   `json:"downloaded"`
	DownloadedAt *time.Time             `json:"downloaded_at,omitempty"`
	ExpiresAt    time.Time              `json:"expires_at"`
	RemainDays   int                    `json:"remain_days"`
	CreatedAt    time.Time              `json:"created_at"`
	Mounts       []AppVolumeBackupMount `json:"mounts"`
}

type GlobalStorageSummary struct {
	Source             string                `json:"source,omitempty"`
	NodeID             string                `json:"node_id,omitempty"`
	NodeName           string                `json:"node_name,omitempty"`
	ServerDisk         StorageServerDiskInfo `json:"server_disk"`
	VolumeRoots        []DeployVolumeRoot    `json:"volume_roots,omitempty"`
	TotalVolumes       int                   `json:"total_volumes"`
	TotalBytes         int64                 `json:"total_bytes"`
	TotalHuman         string                `json:"total_human"`
	AppsStorage        []AppStorageOverview  `json:"apps_storage"`
	OrphanVolumes      []OrphanVolumeInfo    `json:"orphan_volumes,omitempty"`
	OrphanVolumesCount int                   `json:"orphan_volumes_count"`
	OrphanTotalBytes   int64                 `json:"orphan_total_bytes"`
	OrphanTotalHuman   string                `json:"orphan_total_human"`
}

type AppObjectStorage struct {
	ResourceID string `json:"resource_id"`
	Bucket     string `json:"bucket"`
}

// AppStorageOverview 单个应用的存储卷占用总览
type AppStorageOverview struct {
	Source        string                  `json:"source,omitempty"`
	AppSlug       string                  `json:"app_slug"`
	AppName       string                  `json:"app_name"`
	AppStatus     string                  `json:"app_status"`
	MountsCount   int                     `json:"mounts_count"`
	TotalBytes    int64                   `json:"total_bytes"`
	TotalHuman    string                  `json:"total_human"`
	TotalFiles    int                     `json:"total_files"`
	Mounts        []AppStorageMountDetail `json:"mounts"`
	ObjectStorage *AppObjectStorage       `json:"object_storage,omitempty"`
}
