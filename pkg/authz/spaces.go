package authz

import (
	"context"
	"strconv"

	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
)

// EffectiveSpaceEntitlements resolves the spaces of the exact linked account.
// Membership supplies space access only. Named capabilities still come from the
// managed entitlement ledger.
func EffectiveSpaceEntitlements(ctx context.Context, s *storage.Storage, link *model.IdentityLink) ([]model.ManagedEntitlement, error) {
	if link.Status != "active" {
		return nil, ErrIdentityDisabled
	}
	var active bool
	if err := s.DB().WithContext(ctx).Raw(`
		SELECT EXISTS (SELECT 1 FROM users WHERE id = ? AND is_active = true
			AND is_system_admin = false AND deleted_at IS NULL)`, link.KnowledgeUserID).Scan(&active).Error; err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrIdentityDisabled
	}
	managed, err := s.ListActiveManagedEntitlementsBySubject(ctx, link.ImmutableSubject)
	if err != nil || len(managed) == 0 {
		return nil, err
	}
	var ids []uint64
	if err := s.DB().WithContext(ctx).Table("tenant_members AS m").
		Joins("JOIN tenants AS t ON t.id = m.tenant_id").
		Where("m.user_id = ? AND m.status = 'active' AND m.deleted_at IS NULL", link.KnowledgeUserID).
		Where("t.status = 'active' AND t.deleted_at IS NULL").
		Distinct("m.tenant_id").Order("m.tenant_id").Pluck("m.tenant_id", &ids).Error; err != nil {
		return nil, err
	}
	revoked, err := s.ListRevokedSpaces(ctx)
	if err != nil {
		return nil, err
	}
	byTenant := make(map[string][]model.ManagedEntitlement, len(ids))
	for _, id := range ids {
		byTenant[strconv.FormatUint(id, 10)] = nil
	}
	for _, id := range revoked {
		delete(byTenant, id)
	}
	for _, ent := range managed {
		if entries, ok := byTenant[ent.TenantID]; ok {
			byTenant[ent.TenantID] = append(entries, ent)
		}
	}
	var effective []model.ManagedEntitlement
	for _, numericID := range ids {
		id := strconv.FormatUint(numericID, 10)
		if entries, ok := byTenant[id]; ok {
			if len(entries) == 0 {
				effective = append(effective, model.ManagedEntitlement{TenantID: id})
			} else {
				effective = append(effective, entries...)
			}
		}
	}
	return effective, nil
}

func (e *EntitlementChecker) RequireSpace(ctx context.Context, link *model.IdentityLink, tenantID string) error {
	ents, err := EffectiveSpaceEntitlements(ctx, e.storage, link)
	if err != nil {
		return err
	}
	for _, ent := range ents {
		if ent.TenantID == tenantID {
			return nil
		}
	}
	return ErrEntitlementMissing
}
