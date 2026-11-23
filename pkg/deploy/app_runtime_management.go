package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

func (c *CapRoverClient) RestartApplication(ctx context.Context, slug, operationID string, changes map[string]string) error {
	type change struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	items := make([]change, 0, len(changes))
	for key, value := range changes {
		items = append(items, change{Key: key, Value: value})
	}
	body, err := json.Marshal(map[string]any{"operationId": operationID, "changes": items})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/api/v2/user/apps/runtime/%s/restart", c.baseURL, url.PathEscape(slug))
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
		_ = resp.Body.Close()
		if decodeErr == nil && (result.Status == 1106 || result.Status == 1105) {
			c.invalidateToken()
			continue
		}
		if resp.StatusCode != http.StatusOK || decodeErr != nil || result.Status != 100 {
			return fmt.Errorf("restart response HTTP %d, status %d", resp.StatusCode, result.Status)
		}
		return nil
	}
	return fmt.Errorf("deployment service rejected credentials")
}
