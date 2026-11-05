# DSH 一期接口补充

## 文档正文

`GET /integration/v1/knowledge/:id?tenant=<tenant>&knowledge_base_id=<kb>&include_content=true`

`include_content` 是可选参数；未指定时保留原有元数据响应。指定 `true` 后，`data.content` 返回按 `chunk_index` 排序的已启用文本分块，用空行连接；`content_source` 为 `text_chunks`，`content_chunk_count` 表示返回分块数。内容是平台解析后的文本，可能保留分块重叠，不声称是原始上传文件的逐字副本。

读取使用个人桥接身份，不使用管理员凭据或原始文件下载接口。检查文档与每个分块的 tenant/KB/knowledge 归属，并在读取分页后重新校验文档授权、归属和 `updated_at`。范围变化时不返回正文。总预算 12 秒，最多 100 页、每页 100 条、正文最多 8 MiB；超限或上游失败返回错误，不静默截断。

## MCP 策略链路

`/integration/v1/mcp/services` 与 `/integration/v1/mcp/invoke` 统一使用已有 knowledge 受管接口。即使本地数据库存在服务 URL，也不能直接向工具容器发送 `tools/call`，或绕过用户能力/standing policy 返回整租户目录。

审批、decision 和 invocation 查询的现有协议保持不变。此改动可能使此前通过本地目录捷径可见、但未获受管策略授权的工具不可见；应按受管平台策略授权，不恢复直连绕过。

## 验证

新增正文读取测试覆盖排序、分页、停用分块、跨库分块、读取期间移动及撤权；MCP 回归覆盖服务 URL 不能绕过审批以及数据库目录仍需策略过滤。使用 `go test ./...` 检查全仓。

`GET /integration/v1/mcp/services?tenant=<tenant>&include_schema=true` 在受管目录过滤之后，用同一个人身份读取各服务的 metadata，只为已允许的工具补充 `input_schema`。不返回 metadata 中额外的工具或凭据信息；metadata 不可访问或参数定义缺失时返回错误。未传该参数保持原来的目录透传响应。
