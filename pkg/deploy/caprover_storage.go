package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

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
	Volumes  []SwarmVolume `json:"volumes"`
	Services []struct {
		Name         string       `json:"name"`
		RunningTasks int          `json:"running_tasks"`
		Mounts       []SwarmMount `json:"mounts"`
	} `json:"services"`
}

func (c *CapRoverClient) ApplicationStorage(ctx context.Context) (SwarmStorage, error) {
	var snapshot SwarmStorage
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return snapshot, err
		}
		result, err := c.requestApplicationStorage(ctx, token)
		if err != nil {
			return snapshot, err
		}
		if result.Status == 1105 || result.Status == 1106 {
			c.invalidateToken()
			continue
		}
		if result.Status != 100 {
			return snapshot, fmt.Errorf("Deploy storage unavailable (%d)", result.Status)
		}
		if err := json.Unmarshal(result.Data, &snapshot); err != nil {
			return snapshot, err
		}
		if len(snapshot.Disk.Nodes) != 1 || len(snapshot.Disk.Nodes[0].Disks) != 1 || snapshot.Disk.Nodes[0].NodeID == "" || snapshot.Disk.Nodes[0].Disks[0].Total == 0 {
			return snapshot, fmt.Errorf("Deploy did not report a storage node")
		}
		return snapshot, nil
	}
	return snapshot, fmt.Errorf("Deploy storage authentication failed")
}

func (c *CapRoverClient) requestApplicationStorage(ctx context.Context, token string) (caproverResponse, error) {
	var result caproverResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/user/system/application-storage/", nil)
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
