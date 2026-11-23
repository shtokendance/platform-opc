package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
)

// listServicesWithSchemas enriches only the policy-approved names, using the
// same user's metadata snapshot. No database-wide catalog or credentials escape.
func (h *MCPHandler) listServicesWithSchemas(c *gin.Context, token, tenant, subject string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	raw, err := h.upstream.invoke(ctx, http.MethodGet, "/api/v1/integration/mcp/services?actor_kind="+mcpActorKindUser, token, tenant, nil)
	if err != nil {
		denyUpstream(c, err)
		return
	}
	var catalog struct {
		Services []map[string]interface{} `json:"services"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		denyUpstream(c, fmt.Errorf("%w: invalid MCP catalog", errUpstreamUnavailable))
		return
	}
	for _, service := range catalog.Services {
		id, ok := service["service_id"].(string)
		if !ok || !validResourceID(id) {
			denyUpstream(c, fmt.Errorf("%w: invalid MCP service ID", errUpstreamUnavailable))
			return
		}
		rows, ok := service["tools"].([]interface{})
		if !ok {
			denyUpstream(c, fmt.Errorf("%w: invalid MCP tool catalog", errUpstreamUnavailable))
			return
		}
		metadata, err := h.upstream.invoke(ctx, http.MethodGet, "/api/v1/mcp-services/"+url.PathEscape(id)+"/metadata", token, tenant, nil)
		if err != nil {
			denyUpstream(c, err)
			return
		}
		var snapshot struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if json.Unmarshal(metadata, &snapshot) != nil {
			denyUpstream(c, fmt.Errorf("%w: invalid MCP metadata", errUpstreamUnavailable))
			return
		}
		schemas := map[string]json.RawMessage{}
		for _, tool := range snapshot.Tools {
			schemas[tool.Name] = tool.InputSchema
		}
		for i, row := range rows {
			tool, ok := row.(map[string]interface{})
			if !ok {
				if name, stringRow := row.(string); stringRow {
					tool = map[string]interface{}{"name": name}
				} else {
					denyUpstream(c, fmt.Errorf("%w: invalid MCP tool", errUpstreamUnavailable))
					return
				}
			}
			name, _ := tool["name"].(string)
			schema := schemas[name]
			var object map[string]interface{}
			if len(schema) == 0 || json.Unmarshal(schema, &object) != nil || object == nil {
				denyUpstream(c, fmt.Errorf("%w: MCP schema unavailable", errUpstreamUnavailable))
				return
			}
			tool["input_schema"] = object
			rows[i] = tool
		}
	}
	auditEvent(ctx, h.storage, subject, "integration.mcp.services", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{"tenant_id": tenant, "result": "authorized-schemas"})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": catalog})
}

// listServicesByServiceToken 服务 token（code 后端）查询受管 MCP 服务目录及工具 Schema。
// 支持针对特定 tenant 查询，并在未找到时回退到交付中心企业租户（296492）。
// 响应同时兼容顶层 services 字段与 data.services 包装。
func (h *MCPHandler) listServicesByServiceToken(c *gin.Context, svcSubject string) {
	tenantStr := c.Query("tenant")
	var tenantID uint64
	if tenantStr != "" {
		tenantID, _ = strconv.ParseUint(tenantStr, 10, 64)
	}

	ctx := c.Request.Context()
	var services []model.WeKnoraMCPService
	var err error

	if tenantID > 0 {
		services, err = h.storage.ListMCPServicesByTenant(ctx, tenantID)
	}
	// Fallback to enterprise tech delivery center (296492) if no services found for requested tenant
	if (len(services) == 0 || err != nil) && tenantID != 296492 {
		services, err = h.storage.ListMCPServicesByTenant(ctx, 296492)
	}
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "failed to list mcp services: "+err.Error())
		return
	}

	catalogServices := make([]map[string]interface{}, 0, len(services))
	for _, svc := range services {
		toolsList := make([]map[string]interface{}, 0)
		meta, err := h.storage.GetMCPMetadataByService(ctx, svc.TenantID, svc.ID)
		if (err != nil || meta == nil || meta.Tools == "") && svc.TenantID != 296492 {
			meta, err = h.storage.GetMCPMetadataByService(ctx, 296492, svc.ID)
		}
		if err == nil && meta != nil && meta.Tools != "" {
			var rawTools []map[string]interface{}
			if err := json.Unmarshal([]byte(meta.Tools), &rawTools); err == nil {
				for _, t := range rawTools {
					if schema, ok := t["inputSchema"]; ok {
						t["input_schema"] = schema
					} else if schema, ok := t["input_schema"]; ok {
						t["inputSchema"] = schema
					}
					toolsList = append(toolsList, t)
				}
			}
		}

		catalogServices = append(catalogServices, map[string]interface{}{
			"service_id":  svc.ID,
			"name":        svc.Name,
			"description": svc.Description,
			"tools":       toolsList,
		})
	}

	auditEvent(ctx, h.storage, svcSubject, "integration.mcp.services", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result": "service_token_listed",
		"tenant": tenantStr,
		"count":  len(catalogServices),
	})

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"services": catalogServices,
		"data": gin.H{
			"services": catalogServices,
		},
	})
}
