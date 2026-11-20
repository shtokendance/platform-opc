package syncservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errSubjectConflict 表示供给时发现 identity 冲突（knowledge_user_id 被其他 active
// 主体占用，或该主体已有 conflicted 行）：步骤记 skipped 并保留证据，不算失败。
var errSubjectConflict = errors.New("identity conflict")

// stepKey 步骤幂等键（sync_steps.idempotency_key 唯一）：run_id + step_type + subject_ref
// （revoke 追加 tenant 以区分同主体多空间收回，仍满足唯一性）。
func stepKey(runID string, a action) string {
	key := runID + "|" + a.stepType + "|" + a.subject
	if a.stepType == StepRevoke {
		key += "|" + a.tenantID
	}
	return key
}

// applyAll 逐动作执行供给/收回。每动作一行 sync_steps（幂等键找或建）：
// 已 applied/skipped 跳过；失败标 failed/retryable 后继续后续步骤（部分失败不回滚）。
func (s *Service) applyAll(ctx context.Context, run *model.SyncRun, snap *snapshot, actions []action) {
	steps, err := s.storage.ListSyncSteps(ctx, run.RunID)
	if err != nil {
		logf("run %s: list steps failed: %v", run.RunID, err)
		return
	}
	byKey := make(map[string]model.SyncStep, len(steps))
	maxSeq := 0
	for _, st := range steps {
		if st.StepSeq > maxSeq {
			maxSeq = st.StepSeq
		}
		if st.IdempotencyKey != nil {
			byKey[*st.IdempotencyKey] = st
		}
	}

	for _, a := range actions {
		key := stepKey(run.RunID, a)
		if st, ok := byKey[key]; ok {
			if st.Status == "applied" || st.Status == "skipped" {
				continue
			}
		} else {
			maxSeq++
			insert := &model.SyncStep{
				RunID: run.RunID, StepSeq: maxSeq, StepType: a.stepType,
				SubjectRef: &a.subject, IdempotencyKey: &key,
				Status: "pending", Retryable: true,
			}
			if err := s.storage.InsertSyncStepTx(s.freshTx(ctx), insert); err != nil {
				logf("run %s: insert step failed: %v", run.RunID, err)
				continue
			}
			byKey[key] = *insert
		}

		st := byKey[key]
		err := s.executeAction(ctx, run, snap, a)
		switch {
		case err == nil:
			_, _ = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, st.StepSeq, map[string]interface{}{
				"status": "applied", "error": nil, "updated_at": time.Now(),
			})
		case errors.Is(err, errSubjectConflict):
			_, _ = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, st.StepSeq, map[string]interface{}{
				"status": "skipped", "error": err.Error(), "updated_at": time.Now(),
			})
		default:
			msg := err.Error()
			_, _ = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, st.StepSeq, map[string]interface{}{
				"status": "failed", "error": msg, "retryable": true, "updated_at": time.Now(),
			})
		}
	}
}

