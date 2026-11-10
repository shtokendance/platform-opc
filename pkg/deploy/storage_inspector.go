package deploy

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"weknora-tokendance/pkg/model"
)

// StorageInspector 提供底层宿主机磁盘与 Docker 命名卷的使用量探测、文件概览与备份导出功能
type StorageInspector struct {
	dockerSockPath string
	httpClient     *http.Client
}

// NewStorageInspector 创建 StorageInspector 实例
func NewStorageInspector(dockerSockPath string) *StorageInspector {
	if dockerSockPath == "" {
		dockerSockPath = "/var/run/docker.sock"
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, proto, addr string) (net.Conn, error) {
				return net.Dial("unix", dockerSockPath)
			},
		},
		Timeout: 30 * time.Second,
	}
	return &StorageInspector{
		dockerSockPath: dockerSockPath,
		httpClient:     client,
	}
}

// FormatBytes 将字节数转换为人类可读的字符串（如 64.0 KB, 1.25 GB）
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// GetServerDiskInfo 获取当前服务所在文件系统/宿主机磁盘水位信息
func (si *StorageInspector) GetServerDiskInfo(path string) model.StorageServerDiskInfo {
	if path == "" {
		path = "/"
	}
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return model.StorageServerDiskInfo{
			Path:       path,
			TotalHuman: "未知",
			UsedHuman:  "未知",
			FreeHuman:  "未知",
		}
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	used := total - free
	ratio := 0.0
	if total > 0 {
		ratio = float64(used) / float64(total) * 100.0
	}

	return model.StorageServerDiskInfo{
		Path:       path,
		TotalBytes: total,
		UsedBytes:  used,
		FreeBytes:  free,
		TotalHuman: FormatBytes(total),
		UsedHuman:  FormatBytes(used),
		FreeHuman:  FormatBytes(free),
		UsageRatio: float64(int(ratio*10)) / 10.0,
		IsLowSpace: ratio >= 90.0,
	}
}

// DockerVolumeDF Docker 卷详细信息（供全局存储大盘与孤儿卷识别使用）
type DockerVolumeDF struct {
	Name      string `json:"Name"`
	Driver    string `json:"Driver"`
	CreatedAt string `json:"CreatedAt,omitempty"`
	UsageData struct {
		Size     int64 `json:"Size"`
		RefCount int   `json:"RefCount"`
	} `json:"UsageData"`
}

type dockerSystemDFResponse struct {
	Volumes []*DockerVolumeDF `json:"Volumes"`
}

// QueryDockerVolumesRaw 调用 Docker Engine API 获取所有卷的底层元数据
func (si *StorageInspector) QueryDockerVolumesRaw(ctx context.Context) ([]*DockerVolumeDF, error) {
	if _, err := os.Stat(si.dockerSockPath); err != nil {
		return nil, fmt.Errorf("docker socket not accessible: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/system/df", nil)
	if err != nil {
		return nil, err
	}

	resp, err := si.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker /system/df returned %d", resp.StatusCode)
	}

	var dfResp dockerSystemDFResponse
	if err := json.NewDecoder(resp.Body).Decode(&dfResp); err != nil {
		return nil, err
	}
	return dfResp.Volumes, nil
}

// queryDockerVolumes 调用 Docker Engine API 获取所有卷的真实占用字节数字典
func (si *StorageInspector) queryDockerVolumes(ctx context.Context) (map[string]int64, error) {
	vols, err := si.QueryDockerVolumesRaw(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int64)
	for _, v := range vols {
		if v != nil {
			size := v.UsageData.Size
			if size < 0 {
				size = 0
			}
			result[v.Name] = size
		}
	}
	return result, nil
}

type dockerContainerInfo struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// FindAppContainerID 查找属于指定应用活跃的 Docker 容器 ID（优先返回 State=="running" 的健康容器）
func (si *StorageInspector) FindAppContainerID(ctx context.Context, slug string) (string, error) {
	return si.findAppContainerID(ctx, slug)
}

func (si *StorageInspector) findAppContainerID(ctx context.Context, slug string) (string, error) {
	if _, err := os.Stat(si.dockerSockPath); err != nil {
		return "", fmt.Errorf("docker socket not accessible")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/containers/json?all=1", nil)
	if err != nil {
		return "", err
	}

	resp, err := si.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var containers []dockerContainerInfo
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return "", err
	}

	expectedPrefix := fmt.Sprintf("%s.", slug)
	captainPrefix := fmt.Sprintf("captain-%s.", slug)
	matches := func(c dockerContainerInfo) bool {
		for _, name := range c.Names {
			cleanName := strings.TrimPrefix(name, "/")
			if cleanName == slug || strings.HasPrefix(cleanName, expectedPrefix) || strings.HasPrefix(cleanName, captainPrefix) || cleanName == fmt.Sprintf("captain-%s", slug) {
				return true
			}
		}
		if c.Labels != nil {
			svcName := c.Labels["com.docker.swarm.service.name"]
			if svcName == slug || svcName == fmt.Sprintf("captain-%s", slug) || svcName == fmt.Sprintf("srv-captain--%s", slug) {
				return true
			}
		}
		return false
	}

	var fallbackID string
	for _, c := range containers {
		if matches(c) {
			if strings.EqualFold(c.State, "running") {
				return c.ID, nil
			}
			if fallbackID == "" {
				fallbackID = c.ID
			}
		}
	}
	if fallbackID != "" {
		return fallbackID, nil
	}

	return "", fmt.Errorf("no container found for app %s", slug)
}

