package main

import (
	"html/template"
	"strings"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/web"
)

func mustNavTemplate(t *testing.T) *template.Template {
	t.Helper()
	content, err := web.AdminFS.ReadFile("nav.html")
	if err != nil {
		t.Fatalf("read embedded nav.html: %v", err)
	}
	tmpl, err := template.New("nav").Parse(string(content))
	if err != nil {
		t.Fatalf("parse nav template: %v", err)
	}
	return tmpl
}

func TestRenderNavPage_AllConfigured(t *testing.T) {
	cfg := &config.Config{}
	cfg.Admin.Nav = config.NavConfig{
		LLMGatewayURL: "https://opc-litellm.benlai.com",
		MCPAdminURL:   "https://opc-mcp-admin.benlai.com",
		MCPGatewayURL: "https://opc-mcp-admin.benlai.com/mcp-gateway",
		KnowledgeURL:  "https://aiwork.benlai.com",
		LangfuseURL:   "https://langfuse.tokendance.sh",
		DeployPath:    "/admin/deploy",
	}

	out, err := renderNavPage(mustNavTemplate(t), cfg)
	if err != nil {
		t.Fatalf("renderNavPage: %v", err)
	}
	page := string(out)

	if strings.Contains(page, "{{") {
		t.Errorf("rendered page contains unresolved template actions")
	}
	for _, want := range []string{
		`href="https://opc-litellm.benlai.com"`,
		`href="https://opc-mcp-admin.benlai.com"`,
		`href="https://opc-mcp-admin.benlai.com/mcp-gateway"`,
		`href="https://aiwork.benlai.com"`,
		`href="https://langfuse.tokendance.sh"`,
		`href="/admin/deploy"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rendered page missing %s", want)
		}
	}
	// 5 张平台组件卡片 + 1 张可观测性卡片
	if !strings.Contains(page, `border-indigo-200">5<`) {
		t.Errorf("platform count badge not rendered as 5")
	}
	if !strings.Contains(page, `border-rose-200">1<`) {
		t.Errorf("observability count badge not rendered as 1")
	}
}

func TestRenderNavPage_UnconfiguredHidden(t *testing.T) {
	cfg := &config.Config{}
	cfg.Admin.Nav = config.NavConfig{
		KnowledgeURL: "https://aiwork.benlai.com",
	}

	out, err := renderNavPage(mustNavTemplate(t), cfg)
	if err != nil {
		t.Fatalf("renderNavPage: %v", err)
	}
	page := string(out)

	if strings.Contains(page, "{{") {
		t.Errorf("rendered page contains unresolved template actions")
	}
	for _, unwanted := range []string{
		"LLM Gateway Admin",
		"MCP Admin",
		"MCP Gateway",
		"Deploy-OPC",
		"Langfuse",
	} {
		if strings.Contains(page, unwanted) {
			t.Errorf("unconfigured card %q should be hidden", unwanted)
		}
	}
	if !strings.Contains(page, `href="https://aiwork.benlai.com"`) {
		t.Errorf("configured knowledge card missing")
	}
	// 平台组件分组只剩 1 张卡片；可观测性分组整体隐藏
	if !strings.Contains(page, `border-indigo-200">1<`) {
		t.Errorf("platform count badge not rendered as 1")
	}
	if strings.Contains(page, "可观测性") {
		t.Errorf("observability section should be hidden when count is 0")
	}
}

func TestRenderNavPage_NoneConfigured(t *testing.T) {
	cfg := &config.Config{}

	out, err := renderNavPage(mustNavTemplate(t), cfg)
	if err != nil {
		t.Fatalf("renderNavPage: %v", err)
	}
	page := string(out)

	if strings.Contains(page, "{{") {
		t.Errorf("rendered page contains unresolved template actions")
	}
	if strings.Contains(page, "平台组件") || strings.Contains(page, "可观测性") {
		t.Errorf("both card sections should be hidden when nothing is configured")
	}
	// 页面骨架（顶部导航、侧栏菜单）仍在
	for _, want := range []string{"系统导航", "/admin/nav", "返回同步控制台"} {
		if !strings.Contains(page, want) {
			t.Errorf("page skeleton missing %q", want)
		}
	}
}