// sweepFailedSteps Retry 孤儿回收：计划重放后仍 failed/retryable 的步骤
// （通常已不在重算 diff 的计划内，如收回已落库但撤销器/停用失败的 revoke 步骤），
// 依据步骤行重建动作重放；无法重建的（主体已离开快照/授权项已消失）标记 skipped。
// 仅对 full 快照中已离职的主体重试停用及上游撤权。
func (s *Service) sweepFailedSteps(ctx context.Context, run *model.SyncRun, snap *snapshot) {
	steps, err := s.storage.ListSyncSteps(ctx, run.RunID)
	if err != nil {
		logf("run %s: sweep list steps failed: %v", run.RunID, err)
		return
	}
	inSnapshot := make(map[string]bool, len(snap.Users))
	for _, user := range snap.Users {
		inSnapshot[user.ID] = true
	}
	for _, st := range steps {
		if st.StepSeq <= 3 || st.Status != "failed" || !st.Retryable || st.SubjectRef == nil {
			continue
		}
		subject := *st.SubjectRef
		var a *action

		switch st.StepType {
		case StepProvision:
			// 主体已不在快照：供给失去意义，跳过
			inSnap := false
			for i := range snap.Users {
				if snap.Users[i].ID == subject {
					inSnap = true
					break
				}
			}
			if inSnap {
				continue // 仍在计划内，applyAll 已处理
			}
		case StepRevoke:
			if st.IdempotencyKey == nil {
				continue
			}
			parts := strings.Split(*st.IdempotencyKey, "|")
			if len(parts) != 4 {
				continue
			}
			tenantID := parts[3]
			depart := run.Scope == string(ScopeFull) && !inSnapshot[subject]
			if !depart {
				// The fresh plan already handled any transfer still required.
				break
			}
			kuid := subject
			if links, lerr := s.storage.ListIdentityLinksByImmutableSubject(ctx, subject); lerr == nil {
				for _, l := range links {
					if l.IdentitySource == identitySource && l.OrganizationID == s.orgID() {
						kuid = l.KnowledgeUserID
						break
					}
				}
			}
			a = &action{
				stepType: StepRevoke, subject: subject,
				tenantID: tenantID, knowledgeUserID: kuid,
				depart: depart,
			}
		default:
			continue
		}

		if a == nil {
			_, _ = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, st.StepSeq, map[string]interface{}{
				"status": "skipped", "error": "subject left snapshot", "updated_at": time.Now(),
			})
			continue
		}
		if err := s.executeAction(ctx, run, snap, *a); err != nil {
			msg := err.Error()
			if errors.Is(err, errSubjectConflict) {
				msg = err.Error()
			}
			_, _ = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, st.StepSeq, map[string]interface{}{
				"status": "failed", "error": msg, "retryable": true, "updated_at": time.Now(),
			})
			continue
		}
		_, _ = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, st.StepSeq, map[string]interface{}{
			"status": "applied", "error": nil, "updated_at": time.Now(),
		})
	}
}

// executeAction 执行一步供给/收回（动作自身按「先查后写」保证幂等）。
func (s *Service) executeAction(ctx context.Context, run *model.SyncRun, snap *snapshot, a action) error {
	if a.stepType == StepRevoke {
		return s.revoke(ctx, a)
	}
	return s.provision(ctx, run, snap, a)
}

