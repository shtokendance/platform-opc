package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

const volumeResourceType = "docker-volume"

var (
	swarmStorageNodeID = regexp.MustCompile(`^[a-z0-9]{25}$`)
	storageResourceID  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	directoryRoot      = regexp.MustCompile(`^(/[a-zA-Z0-9_.-]+)+$`)
	ossStorageLocation = regexp.MustCompile(`^oss://[a-z0-9][a-z0-9-]{1,61}[a-z0-9](/[a-zA-Z0-9_.-]+)*$`)
)

func isVolumeResource(resourceType string) bool {
	return resourceType == volumeResourceType || deploy.IsDirectoryVolume(resourceType)
}

func isManagedStorage(resourceType string) bool {
	return isVolumeResource(resourceType) || resourceType == objectStorageResourceType
}

var nonVolumeIDChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func suggestDirectoryResourceID(hostPath string, used map[string]bool) string {
	trimmed := strings.Trim(strings.TrimSpace(hostPath), "/")
	base := nonVolumeIDChars.ReplaceAllString(trimmed, "-")
	base = strings.Trim(base, "-.")
	if base == "" || strings.HasPrefix(strings.ToLower(base), "db-") || !storageResourceID.MatchString(base) {
		base = "vol-storage"
	}
	if len(base) > 56 {
		base = strings.TrimRight(base[:56], "-.")
	}
	candidate := base
	for i := 2; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return candidate
}

func normalizeDirectoryHost(hostPath string) string {
	cleaned := strings.TrimSpace(hostPath)
	if cleaned == "/" {
		return "/"
	}
	return strings.TrimRight(cleaned, "/")
}

// syncLocalDirectoryVolumeRoots auto-registers newly mounted Deploy local-directory roots
// into deploy_resources without overwriting existing registrations or default selections.
func (h *Handler) syncLocalDirectoryVolumeRoots(ctx context.Context, roots []deploy.SwarmVolumeRoot) bool {
	if h.storage == nil || len(roots) == 0 {
		return false
	}
	h.storageResourcesMu.Lock()
	defer h.storageResourcesMu.Unlock()

	existing, err := h.storage.ListDeployResources(ctx, false)
	if err != nil {
		return false
	}
	existingByHost := make(map[string]bool, len(existing))
	usedIDs := make(map[string]bool, len(existing))
	hasDefault := false
	for _, r := range existing {
		usedIDs[r.ID] = true
		if deploy.IsDirectoryVolume(r.Type) {
			existingByHost[normalizeDirectoryHost(r.Host)] = true
			if r.Enabled && r.IsDefault {
				hasDefault = true
			}
		}
	}

	changed := false
	for _, root := range roots {
		if !root.Available || root.Backend != deploy.LocalDirectoryVolume {
			continue
		}
		cleanHost := normalizeDirectoryHost(root.Root)
		node := strings.TrimSpace(root.NodeID)
		if cleanHost == "" || cleanHost == "/" || !directoryRoot.MatchString(cleanHost) || !swarmStorageNodeID.MatchString(node) {
			continue
		}
		if existingByHost[cleanHost] {
			continue
		}
		id := suggestDirectoryResourceID(cleanHost, usedIDs)
		usedIDs[id] = true
		existingByHost[cleanHost] = true
		isDefault := !hasDefault
		name := fmt.Sprintf("本地存储 (%s)", cleanHost)
		if isDefault {
			name = fmt.Sprintf("默认存储 (%s)", cleanHost)
		}
		res := &model.DeployResource{
			ID:          id,
			Name:        name,
			Type:        deploy.LocalDirectoryVolume,
			IsDefault:   isDefault,
			Host:        cleanHost,
			EnvVars:     map[string]string{"OPC_STORAGE_NODE_ID": node},
			Description: "由 Deploy 容器已挂载卷自动发现",
			Enabled:     true,
		}
		if err := h.storage.UpsertDeployResource(ctx, res); err == nil {
			changed = true
			if isDefault {
				hasDefault = true
			}
		}
	}
	return changed
}

func (h *Handler) ensureLocalDirectoryVolumesFromDeploy(ctx context.Context) bool {
	if h.storage == nil || h.caproverClient == nil {
		return false
	}
	existing, err := h.storage.ListDeployResources(ctx, false)
	if err == nil {
		for _, r := range existing {
			if deploy.IsDirectoryVolume(r.Type) {
				return false
			}
		}
	}
	syncCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	snapshot, err := h.caproverClient.ApplicationStorage(syncCtx)
	if err != nil || len(snapshot.VolumeRoots) == 0 {
		return false
	}
	return h.syncLocalDirectoryVolumeRoots(ctx, snapshot.VolumeRoots)
}

