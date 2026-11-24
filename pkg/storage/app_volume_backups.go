package storage

import (
	"context"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Storage) CreateAppVolumeBackup(ctx context.Context, backup *model.AppVolumeBackup) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppVolumeBackup{})
	return db.WithContext(ctx).Create(backup).Error
}

func (s *Storage) UpdateAppVolumeBackup(ctx context.Context, backup *model.AppVolumeBackup) error {
	db := s.DeployDB()
	return db.WithContext(ctx).Save(backup).Error
}

func (s *Storage) GetAppVolumeBackupByID(ctx context.Context, id uuid.UUID) (*model.AppVolumeBackup, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppVolumeBackup{})
	var backup model.AppVolumeBackup
	if err := db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", id).First(&backup).Error; err != nil {
		return nil, err
	}
	return &backup, nil
}

func (s *Storage) ListAppVolumeBackups(ctx context.Context, ownerID string, onlyAppDeleted bool) ([]model.AppVolumeBackup, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppVolumeBackup{})
	q := db.WithContext(ctx).Model(&model.AppVolumeBackup{}).
		Where("deleted_at IS NULL AND status = ? AND expires_at > ?", "ready", time.Now().UTC())
	if ownerID != "" {
		q = q.Where("owner_id = ?", ownerID)
	}
	if onlyAppDeleted {
		q = q.Where("app_deleted = ?", true)
	}
	var items []model.AppVolumeBackup
	if err := q.Order("created_at DESC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Storage) MarkAppVolumeBackupDownloaded(ctx context.Context, id uuid.UUID, at time.Time) error {
	db := s.DeployDB()
	return db.WithContext(ctx).Model(&model.AppVolumeBackup{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Update("downloaded_at", at).Error
}

func (s *Storage) MarkAppVolumeBackupsAppDeleted(ctx context.Context, slug string) error {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppVolumeBackup{})
	return db.WithContext(ctx).Model(&model.AppVolumeBackup{}).
		Where("app_slug = ? AND deleted_at IS NULL", slug).
		Update("app_deleted", true).Error
}

func (s *Storage) SoftDeleteAppVolumeBackup(ctx context.Context, id uuid.UUID, at time.Time) error {
	db := s.DeployDB()
	res := db.WithContext(ctx).Model(&model.AppVolumeBackup{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{
			"status":     "deleted",
			"deleted_at": at,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *Storage) ListExpiredAppVolumeBackups(ctx context.Context, now time.Time, limit int) ([]model.AppVolumeBackup, error) {
	db := s.DeployDB()
	_ = db.AutoMigrate(&model.AppVolumeBackup{})
	var items []model.AppVolumeBackup
	q := db.WithContext(ctx).
		Where("deleted_at IS NULL AND status = ? AND expires_at <= ?", "ready", now).
		Order("expires_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Storage) MarkAppVolumeBackupExpired(ctx context.Context, id uuid.UUID, at time.Time) error {
	db := s.DeployDB()
	return db.WithContext(ctx).Model(&model.AppVolumeBackup{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{
			"status":     "expired",
			"deleted_at": at,
		}).Error
}
