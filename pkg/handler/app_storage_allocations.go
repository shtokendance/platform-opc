package handler

import (
	"context"
	"encoding/json"
	"fmt"
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
)

// Resolve only resources explicitly bound by the application. Persisted mounts
// continue using their verified node and volume name, even after defaults change.
func (h *Handler) resolveVolumeResources(ctx context.Context, slug string, enterprise bool, mounts, previous []model.AppStorageMount, resources []config.OnlineResourceConfig) error {
	oldByPath := make(map[string]model.AppStorageMount, len(previous))
	for _, old := range previous {
		oldByPath[old.ContainerPath] = old
	}
	var nodeID string
	for i := range mounts {
		mount := &mounts[i]
		mount.StorageNodeID = ""
		if old, ok := oldByPath[mount.ContainerPath]; ok {
			if mount.ResourceID != "" && mount.ResourceID != old.ResourceID {
				return fmt.Errorf("changing persistent storage for %s requires explicit data migration", mount.ContainerPath)
			}
			mount.ResourceID = old.ResourceID
			mount.StorageNodeID = old.StorageNodeID
		} else if mount.ResourceID != "" {
			if enterprise && !h.isEnterpriseResourceAllowed(slug, mount.ResourceID) {
				return fmt.Errorf("storage resource is not available for %s", mount.ContainerPath)
			}
			var selected *config.OnlineResourceConfig
			for j := range resources {
				if resources[j].ID == mount.ResourceID && resources[j].Type == volumeResourceType {
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
			if nodeID == "" {
				snapshot, err := h.caproverClient.ApplicationStorage(ctx)
				if err != nil || snapshot.Disk.Nodes[0].NodeID != target {
					return fmt.Errorf("cannot verify the Swarm node for volume resource %q", mount.ResourceID)
				}
			}
			mount.StorageNodeID = target
			// A recreated slug must never inherit the volume of a deleted app.
			mount.VolumeName += "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
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
		if resourceType == volumeResourceType {
			references = append(references, app.StorageMountsJSON)
		} else {
			references = append(references, app.ResourceIDsJSON)
		}
	}
	for _, version := range versions {
		if resourceType == volumeResourceType {
			references = append(references, version.StorageMountsJSON)
		} else {
			references = append(references, version.ResourceIDsJSON)
		}
	}
	for _, raw := range references {
		if raw == "" {
			continue
		}
		if resourceType == volumeResourceType {
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
