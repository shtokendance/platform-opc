package ai

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"weknora-tokendance/pkg/storage"
)

// SearchResultHit 直连知识检索命中结果
type SearchResultHit struct {
	KnowledgeID       string  `json:"knowledge_id"`
	ChunkID           string  `json:"chunk_id"`
	Title             string  `json:"title"`
	KnowledgeBaseID   string  `json:"knowledge_base_id"`
	KnowledgeBaseName string  `json:"knowledge_base_name"`
	SourceTenant      string  `json:"source_tenant"`
	Content           string  `json:"content"`
	Score             float64 `json:"score"`
	StartAt           int     `json:"start_at"`
	EndAt             int     `json:"end_at"`
	ChunkIndex        int     `json:"chunk_index"`
}

// SearchService 知识库混合检索与动态重排编排服务
type SearchService struct {
	storage *storage.Storage
	aesKey  []byte
}

func NewSearchService(s *storage.Storage, aesKey []byte) *SearchService {
	return &SearchService{
		storage: s,
		aesKey:  aesKey,
	}
}

func (s *SearchService) Search(ctx context.Context, tenantID uint64, kbID string, knowledgeIDs []string, query string, matchCount int) ([]SearchResultHit, error) {
	if matchCount <= 0 {
		matchCount = 10
	}

	// 1. 粗排召回：直接从数据库查询（严格按 tenant_id 隔离）
	// 取出候选切片（召回最多 matchCount * 4，且不低于 20 条供精排重排）
	recallLimit := matchCount * 4
	if recallLimit < 20 {
		recallLimit = 20
	}
	candidates, err := s.storage.SearchChunks(ctx, tenantID, kbID, knowledgeIDs, query, recallLimit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return []SearchResultHit{}, nil
	}

	// 2. 尝试获取租户或全局的 Rerank 模型
	rerankModel, params, err := s.storage.GetModelByTypeAndTenant(ctx, tenantID, "rerank", s.aesKey)
	hasRerank := (err == nil && rerankModel != nil)

	hits := make([]SearchResultHit, len(candidates))
	chunkTexts := make([]string, len(candidates))
	tenantStr := strconv.FormatUint(tenantID, 10)

	for i, c := range candidates {
		kbName := c.KnowledgeBaseName
		if kbName == "" {
			kbName = c.Chunk.KnowledgeBaseID
		}
		title := c.KnowledgeTitle
		if title == "" {
			title = c.Chunk.KnowledgeID
		}
		hits[i] = SearchResultHit{
			KnowledgeID:       c.Chunk.KnowledgeID,
			ChunkID:           c.Chunk.ID,
			Title:             title,
			KnowledgeBaseID:   c.Chunk.KnowledgeBaseID,
			KnowledgeBaseName: kbName,
			SourceTenant:      tenantStr,
			Content:           c.Chunk.Content,
			StartAt:           c.Chunk.StartAt,
			EndAt:             c.Chunk.EndAt,
			ChunkIndex:        c.Chunk.ChunkIndex,
			Score:             0.5, // 默认基准得分
		}
		chunkTexts[i] = c.Chunk.Content
	}

	// 3. 精排：若配置了 Rerank 模型，则动态调用 Rerank 重新计分
	if hasRerank {
		baseURL, _ := params["base_url"].(string)
		apiKey, _ := params["api_key"].(string)
		modelName := rerankModel.Name
		if m, ok := params["model"].(string); ok && m != "" {
			modelName = m
		}
		if baseURL != "" {
			rerankClient := NewRerankClient(baseURL, apiKey, modelName)
			scores, rerr := rerankClient.Rerank(ctx, query, chunkTexts)
			if rerr == nil && len(scores) > 0 {
				for _, sc := range scores {
					if sc.Index >= 0 && sc.Index < len(hits) {
						hits[sc.Index].Score = sc.Score
					}
				}
			}
		}
	} else {
		// 无 Rerank 时，基于文本简单关键词词频计算粗略得分
		foldedQ := strings.ToLower(strings.TrimSpace(query))
		for i, h := range hits {
			foldedContent := strings.ToLower(h.Content)
			count := strings.Count(foldedContent, foldedQ)
			hits[i].Score = 0.5 + float64(count)*0.1
			if hits[i].Score > 0.99 {
				hits[i].Score = 0.99
			}
		}
	}

	// 4. 按得分降序排序并截取 Top N
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Score > hits[j].Score
	})

	if len(hits) > matchCount {
		hits = hits[:matchCount]
	}

	return hits, nil
}
