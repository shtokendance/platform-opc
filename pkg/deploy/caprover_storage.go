package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"time"

	"weknora-tokendance/pkg/model"
)

const (
	LocalDirectoryVolume = "local-directory"
	AliyunOSSVolume      = "aliyun-oss-volume"
)

func IsDirectoryVolume(backend string) bool {
	return backend == LocalDirectoryVolume || backend == AliyunOSSVolume
}

// PrepareVolumePath operates on the Deploy node, where the application's bind
// mount will run. Once allocated, require the same directory to exist.
func (c *CapRoverClient) PrepareVolumePath(ctx context.Context, backend, nodeID, root, directory string, create bool) error {
	return c.updateApplicationVolumePath(ctx, http.MethodPost, map[string]any{
		"backend": backend, "node_id": nodeID, "root": root, "directory": directory, "create": create,
	})
}

func (c *CapRoverClient) RemoveLocalApplicationDirectory(ctx context.Context, nodeID, root, directory string) error {
	return c.updateApplicationVolumePath(ctx, http.MethodDelete, map[string]any{
		"node_id": nodeID, "root": root, "directory": directory,
	})
}

func (c *CapRoverClient) updateApplicationVolumePath(ctx context.Context, method string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v2/user/system/application-storage/paths/", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-captain-auth", token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		var result caproverResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || result.Status == 1105 || result.Status == 1106 {
			c.invalidateToken()
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("Deploy does not support application volume paths; update Deploy first")
		}
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			return fmt.Errorf("invalid response while checking the Deploy volume path")
		}
		if result.Status != 100 {
			return fmt.Errorf("Deploy volume path unavailable: %s", result.Description)
		}
		return nil
	}
	return fmt.Errorf("Deploy storage authentication failed")
}

func (c *CapRoverClient) CreateVolumePaths(ctx context.Context, mounts, previous []model.AppStorageMount) error {
	return c.prepareVolumePaths(ctx, mounts, func(mount model.AppStorageMount) bool {
		return !slices.ContainsFunc(previous, func(old model.AppStorageMount) bool {
			return old.HostPath == mount.HostPath
		})
	})
}

func (c *CapRoverClient) prepareVolumePaths(ctx context.Context, mounts []model.AppStorageMount, create func(model.AppStorageMount) bool) error {
	for _, mount := range mounts {
		if !IsDirectoryVolume(mount.StorageBackend) {
			continue
		}
		parent := path.Dir(mount.HostPath)
		root := path.Dir(parent)
		directory := path.Join(path.Base(parent), path.Base(mount.HostPath))
		if mount.ResourceID == "" || mount.StorageNodeID == "" || !path.IsAbs(root) || root == "/" || path.Join(root, directory) != mount.HostPath {
			return fmt.Errorf("invalid allocated storage path for %s", mount.ContainerPath)
		}
		if err := c.PrepareVolumePath(ctx, mount.StorageBackend, mount.StorageNodeID, root, directory, create != nil && create(mount)); err != nil {
			return fmt.Errorf("volume for %s: %w", mount.ContainerPath, err)
		}
	}
	return nil
}

type SwarmVolume struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	SizeBytes int64  `json:"size_bytes"`
	RefCount  int    `json:"ref_count"`
}

type SwarmMount struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

type SwarmVolumeRoot struct {
	Root           string  `json:"root"`
	NodeID         string  `json:"node_id"`
	Backend        string  `json:"backend"`
	FilesystemType string  `json:"filesystem_type"`
	Writable       bool    `json:"writable"`
	Available      bool    `json:"available"`
	TotalBytes     uint64  `json:"total_bytes,omitempty"`
	UsedBytes      uint64  `json:"used_bytes,omitempty"`
	FreeBytes      uint64  `json:"free_bytes,omitempty"`
	UsageRatio     float64 `json:"usage_ratio,omitempty"`
	Error          string  `json:"error,omitempty"`
}

type SwarmStorage struct {
	Disk struct {
		Nodes []struct {
			NodeID   string `json:"nodeId"`
			Hostname string `json:"hostname"`
			Disks    []struct {
				Total     uint64 `json:"totalBytes"`
				Used      uint64 `json:"usedBytes"`
				Available uint64 `json:"availableBytes"`
				Severity  string `json:"severity"`
			} `json:"disks"`
		} `json:"nodes"`
	} `json:"disk"`
	Volumes     []SwarmVolume     `json:"volumes"`
	VolumeRoots []SwarmVolumeRoot `json:"volume_roots"`
	Services    []struct {
		Name         string       `json:"name"`
		RunningTasks int          `json:"running_tasks"`
		Mounts       []SwarmMount `json:"mounts"`
	} `json:"services"`
}

