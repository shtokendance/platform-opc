package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestStorage(t *testing.T) (*storage.Storage, []byte) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(
		&model.WeKnoraKnowledgeBase{},
		&model.WeKnoraKnowledge{},
		&model.WeKnoraChunk{},
		&model.WeKnoraModel{},
	)
	if err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}

	aesKey := []byte("12345678901234567890123456789012") // 32 bytes
	s := storage.NewStorageWithDBs(db, db)
	return s, aesKey
}

func TestEmbeddingClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("path = %s, want /embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1, 0.2, 0.3]}]}`))
	}))
	defer server.Close()

	client := NewEmbeddingClient(server.URL, "test-api-key", "text-embedding-3-small")
	vec, err := client.GetEmbedding(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("GetEmbedding failed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 || vec[1] != 0.2 || vec[2] != 0.3 {
		t.Fatalf("unexpected embedding result: %v", vec)
	}
}

func TestRerankClient_Formats(t *testing.T) {
	// 格式 1: Cohere / DashScope results 格式
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.95},{"index":1,"relevance_score":0.42}]}`))
	}))
	defer server1.Close()

	client1 := NewRerankClient(server1.URL, "key", "bge-reranker-large")
	scores1, err := client1.Rerank(context.Background(), "query", []string{"doc1", "doc2"})
	if err != nil {
		t.Fatalf("rerank 1 failed: %v", err)
	}
	if len(scores1) != 2 || scores1[0].Score != 0.95 || scores1[1].Score != 0.42 {
		t.Fatalf("unexpected scores1: %v", scores1)
	}

	// 格式 2: TEI 数组格式
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"index":0,"score":0.88}]`))
	}))
	defer server2.Close()

	client2 := NewRerankClient(server2.URL, "key", "bge-reranker-large")
	scores2, err := client2.Rerank(context.Background(), "query", []string{"doc1"})
	if err != nil {
		t.Fatalf("rerank 2 failed: %v", err)
	}
	if len(scores2) != 1 || scores2[0].Score != 0.88 {
		t.Fatalf("unexpected scores2: %v", scores2)
	}
}

func TestSearchService_WithDirectDBAndRerank(t *testing.T) {
	s, aesKey := setupTestStorage(t)

	// 1. 模拟外部 Rerank 推理服务
	rerankServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string   `json:"query"`
			Texts []string `json:"texts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		// 将第二个切片打高分，第一个打低分，以检验重排重新排序生效
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.35},{"index":1,"relevance_score":0.98}]}`))
	}))
	defer rerankServer.Close()

	// 2. 插入 Rerank 模型（加密 APIKey）
	encKey, _ := crypto.EncryptAESGCM("secret-api-key", aesKey)
	paramsJSON, _ := json.Marshal(map[string]interface{}{
		"base_url": rerankServer.URL,
		"api_key":  encKey,
		"model":    "bge-reranker-large",
	})
	if err := s.DB().Create(&model.WeKnoraModel{
		ID:         "model-rerank-1",
		TenantID:   1001,
		Name:       "bge-reranker-large",
		Type:       "rerank",
		Source:     "remote",
		Status:     "active",
		IsDefault:  true,
		Parameters: string(paramsJSON),
	}).Error; err != nil {
		t.Fatalf("create rerank model failed: %v", err)
	}

	// 3. 插入知识库、文档与切片数据
	if err := s.DB().Create(&model.WeKnoraKnowledgeBase{
		ID:          "kb-1",
		TenantID:    1001,
		Name:        "工程技术库",
		Description: "系统设计文档",
	}).Error; err != nil {
		t.Fatalf("create kb failed: %v", err)
	}

	if err := s.DB().Create(&model.WeKnoraKnowledge{
		ID:              "doc-1",
		TenantID:        1001,
		KnowledgeBaseID: "kb-1",
		Title:           "Go并发设计模式",
	}).Error; err != nil {
		t.Fatalf("create doc failed: %v", err)
	}

	now := time.Now()
	chunks := []model.WeKnoraChunk{
		{
			ID:              "chunk-1",
			TenantID:        1001,
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			Content:         "Go 语言中的 Goroutine 与 Channel 模式实践指南",
			ChunkIndex:      0,
			IsEnabled:       true,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		{
			ID:              "chunk-2",
			TenantID:        1001,
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			Content:         "并发上下文同步与 Worker Pool 协程池架构实践指南",
			ChunkIndex:      1,
			IsEnabled:       true,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		{
			ID:              "chunk-other-tenant",
			TenantID:        9999, // 跨空间切片，绝对不能被查出
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			Content:         "机密数据实践指南",
			ChunkIndex:      0,
			IsEnabled:       true,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
	}
	for _, c := range chunks {
		if err := s.DB().Create(&c).Error; err != nil {
			t.Fatalf("create chunk failed: %v", err)
		}
	}

	// 4. 执行检索
	svc := NewSearchService(s, aesKey)
	hits, err := svc.Search(context.Background(), 1001, "kb-1", nil, "实践指南", 5)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	// 验证经过 Rerank 之后，原本第 2 个切片因 0.98 得分排到了最前面！
	if hits[0].ChunkID != "chunk-2" || hits[0].Score != 0.98 {
		t.Fatalf("top hit should be chunk-2 with score 0.98, got: %+v", hits[0])
	}
	if hits[1].ChunkID != "chunk-1" || hits[1].Score != 0.35 {
		t.Fatalf("second hit should be chunk-1 with score 0.35, got: %+v", hits[1])
	}

	// 验证元数据透传完整性
	if hits[0].KnowledgeBaseName != "工程技术库" || hits[0].Title != "Go并发设计模式" {
		t.Fatalf("metadata incomplete: %+v", hits[0])
	}

	// 验证跨租户绝对隔离
	for _, h := range hits {
		if h.ChunkID == "chunk-other-tenant" || h.SourceTenant != "1001" {
			t.Fatalf("cross-tenant chunk leaked: %+v", h)
		}
	}
}
