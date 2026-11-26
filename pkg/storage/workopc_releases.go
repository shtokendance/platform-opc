package storage

import (
	"context"
	"fmt"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Storage) CreateWorkOPCRelease(ctx context.Context, rel *model.WorkOPCRelease) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if rel.ID == "" {
		rel.ID = uuid.New().String()
	}
	if rel.Status == "" {
		rel.Status = "draft"
	}
	now := time.Now().UTC()
	if rel.CreatedAt.IsZero() {
		rel.CreatedAt = now
	}
	if rel.UpdatedAt.IsZero() {
		rel.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(rel).Error
}

func (s *Storage) GetWorkOPCRelease(ctx context.Context, id string) (*model.WorkOPCRelease, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var rel model.WorkOPCRelease
	if err := s.stateDB.WithContext(ctx).Where("id = ?", id).First(&rel).Error; err != nil {
		return nil, err
	}
	artifacts, err := s.ListWorkOPCArtifactsByRelease(ctx, rel.ID)
	if err != nil {
		return nil, err
	}
	rel.Artifacts = artifacts
	return &rel, nil
}

func (s *Storage) ListWorkOPCReleases(ctx context.Context, channel string) ([]model.WorkOPCRelease, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	q := s.stateDB.WithContext(ctx).Model(&model.WorkOPCRelease{})
	if channel != "" {
		q = q.Where("channel = ?", channel)
	}
	var releases []model.WorkOPCRelease
	if err := q.Order("created_at DESC").Find(&releases).Error; err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return releases, nil
	}
	ids := make([]string, len(releases))
	for i := range releases {
		ids[i] = releases[i].ID
	}
	var artifacts []model.WorkOPCReleaseArtifact
	if err := s.stateDB.WithContext(ctx).Where("release_id IN ?", ids).Order("target ASC, kind ASC").Find(&artifacts).Error; err != nil {
		return nil, err
	}
	byRelease := make(map[string][]model.WorkOPCReleaseArtifact, len(releases))
	for _, a := range artifacts {
		byRelease[a.ReleaseID] = append(byRelease[a.ReleaseID], a)
	}
	for i := range releases {
		releases[i].Artifacts = byRelease[releases[i].ID]
		if releases[i].Artifacts == nil {
			releases[i].Artifacts = []model.WorkOPCReleaseArtifact{}
		}
	}
	return releases, nil
}

func (s *Storage) UpdateWorkOPCRelease(ctx context.Context, id string, updates map[string]interface{}) (*model.WorkOPCRelease, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	updates["updated_at"] = time.Now().UTC()
	res := s.stateDB.WithContext(ctx).Model(&model.WorkOPCRelease{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return s.GetWorkOPCRelease(ctx, id)
}

func (s *Storage) PublishWorkOPCRelease(ctx context.Context, id string, actor string) (*model.WorkOPCRelease, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	now := time.Now().UTC()
	err := s.stateDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var target model.WorkOPCRelease
		if err := tx.Where("id = ?", id).First(&target).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.WorkOPCRelease{}).
			Where("channel = ? AND status = ? AND id <> ?", target.Channel, "published", target.ID).
			Updates(map[string]interface{}{"status": "archived", "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&model.WorkOPCRelease{}).
			Where("id = ?", target.ID).
			Updates(map[string]interface{}{
				"status":       "published",
				"published_by": actor,
				"published_at": now,
				"updated_at":   now,
			}).Error
	})
	if err != nil {
		return nil, err
	}
	return s.GetWorkOPCRelease(ctx, id)
}

func (s *Storage) YankWorkOPCRelease(ctx context.Context, id string) (*model.WorkOPCRelease, error) {
	return s.UpdateWorkOPCRelease(ctx, id, map[string]interface{}{"status": "yanked"})
}

func (s *Storage) DeleteWorkOPCRelease(ctx context.Context, id string) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	return s.stateDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("release_id = ?", id).Delete(&model.WorkOPCReleaseArtifact{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ?", id).Delete(&model.WorkOPCRelease{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

func (s *Storage) UpsertWorkOPCArtifact(ctx context.Context, art *model.WorkOPCReleaseArtifact) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	now := time.Now().UTC()
	return s.stateDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.WorkOPCReleaseArtifact
		err := tx.Where("release_id = ? AND target = ? AND kind = ?", art.ReleaseID, art.Target, art.Kind).First(&existing).Error
		if err == nil {
			art.ID = existing.ID
			art.DownloadCount = existing.DownloadCount
			art.CreatedAt = existing.CreatedAt
			return tx.Model(&model.WorkOPCReleaseArtifact{}).Where("id = ?", existing.ID).Updates(map[string]interface{}{
				"filename":   art.Filename,
				"object_key": art.ObjectKey,
				"size_bytes": art.SizeBytes,
				"sha512":     art.SHA512,
				"sha256":     art.SHA256,
				"signed":     art.Signed,
				"notarized":  art.Notarized,
			}).Error
		}
		if err != gorm.ErrRecordNotFound {
			return err
		}
		if art.ID == "" {
			art.ID = uuid.New().String()
		}
		if art.CreatedAt.IsZero() {
			art.CreatedAt = now
		}
		return tx.Create(art).Error
	})
}