func (h *Handler) configureDirectoryVolume(ctx context.Context, req, old *model.DeployResource) error {
	req.Host = strings.TrimSpace(req.Host)
	if req.Host != "/" {
		req.Host = strings.TrimRight(req.Host, "/")
	}
	req.DatabaseName = strings.TrimRight(strings.TrimSpace(req.DatabaseName), "/")
	node := strings.TrimSpace(req.EnvVars["OPC_STORAGE_NODE_ID"])
	if len(req.Host) > 255 || !directoryRoot.MatchString(req.Host) || path.Clean(req.Host) != req.Host || !swarmStorageNodeID.MatchString(node) {
		return fmt.Errorf("请填写有效的 Swarm 节点 ID 和存储根目录绝对路径")
	}
	if req.Type == deploy.AliyunOSSVolume {
		ossPath := strings.TrimPrefix(req.DatabaseName, "oss://")
		if len(req.DatabaseName) > 128 || !ossStorageLocation.MatchString(req.DatabaseName) || path.Clean(ossPath) != ossPath {
			return fmt.Errorf("请填写有效的阿里云 OSS 桶和前缀，如 oss://company-data/opc")
		}
	} else {
		req.DatabaseName = ""
	}
	if old != nil && (old.Host != req.Host || old.DatabaseName != req.DatabaseName || old.EnvVars["OPC_STORAGE_NODE_ID"] != node) {
		return fmt.Errorf("卷的存储位置不可修改，请新增资源并迁移已有数据")
	}
	if req.Enabled {
		if h.caproverClient == nil {
			return fmt.Errorf("Deploy 未配置")
		}
		if err := h.caproverClient.PrepareVolumePath(ctx, req.Type, node, req.Host, "", false); err != nil {
			return err
		}
	}
	req.EnvVars = map[string]string{"OPC_STORAGE_NODE_ID": node}
	req.Username, req.Password, req.Port = "", "", 0
	return nil
}

