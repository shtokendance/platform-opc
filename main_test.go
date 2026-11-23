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

func TestRenderNavPage_PartialOverridesKeepDefaults(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{ExternalBaseURL: "https://aiwork.banlai.com"},
	}
	cfg.Admin.Nav = config.NavConfig{
		KnowledgeURL: "https://custom-knowledge.example.com",
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
		`href="https://custom-knowledge.example.com"`,
		`href="/admin/deploy"`,
		`href="https://langfuse.tokendance.sh/"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rendered page missing %s", want)
		}
	}
	if got := strings.Count(page, `href="https://opc-mcp-admin.benlai.com"`); got != 2 {
		t.Errorf("MCP admin and gateway links = %d, want 2", got)
	}
	if !strings.Contains(page, `border-indigo-200">5<`) {
		t.Errorf("platform count badge not rendered as 5")
	}
	if !strings.Contains(page, `border-rose-200">1<`) {
		t.Errorf("observability count badge not rendered as 1")
	}
}

func TestRenderNavPage_NoneConfiguredUsesDefaultsAndClassicLayout(t *testing.T) {
	cfg := &config.Config{}

	out, err := renderNavPage(mustNavTemplate(t), cfg)
	if err != nil {
		t.Fatalf("renderNavPage: %v", err)
	}
	page := string(out)

	if strings.Contains(page, "{{") {
		t.Errorf("rendered page contains unresolved template actions")
	}
	for _, want := range []string{
		"系统导航",
		"返回同步控制台",
		"平台组件",
		"可观测性",
		`href="https://opc-litellm.benlai.com"`,
		`href="https://opc-mcp-admin.benlai.com"`,
		`href="/admin/deploy"`,
		`href="https://shtokendance.benlai.com/"`,
		`href="https://langfuse.tokendance.sh/"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("default nav page missing %q", want)
		}
	}
	if strings.Contains(page, "功能菜单") || strings.Contains(page, "<aside") {
		t.Errorf("left sidebar menu should be removed from nav.html")
	}
	adminHTML, err := web.AdminFS.ReadFile("admin.html")
	if err != nil {
		t.Fatalf("read embedded admin.html: %v", err)
	}
	if strings.Contains(string(adminHTML), "功能菜单") || strings.Contains(string(adminHTML), "<aside") {
		t.Errorf("left sidebar menu should be removed from admin.html")
	}
}
