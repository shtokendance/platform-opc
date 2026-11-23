---
project: wecom-opc
document: architecture-baseline
status: confirmed
updatedAt: 2026-11-23
---

# Platform-OPC Architecture Baseline

## 系统职责

Platform-OPC（仓库历史名 wecom-opc）是 Go 1.26 + Gin + GORM(PostgreSQL) 的单二进制长驻服务，提供企业微信 OAuth 登录、组织与身份同步、统一账户中心集成、配置广播及内嵌 `/admin` 管理后台。

## 当前模块边界

- `main.go` 组装 HTTP 路由、鉴权、存储与后台页面。
- `pkg/adminauth` 保护管理台页面和管理 API。
- `pkg/web` 通过 Go `embed` 提供管理后台 HTML 与静态 JavaScript；Deploy-OPC 页面使用 Vue 3 和 Tailwind CSS，无独立前端构建链。
- `pkg/deploy` 等包实现应用、资源、模板和存储治理；管理页面通过既有 `/admin/api/deploy/*` 调用这些能力。
- `migrations` 管理平台状态数据；本轮导航改造不改变任何迁移。

## 当前 Deploy-OPC 页面事实（Loop 1 实施前）

`/admin/deploy` 经管理台会话鉴权后原样返回 `deploy_admin.html`。页面顶部为产品栏和四个横向 Tab，下方为四张概览卡及四个由 `activeTab` 切换的业务区。同步控制台、系统导航和刷新位于顶部栏；模块新增与存储刷新位于 Tab 条右侧。页面尚无 URL hash 恢复。

## 部署基线

生产 SOP 使用 `release` 分支的 7 位提交号构建 `platform-opc:<commit>` 和 `platform-opc:latest`。标准 Compose 服务名为 `platform-opc`，端口为 `3002:3002`，复用宿主机 `config.yaml`、`platform-opc.env` 和 `opc-network`。回滚通过指定旧提交镜像并重新执行 Compose 完成。实际服务器形态必须在部署前重新只读核实，不能仅凭文档假定。

## 验证基线

标准命令为 `go test ./...` 和 `go build .`。现有 Go 测试没有覆盖 `deploy_admin.html` 的布局或 hash 行为，因此 Deploy-OPC UI 变更还需要真实浏览器和部署环境冒烟证据。

## 数据模型

Loop 1 不涉及数据模型变更。长期基线只引用既有迁移体系，不在本文件复制表结构。