// Resolve requested resources or the enterprise default for new mounts.
// Persisted mounts continue using their assigned path after defaults change.
func (h *Handler) resolveVolumeResources(ctx context.Context, slug string, appID uuid.UUID, enterprise bool, mounts, previous []model.AppStorageMount, resources []config.OnlineResourceConfig) error {
	oldByPath := make(map[string]model.AppStorageMount, len(previous))
	for _, old := range previous {
		oldByPath[old.ContainerPath] = old
	}
	var nodeID string
	for i := range mounts {
		mount := &mounts[i]
		mount.StorageNodeID = ""
		mount.StorageLocation = ""
		if old, ok := oldByPath[mount.ContainerPath]; ok {
			if mount.ResourceID != "" && mount.ResourceID != old.ResourceID {
				return fmt.Errorf("changing persistent storage for %s requires explicit data migration", mount.ContainerPath)
			}
			mount.ResourceID = old.ResourceID
			mount.StorageNodeID = old.StorageNodeID
		} else {
			if mount.ResourceID == "" {
				var firstMatch string
				for _, resource := range resources {
					if !deploy.IsDirectoryVolume(resource.Type) {
						continue
					}
					if enterprise && !h.isEnterpriseResourceAllowed(slug, resource.ID) {
						continue
					}
					if mount.Kind == "sqlite" && resource.Type != deploy.LocalDirectoryVolume {
						continue
					}
					if resource.IsDefault {
						mount.ResourceID = resource.ID
						break
					}
					if firstMatch == "" {
						firstMatch = resource.ID
					}
				}
				if mount.ResourceID == "" && (enterprise || firstMatch != "") {
					mount.ResourceID = firstMatch
				}
			}
			if mount.ResourceID == "" {
				continue
			}
			if enterprise && !h.isEnterpriseResourceAllowed(slug, mount.ResourceID) {
				return fmt.Errorf("storage resource is not available for %s", mount.ContainerPath)
			}
			var selected *config.OnlineResourceConfig
			for j := range resources {
				if resources[j].ID == mount.ResourceID && isVolumeResource(resources[j].Type) {
					selected = &resources[j]
					break
				}
			}
			if selected == nil || h.caproverClient == nil {
				return fmt.Errorf("configured volume resource %q is unavailable", mount.ResourceID)
			}
			target := strings.TrimSpace(selected.EnvVars["OPC_STORAGE_NODE_ID"])
			if !swarmStorageNodeID.MatchString(target) {
				return fmt.Errorf("volume resource %q has an invalid Swarm node ID", mount.ResourceID)
			}
			if deploy.IsDirectoryVolume(selected.Type) {
				resource, err := h.storage.GetDeployResourceByID(ctx, selected.ID)
				if err != nil || !resource.Enabled || resource.Type != selected.Type || appID == uuid.Nil {
					return fmt.Errorf("configured volume resource %q is unavailable", mount.ResourceID)
				}
				if selected.Type == deploy.AliyunOSSVolume && mount.Kind == "sqlite" {
					return fmt.Errorf("SQLite 目录 %s 必须使用本地磁盘卷", mount.ContainerPath)
				}
				directory := path.Join(slug+"-"+appID.String(), mount.Name)
				mount.HostPath = path.Join(resource.Host, directory)
				mount.StorageBackend = selected.Type
				if selected.Type == deploy.AliyunOSSVolume {
					mount.StorageLocation = resource.DatabaseName + "/" + directory
				}
			} else if nodeID == "" {
				snapshot, err := h.caproverClient.ApplicationStorage(ctx)
				if err != nil || snapshot.Disk.Nodes[0].NodeID != target {
					return fmt.Errorf("cannot verify the Swarm node for volume resource %q", mount.ResourceID)
				}
			}
			mount.StorageNodeID = target
			// A recreated slug must never inherit the volume of a deleted app.
			if selected.Type == volumeResourceType {
				mount.VolumeName += "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			}
		}
		if mount.StorageNodeID != "" {
			if nodeID != "" && nodeID != mount.StorageNodeID {
				return fmt.Errorf("application volume resources must be on the same Swarm node")
			}
			nodeID = mount.StorageNodeID
		}
	}
	return nil
}

// Managed object storage exposes the shape of the application configuration,
// never the provisioning identity stored on the resource.
const objectStorageResourceType = "minio-app-storage"

var objectStorageConfigKeys = []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY", "AWS_ENDPOINT_URL_S3", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "S3_REGION", "AWS_REGION", "AWS_DEFAULT_REGION", "S3_FORCE_PATH_STYLE"}

func managedObjectResourceConfig(resource *model.DeployResource) (deploy.ApplicationBucketConfig, error) {
	cfg := deploy.ApplicationBucketConfig{
		Endpoint: resource.Host, KeyID: resource.Username, Secret: resource.Password, BucketPrefix: resource.DatabaseName,
	}
	return cfg, cfg.Validate()
}

func objectResourcePlaceholders() map[string]string {
	env := make(map[string]string, len(objectStorageConfigKeys))
	for _, key := range objectStorageConfigKeys {
		env[key] = "allocated-at-deploy"
	}
	return env
}

func (h *Handler) applicationEnvironment(ctx context.Context, slug string, resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem, onlyDeclared bool) ([]model.AppConfigItem, error) {
	if onlyDeclared {
		for _, id := range ids {
			if !slices.ContainsFunc(configs, func(item model.AppConfigItem) bool { return item.ResourceID == id }) {
				return nil, fmt.Errorf("resource %s requires an explicit application configuration binding", id)
			}
		}
	}
	if err := validateObjectResourceBinding(resources, ids, configs); err != nil {
		return nil, err
	}
	env, err := mergeAppEnvironment(resources, ids, configs)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		var managed bool
		for _, resource := range resources {
			managed = managed || (resource.ID == id && resource.Type == objectStorageResourceType)
		}
		if !managed {
			continue
		}
		resource, err := h.storage.GetDeployResourceByID(ctx, id)
		if err != nil || resource.Type != objectStorageResourceType || !resource.Enabled {
			return nil, fmt.Errorf("managed object storage resource is unavailable")
		}
		cfg, err := managedObjectResourceConfig(resource)
		if err != nil {
			return nil, err
		}
		// The resource secret remains on Platform. Derive a stable separate
		// identity for this resource/application, then scope it to its bucket.
		app, err := h.storage.GetAppBySlug(ctx, slug)
		if err != nil || app.ID == uuid.Nil {
			return nil, fmt.Errorf("application identity unavailable for object storage allocation")
		}
		scoped, err := deploy.EnsureApplicationBucket(ctx, cfg, id, app.ID.String())
		if err != nil {
			return nil, err
		}
		used := make(map[string]bool)
		for i := range env {
			key := env[i].Key
			if index := slices.IndexFunc(configs, func(item model.AppConfigItem) bool { return item.Key == env[i].Key && item.ResourceID == id }); index >= 0 {
				key = resourceEnvKey(configs[index])
				if scoped[key] == "" {
					return nil, fmt.Errorf("managed object storage has no app-scoped value for %s", key)
				}
			} else if onlyDeclared {
				continue
			}
			if newValue, ok := scoped[key]; ok {
				env[i].Value = newValue
				used[key] = true
			}
		}
		if !onlyDeclared {
			for _, key := range objectStorageConfigKeys {
				if value := scoped[key]; value != "" && !used[key] {
					env = append(env, model.AppConfigItem{Key: key, Value: value})
				}
			}
		}
		break
	}
	return env, nil
}