func (c *CapRoverClient) ApplicationStorage(ctx context.Context) (SwarmStorage, error) {
	var snapshot SwarmStorage
	if err := c.getApplicationStorage(ctx, "", &snapshot); err != nil {
		return snapshot, err
	}
	if len(snapshot.Disk.Nodes) != 1 || len(snapshot.Disk.Nodes[0].Disks) != 1 || snapshot.Disk.Nodes[0].NodeID == "" || snapshot.Disk.Nodes[0].Disks[0].Total == 0 {
		return snapshot, fmt.Errorf("Deploy did not report a storage node")
	}
	return snapshot, nil
}

type SwarmApplicationBinds struct {
	NodeID string       `json:"node_id"`
	Mounts []SwarmMount `json:"mounts"`
}

func (c *CapRoverClient) RunningApplicationBinds(ctx context.Context, slug string) ([]SwarmApplicationBinds, error) {
	var tasks []SwarmApplicationBinds
	err := c.getApplicationStorage(ctx, "paths/"+url.PathEscape(slug), &tasks)
	return tasks, err
}

func (c *CapRoverClient) getApplicationStorage(ctx context.Context, suffix string, data any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return err
		}
		result, err := c.requestApplicationStorage(ctx, suffix, token)
		if err != nil {
			return err
		}
		if result.Status == 1105 || result.Status == 1106 {
			c.invalidateToken()
			continue
		}
		if result.Status != 100 {
			return fmt.Errorf("Deploy storage unavailable (%d): %s", result.Status, result.Description)
		}
		return json.Unmarshal(result.Data, data)
	}
	return fmt.Errorf("Deploy storage authentication failed")
}

func (c *CapRoverClient) requestApplicationStorage(ctx context.Context, suffix, token string) (caproverResponse, error) {
	var result caproverResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/user/system/application-storage/"+suffix, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("x-captain-auth", token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Status = 1106
		return result, nil
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("Deploy storage returned HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result, err
}

type ApplicationVolumePathStat struct {
	HostPath    string   `json:"host_path"`
	Exists      bool     `json:"exists"`
	FileCount   int      `json:"file_count"`
	SizeBytes   int64    `json:"size_bytes"`
	SampleFiles []string `json:"sample_files"`
}

type ApplicationVolumeExportMount struct {
	ContainerPath string `json:"container_path"`
	HostPath      string `json:"host_path"`
	Name          string `json:"name,omitempty"`
}

type ApplicationVolumeExportRequest struct {
	NodeID   string                         `json:"node_id"`
	AppSlug  string                         `json:"app_slug"`
	AppName  string                         `json:"app_name,omitempty"`
	Mounts   []ApplicationVolumeExportMount `json:"mounts"`
	MaxBytes int64                          `json:"max_bytes,omitempty"`
}

func (c *CapRoverClient) InspectApplicationVolumePaths(ctx context.Context, nodeID string, paths []string) ([]ApplicationVolumePathStat, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"node_id": nodeID,
		"paths":   paths,
	})
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/user/system/application-storage/inspect/", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-captain-auth", token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		var result caproverResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || result.Status == 1105 || result.Status == 1106 {
			c.invalidateToken()
			continue
		}
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			return nil, fmt.Errorf("Deploy storage inspect returned HTTP %d", resp.StatusCode)
		}
		if result.Status != 100 {
			return nil, fmt.Errorf("Deploy storage inspect failed: %s", result.Description)
		}
		var payload struct {
			Paths []ApplicationVolumePathStat `json:"paths"`
		}
		if err := json.Unmarshal(result.Data, &payload); err != nil {
			return nil, err
		}
		return payload.Paths, nil
	}
	return nil, fmt.Errorf("Deploy storage authentication failed")
}

func (c *CapRoverClient) ExportApplicationVolumes(ctx context.Context, exportReq ApplicationVolumeExportRequest) (io.ReadCloser, error) {
	body, err := json.Marshal(exportReq)
	if err != nil {
		return nil, err
	}
	exportClient := &http.Client{Timeout: 10 * time.Minute}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/user/system/application-storage/export/", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-captain-auth", token)
		resp, err := exportClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			_ = resp.Body.Close()
			c.invalidateToken()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("Deploy volume export failed (HTTP %d): %s", resp.StatusCode, string(msg))
		}
		return resp.Body, nil
	}
	return nil, fmt.Errorf("Deploy storage authentication failed")
}
