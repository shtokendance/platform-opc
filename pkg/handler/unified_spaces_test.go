package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUnifiedSpace_ResourcesKBDedupesOwnedAndMultiShared(t *testing.T) {
	fake := newFakeKnowledge()
	fake.sharedKBsByTenant = map[string][]map[string]interface{}{}
	fake.kbListByTenant["1001"] = []map[string]interface{}{
		{"id": "kb-own", "name": "研发规范库"},
	}
	fake.sharedKBsByTenant["1001"] = []map[string]interface{}{
		{"knowledge_base": map[string]interface{}{"id": "kb-ext", "name": "财务共享库"}, "source_tenant_id": 1003, "permission": "viewer"},
	}
	fake.kbListByTenant["1002"] = []map[string]interface{}{
		{"id": "kb-b", "name": "产品文档库"},
	}
	fake.sharedKBsByTenant["1002"] = []map[string]interface{}{
		{"knowledge_base": map[string]interface{}{"id": "kb-own", "name": "研发规范库"}, "source_tenant_id": 1001, "permission": "viewer"},
		{"knowledge_base": map[string]interface{}{"id": "kb-ext", "name": "财务共享库"}, "source_tenant_id": 1003, "permission": "editor"},
	}

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-unified", "ku-unified", "1001", "1002")
	seedTenantRow(t, s, 1001, "研发空间")
	seedTenantRow(t, s, 1002, "产品空间")
	seedTenantRow(t, s, 1003, "财务空间")

	w := doGet(t, r, "/integration/v1/resources/knowledge-bases?include_shared=true", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["complete"] != true {
		t.Fatalf("complete = %v", body["complete"])
	}
	if body["total"] != float64(3) {
		t.Fatalf("total = %v, want 3, body=%s", body["total"], w.Body.String())
	}
	items := body["items"].([]interface{})
	byID := map[string]map[string]interface{}{}
	for _, raw := range items {
		it := raw.(map[string]interface{})
		byID[it["knowledge_base_id"].(string)] = it
	}

	own := byID["kb-own"]
	if own == nil || own["tenant_id"] != "1001" || own["source_tenant_id"] != "1001" {
		t.Fatalf("kb-own should prefer owned entry in 1001, got %v", own)
	}
	ext := byID["kb-ext"]
	if ext == nil || ext["tenant_id"] != "1002" || ext["source_tenant_id"] != "1003" || ext["permission"] != "editor" {
		t.Fatalf("kb-ext should prefer higher-permission entry in 1002, got %v", ext)
	}
	if ext["source_tenant_name"] != "财务空间" {
		t.Fatalf("kb-ext source_tenant_name = %v", ext["source_tenant_name"])
	}
	b := byID["kb-b"]
	if b == nil || b["tenant_id"] != "1002" || b["tenant_name"] != "产品空间" {
		t.Fatalf("kb-b = %v", b)
	}
}

func TestUnifiedSpace_KnowledgeSearchAllSpacesDedupesSharedKB(t *testing.T) {
	fake := newFakeKnowledge()
	fake.sharedKBsByTenant = map[string][]map[string]interface{}{}
	fake.kbListByTenant["1001"] = []map[string]interface{}{
		{"id": "kb-own", "name": "研发规范库"},
	}
	fake.sharedKBsByTenant["1001"] = []map[string]interface{}{
		{"knowledge_base": map[string]interface{}{"id": "kb-ext", "name": "财务共享库"}, "source_tenant_id": 1003, "permission": "viewer"},
	}
	fake.kbListByTenant["1002"] = []map[string]interface{}{}
	fake.sharedKBsByTenant["1002"] = []map[string]interface{}{
		{"knowledge_base": map[string]interface{}{"id": "kb-own", "name": "研发规范库"}, "source_tenant_id": 1001, "permission": "viewer"},
		{"knowledge_base": map[string]interface{}{"id": "kb-ext", "name": "财务共享库"}, "source_tenant_id": 1003, "permission": "editor"},
	}
	fake.searchHits = []map[string]interface{}{
		{"id": "chunk-1", "knowledge_id": "kn-1", "knowledge_title": "制度文档", "content": "命中片段", "score": 0.9},
	}

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-search", "ku-search", "1001", "1002")
	seedTenantRow(t, s, 1001, "研发空间")
	seedTenantRow(t, s, 1002, "产品空间")
	seedTenantRow(t, s, 1003, "财务空间")

	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"all_spaces":     true,
		"include_shared": true,
		"query":          "制度",
	}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	results := body["results"].([]interface{})
	if len(results) != 2 {
		t.Fatalf("expected 2 deduplicated KB searches (2 hits), got %d: %s", len(results), w.Body.String())
	}

	hitByKB := map[string]map[string]interface{}{}
	for _, raw := range results {
		h := raw.(map[string]interface{})
		hitByKB[h["knowledge_base_id"].(string)] = h
	}
	if hitByKB["kb-own"]["tenant_id"] != "1001" || hitByKB["kb-own"]["source_tenant"] != "1001" {
		t.Fatalf("kb-own hit = %v", hitByKB["kb-own"])
	}
	if hitByKB["kb-ext"]["tenant_id"] != "1002" || hitByKB["kb-ext"]["source_tenant"] != "1003" {
		t.Fatalf("kb-ext hit = %v", hitByKB["kb-ext"])
	}
}

