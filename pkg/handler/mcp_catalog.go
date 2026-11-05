package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

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