// provision 供给一名成员：确保部门空间（既有 SyncDepartment 语义）→ knowledge 开户
// （SyncUserAndMemberships：UpsertUser + 母空间成员）→ identity_links(active) +
// managed_entitlements(sync)。冲突（knowledge_user_id 被其他 active 主体占用）落
// conflicted 行 + 证据，步骤记 skipped。
func (s *Service) provision(ctx context.Context, run *model.SyncRun, snap *snapshot, a action) error {
	if a.user == nil {
		return fmt.Errorf("provision action missing user: subject=%s", a.subject)
	}
	dept, ok := snap.DeptMap[a.deptCode]
	if !ok {
		return fmt.Errorf("target department %q not in snapshot", a.deptCode)
	}

	// 已有 conflicted 行：保持冲突，不重复开户
	if existing, err := s.storage.FindIdentityLinkBySubject(ctx, identitySource, s.orgID(), a.subject); err == nil {
		if existing.Status == linkConflicted {
			return errSubjectConflict
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	// 1. 确保部门空间与映射就绪（tenant + 模型/存储/沙箱级联为既有语义）
	if err := s.syncer.SyncDepartment(ctx, dept, snap.DeptMap); err != nil {
		return fmt.Errorf("sync department %s: %w", a.deptCode, err)
	}
	mapping, err := s.storage.FindTenantByDeptCode(ctx, a.deptCode)
	if err != nil || mapping == nil {
		return fmt.Errorf("tenant mapping for department %s not ready", a.deptCode)
	}
	tenantID := strconv.FormatUint(mapping.TenantID, 10)

	// 2. knowledge 侧开户 + 空间成员（复用既有兼容行为，P09 再隔离）
	isLeader := dept.ThirdManagerID != "" && dept.ThirdManagerID == a.user.ThirdUserID
	knowledgeUserID, err := s.syncer.SyncUserAndMemberships(ctx, a.user, &dept, isLeader)
	if err != nil {
		return fmt.Errorf("provision user %s: %w", a.subject, err)
	}
	if knowledgeUserID == "" {
		return fmt.Errorf("provision user %s: empty knowledge user id", a.subject)
	}

	// 3. 占用检查：knowledge_user_id 被其他 active 主体占用 → 冲突挂起
	occupants, err := s.storage.ListIdentityLinksByKnowledgeUser(ctx, knowledgeUserID)
	if err != nil {
		return err
	}
	for _, l := range occupants {
		if l.Status == linkActive && l.ImmutableSubject != a.subject {
			evidence, _ := json.Marshal(map[string]interface{}{
				"reason":            "knowledge_user_id occupied by another active subject",
				"subject":           a.subject,
				"occupant_subject":  l.ImmutableSubject,
				"knowledge_user_id": knowledgeUserID,
				"run_id":            run.RunID,
				"at":                time.Now().UTC(),
			})
			if cerr := s.markConflict(ctx, a.subject, knowledgeUserID, string(evidence)); cerr != nil {
				return cerr
			}
			return errSubjectConflict
		}
	}

	// 4. identity link upsert（disabled 复用，active 校正 knowledge_user_id）
	now := time.Now()
	link, err := s.storage.FindIdentityLinkBySubject(ctx, identitySource, s.orgID(), a.subject)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := s.storage.InsertIdentityLink(ctx, &model.IdentityLink{
			IdentitySource:   identitySource,
			OrganizationID:   s.orgID(),
			ImmutableSubject: a.subject,
			KnowledgeUserID:  knowledgeUserID,
			Status:           linkActive,
		}); err != nil {
			return err
		}
	case err != nil:
		return err
	case link.Status == linkDisabled:
		if _, err := s.storage.UpdateIdentityLinkTx(s.freshTx(ctx), link.ID, map[string]interface{}{
			"knowledge_user_id": knowledgeUserID,
			"status":            linkActive,
			"updated_at":        now,
		}); err != nil {
			return err
		}
	case link.KnowledgeUserID != knowledgeUserID:
		if _, err := s.storage.UpdateIdentityLinkTx(s.freshTx(ctx), link.ID, map[string]interface{}{
			"knowledge_user_id": knowledgeUserID,
			"updated_at":        now,
		}); err != nil {
			return err
		}
	}

	// 部门和公共空间在同一事务提交，只在授权变化时提升版本。
	tenantIDs := []string{tenantID}
	if root := s.cfg.WeKnora.RootTenantID; root > 0 && strconv.FormatUint(root, 10) != tenantID {
		tenantIDs = append(tenantIDs, strconv.FormatUint(root, 10))
	}
	return s.storage.StateDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var changed []string
		for _, id := range tenantIDs {
			var blocked int64
			if err := tx.Model(&model.SpaceRevocation{}).Where("tenant_id = ?", id).Count(&blocked).Error; err != nil {
				return err
			}
			if blocked > 0 {
				continue
			}
			ent := model.ManagedEntitlement{
				ID: uuid.NewString(), Subject: a.subject, TenantID: id,
				Capability: entitlementCapability, Source: "sync", RuleVersion: &run.RuleVersion,
				GrantedBy: &run.TriggeredBy, GrantedAt: &now, CreatedAt: now, UpdatedAt: now,
			}
			res := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "subject"}, {Name: "tenant_id"}, {Name: "capability"}, {Name: "source"}},
				DoUpdates: clause.AssignmentColumns([]string{"revoked_at", "rule_version", "granted_by", "granted_at", "updated_at"}),
				Where:     clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "managed_entitlements.revoked_at IS NOT NULL"}}},
			}).Create(&ent)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				changed = append(changed, id)
			}
		}
		if len(changed) == 0 {
			return nil
		}
		version, err := storage.BumpAuthzVersionTx(tx, a.subject, now)
		if err != nil {
			return err
		}
		return tx.Model(&model.ManagedEntitlement{}).
			Where("subject = ? AND tenant_id IN ? AND capability = ? AND source = 'sync'", a.subject, changed, entitlementCapability).
			Update("authz_version", version).Error
	})

}

// markConflict 把主体的 identity link 落为 conflicted 并记录证据（无则新建行）。
func (s *Service) markConflict(ctx context.Context, subject, knowledgeUserID, evidence string) error {
	now := time.Now()
	existing, err := s.storage.FindIdentityLinkBySubject(ctx, identitySource, s.orgID(), subject)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.storage.InsertIdentityLink(ctx, &model.IdentityLink{
			IdentitySource:   identitySource,
			OrganizationID:   s.orgID(),
			ImmutableSubject: subject,
			KnowledgeUserID:  knowledgeUserID,
			Status:           linkConflicted,
			ConflictEvidence: &evidence,
		})
	}
	if err != nil {
		return err
	}
	_, err = s.storage.UpdateIdentityLinkTx(s.freshTx(ctx), existing.ID, map[string]interface{}{
		"status":            linkConflicted,
		"knowledge_user_id": knowledgeUserID,
		"conflict_evidence": evidence,
		"updated_at":        now,
	})
	return err
}