func TestUnifiedSpace_MCPListAllSpacesIncludesTenantAndDedupesShared(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
	link := seedKnowledgeSubject(t, s, "subj-mcp", "ku-mcp", "1001", "1002")
	seedTenantRow(t, s, 1001, "研发空间")
	seedTenantRow(t, s, 1002, "产品空间")

	fake.routes["GET /api/v1/integration/mcp/services"] = fakeMCPResponse{
		status: http.StatusOK,
		body:   `{"success":true,"data":{"services":[{"service_id":"svc-shared","name":"共享服务","tools":[{"name":"t1"}]}]}}`,
	}
	fake.routes["GET /api/v1/mcp-services/svc-shared/metadata"] = fakeMCPResponse{
		status: http.StatusOK,
		body:   `{"success":true,"data":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}]}}`,
	}

	w := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/services?all_spaces=true&include_schema=true", "", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["complete"] != true {
		t.Fatalf("complete = %v, body=%s", body["complete"], w.Body.String())
	}
	data := body["data"].(map[string]interface{})
	services := data["services"].([]interface{})
	if len(services) != 2 {
		t.Fatalf("expected 2 independent tenant services (1001 & 1002), got %d: %s", len(services), w.Body.String())
	}
	tenants := map[string]string{}
	for _, raw := range services {
		svc := raw.(map[string]interface{})
		tenants[svc["tenant_id"].(string)] = svc["tenant_name"].(string)
	}
	if tenants["1001"] != "研发空间" || tenants["1002"] != "产品空间" {
		t.Fatalf("tenants = %v", tenants)
	}

	fake.routes["GET /api/v1/integration/mcp/services"] = fakeMCPResponse{
		status: http.StatusOK,
		body:   `{"success":true,"data":{"services":[{"service_id":"svc-shared","source_tenant_id":"1001","name":"共享服务","tools":[{"name":"t1"}]}]}}`,
	}
	w2 := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/services?all_spaces=true&include_schema=true", "", platformAccessHeaders(t, s, link.ID))
	if w2.Code != http.StatusOK {
		t.Fatalf("status2 = %d, body=%s", w2.Code, w2.Body.String())
	}
	body2 := decodeBody(t, w2)
	services2 := body2["data"].(map[string]interface{})["services"].([]interface{})
	if len(services2) != 1 {
		t.Fatalf("expected 1 deduplicated shared MCP service, got %d: %s", len(services2), w2.Body.String())
	}
	svc := services2[0].(map[string]interface{})
	if svc["tenant_id"] != "1001" || svc["source_tenant_id"] != "1001" {
		t.Fatalf("deduped mcp = %v", svc)
	}
}
