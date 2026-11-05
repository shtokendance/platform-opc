# platform-opc 一体化直连与去中继架构 实施计划

> **针对智能体工作者：** 必需子技能：使用 superpowers:subagent-driven-development 或 superpowers:executing-plans 按任务实施此计划。步骤使用复选框 (`- [ ]`) 语法进行追踪。

**目标：** 将 `platform-opc` 彻底改造为一体化直连引擎：移除短效 Token 频繁换签与插表逻辑、资源与工具箱通过数据库前置过滤与批量直查、网关直接调度模型与底层存储。

**架构：** 客户端凭证单一化，通过 `X-Tenant-ID` 声明目标空间；网关入口单次查询数据库验证成员权限并 Fail-Fast 拦截；知识库元数据与工具目录直查 PostgreSQL，向量检索直连 Milvus（带租户标量过滤），模型按需读取 `models` 表配置动态调用 Embedding 与 Rerank API。

**技术栈：** Go 1.26+, Gin, GORM, PostgreSQL, Milvus SDK, MinIO SDK, JSON-RPC 2.0 (MCP)。

---

### 任务 1：创建功能开发分支

**目标：** 为本次重构创建隔离的工作分支。

- [x] **步骤 1：基于 `release` 分支创建新分支**

```bash
cd /Users/fangqq/Code/tokendance/platform-opc
git checkout -b feature/consolidated-direct-engine
```

- [x] **步骤 2：验证当前分支状态**

运行：`git status`  
预期：On branch `feature/consolidated-direct-engine`，工作区干净。

---

### 任务 2：彻底拔除 kbridge 短效 Token 与 auth_tokens 频繁插表机制

**文件：**
- 修改：`platform-opc/pkg/kbridge/bridge.go`
- 修改：`platform-opc/pkg/handler/knowledge.go`
- 修改：`platform-opc/pkg/handler/resources.go`
- 修改：`platform-opc/pkg/handler/mcp.go`
- 测试：`platform-opc/pkg/kbridge/bridge_test.go`
- 测试：`platform-opc/pkg/handler/knowledge_fake_test.go`

- [x] **步骤 1：移除 `kbridge` 中 ≤120s TTL 与写 `auth_tokens` 表的逻辑**
  - 删除 `MaxUpstreamTokenTTL = 120 * time.Second` 限制；
  - 移除对 `weknora.CreateAuthToken` 的高频插表调用；
  - `kbridge` 改造为纯粹的用户空间权限校验与长效/统一 Token 适配器，保留内存 Token 缓存或直接透传；
  - 在转发给外部的请求中强制绑定 `X-Tenant-ID` Header。

- [x] **步骤 2：调整 Handler 调用点**
  - 在 `knowledge.go`、`resources.go`、`mcp.go` 中，调用点不再传入 120s 短命 TTL，统一透传用户会话或使用标准会话生命周期。

- [x] **步骤 3：运行测试并验证通过**

运行：`cd /Users/fangqq/Code/tokendance/platform-opc && go test -count=1 ./pkg/kbridge/... ./pkg/handler/...`  
预期：PASS。

- [x] **步骤 4：提交代码**

```bash
git commit -am "refactor(kbridge): remove short-lived auth_token minting and db insertion"
```

---

### 任务 3：资源网关 (Resources) 改造：数据库直查与批量前置过滤

**目标：** 消除用户有 $N$ 个空间时循环向 upstream 发送 $N$ 次 HTTP 请求的瓶颈，改为一条 SQL 批量查库。

**文件：**
- 修改：`platform-opc/pkg/model/weknora.go`（增加 `WeKnoraKnowledgeBase` 模型）
- 修改：`platform-opc/pkg/storage/storage.go`（增加 `ListKnowledgeBasesByTenants` 方法）
- 修改：`platform-opc/pkg/handler/resources.go`（改造 `ListKnowledgeBases`）
- 测试：`platform-opc/pkg/handler/resources_test.go`

- [x] **步骤 1：在 `model/weknora.go` 中定义 `WeKnoraKnowledgeBase` 映射**
  - 映射 PostgreSQL 的 `knowledge_bases` 表（字段：`id`, `tenant_id`, `name`, `description`, `is_active`, `created_at` 等）。

- [x] **步骤 2：在 `storage` 中实现单条 SQL 批量查询**
  ```go
  func (s *Storage) ListKnowledgeBasesByTenants(ctx context.Context, tenantIDs []uint64, query string) ([]model.WeKnoraKnowledgeBase, error)
  ```
  - 直接执行：`SELECT * FROM knowledge_bases WHERE tenant_id IN (?) AND is_temporary = false`（若带 query 增加名称模糊匹配）。

- [x] **步骤 3：改造 `resources.go` 的 `listResources`**
  - 查出用户当前所有的 `allowed_tenant_ids`；
  - 如果请求带了 `tenant_id`，校验是否在白名单中，不在直接 403；
  - 直接调用 `storage.ListKnowledgeBasesByTenants`，一次性查回所有可见知识库，不再通过 upstream 进行 HTTP 循环代理！

