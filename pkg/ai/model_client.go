package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EmbeddingClient 负责调用向量嵌入模型（支持 OpenAI 规范接口）
type EmbeddingClient struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewEmbeddingClient(baseURL, apiKey, model string) *EmbeddingClient {
	return &EmbeddingClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *EmbeddingClient) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	reqBody := map[string]interface{}{
		"model": c.Model,
		"input": text,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API status %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if len(res.Data) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return res.Data[0].Embedding, nil
}

// RerankScore 重排得分项
type RerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// RerankClient 负责调用重排模型（支持 TEI / Cohere / DashScope / BGE 规范接口）
type RerankClient struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewRerankClient(baseURL, apiKey, model string) *RerankClient {
	return &RerankClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *RerankClient) Rerank(ctx context.Context, query string, texts []string) ([]RerankScore, error) {
	if len(texts) == 0 {
		return []RerankScore{}, nil
	}
	reqBody := map[string]interface{}{
		"model":      c.Model,
		"query":      query,
		"texts":      texts,
		"documents":  texts,
		"top_n":      len(texts),
		"raw_scores": false,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/rerank"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("rerank API status %d: %s", resp.StatusCode, string(body))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 格式 1 (Cohere / BGE / DashScope): {"results": [{"index": 0, "relevance_score": 0.95}]}
	var standardResp struct {
		Results []struct {
			Index          int     `json:"index"`
			Score          float64 `json:"score"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &standardResp); err == nil && len(standardResp.Results) > 0 {
		scores := make([]RerankScore, 0, len(standardResp.Results))
		for _, r := range standardResp.Results {
			s := r.Score
			if s == 0 && r.RelevanceScore != 0 {
				s = r.RelevanceScore
			}
			scores = append(scores, RerankScore{Index: r.Index, Score: s})
		}
		return scores, nil
	}

	// 格式 2 (TEI 直接数组): [{"index": 0, "score": 0.95}]
	var listResp []struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal(respBody, &listResp); err == nil && len(listResp) > 0 {
		scores := make([]RerankScore, 0, len(listResp))
		for _, r := range listResp {
			scores = append(scores, RerankScore{Index: r.Index, Score: r.Score})
		}
		return scores, nil
	}

	return nil, fmt.Errorf("unrecognized rerank response format: %s", string(respBody))
}
