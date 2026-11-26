package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/storage"
)

const (
	defaultLiteLLMBaseURL = "http://opc-llm-gateway:4000"
	defaultLLMProviderName = "本来 AI"
	maxLLMRequestBodyBytes = 25 << 20
)

type LLMModelItem struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	ContextWindow     int    `json:"context_window,omitempty"`
	MaxTokens         int    `json:"max_tokens,omitempty"`
	SupportsVision    bool   `json:"supports_vision,omitempty"`
	SupportsReasoning bool   `json:"supports_reasoning,omitempty"`
}

type LLMHandler struct {
	cfg          *config.Config
	storage      *storage.Storage
	metaClient   *http.Client
	streamClient *http.Client
}

func NewLLMHandler(cfg *config.Config, s *storage.Storage) *LLMHandler {
	return &LLMHandler{
		cfg:     cfg,
		storage: s,
		metaClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		streamClient: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Minute,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

func (h *LLMHandler) rootTenantID() uint64 {
	if h.cfg != nil && h.cfg.WeKnora.RootTenantID != 0 {
		return h.cfg.WeKnora.RootTenantID
	}
	return 10001
}

func (h *LLMHandler) resolveUpstream(ctx context.Context) (string, string, bool, error) {
	var aesKey []byte
	if h.cfg != nil {
		aesKey = h.cfg.GetAESKey()
	}
	baseURL, apiKey, found, err := h.storage.GetRootModelCredential(ctx, h.rootTenantID(), storage.DSHPluginOPCModelID, aesKey)
	if err != nil {
		return "", "", false, err
	}
	if !found || strings.TrimSpace(apiKey) == "" {
		return "", "", false, nil
	}
	baseURL = normalizeLiteLLMBase(baseURL)
	return baseURL, strings.TrimSpace(apiKey), true, nil
}

func normalizeLiteLLMBase(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return defaultLiteLLMBaseURL
	}
	if strings.HasSuffix(strings.ToLower(trimmed), "/v1") {
		trimmed = strings.TrimRight(trimmed[:len(trimmed)-3], "/")
	}
	if trimmed == "" {
		return defaultLiteLLMBaseURL
	}
	return trimmed
}

func (h *LLMHandler) ListModels(c *gin.Context) {
	if _, ok := resolveAccessSession(h.cfg, h.storage, c); !ok {
		return
	}
	baseURL, apiKey, configured, err := h.resolveUpstream(c.Request.Context())
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "load llm configuration failed")
		return
	}
	if !configured {
		c.JSON(http.StatusOK, gin.H{
			"configured":    false,
			"provider_name": defaultLLMProviderName,
			"models":        []LLMModelItem{},
		})
		return
	}

	models, err := h.fetchLiteLLMModelInfo(c.Request.Context(), baseURL, apiKey)
	if err != nil {
		models, err = h.fetchLiteLLMV1Models(c.Request.Context(), baseURL, apiKey)
		if err != nil {
			writeErrorEnvelope(c, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "litellm model discovery failed")
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"configured":    true,
		"provider_name": defaultLLMProviderName,
		"models":        models,
	})
}

func (h *LLMHandler) fetchLiteLLMModelInfo(ctx context.Context, baseURL, apiKey string) ([]LLMModelItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/model/info", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := h.metaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("model/info non-200")
	}

	var payload struct {
		Data []struct {
			ModelName     string                 `json:"model_name"`
			LiteLLMParams map[string]interface{} `json:"litellm_params"`
			ModelInfo     map[string]interface{} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(payload.Data))
	items := make([]LLMModelItem, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ModelName)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		mode := strings.ToLower(strings.TrimSpace(llmStringValue(entry.ModelInfo["mode"])))
		if mode != "" && mode != "chat" && mode != "responses" {
			continue
		}
		seen[id] = struct{}{}
		item := LLMModelItem{
			ID:                id,
			Name:              id,
			ContextWindow:     llmIntValue(entry.ModelInfo["max_input_tokens"]),
			MaxTokens:         llmIntValue(entry.ModelInfo["max_output_tokens"]),
			SupportsVision:    llmBoolValue(entry.ModelInfo["supports_vision"]),
			SupportsReasoning: llmBoolValue(entry.ModelInfo["supports_reasoning"]) || strings.TrimSpace(llmStringValue(entry.LiteLLMParams["reasoning_effort"])) != "",
		}
		if item.ContextWindow == 0 {
			item.ContextWindow = llmIntValue(entry.ModelInfo["max_tokens"])
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *LLMHandler) fetchLiteLLMV1Models(ctx context.Context, baseURL, apiKey string) ([]LLMModelItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := h.metaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("v1/models non-200")
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(payload.Data))
	items := make([]LLMModelItem, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, LLMModelItem{ID: id, Name: id})
	}
	return items, nil
}

func (h *LLMHandler) ChatCompletions(c *gin.Context) {
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	baseURL, apiKey, configured, err := h.resolveUpstream(c.Request.Context())
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "load llm configuration failed")
		return
	}
	if !configured {
		writeErrorEnvelope(c, http.StatusServiceUnavailable, "LLM_NOT_CONFIGURED", "dsh-plugin-opc model is not configured in root space")
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, maxLLMRequestBodyBytes+1))
	if err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, "BAD_REQUEST", "read request body failed")
		return
	}
	if len(rawBody) > maxLLMRequestBodyBytes {
		writeErrorEnvelope(c, http.StatusRequestEntityTooLarge, "BAD_REQUEST", "request body exceeds limit")
		return
	}

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, "BAD_REQUEST", "invalid json body")
		return
	}
	if sess.link != nil && strings.TrimSpace(sess.link.ImmutableSubject) != "" {
		bodyMap["user"] = sess.link.ImmutableSubject
	}
	forwardedBody, err := json.Marshal(bodyMap)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "marshal request body failed")
		return
	}

	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(forwardedBody))
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "create upstream request failed")
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	upstreamReq.Header.Set("Content-Type", "application/json")
	if accept := c.GetHeader("Accept"); accept != "" {
		upstreamReq.Header.Set("Accept", accept)
	}

	resp, err := h.streamClient.Do(upstreamReq)
	if err != nil {
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			return
		}
		writeErrorEnvelope(c, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "litellm upstream unavailable")
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Writer.Header().Set("Content-Type", ct)
	} else {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.WriteHeader(resp.StatusCode)

	flusher, canFlush := c.Writer.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func llmStringValue(v interface{}) string {
	s, _ := v.(string)
	return s
}

func llmBoolValue(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

func llmIntValue(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
