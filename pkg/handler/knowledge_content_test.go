package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKnowledgeContent_AuthorizedOrderedTextAndRecheck(t *testing.T) {
	for _, scenario := range []string{"ok", "moved", "chunk-mismatch", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			metadataReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer user-token" || r.Header.Get("X-Tenant-ID") != "1001" {
					t.Error("missing user authorization context")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/knowledge/doc" {
					metadataReads++
					if scenario == "revoked" && metadataReads == 2 {
						w.WriteHeader(403)
						fmt.Fprint(w, `{"error":{"code":"FORBIDDEN"}}`)
						return
					}
					kb := "kb"
					if scenario == "moved" && metadataReads == 2 {
						kb = "other"
					}
					fmt.Fprintf(w, `{"success":true,"data":{"id":"doc","tenant_id":1001,"knowledge_base_id":%q,"title":"Title","updated_at":"v1"}}`, kb)
					return
				}
				if r.URL.Path != "/api/v1/chunks/doc" || r.URL.Query().Get("page_size") != "100" {
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(404)
					return
				}
				kb := "kb"
				if scenario == "chunk-mismatch" {
					kb = "other"
				}
				fmt.Fprintf(w, `{"success":true,"data":[{"id":"two","knowledge_id":"doc","knowledge_base_id":%q,"tenant_id":1001,"chunk_index":2,"is_enabled":true,"content":"second"},{"id":"one","knowledge_id":"doc","knowledge_base_id":"kb","tenant_id":1001,"chunk_index":1,"is_enabled":true,"content":"first"},{"id":"off","knowledge_id":"doc","knowledge_base_id":"kb","tenant_id":1001,"chunk_index":0,"is_enabled":false,"content":"hidden"}],"meta":{"total":3}}`, kb)
			}))
			defer server.Close()
			upstream := &knowledgeUpstream{baseURL: server.URL, client: server.Client()}
			data, err := upstream.getKnowledgeContent(context.Background(), "user-token", "1001", "doc")
			if scenario != "ok" {
				if err == nil || len(data) != 0 {
					t.Fatalf("must not expose text: data=%s err=%v", data, err)
				}
				var upstreamErr *upstreamError
				if !errors.As(err, &upstreamErr) {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]interface{}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc["content"] != "first\n\nsecond" || doc["content_source"] != "text_chunks" || metadataReads != 2 {
				t.Fatalf("unexpected document %s", data)
			}
		})
	}
}

func TestKnowledgeContent_PaginatesWithoutUsingSearchSnippets(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/knowledge/") {
			fmt.Fprint(w, `{"success":true,"data":{"tenant_id":1001,"knowledge_base_id":"kb"}}`)
			return
		}
		pages++
		rows := []map[string]interface{}{}
		if pages == 1 {
			for i := 0; i < 100; i++ {
				rows = append(rows, map[string]interface{}{"id": fmt.Sprint(i), "knowledge_id": "doc", "knowledge_base_id": "kb", "tenant_id": 1001, "chunk_index": i, "is_enabled": true, "content": "text"})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": rows})
	}))
	defer server.Close()
	u := &knowledgeUpstream{baseURL: server.URL, client: server.Client()}
	raw, err := u.getKnowledgeContent(context.Background(), "user-token", "1001", "doc")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(raw, &doc)
	if pages != 2 || doc["content_chunk_count"] != float64(100) {
		t.Fatalf("wrong pagination pages=%d data=%s", pages, raw)
	}
}