// ListAppContainers 批量扫描所有容器并建立 slug -> containerID 映射（优先匹配 running 状态容器）
func (si *StorageInspector) ListAppContainers(ctx context.Context) (map[string]string, error) {
	if _, err := os.Stat(si.dockerSockPath); err != nil {
		return nil, fmt.Errorf("docker socket not accessible: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}

	resp, err := si.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var containers []dockerContainerInfo
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}

	runningMap := make(map[string]string)
	fallbackMap := make(map[string]string)

	for _, c := range containers {
		var matchedSlug string
		for _, name := range c.Names {
			cleanName := strings.TrimPrefix(name, "/")
			if idx := strings.Index(cleanName, "."); idx > 0 {
				prefix := cleanName[:idx]
				matchedSlug = strings.TrimPrefix(prefix, "captain-")
				break
			}
		}
		if matchedSlug == "" && c.Labels != nil {
			svcName := c.Labels["com.docker.swarm.service.name"]
			if svcName != "" {
				s := strings.TrimPrefix(svcName, "srv-captain--")
				matchedSlug = strings.TrimPrefix(s, "captain-")
			}
		}

		if matchedSlug != "" {
			if strings.EqualFold(c.State, "running") {
				runningMap[matchedSlug] = c.ID
			} else if _, exists := fallbackMap[matchedSlug]; !exists {
				fallbackMap[matchedSlug] = c.ID
			}
		}
	}

	for slug, id := range fallbackMap {
		if _, exists := runningMap[slug]; !exists {
			runningMap[slug] = id
		}
	}
	return runningMap, nil
}

// InspectAppMounts 探测某个应用配置的所有挂载点的真实运行时信息
func (si *StorageInspector) InspectAppMounts(ctx context.Context, slug string, mounts []model.AppStorageMount) []model.AppStorageMountDetail {
	volSizes, _ := si.queryDockerVolumes(ctx)
	containerID, _ := si.findAppContainerID(ctx, slug)
	return si.InspectAppMountsWithCache(ctx, slug, mounts, volSizes, containerID)
}

// InspectAppMountsWithCache 使用外部缓存的卷大小和容器 ID 进行高效探测，避免循环反复调用 Docker API
func (si *StorageInspector) InspectAppMountsWithCache(ctx context.Context, slug string, mounts []model.AppStorageMount, volSizes map[string]int64, containerID string) []model.AppStorageMountDetail {
	details := make([]model.AppStorageMountDetail, 0, len(mounts))
	for _, m := range mounts {
		det := model.AppStorageMountDetail{
			AppStorageMount: m,
			Status:          "healthy",
			SeedProtected:   m.SeedFilesCount > 0,
		}

		if volSizes != nil && m.VolumeName != "" {
			if sz, ok := volSizes[m.VolumeName]; ok {
				det.SizeBytes = sz
				det.SizeHuman = FormatBytes(uint64(sz))
			} else {
				det.SizeBytes = 0
				det.SizeHuman = "0 B"
			}
		}

		// 若能拿到容器 ID，尝试通过 container archive 探查目录内的文件列表
		if containerID != "" && m.ContainerPath != "" {
			files, count := si.sampleContainerFiles(ctx, containerID, m.ContainerPath)
			det.FileCount = count
			det.SampleFiles = files
		}

		if det.SizeHuman == "" {
			det.SizeHuman = FormatBytes(uint64(det.SizeBytes))
		}

		details = append(details, det)
	}
	return details
}

// RemoveDockerVolume 删除指定的 Docker 卷
func (si *StorageInspector) RemoveDockerVolume(ctx context.Context, volumeName string, force bool) error {
	if _, err := os.Stat(si.dockerSockPath); err != nil {
		return fmt.Errorf("docker socket not accessible: %w", err)
	}
	urlStr := fmt.Sprintf("http://localhost/volumes/%s", url.PathEscape(volumeName))
	if force {
		urlStr += "?force=1"
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", urlStr, nil)
	if err != nil {
		return err
	}
	resp, err := si.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker volume remove %s failed (%d): %s", volumeName, resp.StatusCode, string(body))
	}
	return nil
}

// sampleContainerFiles 浅层读取容器挂载目录下的文件清单
func (si *StorageInspector) sampleContainerFiles(ctx context.Context, containerID, containerPath string) ([]string, int) {
	urlStr := fmt.Sprintf("http://localhost/containers/%s/archive?path=%s", containerID, url.QueryEscape(containerPath))
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, 0
	}

	resp, err := si.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, 0
	}
	defer resp.Body.Close()

	tr := tar.NewReader(resp.Body)
	var files []string
	count := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF || err != nil {
			break
		}
		cleanName := strings.TrimPrefix(hdr.Name, "./")
		if cleanName == "" || cleanName == "." {
			continue
		}
		count++
		if len(files) < 10 {
			typeMark := ""
			if hdr.Typeflag == tar.TypeDir {
				typeMark = "/"
			}
			files = append(files, fmt.Sprintf("%s%s (%s)", cleanName, typeMark, FormatBytes(uint64(hdr.Size))))
		}
		if count >= 100 {
			break
		}
	}

	return files, count
}

// StreamMountBackup 打包下载指定挂载点或应用全部卷的 tar.gz 归档流
func (si *StorageInspector) StreamMountBackup(ctx context.Context, slug string, containerPath string, w io.Writer) error {
	containerID, err := si.findAppContainerID(ctx, slug)
	if err != nil {
		return fmt.Errorf("app container is not running: %w", err)
	}

	urlStr := fmt.Sprintf("http://localhost/containers/%s/archive?path=%s", containerID, url.QueryEscape(containerPath))
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return err
	}

	resp, err := si.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("archive stream failed (%d): %s", resp.StatusCode, string(body))
	}

	_, err = io.Copy(w, resp.Body)
	return err
}
