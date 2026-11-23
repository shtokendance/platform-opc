package authz

import (
	"context"
	"errors"
	"testing"

	"weknora-tokendance/pkg/model"
)

func TestEffectiveSpaceEntitlementsAllowsSystemAdmin(t *testing.T) {
	s, _ := setupAuthzStorage(t)
	if err := s.DB().AutoMigrate(&model.WeKnoraUser{}, &model.WeKnoraTenant{}, &model.WeKnoraTenantMember{}); err != nil {
		t.Fatalf("migrate knowledge schema: %v", err)
	}

	user := model.WeKnoraUser{
		ID: "ku-admin", Username: "admin", Email: "admin@example.com", PasswordHash: "hash",
		TenantID: 42, IsActive: true, IsSystemAdmin: true, CanAccessAllTenants: true,
	}
	if err := s.DB().Create(&user).Error; err != nil {
		t.Fatalf("create system admin: %v", err)
	}
	if err := s.DB().Create(&model.WeKnoraTenant{ID: 42, Name: "delivery", Status: "active"}).Error; err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.DB().Create(&model.WeKnoraTenantMember{UserID: user.ID, TenantID: 42, Role: "admin", Status: "active"}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	insertAuthzTestLink(t, s, "104032", user.ID, "active")
	insertAuthzTestEntitlement(t, s, "104032", "42", "member")

	var link model.IdentityLink
	if err := s.StateDB().Where("immutable_subject = ?", "104032").First(&link).Error; err != nil {
		t.Fatalf("load identity link: %v", err)
	}
	ents, err := EffectiveSpaceEntitlements(context.Background(), s, &link)
	if err != nil {
		t.Fatalf("system admin should remain provisioned: %v", err)
	}
	if len(ents) != 1 || ents[0].TenantID != "42" || ents[0].Capability != "member" {
		t.Fatalf("entitlements = %#v, want only tenant 42 membership", ents)
	}

	if err := s.DB().Exec("UPDATE users SET is_active = false WHERE id = ?", user.ID).Error; err != nil {
		t.Fatalf("deactivate user: %v", err)
	}
	if _, err := EffectiveSpaceEntitlements(context.Background(), s, &link); !errors.Is(err, ErrIdentityDisabled) {
		t.Fatalf("inactive user error = %v, want ErrIdentityDisabled", err)
	}
}
