package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// getKnowledgeContent adds authorized parsed text to the metadata-only upstream
// response. It never downloads the original file (a stronger permission boundary).
// The opt-in integration query preserves existing metadata consumers.
func (u *knowledgeUpstream) getKnowledgeContent(ctx context.Context, bearer, tenant, id string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	original, err := u.getKnowledge(ctx, bearer, tenant, id)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(original, &doc); err != nil {
		return nil, fmt.Errorf("%w: invalid document", errUpstreamUnavailable)
	}
	var kb string
	if err := json.Unmarshal(doc["knowledge_base_id"], &kb); err != nil || kb == "" || !knowledgeMatchesScope(original, tenant, kb) {
		return nil, &upstreamError{Status: http.StatusForbidden, Code: errCodeScopeMismatch, Message: "document ownership mismatch"}
	}
	if raw, ok := doc["content"]; ok {
		var content string
		if json.Unmarshal(raw, &content) == nil {
			return original, nil
		}
	}
	type chunk struct {
		ID              string `json:"id"`
		KnowledgeID     string `json:"knowledge_id"`
		KnowledgeBaseID string `json:"knowledge_base_id"`
		TenantID        uint64 `json:"tenant_id"`
		Content         string `json:"content"`
		Index           int    `json:"chunk_index"`
		Enabled         bool   `json:"is_enabled"`
	}
	chunks := []chunk{}
	seen := map[string]bool{}
	bytes := 0
	for page := 1; ; page++ {
		if page > 100 {
			return nil, fmt.Errorf("%w: document exceeds chunk limit", errUpstreamUnavailable)
		}
		raw, err := u.invoke(ctx, http.MethodGet, fmt.Sprintf("/api/v1/chunks/%s?page=%d&page_size=100&chunk_type=text", url.PathEscape(id), page), bearer, tenant, nil)
		if err != nil {
			return nil, err
		}
		var rows []chunk
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("%w: invalid chunks", errUpstreamUnavailable)
		}
		for _, row := range rows {
			if row.KnowledgeID != id || row.KnowledgeBaseID != kb || fmt.Sprint(row.TenantID) != tenant {
				return nil, &upstreamError{Status: http.StatusForbidden, Code: errCodeScopeMismatch, Message: "chunk ownership mismatch"}
			}
			if row.ID == "" || seen[row.ID] {
				return nil, fmt.Errorf("%w: duplicate chunk page", errUpstreamUnavailable)
			}
			seen[row.ID] = true
			if !row.Enabled {
				continue
			}
			bytes += len(row.Content)
			if bytes > 8<<20 {
				return nil, fmt.Errorf("%w: document exceeds text limit", errUpstreamUnavailable)
			}
			chunks = append(chunks, row)
		}
		if len(rows) < 100 {
			break
		}
	}
	// Recheck authorization and ownership after collecting pages. A document moved
	// or modified during the read must not expose mixed-version text under old scope.
	latest, err := u.getKnowledge(ctx, bearer, tenant, id)
	if err != nil {
		return nil, err
	}
	var final map[string]json.RawMessage
	if json.Unmarshal(latest, &final) != nil || !knowledgeMatchesScope(latest, tenant, kb) || string(final["updated_at"]) != string(doc["updated_at"]) {
		return nil, &upstreamError{Status: http.StatusForbidden, Code: errCodeScopeMismatch, Message: "document changed during content read"}
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })
	parts := make([]string, 0, len(chunks))
	for _, row := range chunks {
		parts = append(parts, row.Content)
	}
	final["content"], _ = json.Marshal(strings.Join(parts, "\n\n"))
	final["content_source"] = json.RawMessage(`"text_chunks"`)
	final["content_chunk_count"], _ = json.Marshal(len(chunks))
	return json.Marshal(final)
}
