package handler

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/storage"
)

const (
	defaultLiteLLMPublicURL = "https://opc-litellm.benlai.com"
	defaultLLMProviderName  = "本来 AI"
)

type LLMModelItem struct {
	ID                string `json:"id"`
	ModelName         string `json:"model_name"`
	Name              string `json:"name"`
	BaseURL           string `json:"base_url"`
	APIKey            string `json:"api_key"`
	ContextWindow     int    `json:"context_window,omitempty"`
	MaxTokens         int    `json:"max_tokens,omitempty"`
	SupportsVision    bool   `json:"supports_vision,omitempty"`
	SupportsReasoning bool   `json:"supports_reasoning,omitempty"`
	IsDefault         bool   `json:"is_default,omitempty"`
}

type LLMHandler struct {
	cfg     *config.Config
	storage *storage.Storage
}

func NewLLMHandler(cfg *config.Config, s *storage.Storage) *LLMHandler {
	return &LLMHandler{
		cfg:     cfg,
		storage: s,
	}
}

func (h *LLMHandler) rootTenantID() uint64 {
	if h.cfg != nil && h.cfg.WeKnora.RootTenantID != 0 {
		return h.cfg.WeKnora.RootTenantID
	}
	return 10001
}

func (h *LLMHandler) resolvePublicLiteLLMURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("LITELLM_PUBLIC_URL")), "/"); v != "" {
		return v
	}
	if h.cfg != nil {
		if v := strings.TrimRight(strings.TrimSpace(h.cfg.Admin.Nav.LLMGatewayURL), "/"); v != "" {
			return v
		}
	}
	return defaultLiteLLMPublicURL
}

func resolveClientBaseURL(rawBaseURL, publicBaseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
	pub := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if pub == "" {
		pub = defaultLiteLLMPublicURL
	}
	if trimmed == "" {
		return ensureV1Suffix(pub)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return ensureV1Suffix(pub)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "opc-llm-gateway" || host == "litellm" || host == "localhost" || host == "127.0.0.1" {
		path := strings.TrimRight(parsed.Path, "/")
		if path == "" {
			return ensureV1Suffix(pub)
		}
		return pub + path
	}
	return trimmed
}

func ensureV1Suffix(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(strings.ToLower(trimmed), "/v1") {
		return trimmed
	}
	return trimmed + "/v1"
}

func (h *LLMHandler) ListModels(c *gin.Context) {
	if _, ok := resolveAccessSession(h.cfg, h.storage, c); !ok {
		return
	}
	var aesKey []byte
	if h.cfg != nil {
		aesKey = h.cfg.GetAESKey()
	}
	rows, err := h.storage.ListRootKnowledgeQAModels(c.Request.Context(), h.rootTenantID(), aesKey)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "load root KnowledgeQA models failed")
		return
	}
	publicURL := h.resolvePublicLiteLLMURL()
	items := make([]LLMModelItem, 0, len(rows))
	for _, row := range rows {
		if row.Name == "" || row.APIKey == "" {
			continue
		}
		displayName := row.DisplayName
		if displayName == "" {
			displayName = row.Name
		}
		id := row.ID
		if id == "" {
			id = row.Name
		}
		items = append(items, LLMModelItem{
			ID:                id,
			ModelName:         row.Name,
			Name:              displayName,
			BaseURL:           resolveClientBaseURL(row.BaseURL, publicURL),
			APIKey:            row.APIKey,
			ContextWindow:     row.ContextWindow,
			MaxTokens:         row.MaxTokens,
			SupportsVision:    row.SupportsVision,
			SupportsReasoning: row.SupportsReasoning,
			IsDefault:         row.IsDefault,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"configured":         len(items) > 0,
		"provider_name":      defaultLLMProviderName,
		"default_public_url": publicURL,
		"models":             items,
	})
}