// 调岗撤销旧空间的同步授权。离职时同时撤销全部授权、身份关联和会话；
// 提交后撤销上游 token 并停用 knowledge 用户。
// 调岗收回不动登录态。撤销器/停用失败 → 步骤 failed 可重试（持久化已生效，重试幂等）。
func (s *Service) revoke(ctx context.Context, a action) error {
	now := time.Now()
	if err := s.storage.StateDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows int64
		if a.depart {
			entRows, err := storage.RevokeManagedEntitlementsBySubjectTx(tx, a.subject, now)
			if err != nil {
				return err
			}
			var links []model.IdentityLink
			if err := tx.Where("immutable_subject = ?", a.subject).Find(&links).Error; err != nil {
				return err
			}
			linkRows, err := storage.DisableIdentityLinksBySubjectTx(tx, a.subject, now)
			if err != nil {
				return err
			}
			linkIDs := make([]string, 0, len(links))
			for _, link := range links {
				linkIDs = append(linkIDs, link.ID)
			}
			if len(linkIDs) > 0 {
				if _, err := storage.RevokeSessionsByIdentityLinkIDsTx(tx, linkIDs, "revoke", now); err != nil {
					return err
				}
			}
			rows = entRows + linkRows
		} else {
			res := tx.Model(&model.ManagedEntitlement{}).
				Where("subject = ? AND tenant_id = ? AND source = 'sync' AND revoked_at IS NULL", a.subject, a.tenantID).
				Updates(map[string]interface{}{"revoked_at": now, "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			rows = res.RowsAffected
		}
		if rows > 0 {
			_, err := storage.BumpAuthzVersionTx(tx, a.subject, now)
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if !a.depart {
		return nil
	}
	if _, err := s.revoker.RevokeTokens(ctx, a.knowledgeUserID); err != nil {
		return fmt.Errorf("revoke upstream tokens: %w", err)
	}
	if _, err := s.storage.SetUserActive(ctx, a.knowledgeUserID, false); err != nil {
		return fmt.Errorf("deactivate knowledge user: %w", err)
	}
	return nil
}

// reconcile 对账：核对 applied==planned（逐行动作步骤，排除 skipped），
// 写 run 完成态（succeeded / partial_failed）与 finished_at 并释放锁。
func (s *Service) reconcile(ctx context.Context, run *model.SyncRun) error {
	steps, err := s.storage.ListSyncSteps(ctx, run.RunID)
	if err != nil {
		return err
	}
	maxSeq := 0
	reconcileSeq := 0
	for _, st := range steps {
		if st.StepSeq > maxSeq {
			maxSeq = st.StepSeq
		}
		if st.StepType == StepReconcile {
			reconcileSeq = st.StepSeq
		}
	}
	if reconcileSeq == 0 {
		maxSeq++
		reconcileSeq = maxSeq
		if err := s.storage.InsertSyncStepTx(s.freshTx(ctx), &model.SyncStep{
			RunID: run.RunID, StepSeq: reconcileSeq, StepType: StepReconcile,
			Status: "pending", Retryable: false,
		}); err != nil {
			return err
		}
	}

	planned, applied, err := s.storage.CountActionSteps(ctx, run.RunID)
	if err != nil {
		return err
	}
	status := StatusSucceeded
	if planned != applied {
		status = StatusPartialFailed
	}
	now := time.Now()
	if _, err := s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, reconcileSeq, map[string]interface{}{
		"status": "applied", "updated_at": now,
	}); err != nil {
		return err
	}
	_, err = s.storage.UpdateSyncRunTx(s.freshTx(ctx), run.RunID, map[string]interface{}{
		"status": status, "finished_at": now, "lock_until": nil, "updated_at": now,
	})
	if err == nil {
		run.Status = status
	}
	return err
}

func logf(format string, args ...interface{}) {
	log.Printf("[SyncService] "+format, args...)
}
