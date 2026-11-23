package handler

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"weknora-tokendance/pkg/model"
)

func (h *Handler) pollAppRuntimeOperations(ctx context.Context) {
	var operations []model.AppRuntimeOperation
	err := h.storage.DeployDB().WithContext(ctx).
		Where("state IN ? AND (lease_until IS NULL OR lease_until < ?)", []string{"pending", "restarting", "checking", "restoring"}, time.Now().UTC()).
		Order("updated_at").Limit(8).Find(&operations).Error
	if err != nil {
		log.Printf("[AppRuntime] could not read operations: %v", err)
		return
	}
	for _, operation := range operations {
		go h.runAppRuntimeOperation(ctx, operation)
	}
}

func (h *Handler) runAppRuntimeOperation(parent context.Context, operation model.AppRuntimeOperation) {
	if !h.tryLockDeploySlug(operation.Slug) {
		return
	}
	defer h.unlockDeploySlug(operation.Slug)
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	now := time.Now().UTC()
	lease := now.Add(5 * time.Minute).Truncate(time.Microsecond)
	row := h.storage.DeployDB().Model(&model.AppRuntimeOperation{}).Where("id = ?", operation.ID)
	claim := row.Session(&gorm.Session{}).WithContext(ctx).
		Where("state = ? AND (lease_until IS NULL OR lease_until < ?)", operation.State, now).Update("lease_until", lease)
	if claim.Error != nil || claim.RowsAffected != 1 {
		return
	}
	owned := row.Where("lease_until = ?", lease)
	defer func() {
		finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		owned.Session(&gorm.Session{}).WithContext(finish).Update("lease_until", nil)
	}()
	update := func(state, message string, submitted bool) bool {
		result := owned.Session(&gorm.Session{}).WithContext(ctx).Updates(map[string]any{"state": state, "message": message, "submitted": submitted})
		return result.Error == nil && result.RowsAffected == 1
	}

	app, err := h.storage.GetAppBySlug(ctx, operation.Slug)
	if err != nil {
		return
	}
	version, err := h.storage.GetActiveAppVersion(ctx, operation.Slug)
	if err != nil {
		return
	}
	if app.ID != operation.AppID || version.ID != operation.ReleaseID || version.ConfigRevision != operation.BaseRevision {
		update("failed", "应用版本或配置已变化，本次操作已结束", operation.Submitted)
		return
	}
	changes := map[string]string{}
	previous := map[string]string{}
	if operation.Kind != "restart" {
		oldItems, oldErr := runtimeConfigItems(version.AppConfigJSON)
		newItems, newErr := runtimeConfigItems(operation.ConfigJSON)
		if oldErr != nil || newErr != nil {
			update("unknown", "暂时无法读取配置，操作状态待确认", operation.Submitted)
			return
		}
		oldItems, oldErr = h.openAppConfig(oldItems)
		newItems, newErr = h.openAppConfig(newItems)
		if oldErr != nil || newErr != nil {
			update("unknown", "暂时无法读取配置，操作状态待确认", operation.Submitted)
			return
		}
		for i, item := range oldItems {
			if i >= len(newItems) || newItems[i].Key != item.Key {
				update("unknown", "配置记录不一致，操作状态待确认", operation.Submitted)
				return
			}
			if newItems[i].Value != item.Value {
				previous[item.Key], changes[item.Key] = item.Value, newItems[i].Value
			}
		}
	}

	if operation.State != "restoring" {
		if operation.State == "pending" || operation.State == "restarting" {
			if !update("restarting", "正在重启应用", operation.Submitted) {
				return
			}
			if err := h.submitRuntimeRestart(ctx, operation, operation.ID.String(), changes); err != nil {
				update("unknown", "暂时无法确认重启请求是否已生效，请重新检查", false)
				return
			}
			if !update("checking", "正在等待新实例启动并检查应用地址", true) {
				return
			}
		}
		ready, observed := h.waitRuntimeRestart(ctx, operation, operation.ID.String(), app.AppURL)
		if ready {
			if operation.Kind == "restart" {
				update("succeeded", "应用已恢复访问", true)
				return
			}
			err := h.storage.DeployDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				active := tx.Model(&model.AppDeploymentVersion{}).
					Where("id = ? AND is_active = ? AND config_revision = ?", version.ID, true, operation.BaseRevision).
					Updates(map[string]any{"config_revision": operation.ConfigRevision, "app_config_json": operation.ConfigJSON,
						"deployment_snapshot_sha": operation.SnapshotSHA})
				if active.Error != nil {
					return active.Error
				}
				if active.RowsAffected != 1 {
					return gorm.ErrRecordNotFound
				}
				if err := tx.Model(&model.DeployedApp{}).Where("id = ?", app.ID).Update("app_config_json", operation.ConfigJSON).Error; err != nil {
					return err
				}
				return tx.Model(&model.AppRuntimeOperation{}).Where("id = ? AND lease_until = ?", operation.ID, lease).
					Updates(map[string]any{"state": "succeeded", "message": "配置已生效，应用已恢复访问", "submitted": true}).Error
			})
			if err != nil {
				log.Printf("[AppRuntime][%s] could not confirm active configuration: %v", operation.Slug, err)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !observed {
			update("unknown", "暂时无法确认应用是否启动，请重新检查", true)
			return
		}
		if operation.Kind == "restart" {
			update("failed", "等待应用恢复访问超时，请查看日志或重新检查", true)
			return
		}
		if !update("restoring", "新配置未能启动应用，正在恢复上一次生效的配置", true) {
			return
		}
	}
	if err := h.submitRuntimeRestart(ctx, operation, operation.RestorationID.String(), previous); err != nil {
		update("recovery_unknown", "暂时无法确认上一份配置是否恢复，请继续检查", true)
		return
	}
	ready, _ := h.waitRuntimeRestart(ctx, operation, operation.RestorationID.String(), app.AppURL)
	if ready {
		update("restored", "新配置未生效，已恢复上一次配置，应用可以访问", true)
	} else if ctx.Err() == nil {
		update("recovery_unknown", "恢复后仍无法确认应用可以访问，请查看日志并重新检查", true)
	}
}

func (h *Handler) submitRuntimeRestart(ctx context.Context, operation model.AppRuntimeOperation, id string, changes map[string]string) error {
	request, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return h.caproverClient.RestartApplication(request, operation.Slug, id, changes)
}

func (h *Handler) waitRuntimeRestart(parent context.Context, operation model.AppRuntimeOperation, id, appURL string) (bool, bool) {
	wait := h.caproverClient.AppReadyTimeout
	if wait <= 0 || wait > 100*time.Second {
		wait = 90 * time.Second
	}
	interval := h.caproverClient.ProbeInterval
	if interval <= 0 {
		interval = time.Second
	}
	ctx, cancel := context.WithTimeout(parent, wait)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	observed := false
	for ctx.Err() == nil {
		running, _, available := h.caproverClient.ObserveRuntimeRestart(ctx, operation.Slug, id)
		if ctx.Err() == nil {
			observed = available
		}
		if running {
			ready, _, err := h.caproverClient.ProbeAppEndpoint(ctx, appURL)
			if err == nil && ready {
				running, _, _ = h.caproverClient.ObserveRuntimeRestart(ctx, operation.Slug, id)
				if running {
					return true, true
				}
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
	}
	return false, observed
}