- [x] **步骤 4：运行测试验证批量查询与权限隔离**

运行：`cd /Users/fangqq/Code/tokendance/platform-opc && go test -count=1 ./pkg/handler/...`  
预期：PASS。

- [x] **步骤 5：提交代码**

```bash
git commit -am "feat(resources): direct db query for knowledge bases with push-down tenant filter"
```

---

### 任务 4：工具箱 (Toolbox / MCP) 改造：mcp_services 数据库直查与直接调度

**目标：** 工具目录从数据库 `mcp_services` 直出；工具调用直接作为 MCP Client 向目标工具容器发包。

**文件：**
- 修改：`platform-opc/pkg/model/weknora.go`（增加 `WeKnoraMCPService` 模型）
- 修改：`platform-opc/pkg/storage/storage.go`（增加 `ListMCPServicesByTenant`、`GetMCPServiceByID` 方法）
- 修改：`platform-opc/pkg/handler/mcp.go`（改造 `ListServices` 与 `Invoke`）
- 测试：`platform-opc/pkg/handler/mcp_test.go`

- [x] **步骤 1：在 `model/weknora.go` 中定义 `WeKnoraMCPService` 模型**
  - 映射 PostgreSQL 的 `mcp_services` 表（字段：`id`, `tenant_id`, `name`, `description`, `transport_type`, `url`, `enabled`, `is_builtin`, `auth_config`）。

- [x] **步骤 2：在 `storage` 中实现工具服务直查方法**
  - `ListMCPServicesByTenant(ctx, tenantID)`：查询 `(tenant_id = ? OR is_builtin = true) AND enabled = true`；
  - `GetMCPServiceByID(ctx, tenantID, serviceID)`：查询指定服务及调用配置。

- [x] **步骤 3：改造 `mcp.go`**
  - `ListServices`：直接通过 `storage` 读取工具目录并返回给客户端，不再转发 HTTP 到 upstream；
  - `Invoke`：前置校验用户在当前租户的权限，从 `storage` 获取该服务的真实 `URL`，作为标准 MCP Client 直接向目标服务容器发送 JSON-RPC 2.0 `tools/call` 请求。

- [x] **步骤 4：运行单元测试**

运行：`cd /Users/fangqq/Code/tokendance/platform-opc && go test -count=1 ./pkg/handler/...`  
预期：PASS。

- [x] **步骤 5：提交代码**

```bash
git commit -am "feat(mcp): direct db query for mcp services and direct tool invocation"
```

---

### 任务 5：知识库混合检索直连：Milvus 向量 + models 表动态读取 Embedding / Rerank

**目标：** 在 `platform-opc` 中闭环知识库问答检索链路，彻底省去外部知识 Web 服务中继。

**文件：**
- 修改：`platform-opc/pkg/model/weknora.go`（完善 `WeKnoraDocumentChunk` 等模型）
- 新建：`platform-opc/pkg/ai/model_client.go`（轻量 Embedding 与 Rerank HTTP Client）
- 新建：`platform-opc/pkg/ai/search_service.go`（混合检索与 Rerank 编排）
- 修改：`platform-opc/pkg/handler/knowledge.go`（对接直连检索）
- 测试：`platform-opc/pkg/ai/search_service_test.go`

- [x] **步骤 1：实现轻量 Model Client**
  - 从 `models` 表根据当前 `tenant_id` 取出 `Embedding` 和 `Rerank` 的 `parameters`（解密 APIKey）；
  - 发送 HTTP 请求生成 Query Embedding；
  - 发送 HTTP 请求调用 Rerank 模型计算相关度得分。

- [x] **步骤 2：实现混合检索与 Rerank 编排**
  - 粗排：Milvus 向量检索（强制附带 `filter: "tenant_id == currentTenantID"`）+ PostgreSQL 关键词检索；
  - 精排：调用 Rerank 挑选 Top 3~5 个最佳切片。

- [x] **步骤 3：对接 `/api/v1/knowledge/search` 路由**
  - 替换原来的 HTTP 代理转发，直接返回编排检索出的切片数据。

- [x] **步骤 4：编写测试并验证全流程通过**

运行：`cd /Users/fangqq/Code/tokendance/platform-opc && go test -count=1 ./...`  
预期：PASS。

- [x] **步骤 5：提交代码**

```bash
git commit -am "feat(knowledge): direct vector search and dynamic rerank model integration"
```

---

### 任务 6：全量测试套件回归验证与分支合并

- [x] **步骤 1：运行全仓库单元测试**
  ```bash
  cd /Users/fangqq/Code/tokendance/platform-opc && go test -count=1 ./...
  ```
- [x] **步骤 2：检查代码格式与工作区状态**
  ```bash
  git status
  ```
- [ ] **步骤 3：向用户汇报改造结果与合并准备**
