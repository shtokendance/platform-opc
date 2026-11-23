package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
)

func (h *MCPHandler) tenantDisplayName(ctx context.Context, tenantID string) string {
	numeric, err := strconv.ParseUint(tenantID, 10, 64)
	if err != nil {
		return tenantID
	}
	names, err := h.storage.ListWeKnoraTenantNames(ctx, []uint64{numeric})
	if err != nil {
		return tenantID
	}
	if name := names[numeric]; name != "" {
		return name
	}
	return tenantID
}

func (h *MCPHandler) fetchTenantServices(ctx context.Context, token, tenant string, includeSchema bool) ([]map[string]interface{}, error) {
	raw, err := h.upstream.invoke(ctx, http.MethodGet, "/api/v1/integration/mcp/services?actor_kind="+mcpActorKindUser, token, tenant, nil)
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Services []map[string]interface{} `json:"services"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("%w: invalid MCP catalog", errUpstreamUnavailable)
	}
	if includeSchema {
		for _, service := range catalog.Services {
			id, ok := service["service_id"].(string)
			if !ok || !validResourceID(id) {
				return nil, fmt.Errorf("%w: invalid MCP service ID", errUpstreamUnavailable)
			}
			rows, ok := service["tools"].([]interface{})
			if !ok {
				return nil, fmt.Errorf("%w: invalid MCP tool catalog", errUpstreamUnavailable)
			}
			metadata, err := h.upstream.invoke(ctx, http.MethodGet, "/api/v1/mcp-services/"+url.PathEscape(id)+"/metadata", token, tenant, nil)
			if err != nil {
				return nil, err
			}
			var snapshot struct {
				Tools []struct {
					Name        string          `json:"name"`
					InputSchema json.RawMessage `json:"inputSchema"`
				} `json:"tools"`
			}
			if json.Unmarshal(metadata, &snapshot) != nil {
				return nil, fmt.Errorf("%w: invalid MCP metadata", errUpstreamUnavailable)
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
						return nil, fmt.Errorf("%w: invalid MCP tool", errUpstreamUnavailable)
					}
				}
				name, _ := tool["name"].(string)
				schema := schemas[name]
				var object map[string]interface{}
				if len(schema) == 0 || json.Unmarshal(schema, &object) != nil || object == nil {
					return nil, fmt.Errorf("%w: MCP schema unavailable", errUpstreamUnavailable)
				}
				tool["input_schema"] = object
				rows[i] = tool
			}
		}
	}
	return catalog.Services, nil
}

func (h *MCPHandler) listAllSpaceServices(c *gin.Context, sess *accessSession, includeSchema bool) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	tenants, err := entitledTenants(ctx, h.storage, sess.link)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	if len(tenants) == 0 {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "subject has no active entitlement")
		return
	}

	failures := make([]tenantFailure, 0)
	services := make([]map[string]interface{}, 0)
	bestIdx := make(map[string]int)

	for _, tenant := range tenants {
		token, terr := h.bridge.GetOrIssueUpstreamToken(ctx, sess.link.ID, tenant, mcpBridgeTokenTTL)
		if terr != nil {
			if errors.Is(terr, kbridge.ErrAdminTokenRefused) {
				writeErrorEnvelope(c, http.StatusForbidden, errCodeSystemAdminAccount, systemAdminAccountMessage)
				return
			}
			failures = append(failures, tenantFailure{TenantID: tenant, Code: bridgeFailureCode(terr)})
			continue
		}
		list, lerr := h.fetchTenantServices(ctx, token, tenant, includeSchema)
		if lerr != nil {
			var uerr *upstreamError
			code := errCodeUpstreamUnavailable
			if errors.As(lerr, &uerr) {
				code = uerr.Code
			}
			failures = append(failures, tenantFailure{TenantID: tenant, Code: code})
			continue
		}
		tenantName := h.tenantDisplayName(ctx, tenant)
		for _, svc := range list {
			id, _ := svc["service_id"].(string)
			if id == "" {
				continue
			}
			svc["tenant_id"] = tenant
			svc["tenant_name"] = tenantName
			sourceTenant, _ := svc["source_tenant_id"].(string)
			if sourceTenant != "" && sourceTenant != tenant {
				if sourceName, _ := svc["source_tenant_name"].(string); sourceName == "" {
					svc["source_tenant_name"] = h.tenantDisplayName(ctx, sourceTenant)
				}
			}
			owner := sourceTenant
			if owner == "" {
				owner = tenant
			}
			permission, _ := svc["permission"].(string)
			key := owner + "\x00" + id
			if idx, exists := bestIdx[key]; exists {
				cur := services[idx]
				curTenant, _ := cur["tenant_id"].(string)
				curSource, _ := cur["source_tenant_id"].(string)
				curPerm, _ := cur["permission"].(string)
				curRank := resourceAccessRank(curTenant, curSource, curPerm)
				newRank := resourceAccessRank(tenant, sourceTenant, permission)
				if newRank > curRank || (newRank == curRank && tenant < curTenant) {
					services[idx] = svc
				}
				continue
			}
			bestIdx[key] = len(services)
			services = append(services, svc)
		}
	}

	if len(failures) == len(tenants) {
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "all entitled spaces failed to load mcp services")
		return
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.mcp.services", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"all_spaces": true,
		"tenants":    len(tenants),
		"count":      len(services),
		"failures":   len(failures),
		"result":     "authorized-all-spaces",
	})
	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"complete": len(failures) == 0,
		"failures": failures,
		"data":     gin.H{"services": services},
	})
}

// listServicesWithSchemas enriches only the policy-approved names, using the
// same user's metadata snapshot. No database-wide catalog or credentials escape.
func (h *MCPHandler) listServicesWithSchemas(c *gin.Context, token, tenant, subject string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	services, err := h.fetchTenantServices(ctx, token, tenant, true)
	if err != nil {
		denyUpstream(c, err)
		return
	}
	auditEvent(ctx, h.storage, subject, "integration.mcp.services", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{"tenant_id": tenant, "result": "authorized-schemas"})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"services": services}})
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
