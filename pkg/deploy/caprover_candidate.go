package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"weknora-tokendance/pkg/model"
)

type CandidateProbe struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type CandidateProbeResult struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

func (c *CapRoverClient) callCandidate(ctx context.Context, method, slug, receipt, subpath string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := c.baseURL + "/api/v2/user/apps/runtime/" + url.PathEscape(slug) + "/candidates/" + url.PathEscape(receipt) + subpath
	for range 2 {
		token, err := c.getToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-captain-auth", token)
		response, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		var reply caproverResponse
		err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&reply)
		response.Body.Close()
		if err != nil {
			return fmt.Errorf("isolated candidate service returned no valid response: HTTP %d", response.StatusCode)
		}
		if reply.Status == 1105 || reply.Status == 1106 {
			c.invalidateToken()
			continue
		}
		if response.StatusCode != http.StatusOK || reply.Status != 100 {
			return fmt.Errorf("isolated candidate operation failed: HTTP %d, status %d (%s)", response.StatusCode, reply.Status, reply.Description)
		}
		if result != nil {
			return json.Unmarshal(reply.Data, result)
		}
		return nil
	}
	return fmt.Errorf("isolated candidate authentication failed")
}

func (c *CapRoverClient) StartCandidate(ctx context.Context, slug, receipt string, env []model.AppConfigItem) error {
	// Send only the launch values; descriptions, resource bindings and other
	// production environment metadata are unrelated to the isolated runtime.
	items := make([]map[string]string, 0, len(env))
	for _, item := range env {
		items = append(items, map[string]string{"key": item.Key, "value": item.Value})
	}
	return c.callCandidate(ctx, http.MethodPost, slug, receipt, "", map[string]any{"env": items}, nil)
}

func (c *CapRoverClient) RemoveCandidate(ctx context.Context, slug, receipt string) error {
	return c.callCandidate(ctx, http.MethodDelete, slug, receipt, "", nil, nil)
}

func (c *CapRoverClient) CandidateLogs(ctx context.Context, slug, receipt string) (string, error) {
	var result struct {
		Logs string `json:"logs"`
	}
	err := c.callCandidate(ctx, http.MethodGet, slug, receipt, "/logs", nil, &result)
	return result.Logs, err
}

func (c *CapRoverClient) ProbeCandidate(ctx context.Context, slug, receipt string, port int, probe CandidateProbe) (CandidateProbeResult, error) {
	var result CandidateProbeResult
	err := c.callCandidate(ctx, http.MethodPost, slug, receipt, "/probe", struct {
		CandidateProbe
		Port int `json:"port"`
	}{probe, port}, &result)
	return result, err
}
