package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
)

func TestLLMListRootKnowledgeQAModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mainDB := openIntegrationTestSQLite(t)
	stateDB := openIntegrationTestSQLite(t)
	if err := mainDB.AutoMigrate(&model.WeKnoraModel{}); err != nil {
		t.Fatalf("migrate main: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.IdentityLink{}, &model.Session{}, &model.AuditEvent{}); err != nil {
		t.Fatalf("migrate state: %v", err)
	}
	s := storage.NewStorageWithDBs(mainDB, stateDB)
	now := time.Now()
	link := &model.IdentityLink{
		ID:               "link-emp-001",
		ImmutableSubject: "emp-001",
		Status:           "active",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := stateDB.Create(link).Error; err != nil {
		t.Fatalf("seed identity link: %v", err)
	}
	headers := platformAccessHeaders(t, s, link.ID)

	aesSecret := "0123456789abcdef0123456789abcdef"
	cfg := &config.Config{
		Auth:         config.AuthConfig{Key: testAuthKey},
		WeKnora:      config.WeKnoraConfig{RootTenantID: 10001},
		SystemAESKey: aesSecret,
	}

	r := gin.New()
	lh := NewLLMHandler(cfg, s)
	r.GET("/integration/v1/llm/models", lh.ListModels)

	unauthRec := httptest.NewRecorder()
	unauthReq := httptest.NewRequest(http.MethodGet, "/integration/v1/llm/models", nil)
	r.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", unauthRec.Code)
	}

	encKey1, err := crypto.EncryptAESGCM("sk-key-auto", cfg.GetAESKey())
	if err != nil {
		t.Fatalf("encrypt key1: %v", err)
	}
	encKey2, err := crypto.EncryptAESGCM("sk-key-minimax", cfg.GetAESKey())
	if err != nil {
		t.Fatalf("encrypt key2: %v", err)
	}
	encKey3, err := crypto.EncryptAESGCM("sk-key-direct", cfg.GetAESKey())
	if err != nil {
		t.Fatalf("encrypt key3: %v", err)
	}

	mustJSON := func(v map[string]interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	seedModels := []model.WeKnoraModel{
		{
			ID:          "m-auto",
			TenantID:    10001,
			Name:        "auto",
			DisplayName: "kimi 订阅模型（首选）",
			Type:        "KnowledgeQA",
			Source:      "remote",
			Parameters: mustJSON(map[string]interface{}{
				"base_url":       "http://opc-llm-gateway:4000/v1",
				"api_key":        encKey1,
				"context_window": 1000000,
			}),
			Status:    "active",
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:          "m-minimax",
			TenantID:    10001,
			Name:        "minimax 3.1",
			DisplayName: "多模态模型（很贵）",
			Type:        "KnowledgeQA",
			Source:      "remote",
			Parameters: mustJSON(map[string]interface{}{
				"base_url":        "http://opc-llm-gateway:4000/v1",
				"api_key":         encKey2,
				"supports_vision": true,
			}),
			Status:    "active",
			CreatedAt: now.Add(time.Second),
			UpdatedAt: now.Add(time.Second),
		},
		{
			ID:          "m-external",
			TenantID:    10001,
			Name:        "glm-5.3-flash",
			DisplayName: "",
			Type:        "KnowledgeQA",
			Source:      "remote",
			Parameters: mustJSON(map[string]interface{}{
				"base_url": "https://moma.cmecloud.cn/v1",
				"api_key":  encKey3,
			}),
			Status:    "active",
			CreatedAt: now.Add(2 * time.Second),
			UpdatedAt: now.Add(2 * time.Second),
		},
		{
			ID:          "m-embed",
			TenantID:    10001,
			Name:        "bge-m3",
			DisplayName: "Embedding",
			Type:        "Embedding",
			Source:      "remote",
			Parameters:  mustJSON(map[string]interface{}{"base_url": "http://opc-llm-gateway:4000/v1", "api_key": encKey1}),
			Status:      "active",
			CreatedAt:   now.Add(3 * time.Second),
			UpdatedAt:   now.Add(3 * time.Second),
		},
	}
	for _, item := range seedModels {
		if err := mainDB.Create(&item).Error; err != nil {
			t.Fatalf("seed model %s: %v", item.ID, err)
		}
	}

	rec := doGet(t, r, "/integration/v1/llm/models", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Configured       bool   `json:"configured"`
		ProviderName     string `json:"provider_name"`
		DefaultPublicURL string `json:"default_public_url"`
		Models           []struct {
			ID             string `json:"id"`
			ModelName      string `json:"model_name"`
			Name           string `json:"name"`
			BaseURL        string `json:"base_url"`
			APIKey         string `json:"api_key"`
			ContextWindow  int    `json:"context_window"`
			SupportsVision bool   `json:"supports_vision"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Configured || resp.ProviderName != "本来 AI" || resp.DefaultPublicURL != "https://opc-litellm.benlai.com" {
		t.Fatalf("unexpected header fields: %+v", resp)
	}
	if len(resp.Models) != 3 {
		t.Fatalf("expected 3 KnowledgeQA models, got %d: %+v", len(resp.Models), resp.Models)
	}
	if resp.Models[0].ID != "m-auto" || resp.Models[0].ModelName != "auto" || resp.Models[0].Name != "kimi 订阅模型（首选）" ||
		resp.Models[0].BaseURL != "https://opc-litellm.benlai.com/v1" || resp.Models[0].APIKey != "sk-key-auto" || resp.Models[0].ContextWindow != 1000000 {
		t.Fatalf("unexpected model[0]: %+v", resp.Models[0])
	}
	if resp.Models[1].ID != "m-minimax" || resp.Models[1].ModelName != "minimax 3.1" || !resp.Models[1].SupportsVision || resp.Models[1].APIKey != "sk-key-minimax" {
		t.Fatalf("unexpected model[1]: %+v", resp.Models[1])
	}
	if resp.Models[2].ID != "m-external" || resp.Models[2].Name != "glm-5.3-flash" || resp.Models[2].BaseURL != "https://moma.cmecloud.cn/v1" || resp.Models[2].APIKey != "sk-key-direct" {
		t.Fatalf("unexpected model[2]: %+v", resp.Models[2])
	}

	t.Setenv("LITELLM_PUBLIC_URL", "https://custom-llm.benlai.com")
	customRec := doGet(t, r, "/integration/v1/llm/models", headers)
	if customRec.Code != http.StatusOK {
		t.Fatalf("expected 200 with custom url, got %d", customRec.Code)
	}
	var customResp struct {
		DefaultPublicURL string `json:"default_public_url"`
		Models           []struct {
			BaseURL string `json:"base_url"`
		} `json:"models"`
	}
	_ = json.Unmarshal(customRec.Body.Bytes(), &customResp)
	if customResp.DefaultPublicURL != "https://custom-llm.benlai.com" || customResp.Models[0].BaseURL != "https://custom-llm.benlai.com/v1" {
		t.Fatalf("expected custom public url rewrite, got %+v", customResp)
	}
}
