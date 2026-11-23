package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

type appRuntimeCommand struct {
	Kind         string            `json:"kind"`
	BaseVersion  int               `json:"base_version"`
	BaseRevision string            `json:"base_config_revision"`
	Changes      map[string]string `json:"changes"`
	OperationID  uuid.UUID         `json:"operation_id"`
}

func editableRuntimeItem(item model.AppConfigItem) bool {
	return appEnvName.MatchString(item.Key) && !strings.EqualFold(item.Key, "PORT") &&
		item.ResourceID == "" && !item.PreserveOnUpgrade && !connectionIdentityKey.MatchString(item.Key)
}

func editRuntimeConfig(current []model.AppConfigItem, changes map[string]string) ([]model.AppConfigItem, map[string]string, error) {
	next := slices.Clone(current)
	updates := make(map[string]string)
	for key, value := range changes {
		i := slices.IndexFunc(next, func(item model.AppConfigItem) bool { return item.Key == key })
		if i < 0 || !editableRuntimeItem(next[i]) {
			return nil, nil, fmt.Errorf("配置 %s 需要通过更新向导修改", key)
		}
		if strings.ContainsRune(value, 0) || (next[i].Required && strings.TrimSpace(value) == "") {
			return nil, nil, fmt.Errorf("请为配置 %s 填写有效值", key)
		}
		if next[i].Value != value {
			next[i].Value = value
			next[i].ValueSource = "manual"
			next[i].HasValue = value != ""
			updates[key] = value
		}
	}
	return next, updates, nil
}

func runtimeConfigItems(raw string) ([]model.AppConfigItem, error) {
	var configs []model.AppConfigItem
	if raw == "" {
		raw = "[]"
	}
	err := json.Unmarshal([]byte(raw), &configs)
	return configs, err
}

func (h *Handler) runtimeManagementApp(c *gin.Context) (*model.DeployedApp, *model.AppDeploymentVersion, bool) {
	app, err := h.storage.GetAppBySlug(c.Request.Context(), c.Param("slug"))
	if err != nil {
		writeErrorEnvelope(c, http.StatusNotFound, "APP_NOT_FOUND", "应用不存在")
		return nil, nil, false
	}
	if app.CreatedBy == "" || app.CreatedBy != c.GetString(appOwnerKey) {
		writeErrorEnvelope(c, http.StatusForbidden, "APP_FORBIDDEN", "只有应用创建人可以操作")
		return nil, nil, false
	}
	version, err := h.storage.GetActiveAppVersion(c.Request.Context(), app.Slug)
	if err != nil || version.Version != app.CurrentVersion {
		writeErrorEnvelope(c, http.StatusConflict, "APP_RELEASE_UNAVAILABLE", "当前没有可管理的应用版本")
		return nil, nil, false
	}
	return app, version, true
}

func (h *Handler) runtimeEditableKeys(ctx context.Context, version *model.AppDeploymentVersion, items []model.AppConfigItem) ([]string, error) {
	var mounts []model.AppStorageMount
	var ids []string
	if version.StorageMountsJSON != "" && json.Unmarshal([]byte(version.StorageMountsJSON), &mounts) != nil ||
		version.ResourceIDsJSON != "" && json.Unmarshal([]byte(version.ResourceIDsJSON), &ids) != nil {
		return nil, errors.New("无法读取当前存储和资源配置")
	}
	resources := h.loadEffectiveOnlineResources(ctx)
	editable := make([]string, 0, len(items))
	for _, item := range items {
		if !editableRuntimeItem(item) || slices.ContainsFunc(mounts, func(m model.AppStorageMount) bool {
			return m.ContainerPath != "" && (item.Value == m.ContainerPath || strings.HasPrefix(item.Value, strings.TrimRight(m.ContainerPath, "/")+"/"))
		}) {
			continue
		}
		resourceKey := false
		for _, resource := range resources {
			if slices.Contains(ids, resource.ID) {
				_, exists := resource.EnvVars[item.Key]
				resourceKey = resourceKey || exists
			}
		}
		if !resourceKey {
			editable = append(editable, item.Key)
		}
	}
	return editable, nil
}

func (h *Handler) GetAppRuntimeManagement(c *gin.Context) {
	app, version, ok := h.runtimeManagementApp(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	rows := h.storage.DeployDB().WithContext(ctx).Where("app_id = ? AND release_id = ?", app.ID, version.ID)
	var latest model.AppRuntimeOperation
	err := rows.Session(&gorm.Session{}).Where("kind <> ?", "baseline").Order("created_at DESC").First(&latest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_STATE_UNAVAILABLE", "暂时无法读取应用操作状态")
		return
	}
	var history []model.AppRuntimeOperation
	if err := rows.Session(&gorm.Session{}).Where("config_number > 0").Order("config_number DESC").Limit(50).Find(&history).Error; err != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_STATE_UNAVAILABLE", "暂时无法读取配置历史")
		return
	}
	configs, err := runtimeConfigItems(version.AppConfigJSON)
	var keys []string
	if err == nil {
		configs, err = h.openAppConfig(configs)
	}
	if err == nil {
		keys, err = h.runtimeEditableKeys(ctx, version, configs)
	}
	if err != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_CONFIG_UNAVAILABLE", "暂时无法读取运行配置")
		return
	}
	number := 1
	for _, item := range history {
		if item.State == "succeeded" && item.ConfigRevision == version.ConfigRevision {
			number = item.ConfigNumber
			break
		}
	}
	var operation *model.AppRuntimeOperation
	if latest.ID != uuid.Nil {
		operation = &latest
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"operation": operation, "revisions": history, "config_revision": version.ConfigRevision,
		"config_number": number, "editable_keys": keys})
}

