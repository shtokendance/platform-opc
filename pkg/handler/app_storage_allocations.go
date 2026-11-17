package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

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

func (h *Handler) configureDirectoryVolume(ctx context.Context, req, old *model.DeployResource) error {
	req.Host = strings.TrimSpace(req.Host)
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
			if mount.ResourceID == "" && enterprise {
				for _, resource := range resources {
					if deploy.IsDirectoryVolume(resource.Type) && h.isEnterpriseResourceAllowed(slug, resource.ID) &&
						(mount.Kind != "sqlite" || resource.Type == deploy.LocalDirectoryVolume) {
						mount.ResourceID = resource.ID
						break
					}
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

func (h *Handler) applicationEnvironment(ctx context.Context, slug string, resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem) ([]model.AppConfigItem, error) {
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
		for i := range env {
			if newValue, ok := scoped[env[i].Key]; ok {
				env[i].Value = newValue
				delete(scoped, env[i].Key)
			}
		}
		for _, key := range objectStorageConfigKeys {
			if value := scoped[key]; value != "" {
				env = append(env, model.AppConfigItem{Key: key, Value: value})
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
	if err := json.Unmarshal([]byte(app.ResourceIDsJSON), &previous); err != nil {
		return err
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