func (s *Storage) GetWorkOPCArtifact(ctx context.Context, artifactID string) (*model.WorkOPCReleaseArtifact, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var art model.WorkOPCReleaseArtifact
	if err := s.stateDB.WithContext(ctx).Where("id = ?", artifactID).First(&art).Error; err != nil {
		return nil, err
	}
	return &art, nil
}

func (s *Storage) ListWorkOPCArtifactsByRelease(ctx context.Context, releaseID string) ([]model.WorkOPCReleaseArtifact, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var artifacts []model.WorkOPCReleaseArtifact
	err := s.stateDB.WithContext(ctx).Where("release_id = ?", releaseID).Order("target ASC, kind ASC").Find(&artifacts).Error
	if artifacts == nil {
		artifacts = []model.WorkOPCReleaseArtifact{}
	}
	return artifacts, err
}

func (s *Storage) DeleteWorkOPCArtifact(ctx context.Context, releaseID, artifactID string) (*model.WorkOPCReleaseArtifact, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var art model.WorkOPCReleaseArtifact
	if err := s.stateDB.WithContext(ctx).Where("id = ? AND release_id = ?", artifactID, releaseID).First(&art).Error; err != nil {
		return nil, err
	}
	if err := s.stateDB.WithContext(ctx).Where("id = ?", artifactID).Delete(&model.WorkOPCReleaseArtifact{}).Error; err != nil {
		return nil, err
	}
	return &art, nil
}

func (s *Storage) GetPublishedWorkOPCRelease(ctx context.Context, channel string) (*model.WorkOPCRelease, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var rel model.WorkOPCRelease
	err := s.stateDB.WithContext(ctx).
		Where("channel = ? AND status = ?", channel, "published").
		Order("published_at DESC, updated_at DESC").
		First(&rel).Error
	if err != nil {
		return nil, err
	}
	artifacts, err := s.ListWorkOPCArtifactsByRelease(ctx, rel.ID)
	if err != nil {
		return nil, err
	}
	rel.Artifacts = artifacts
	return &rel, nil
}

func (s *Storage) FindWorkOPCArtifactForDownload(ctx context.Context, channel, target, filename string) (*model.WorkOPCReleaseArtifact, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var art model.WorkOPCReleaseArtifact
	q := s.stateDB.WithContext(ctx).
		Table("workopc_release_artifacts AS a").
		Select("a.*").
		Joins("JOIN workopc_releases AS r ON r.id = a.release_id").
		Where("a.target = ? AND a.filename = ? AND r.status <> ?", target, filename, "yanked")
	if channel != "" {
		q = q.Where("r.channel = ?", channel)
	}
	err := q.Order("CASE WHEN r.status = 'published' THEN 0 ELSE 1 END ASC, r.published_at DESC, a.created_at DESC").First(&art).Error
	if err != nil {
		return nil, err
	}
	return &art, nil
}

func (s *Storage) IncrementWorkOPCArtifactDownload(ctx context.Context, artifactID string) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	return s.stateDB.WithContext(ctx).
		Model(&model.WorkOPCReleaseArtifact{}).
		Where("id = ?", artifactID).
		UpdateColumn("download_count", gorm.Expr("download_count + 1")).Error
}

func (s *Storage) InsertWorkOPCAuditLog(ctx context.Context, entry *model.WorkOPCReleaseAuditLog) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	return s.stateDB.WithContext(ctx).Create(entry).Error
}

func (s *Storage) ListWorkOPCAuditLogs(ctx context.Context, limit int) ([]model.WorkOPCReleaseAuditLog, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var logs []model.WorkOPCReleaseAuditLog
	err := s.stateDB.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&logs).Error
	if logs == nil {
		logs = []model.WorkOPCReleaseAuditLog{}
	}
	return logs, err
}

func (s *Storage) GetWorkOPCTotalDownloads(ctx context.Context) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	var total int64
	if err := s.stateDB.WithContext(ctx).
		Model(&model.WorkOPCReleaseArtifact{}).
		Select("COALESCE(SUM(download_count), 0)").
		Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("sum workopc downloads: %w", err)
	}
	return total, nil
}
