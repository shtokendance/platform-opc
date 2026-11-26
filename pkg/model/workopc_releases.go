package model

import "time"

type WorkOPCRelease struct {
	ID                  string                   `gorm:"column:id;type:text;primaryKey" json:"id"`
	Version             string                   `gorm:"column:version;type:text;not null;uniqueIndex:idx_workopc_version_channel" json:"version"`
	Channel             string                   `gorm:"column:channel;type:text;not null;uniqueIndex:idx_workopc_version_channel" json:"channel"`
	Status              string                   `gorm:"column:status;type:text;not null;default:'draft'" json:"status"`
	ReleaseNotes        string                   `gorm:"column:release_notes;type:text;not null;default:''" json:"release_notes"`
	RolloutPercentage   int                      `gorm:"column:rollout_percentage;not null;default:100" json:"rollout_percentage"`
	IsMandatory         bool                     `gorm:"column:is_mandatory;not null;default:false" json:"is_mandatory"`
	MinSupportedVersion string                   `gorm:"column:min_supported_version;type:text;not null;default:''" json:"min_supported_version"`
	CommitSHA           string                   `gorm:"column:commit_sha;type:text;not null;default:''" json:"commit_sha"`
	PublishedBy         string                   `gorm:"column:published_by;type:text;not null;default:''" json:"published_by"`
	PublishedAt         *time.Time               `gorm:"column:published_at" json:"published_at,omitempty"`
	CreatedAt           time.Time                `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt           time.Time                `gorm:"column:updated_at;not null" json:"updated_at"`
	Artifacts           []WorkOPCReleaseArtifact `gorm:"-" json:"artifacts,omitempty"`
}

func (WorkOPCRelease) TableName() string {
	return "workopc_releases"
}

type WorkOPCReleaseArtifact struct {
	ID            string    `gorm:"column:id;type:text;primaryKey" json:"id"`
	ReleaseID     string    `gorm:"column:release_id;type:text;not null;index" json:"release_id"`
	Target        string    `gorm:"column:target;type:text;not null" json:"target"`
	Kind          string    `gorm:"column:kind;type:text;not null" json:"kind"`
	Filename      string    `gorm:"column:filename;type:text;not null" json:"filename"`
	ObjectKey     string    `gorm:"column:object_key;type:text;not null;uniqueIndex" json:"object_key"`
	SizeBytes     int64     `gorm:"column:size_bytes;not null" json:"size_bytes"`
	SHA512        string    `gorm:"column:sha512;type:text;not null" json:"sha512"`
	SHA256        string    `gorm:"column:sha256;type:text;not null" json:"sha256"`
	Signed        bool      `gorm:"column:signed;not null;default:false" json:"signed"`
	Notarized     bool      `gorm:"column:notarized;not null;default:false" json:"notarized"`
	DownloadCount int64     `gorm:"column:download_count;not null;default:0" json:"download_count"`
	CreatedAt     time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (WorkOPCReleaseArtifact) TableName() string {
	return "workopc_release_artifacts"
}

type WorkOPCReleaseAuditLog struct {
	ID        string    `gorm:"column:id;type:text;primaryKey" json:"id"`
	ReleaseID string    `gorm:"column:release_id;type:text;not null;default:''" json:"release_id"`
	Actor     string    `gorm:"column:actor;type:text;not null" json:"actor"`
	Action    string    `gorm:"column:action;type:text;not null" json:"action"`
	Detail    string    `gorm:"column:detail;type:text;not null;default:''" json:"detail"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index" json:"created_at"`
}

func (WorkOPCReleaseAuditLog) TableName() string {
	return "workopc_release_audit_logs"
}
