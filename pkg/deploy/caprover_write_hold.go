package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

func (c *CapRoverClient) SetApplicationWriteHold(ctx context.Context, slug string, hold bool) error {
	endpoint := c.baseURL + "/api/v2/user/apps/runtime/" + url.PathEscape(slug) + "/write-hold"
	payload, _ := json.Marshal(map[string]bool{"hold": hold})
	if _, err := c.requestWriteHold(ctx, http.MethodPost, endpoint, payload); err != nil {
		return err
	}
	if !hold {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		data, err := c.requestWriteHold(waitCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		var state struct {
			Held bool `json:"held"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
		if state.Held {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("application writers did not stop: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (c *CapRoverClient) requestWriteHold(ctx context.Context, method, endpoint string, payload []byte) (json.RawMessage, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-captain-auth", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != http.StatusOK || result.Status != 100 {
		return nil, fmt.Errorf("write hold request failed: HTTP %d, status %d", resp.StatusCode, result.Status)
	}
	return result.Data, nil
}
