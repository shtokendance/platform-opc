package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type AppResourceMetrics struct {
	Available            bool    `json:"available"`
	Message              string  `json:"message,omitempty"`
	ReplicasRunning      int     `json:"replicas_running"`
	ReplicasDesired      int     `json:"replicas_desired"`
	CPUPercent           float64 `json:"cpu_percent"`
	OnlineCPUs           int     `json:"online_cpus"`
	CPULimitCores        float64 `json:"cpu_limit_cores"`
	MemoryUsageBytes     int64   `json:"memory_usage_bytes"`
	MemoryLimitBytes     int64   `json:"memory_limit_bytes"`
	HostMemoryBytes      int64   `json:"host_memory_bytes"`
	MemoryPercent        float64 `json:"memory_percent"`
	ContainerRwBytes     int64   `json:"container_rw_bytes"`
	ContainerRootfsBytes int64   `json:"container_rootfs_bytes"`
	NetworkRxBytes       int64   `json:"network_rx_bytes"`
	NetworkTxBytes       int64   `json:"network_tx_bytes"`
	BlockReadBytes       int64   `json:"block_read_bytes"`
	BlockWriteBytes      int64   `json:"block_write_bytes"`
	PIDs                 int     `json:"pids"`
	RestartCount         int     `json:"restart_count"`
	StartedAt            string  `json:"started_at,omitempty"`
	NodeHostname         string  `json:"node_hostname,omitempty"`
	CollectedAt          string  `json:"collected_at,omitempty"`
}

func (c *CapRoverClient) GetAppResourceMetrics(ctx context.Context, slug string) (AppResourceMetrics, error) {
	var metrics AppResourceMetrics
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return metrics, err
		}
		result, err := c.requestAppResourceMetrics(ctx, slug, token)
		if err != nil {
			return metrics, err
		}
		if result.Status == 1105 || result.Status == 1106 {
			c.invalidateToken()
			continue
		}
		if result.Status != 100 {
			return metrics, fmt.Errorf("Deploy metrics unavailable (%d): %s", result.Status, result.Description)
		}
		err = json.Unmarshal(result.Data, &metrics)
		return metrics, err
	}
	return metrics, fmt.Errorf("Deploy metrics authentication failed")
}

func (c *CapRoverClient) requestAppResourceMetrics(ctx context.Context, slug, token string) (caproverResponse, error) {
	var result caproverResponse
	endpoint := c.baseURL + "/api/v2/user/apps/runtime/" + url.PathEscape(slug) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
		return result, fmt.Errorf("Deploy metrics returned HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result, err
}