func validateObjectResourceBinding(resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem) error {
	var selected string
	for _, resource := range resources {
		if resource.Type == objectStorageResourceType && slices.Contains(ids, resource.ID) {
			if selected != "" {
				return fmt.Errorf("only one managed object storage resource may be bound to each application")
			}
			selected = resource.ID
		}
	}
	if selected == "" {
		return nil
	}
	for _, item := range configs {
		if slices.Contains(objectStorageConfigKeys, item.Key) && item.ResourceID != selected {
			return fmt.Errorf("bind %s to the selected object storage resource", item.Key)
		}
	}
	return nil
}

func (h *Handler) appUsesResource(ctx context.Context, slug, id string) bool {
	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		return false
	}
	var ids []string
	return json.Unmarshal([]byte(app.ResourceIDsJSON), &ids) == nil && slices.Contains(ids, id)
}

func (h *Handler) checkObjectStorageChange(ctx context.Context, slug string, next []string) error {
	for _, id := range next {
		if resource, err := h.storage.GetDeployResourceByID(ctx, id); err == nil &&
			resource.Type == objectStorageResourceType && !h.appUsesResource(ctx, slug, id) {
			return fmt.Errorf("MinIO 自动分桶仅保留已有绑定，请为新应用配置持久卷")
		}
	}
	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		return nil
	}
	var previous []string
	if strings.TrimSpace(app.ResourceIDsJSON) != "" {
		if err := json.Unmarshal([]byte(app.ResourceIDsJSON), &previous); err != nil {
			return err
		}
	}
	for _, id := range previous {
		resource, err := h.storage.GetDeployResourceByID(ctx, id)
		if err == nil && resource.Type == objectStorageResourceType && !slices.Contains(next, id) {
			return fmt.Errorf("changing object storage for %s requires explicit data migration", slug)
		}
	}
	return nil
}

func (h *Handler) storageResourceInUse(ctx context.Context, id, resourceType string) (bool, error) {
	var apps []model.DeployedApp
	if err := h.storage.DeployDB().WithContext(ctx).Select("resource_ids_json", "storage_mounts_json").Find(&apps).Error; err != nil {
		return false, err
	}
	var versions []model.AppDeploymentVersion
	if err := h.storage.DeployDB().WithContext(ctx).Select("resource_ids_json", "storage_mounts_json").Find(&versions).Error; err != nil {
		return false, err
	}
	references := make([]string, 0, len(apps)+len(versions))
	for _, app := range apps {
		if isVolumeResource(resourceType) {
			references = append(references, app.StorageMountsJSON)
		} else {
			references = append(references, app.ResourceIDsJSON)
		}
	}
	for _, version := range versions {
		if isVolumeResource(resourceType) {
			references = append(references, version.StorageMountsJSON)
		} else {
			references = append(references, version.ResourceIDsJSON)
		}
	}
	for _, raw := range references {
		if raw == "" {
			continue
		}
		if isVolumeResource(resourceType) {
			var mounts []model.AppStorageMount
			if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
				return false, err
			}
			if slices.ContainsFunc(mounts, func(m model.AppStorageMount) bool { return m.ResourceID == id }) {
				return true, nil
			}
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			return false, err
		}
		if slices.Contains(ids, id) {
			return true, nil
		}
	}
	return false, nil
}

func appObjectStorage(app model.DeployedApp, resources map[string]model.DeployResource) *model.AppObjectStorage {
	var ids []string
	if json.Unmarshal([]byte(app.ResourceIDsJSON), &ids) != nil {
		return nil
	}
	for _, id := range ids {
		if resource, ok := resources[id]; ok {
			return &model.AppObjectStorage{
				ResourceID: id,
				Bucket:     deploy.ApplicationBucketName(resource.DatabaseName, id, app.ID.String()),
			}
		}
	}
	return nil
}

func (h *Handler) managedObjectResources(ctx context.Context) (map[string]model.DeployResource, error) {
	var list []model.DeployResource
	if err := h.storage.DeployDB().WithContext(ctx).
		Select("id", "database_name").Where("type = ?", objectStorageResourceType).Find(&list).Error; err != nil {
		return nil, err
	}
	resources := make(map[string]model.DeployResource, len(list))
	for _, item := range list {
		resources[item.ID] = item
	}
	return resources, nil
}
