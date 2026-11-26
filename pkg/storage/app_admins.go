package storage

import (
	"context"
	"errors"
	"strings"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Storage) ListAppAdmins(ctx context.Context, slug string) ([]model.AppAdmin, error) {
	db := s.DeployDB()
	if db == nil {
		return []model.AppAdmin{}, nil
	}
	var admins []model.AppAdmin
	if err := db.WithContext(ctx).Where("app_slug = ?", slug).Order("created_at ASC").Find(&admins).Error; err != nil {
		return nil, err
	}
	return admins, nil
}

func (s *Storage) ListAppAdminsBySlugs(ctx context.Context, slugs []string) (map[string][]model.AppAdmin, error) {
	out := make(map[string][]model.AppAdmin)
	db := s.DeployDB()
	if db == nil || len(slugs) == 0 {
		return out, nil
	}
	var admins []model.AppAdmin
	if err := db.WithContext(ctx).Where("app_slug IN ?", slugs).Order("created_at ASC").Find(&admins).Error; err != nil {
		return nil, err
	}
	for _, item := range admins {
		out[item.AppSlug] = append(out[item.AppSlug], item)
	}
	return out, nil
}

func (s *Storage) IsAppAdmin(ctx context.Context, slug, ownerID string) (bool, error) {
	slug = strings.TrimSpace(slug)
	ownerID = strings.TrimSpace(ownerID)
	db := s.DeployDB()
	if db == nil || slug == "" || ownerID == "" {
		return false, nil
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.AppAdmin{}).
		Where("app_slug = ? AND owner_id = ?", slug, ownerID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Storage) AddAppAdmin(ctx context.Context, admin *model.AppAdmin) error {
	db := s.DeployDB()
	if db == nil {
		return errors.New("deploy database not configured")
	}
	if err := db.AutoMigrate(&model.AppAdmin{}); err != nil {
		return err
	}
	var existing model.AppAdmin
	err := db.WithContext(ctx).
		Where("app_slug = ? AND owner_id = ?", admin.AppSlug, admin.OwnerID).
		First(&existing).Error
	if err == nil {
		return db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
			"user_id":    admin.UserID,
			"username":   admin.Username,
			"email":      admin.Email,
			"granted_by": admin.GrantedBy,
		}).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if admin.ID == "" {
		admin.ID = uuid.NewString()
	}
	return db.WithContext(ctx).Create(admin).Error
}

func (s *Storage) RemoveAppAdminByUserID(ctx context.Context, slug, userID string) error {
	db := s.DeployDB()
	if db == nil {
		return errors.New("deploy database not configured")
	}
	return db.WithContext(ctx).
		Where("app_slug = ? AND user_id = ?", slug, userID).
		Delete(&model.AppAdmin{}).Error
}

func (s *Storage) GetWeKnoraUserByID(ctx context.Context, userID string) (*model.WeKnoraUser, error) {
	if s.db == nil {
		return nil, gorm.ErrRecordNotFound
	}
	var user model.WeKnoraUser
	if err := s.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", userID).
		First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Storage) ListWeKnoraUsersByIDs(ctx context.Context, userIDs []string) (map[string]model.WeKnoraUser, error) {
	out := make(map[string]model.WeKnoraUser)
	if s.db == nil || len(userIDs) == 0 {
		return out, nil
	}
	var users []model.WeKnoraUser
	if err := s.db.WithContext(ctx).
		Where("id IN ? AND deleted_at IS NULL", userIDs).
		Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}