func (h *Handler) StartAppRuntimeManagement(c *gin.Context) {
	ctx := c.Request.Context()
	var input appRuntimeCommand
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
	if err := c.ShouldBindJSON(&input); err != nil || len(input.Changes) > 256 {
		writeErrorEnvelope(c, http.StatusBadRequest, "RUNTIME_REQUEST_INVALID", "运行配置请求无效")
		return
	}
	slug := c.Param("slug")
	if !h.tryLockDeploySlug(slug) {
		writeErrorEnvelope(c, http.StatusConflict, "APP_BUSY", "应用正在执行部署或管理操作")
		return
	}
	defer h.unlockDeploySlug(slug)
	h.storageResourcesMu.RLock()
	defer h.storageResourcesMu.RUnlock()
	app, version, ok := h.runtimeManagementApp(c)
	if !ok || !h.allowDeliveryChange(c, slug) || !h.allowChangeAfterMigration(c, slug) {
		return
	}
	if h.caproverClient == nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_SERVICE_UNAVAILABLE", "暂时无法连接部署服务")
		return
	}
	db := h.storage.DeployDB().WithContext(ctx)
	rows := db.Where("app_id = ? AND release_id = ?", app.ID, version.ID)
	if input.Kind == "recheck" {
		var operation model.AppRuntimeOperation
		err := rows.Where("id = ?", input.OperationID).First(&operation).Error
		if err != nil || operation.BaseRevision != version.ConfigRevision {
			writeErrorEnvelope(c, http.StatusConflict, "RUNTIME_OPERATION_CHANGED", "此操作已更新，请刷新后重试")
			return
		}
		state := "checking"
		switch operation.State {
		case "recovery_unknown":
			state = "restoring"
		case "unknown":
			if !operation.Submitted {
				state = "pending"
			}
		case "failed":
			if operation.Kind != "restart" || !operation.Submitted {
				writeErrorEnvelope(c, http.StatusConflict, "RUNTIME_OPERATION_FINISHED", "此操作已结束")
				return
			}
		default:
			writeErrorEnvelope(c, http.StatusConflict, "RUNTIME_OPERATION_FINISHED", "当前操作无法重新检查")
			return
		}
		result := db.Model(&operation).Where("state = ? AND (lease_until IS NULL OR lease_until < ?)", operation.State, time.Now().UTC()).
			Updates(map[string]any{"state": state, "message": "正在重新检查本次操作"})
		if result.Error != nil || result.RowsAffected != 1 {
			writeErrorEnvelope(c, http.StatusConflict, "APP_BUSY", "操作仍在更新，请稍后重新检查")
			return
		}
		operation.State, operation.Message = state, "正在重新检查本次操作"
		c.JSON(http.StatusAccepted, operation)
		return
	}
	if !h.allowRuntimeChange(c, slug) {
		return
	}
	operation := model.AppRuntimeOperation{
		ID: uuid.New(), RestorationID: uuid.New(), AppID: app.ID, Slug: slug, ReleaseID: version.ID,
		Version: version.Version, Kind: input.Kind, State: "pending", BaseRevision: version.ConfigRevision,
		Message: "操作已提交，正在等待处理", CreatedBy: c.GetString(appOwnerKey),
	}
	if input.Kind == "restart" {
		if err := db.Create(&operation).Error; err != nil {
			writeErrorEnvelope(c, http.StatusConflict, "APP_BUSY", "无法提交重启，请刷新应用状态后重试")
			return
		}
		c.JSON(http.StatusAccepted, operation)
		return
	}
	if input.Kind != "config" && input.Kind != "restore" {
		writeErrorEnvelope(c, http.StatusBadRequest, "RUNTIME_REQUEST_INVALID", "未知的应用操作")
		return
	}
	if input.BaseVersion != version.Version || input.BaseRevision != version.ConfigRevision {
		writeErrorEnvelope(c, http.StatusConflict, "RUNTIME_CONFIG_CHANGED", "运行配置已经更新，请刷新后再保存")
		return
	}
	current, err := runtimeConfigItems(version.AppConfigJSON)
	if err == nil {
		current, err = h.openAppConfig(current)
	}
	if err != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_CONFIG_UNAVAILABLE", "无法读取当前运行配置")
		return
	}
	editable, err := h.runtimeEditableKeys(ctx, version, current)
	if err != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_CONFIG_UNAVAILABLE", err.Error())
		return
	}
	if input.Kind == "restore" {
		var target model.AppRuntimeOperation
		if err := rows.Session(&gorm.Session{}).Where("id = ? AND config_number > 0 AND state = ?", input.OperationID, "succeeded").First(&target).Error; err != nil {
			writeErrorEnvelope(c, http.StatusConflict, "RUNTIME_REVISION_UNAVAILABLE", "该配置版本已不可恢复")
			return
		}
		configs, err := runtimeConfigItems(target.ConfigJSON)
		if err == nil {
			configs, err = h.openAppConfig(configs)
		}
		if err != nil {
			writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_CONFIG_UNAVAILABLE", "无法读取历史配置")
			return
		}
		input.Changes = make(map[string]string)
		for _, item := range configs {
			if slices.Contains(editable, item.Key) {
				input.Changes[item.Key] = item.Value
			}
		}
	}
	for key := range input.Changes {
		if !slices.Contains(editable, key) {
			writeErrorEnvelope(c, http.StatusBadRequest, "RUNTIME_CONFIG_PROTECTED", fmt.Sprintf("配置 %s 需要通过更新向导修改", key))
			return
		}
	}
	next, changes, err := editRuntimeConfig(current, input.Changes)
	if err != nil || len(changes) == 0 {
		message := "运行配置没有变化"
		if err != nil {
			message = err.Error()
		}
		writeErrorEnvelope(c, http.StatusBadRequest, "RUNTIME_CONFIG_INVALID", message)
		return
	}
	next, err = h.protectAppConfig(next, true)
	if err != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_CONFIG_UNAVAILABLE", "无法保存运行配置")
		return
	}
	data, _ := json.Marshal(next)
	operation.ConfigJSON = string(data)
	operation.ChangedKeys = strings.Join(slices.Sorted(maps.Keys(changes)), ", ")
	operation.ConfigRevision = operation.ID.String()
	if version.DeploymentSnapshotSHA != "" {
		snapshot := *version
		snapshot.ConfigRevision, snapshot.AppConfigJSON, snapshot.CreatedBy = operation.ConfigRevision, operation.ConfigJSON, operation.CreatedBy
		prefix := strings.TrimSuffix(deploy.BuildVersionObjectKey(slug, version.Version, ""), "/")
		if err := h.archiveConfirmedConfig(ctx, h.resolveArtifactStore(ctx), &snapshot, prefix, h.loadEffectiveOnlineResources(ctx)); err != nil {
			writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_CONFIG_UNAVAILABLE", "无法归档运行配置")
			return
		}
		operation.SnapshotSHA = snapshot.DeploymentSnapshotSHA
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		var number int
		query := tx.Model(&model.AppRuntimeOperation{}).Where("app_id = ? AND release_id = ?", app.ID, version.ID)
		if err := query.Select("COALESCE(MAX(config_number), 0)").Scan(&number).Error; err != nil {
			return err
		}
		var baselineCount int64
		if err := query.Session(&gorm.Session{}).Where("config_revision = ? AND config_number > 0 AND state = ?", version.ConfigRevision, "succeeded").Count(&baselineCount).Error; err != nil {
			return err
		}
		if baselineCount == 0 {
			number++
			baseline := model.AppRuntimeOperation{ID: uuid.New(), AppID: app.ID, Slug: slug, ReleaseID: version.ID,
				Version: version.Version, Kind: "baseline", State: "succeeded", ConfigNumber: number, ConfigRevision: version.ConfigRevision,
				ConfigJSON: version.AppConfigJSON, CreatedBy: version.CreatedBy}
			if err := tx.Create(&baseline).Error; err != nil {
				return err
			}
		}
		operation.ConfigNumber = number + 1
		return tx.Create(&operation).Error
	}); err != nil {
		writeErrorEnvelope(c, http.StatusConflict, "APP_BUSY", "无法提交配置，请刷新应用状态后重试")
		return
	}
	c.JSON(http.StatusAccepted, operation)
}

func (h *Handler) allowRuntimeChange(c *gin.Context, slug string) bool {
	var count int64
	err := h.storage.DeployDB().WithContext(c.Request.Context()).Model(&model.AppRuntimeOperation{}).
		Where("slug = ? AND state IN ? AND app_id IN (SELECT id FROM deployed_apps WHERE slug = ?)", slug, model.BlockingAppRuntimeStates, slug).Count(&count).Error
	if err != nil {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "RUNTIME_STATE_UNAVAILABLE", "暂时无法核对应用操作状态")
		return false
	}
	if count > 0 {
		writeErrorEnvelope(c, http.StatusConflict, "APP_RUNTIME_PENDING", "应用有未完成的重启或配置操作，请先查看操作状态")
		return false
	}
	return true
}
