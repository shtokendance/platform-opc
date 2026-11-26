---
document: hotfix
hotfixId: HF-20261126-01
status: confirmed
hotfixState: implementation
reviewRoute: current-subagent
baseBranch: release
baseCommit: 5a51f5b8867c00cc00e7b955d9ffa05c606357c7
fixFingerprint: null
architectureImpact: none
openedAt: 2026-11-26T14:38:53Z
updatedAt: 2026-11-26T14:38:53Z
---

# HF-20261126-01 修复全量同步「假完成」

确认留痕：2026-11-26，提交者 yj198788@hotmail.com 在 DP-5 路线确认中选择「Hotfix（推荐）」，确认时的诊断与范围即本文第 1–3 节（同一会话中已展示）。

## 1. 问题、影响与复现

现象（DP-5）：在同步控制台点击「立即全量同步」，界面提示「全量同步已执行完成」，但模型缺失依旧——76 个空间仅 1 个模型同步就绪，绝大多数部门空间显示「仅 4 个（缺 2）」。

复现：任意部门空间成员授权已齐全（无人员差异）时，点击「立即全量同步」，运行以 succeeded 结束，但该空间的模型/存储/沙箱配置不发生任何变化。

根因（只读勘察结论）：

1. 「立即全量同步」→ `POST /admin/api/sync/all` → `AdminTriggerSyncAll`（`pkg/handler/admin.go:62`）→ `syncservice.Run(scope=full)`。该运行只做组织架构/成员授权同步（快照→校验→差异→供给/收回→对账）。
2. 模型/存储/沙箱级联只发生在 `provision()` 内的 `SyncDepartment`（`pkg/syncservice/apply.go:213`），而 provision 动作仅当成员缺少部门或母空间授权时才生成（`pkg/syncservice/service.go:372`）。老空间成员授权早已齐全 → 差异计划为空 → 运行成功结束，但配置级联从未执行。
3. 这是 H5/BND-130 重构的回归：旧 `syncer.SyncAll` 对每个部门无条件级联，已删除（`pkg/syncer/syncer.go:45` 注释）；定时轮询注册也已移除（`main.go:112`），配置漂移没有任何自动修复通道。
4. 叠加问题：`AdminTriggerSyncAll` 无论运行终态是 succeeded / partial_failed / failed 都返回「全量同步已执行完成」，前端无条件 toast 成功（`pkg/web/admin.html:1133`），失败被进一步掩盖。

## 2. 修复范围与非目标

范围：

- full run：对快照内全部一级目标部门无条件执行配置级联（模型/存储/沙箱），逐部门落 `config_sync` 步骤账本（幂等键 `run_id|config_sync|<deptCode>`），失败可重试。
- department run：对请求部门解析出的一级目标空间执行同样的级联（一个步骤）。
- 级联复用既有 `SyncDepartment` 语义（确保 tenant/映射 + 模型 + 存储 + 沙箱 + 主管），幂等。
- `CountActionSteps` 纳入 `config_sync`：级联失败 → 运行 partial_failed，可用既有 Retry 重放。
- Retry 时级联阶段整体重跑；已不在快照目标中的孤儿 config_sync 步骤标记 skipped。
- `AdminTriggerSyncAll` 响应 message 按运行终态如实区分（succeeded/partial_failed/failed），前端按 `data.status` 给出成功/失败提示。
- 测试装配补齐 models/storage_backends/sandbox 表，新增级联与状态口径测试。
- `.gitignore` 增加 `.codeopc/`、`.kimi/`、`.attachments` 等运行时会话目录，保证候选交付清单只含真实变更。

非目标：

- 不恢复 cron 定时轮询（BND-130 已决策归一到手动运行，是否恢复定时属产品决定）。
- 不处理 Default Tenant(1) / admin's Workspace(10000) 两个非部门映射空间的级联（可用既有「同步配置」单空间按钮；是否纳入级联另议）。
- 不同步控制台「上次同步状态」卡片的数据填充（`last_sync_time`/`sync_state` 一直是未填充的展示桩）、不改动 `provision()` 内既有的 SyncDepartment 调用。
- 不部署生产环境。

## 3. 风险提示及用户确认

- API：`POST /admin/api/sync/all` 响应 message 文案按终态区分，`data.status` 字段不变；唯一消费方是内嵌 admin.html，同步修改。`config_sync` 步骤出现在运行明细中，属新增可见步骤类型。
- 数据：级联会软删除部门空间中母空间未配置的多余模型（`SyncTenantModelsFromParent` 既有语义，与单空间「同步配置」按钮一致）；存储/沙箱级联保留子空间既有主键。无 schema 迁移（`sync_steps.step_type` 为 TEXT）。
- 权限：不改动任何鉴权路径。
- 依赖：无新增依赖。
- 部署：合并后需部署测试环境复测；生产部署需用户另行明确授权。
- 回滚：revert 本 Hotfix 提交即可；已落库的 config_sync 步骤行不影响旧代码运行（旧代码不读该类型步骤，仅 CountActionSteps 口径随代码回退）。

## 4. 实施摘要与回滚方法

（实施完成后填写）

## 5. Testing/PBT/Security/Resiliency 证据

（验证后填写）

## 6. Architecture Impact

（对账后填写；预判 none——级联语义复用既有 SyncDepartment，仅触发点补齐，不改变模块边界与依赖方向。）

## 7. AI Review

（审查后填写）

## 8. Human Sign-off

（签署后填写）
