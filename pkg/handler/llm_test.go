package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"weknora-tokendance/config"
	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
)

func TestLLMHandlerAndSyncExclusion(t *testing.T) {
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

	var capturedAuth string
	var capturedChatBody map[string]interface{}
	fakeLiteLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/model/info":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"data": [
					{
						"model_name": "kimi-3.1",
						"litellm_params": {"model": "openai/k3.1-pro", "reasoning_effort": "high"},
						"model_info": {"mode": "chat", "max_input_tokens": 131072, "max_output_tokens": 8192, "supports_reasoning": true}
					},
					{
						"model_name": "kimi-3.1",
						"litellm_params": {"model": "openai/k3.1-pro"},
						"model_info": {"mode": "chat"}
					},
					{
						"model_name": "minimax 3.1",
						"litellm_params": {"model": "openai/MiniMax-M3.1-Flash"},
						"model_info": {"mode": "chat", "supports_vision": true}
					},
					{
						"model_name": "bge-m3",
						"litellm_params": {"model": "openai/bge-m3"},
						"model_info": {"mode": "embedding"}
					}
				]
			}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			bodyBytes, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(bodyBytes, &capturedChatBody)
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeLiteLLM.Close()

	aesSecret := "0123456789abcdef0123456789abcdef"
	t.Setenv("SYSTEM_AES_KEY", aesSecret)
	cfg := &config.Config{
		Auth:         config.AuthConfig{Key: testAuthKey},
		WeKnora:      config.WeKnoraConfig{RootTenantID: 10001},
		SystemAESKey: aesSecret,
	}
	r := gin.New()
	lh := NewLLMHandler(cfg, s)
	r.GET("/integration/v1/llm/models", lh.ListModels)
	r.POST("/integration/v1/llm/v1/chat/completions", lh.ChatCompletions)

	unauthRec := httptest.NewRecorder()
	unauthReq := httptest.NewRequest(http.MethodGet, "/integration/v1/llm/models", nil)
	r.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", unauthRec.Code)
	}

	emptyRec := doGet(t, r, "/integration/v1/llm/models", headers)
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("expected 200 when unconfigured, got %d: %s", emptyRec.Code, emptyRec.Body.String())
	}
	var emptyPayload struct {
		Configured   bool   `json:"configured"`
		ProviderName string `json:"provider_name"`
		Models       []any  `json:"models"`
	}
	if err := json.Unmarshal(emptyRec.Body.Bytes(), &emptyPayload); err != nil {
		t.Fatalf("unmarshal empty models response: %v", err)
	}
	if emptyPayload.Configured || emptyPayload.ProviderName != "本来 AI" || len(emptyPayload.Models) != 0 {
		t.Fatalf("unexpected unconfigured payload: %+v", emptyPayload)
	}

	encKey, err := crypto.EncryptAESGCM("sk-litellm-virtual-key", cfg.GetAESKey())
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	paramsBytes, _ := json.Marshal(map[string]interface{}{
		"base_url": fakeLiteLLM.URL + "/v1",
		"api_key":  encKey,
		"provider": "generic",
	})
	if err := mainDB.Create(&model.WeKnoraModel{
		ID:          storage.DSHPluginOPCModelID,
		TenantID:    10001,
		Name:        "dsh-plugin-opc",
		DisplayName: "本来 AI",
		Type:        "KnowledgeQA",
		Source:      "remote",
		Parameters:  string(paramsBytes),
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error; err != nil {
		t.Fatalf("seed dsh-plugin-opc root model: %v", err)
	}
	if err := mainDB.Create(&model.WeKnoraModel{
		ID:          "regular-chat-model",
		TenantID:    10001,
		Name:        "glm-5.3-flash",
		DisplayName: "GLM 5.3 Flash",
		Type:        "KnowledgeQA",
		Source:      "remote",
		Parameters:  `{"base_url":"http://example.invalid/v1"}`,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error; err != nil {
		t.Fatalf("seed regular root model: %v", err)
	}

	if err := s.SyncTenantModelsFromParent(context.Background(), 10001, 20002); err != nil {
		t.Fatalf("sync tenant models: %v", err)
	}
	syncedModels, err := s.GetModelsByTenantID(context.Background(), 20002)
	if err != nil {
		t.Fatalf("query child models: %v", err)
	}
	if len(syncedModels) != 1 || syncedModels[0].Name != "glm-5.3-flash" {
		t.Fatalf("expected dsh-plugin-opc to be excluded from child sync, got %+v", syncedModels)
	}

	modelsRec := doGet(t, r, "/integration/v1/llm/models", headers)
	if modelsRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for models, got %d: %s", modelsRec.Code, modelsRec.Body.String())
	}
	if capturedAuth != "Bearer sk-litellm-virtual-key" {
		t.Fatalf("expected decrypted key forwarded to LiteLLM, got %q", capturedAuth)
	}
	var modelsResp struct {
		Configured   bool   `json:"configured"`
		ProviderName string `json:"provider_name"`
		Models       []struct {
			ID                string `json:"id"`
			ContextWindow     int    `json:"context_window"`
			MaxTokens         int    `json:"max_tokens"`
			SupportsVision    bool   `json:"supports_vision"`
			SupportsReasoning bool   `json:"supports_reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(modelsRec.Body.Bytes(), &modelsResp); err != nil {
		t.Fatalf("unmarshal modelsResp: %v", err)
	}
	if !modelsResp.Configured || modelsResp.ProviderName != "本来 AI" || len(modelsResp.Models) != 2 {
		t.Fatalf("unexpected modelsResp: %+v", modelsResp)
	}
	if modelsResp.Models[0].ID != "kimi-3.1" || !modelsResp.Models[0].SupportsReasoning || modelsResp.Models[0].ContextWindow != 131072 {
		t.Fatalf("unexpected first model: %+v", modelsResp.Models[0])
	}
	if modelsResp.Models[1].ID != "minimax 3.1" || !modelsResp.Models[1].SupportsVision {
		t.Fatalf("unexpected second model: %+v", modelsResp.Models[1])
	}

	chatReq := httptest.NewRequest(http.MethodPost, "/integration/v1/llm/v1/chat/completions", strings.NewReader(`{"model":"kimi-3.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	chatReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		chatReq.Header.Set(k, v)
	}
	chatRec := httptest.NewRecorder()
	r.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for chat completions, got %d: %s", chatRec.Code, chatRec.Body.String())
	}
	if chatRec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("expected X-Accel-Buffering: no, got %q", chatRec.Header().Get("X-Accel-Buffering"))
	}
	if !strings.Contains(chatRec.Body.String(), `"content":"你好"`) || !strings.Contains(chatRec.Body.String(), "data: [DONE]") {
		t.Fatalf("unexpected SSE body: %s", chatRec.Body.String())
	}
	if capturedChatBody["user"] != link.ImmutableSubject {
		t.Fatalf("expected injected user %q, got %v", link.ImmutableSubject, capturedChatBody["user"])
	}
}
