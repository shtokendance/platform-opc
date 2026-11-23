# wecom-opc Agent 规则

## 项目上下文

wecom-opc（生产部署名 weknora-gateway）是企业微信组织与身份集成网关：Go 1.26 + Gin + GORM(PostgreSQL) + robfig/cron 的单二进制长驻服务。职责四块：企微 OAuth 登录桥（扫码/静默授权后为员工即时开户并签发 OPC 会话）、统一账户中心（GraphQL）组织架构到 OPC 空间/成员的定时全量同步、母空间（tenant 10001）模型/存储/沙箱配置向部门空间的级联广播、内嵌 /admin 管理后台。外部依赖：企业微信 API、统一账户中心 GraphQL、OPC-2.0（WeKnora fork）的 PostgreSQL。测试 `go test ./...`，容器化部署在 OPC 同一 Docker 网络。设计文档在 `docs/`（plans / specs / superpowers）。

## Loop 状态入口

项目当前状态只维护在 Loop 状态文件里。开始任何工作前必须先读取它，不在 `AGENTS.md` 重复维护 Loop 编号、名称、阶段或文档清单。

未启用 demand unit 的单流仓库，状态文件全仓库只有一份：`docs/loops/status.md`。

- `activeLoop` 为具体编号时，继续读取状态文件指向的活跃 Loop 目录及其中已有阶段文档，并按 frontmatter 判断当前门禁。
- `activeLoop: null` 表示当前没有活跃 Loop。此时不得继续上一 Loop、创建 Specification/Tasks 或实施代码；下一步只能在用户明确新目标后，为 `nextLoop` 创建 Requirements。
- `docs/archive/` 只用于追溯已关闭或被替代的 Loop，不是当前规范入口，不得从归档文档自动恢复任务。
- 参考材料和历史提交不能自动改变状态文件或活跃 Loop 的已确认文档。
- 如果状态文件、活跃目录和阶段文档互相矛盾，应停止相关工作并请求用户确认。

这一条不是靠自觉执行的：每轮 Loop 开局跑一次 `sdd-loop check`（pi 里是 `sdd_loop_check` 工具），它把状态文件的声明和文件里的事实摆在一起比。它只读，不改任何文件——矛盾由人解决。

### 并发需求单元

- `sdd-init` 是仓库级的一次性动作，不因新需求重复执行。单仓需要多个需求并行时，先用 `sdd-upgrade` 把根状态迁到 `docs/loops/demands/<demandId>/status.md`；迁移后每个需求单元独立维护自己的 Loop 与门禁，根目录不再保留第二份状态权威。
- demand unit 默认按当前 Git 分支唯一匹配状态文件中的 `branch`；需要操作另一需求时显式使用 `sdd-loop check --demand <demandId>`。无法唯一匹配时停止并请人选择，不猜。`--demand` 与 `--stream` 互斥；`stream` 只表示同一仓库内可独立交付的子系统，不表示 Git 仓库。
- 一个业务需求跨多个 Git 仓库时使用 `sdd-solo`：最初进入的仓库是主仓，权威需求与交互都留在主仓；参与仓不另开独立 SDD 对话、不维护第二份可编辑 Requirements，也不修改各自可能存在的 `activeLoop`。参与仓只接收最终可追溯的只读快照和本仓真实实施、验证、审查证据。

## SDD 与 Loop

治理 v1 的 Loop 按以下顺序推进；治理 v2 按启动时锁定的依赖图推进：

```text
Requirements
  -> Approval (stop)
  -> Continue (later user action)
  -> Architecture + Architecture Baseline
  -> Specification
  -> Tasks
  -> Worktree Ready
  -> Implementation
  -> Automated Verification
  -> Architecture Reconciliation
  -> AI Review
  -> Human Review
  -> Loop Closed
```

所有阶段都属于同一个 Loop。需求文档完成不代表当前 Loop 完成，也不代表可以开启下一个 Loop。

只有当前 Loop 的需求、架构、规格、任务、实现和验证全部闭合后，才可以关闭它并创建新 Loop。新 Loop 必须重新确认目标和范围，不得自动继承为新的开发任务。

## 治理、角色与审计

**新工作按所属流状态文件的 `governanceVersion` 运行；新建或已升级到 v2 的流走 v2。**仍为 v1 的流，其当前及后续工作继续遵守 v1 门禁，直到该流单独完成升级。本节下方以 `governance@1`、`stage_approved`、`continue_authorized` 和 `deliveryScope` 为名的旧规则只适用于 v1 工作；不得用它们重释已锁定的 v2 工作。v2 工作开局先运行 `sdd-loop capabilities --require governance@2 --host <当前宿主>`，并加载 `sdd-route`：根据任务推荐 Loop、Hotfix 或 Debug，向用户说明理由并取得具体路线确认，然后固定项目或流级 `workflow.md`。项目定义在 `docs/sdd/workflows/<route>/workflow.md`，流级完整覆盖在 `docs/sdd/workflows/<stream>/<route>/workflow.md`；已启动工作只按启动时锁定的定义执行。

v2 人工测试每轮只追加问题、修复范围、定向测试、部署版本和复测结果；新产品决定当场取得用户确认，架构影响当场记事实。候选稳定后只回写受影响条款及其确认，重验受影响下游证据。最终测试、架构对账、AI Review、人工签署或预声明免签按锁定依赖图推进；Review 豁免也必须预声明。提交者本人可以签署。Review 后再修复时，重新固定候选并对新差异补测试、复测及 Review。其他流修改交付清单外文件不使本轮审查失效；共享文件变化需定向测试和差量 Review。关闭时核对历史快照、审计链和签署，交付提交区间运行只读 `sdd-loop provenance check`，不得缩小真实交付范围来消红。

以下条款整块属于 v1 治理协议，只适用于治理版本仍为 v1 的流及其当前、后续 v1 工作；其中角色邮箱映射、`audit/*.jsonl`、固定审批/Continue、工程扩展和 C10 接受流程不得强加给 v2 工作。v2 事件按锁定流程写入 `workflow.audit.jsonl`，写入前同样脱敏敏感内容。

- v1 工作要求本机 sdd-loop 提供 `governance@1`，且治理 Skills 已安装到当前宿主。v1 每轮开局必须先运行 `sdd-loop capabilities --require governance@1 --host <当前宿主>`，成功后再运行 `sdd-loop check`；宿主值为 Claude Code=`claude`、Codex/Kimi Code 等共享 Agent Skills 宿主=`agents`、OpenClaw=`openclaw`、Hermes=`hermes`、pi=`pi`。命令不存在、退出非 0、协议不受支持或宿主未就绪时立即停止。旧 CLI 可能忽略 `governanceVersion`，所以 check 绿色不能替代能力预检。工具与 Skills 的唯一来源是 `https://github.com/Roger0808/sdd-loop`；按该仓库 README 的更新步骤升级并开启新会话后重试。未经用户明确授权不得自动下载、安装、链接或切换工具版本。
- v1 状态文件包含 `governanceVersion`、`gateStage`、`gateState`、`gateFingerprint`、`enabledExtensions` 和六类 `role*` 映射。v2 状态文件不再要求三个 `gate*` 字段；当前值只保存在状态文件，不在其他文件复制。
- 治理事件必须读取当前仓库的 `git user.name` / `git user.email`，并匹配状态文件登记的 Requester、Product、Architect、Implementer、Reviewer 或 Approver。缺身份、角色不匹配时停止，不编造临时身份。
- 原始需求、问题答案、阶段审批、Continue、变更理由、工程扩展结论、AI Review 和人工签署写进当前 Loop 的 `audit/*.jsonl`。分片只追加，不修改、删除、重排旧事件；不得记录本机绝对 worktree 路径。
- 写入审计前对 Token、密码、私钥等敏感内容脱敏；保留完整输入的 SHA-256 和脱敏说明，不保存完整 AI 输出或无关命令日志。
- 阶段文档得到明确确认后，先标 `confirmed`，再通过 `sdd-loop _governance record --event-json <临时文件>` 记录 `stage_approved`。记录后必须停止当前响应，`nextPhase` 不变。
- 只有用户在**后续消息**明确要求继续，才能记录独立的 `continue_authorized`；工具会重新校验角色和文档指纹，再推进 `nextPhase`。即使同一人承担两个角色，也不得把“批准并继续”合成一步。
- 审批后文档变化会使旧审批失效；AI Review 后代码、配置、测试、Loop 关键文档或 Architecture Baseline 变化会使旧审查和人工签署失效。
- 分流仓库的 `review_completed` 必须把本轮 change surface 中真实交付的仓库相对文件/目录记录为 `deliveryScope`；C10 关闭后只保护这组范围，其他流修改自己范围不得使本流变红。`deliveryScope` 是本轮实际改动面，不是永久的流目录配置，也不得为了变绿漏掉本轮交付文件。
- 已关闭 Loop 因自身 `deliveryScope` 后续变化触发 C10 时，不得改写或伪造旧的 `review_completed`、`human_signed`、`loop_closed`。只有 Approver 核对当前漂移并明确接受后，才能追加 `closure_drift_accepted`；事件必须写明接受原因和已核对的签署后变化，且只绑定记录当时的交付指纹。没有 `deliveryScope` 的存量分流 Loop 在首次接受时必须补录真实范围；范围建立后不得缩小或替换。仓库在该范围内再次变化会重新触发 C10。
- `testing,pbt,security,resiliency` 四个工程扩展默认启用。各阶段按需读取 `sdd-init` skill 的 `references/extensions/`；Verification 必须逐项写 `PASS`、`FAIL` 或有具体理由的 `N/A`，空白和“已检查”都不算证据。
- 项目自定义扩展放在 `docs/sdd/extensions/<domain>/<name>.md`；存在同名 `.opt-in.md` 时只在 Requirements 提问决定是否启用，没有 opt-in 时始终启用。完整规则只在相关阶段加载；重名时停止，不静默覆盖。

## Hotfix 独立通道

以下以 `hotfix@1` 和 `hotfix_closed` 为名的条款仅适用于既有 v1 Hotfix；新 v2 Hotfix 以启动时锁定的 Hotfix `workflow.md` 为准。

- Hotfix 只能加载 `sdd-hotfix` skill 启动；开工前除 `governance@1` 外还必须运行 `sdd-loop capabilities --require hotfix@1 --host <当前宿主>`。能力缺失时停止并提示更新，不得自动安装本机 Skill。
- Hotfix 与普通 Loop 并行，不修改 `activeLoop`、`nextLoop`、`gateStage` 或 `gateState`，也不是跳过普通 Loop 阶段的开关。用户决定是否采用 Hotfix；风险必须展示和留痕，但不按类别或 diff 大小自动拒绝。
- 首次响应只读勘察并展示问题/目标、修改范围、API/数据/权限/依赖/部署风险、基线和 reviewer 路径；用户一次确认后，才创建独立分支或 worktree 和一份 Hotfix 文档。
- Hotfix 审计位于文档相邻的 `audit/hotfix-YYYYMMDD-NN/*.jsonl`。编号按每条流、每天扫描活跃与归档目录后递增，不得重号。
- 单流 Hotfix 放在 `docs/loops/hotfix/hotfix-YYYYMMDD-NN.md`，归档到 `docs/archive/hotfix/`。
- Testing 必须 PASS；PBT、Security、Resiliency 必须 PASS 或有具体理由的 `N/A`。`architectureImpact: updated` 时必须回写 Architecture Baseline 并记录 `architecture_reconciled`。
- 启动时选择的只读 AI reviewer 路径不得中途静默更换。只有 `READY_FOR_HUMAN_REVIEW` 后，用户才能对当前修复指纹签署；随后把文档和对应审计目录移入对应的归档 Hotfix 目录并记录 `hotfix_closed`。

## Debug 人工测试通道

以下以 `debug@1` 和 `debug_closed` 为名的条款仅适用于既有 v1 Debug；新 v2 Debug 以启动时锁定的 Debug `workflow.md` 为准。

- `/sdd-debug`（pi 为 `/sdd debug`）只能加载 `sdd-debug` skill 启动；先运行 `sdd-loop capabilities --require debug@1 --host <当前宿主>`，并确认当前状态文件已有 `governanceVersion: 1` 与 `roleApprover` 映射。缺失时必须在开始修复前 fail-fast，先走 `/sdd upgrade` 的治理升级，不得拖到“开始收口”才发现无法关闭。Debug 与普通 Loop 并行，不修改 `activeLoop`、`nextLoop`、`gateStage` 或 `gateState`。
- Debug 启动时只记录 Git/工作区基线和测试环境，不创建文档、不选择 reviewer、不做范围确认。人工测试期间按“用户反馈 → AI 排查修复 → 定向测试 → 测试环境部署 → 用户复测”循环；为解决当前问题所需的跨模块修改不因超出原 Loop 的预测改动面而另设门禁。
- Debug 只有遇到新的产品行为选择、生产环境操作、破坏性或不可逆数据变更、Secrets/权限/安全策略变化、外部费用或无法安全回滚的共享环境操作时，才停止相关动作请求用户决定；未经明确授权仍不得提交、推送或部署生产环境。
- 用户完成手测后说“开始收口”即授权冻结当前版本、执行最终测试/构建/冒烟、按最终代码更新长期 Architecture Baseline，并在归档 Hotfix 目录反写一份 `route: debug` 文档、记录 `debug_closed` 后关闭，不再索要第二次确认。Debug 必须明确 `acceptanceMode: manual-test`、`reviewStatus: waived` 和 `MANUAL_TEST_ACCEPTED_NO_INDEPENDENT_REVIEW`；不得伪造 `review_completed`、`human_signed`、`hotfix_closed` 或 `READY_FOR_HUMAN_REVIEW`。

## 阶段门禁

v2 的阶段顺序和 Review/签署策略由锁定的依赖图决定；下列各阶段内容要求仍适用，v1 的固定推进顺序只约束未迁移的 v1 工作。

### Requirements

- 说明问题、目标、范围、非目标、用户场景、约束和成功标准。
- **开一轮新 Requirements 前先读 `docs/backlog.md`**，把里面每一条摆给用户，由用户逐条决定这轮做还是不做。用户说做的才写进本轮需求（照常编号）并从 backlog 移走；说不做的原样留着，**不得替用户删**。backlog 为空或不存在时说一句「backlog 为空」，不得不吭声跳过——跳过了用户不会知道上一轮的欠账没人问。
- 需求必须得到用户明确确认后，状态才能设为 `confirmed`。
- 需求设计不负责决定目录、数据库拓扑、接口结构和迁移步骤。

### Architecture

- 开始设计前必须盘点真实代码、依赖、配置、数据、部署和测试。
- 必须描述目标架构、模块边界、依赖方向和部署拓扑。
- Architecture 阶段必须建立或审查长期 Architecture Baseline：优先沿用项目已有 `docs/architecture/` 结构；无旧结构时，单系统默认 `docs/architecture/overview.md`，分流默认 `docs/architecture/<stream>.md`。Loop 内 `architecture.md` 描述本轮方案，Architecture Baseline 描述系统当前总体事实，两者不得互相代替。
- 架构图只使用可评审的 Mermaid 或 ASCII；不以 PNG/HTML 作为唯一真相源。只有存在真实跨系统关系时才新建总体 `overview.md`，不为了目录对称创建空壳。
- 每项架构结论必须区分：已确认约束、已验证事实、候选方案、待决问题。
- 本轮改动数据模型（新增/修改/删除表、字段、索引、约束）时，必须写：变更清单、迁移步骤、已有数据的处理方式、回滚路径与兼容窗口。**不涉及时明确写一句「本轮不涉及数据模型变更」**——留空等于没判断过，看文档的人分不出是想过了还是忘了。
- 架构状态为 `draft` 时，只允许调研和修改文档；不得开始正式实施。
- 架构经用户明确确认后，才能进入 Specification。

### Specification

- 规格描述可验证行为，包括接口、数据、权限、安全、错误语义、状态机和验收场景。
- 每条强制行为使用稳定编号，供任务、代码变更和测试引用。
- 不得在架构确认前创建正式规格。

### Tasks

- 每项任务必须引用需求、架构或规格编号，并包含完成条件和验证方法。
- 不得创建无法追溯到已确认规格的实施任务。
- 不得在规格确认前开始正式任务拆分。

### Implementation

- 开始实施时，必须在 `implementation.md` 记录基线分支、基线 commit、当前分支和任务范围；不记录本机 worktree 绝对路径。
- 只实施当前 Loop 已确认范围内的任务。
- 发现新需求时先记录，不得静默扩大范围。**记进 `docs/backlog.md`**：本轮不修的缺陷、冒出来的新需求、临时绕过去的问题都写在那里，一条一行，写清是什么、在哪个文件或场景、为什么本轮不做。只说一句「先记下」而不落盘等于没记——会话一结束就没了，下一轮谁也捞不到。
- 如果实现需要改变已确认架构或规格，先修改相应文档并重新确认。

### Verification

- 验证必须覆盖规格中的正常、失败、权限和边界路径。
- 未执行测试、构建、部署或必要的人工检查，不得宣称完成。
- `skip`、环境缺失或仅阅读代码不等于验证通过。
- 本轮执行过数据迁移的，必须验证回滚路径，并在有代表性存量数据上跑过；只在空库上通过不算验证。
- 验证结果、未完成项和差异必须写入当前 Loop 的 verification 文档。
- 自动化验证通过不等于代码审查通过。人工审查前必须先根据最终代码反向更新 Architecture Baseline，并写出本轮 change surface：代码、配置、数据、接口、部署、测试和文档影响。
- 必须加载 `sdd-review`。AI Review 前先停下向人类确认：留在当前 Agent 的只读 subagent 审，还是换一个 Agent；换 Agent 时还要确认具体对象并写对应 handoff。选择未明确时不得启动审查或写 `review_completed`。当前 Agent 本体不得自审；没有可用 subagent 或独立只读 reviewer 时停止并报告 `NOT_REVIEWABLE_SAFELY`，不得以清理上下文替代独立审查。
- AI Review 只能给出 `READY_FOR_HUMAN_REVIEW`、`CHANGES_REQUIRED` 或 `NOT_REVIEWABLE_SAFELY`。`CHANGES_REQUIRED` 返回 Implementation；审查后代码或关键文档发生变化，旧审查立即失效。
- `verification.md` 必须包含自动化验证、AI 代码审查、架构对账/change surface 和人工审查包。人工审查必须记录确认人、时间、审查版本和代码指纹；人工未明确通过时不得关闭 Loop。
- Verification 还必须包含 Engineering Extensions 表，对 `testing`、`pbt`、`security`、`resiliency` 逐项记录 `PASS`、`FAIL` 或有理由的 `N/A`。PASS 必须指向证据；空白和“已检查”不算完成。
- 没有 PBT 库时，先判断是否存在有价值的不变量；简单属性用现有测试框架加确定性 seed 生成器，复杂生成或 shrinking 才申请新增依赖。**没有 PBT 库本身不是 N/A 理由。**PBT PASS 必须记录 Property、case 数和 seed，失败反例缩减后固化为普通回归测试。
- AI Review 前先记录当前指纹的 `architecture_reconciled` 和四项 `extension_evaluated`；结论写 `review_completed`，分流仓库同时写入本轮 `deliveryScope`。人工明确通过后写 `human_signed`，按项目规则归档六份阶段文档并更新状态，最后写 `loop_closed`。关闭后若本轮保护范围发生合法漂移，按治理节的人工核对路径追加 `closure_drift_accepted`，不得补签旧事件。这些事件只留痕和校验，不替 agent 自动归档或自动接受漂移。

## 事实优先级

出现冲突时按以下优先级处理：

1. 用户本轮明确指令
2. 当前 Loop 已确认需求
3. 当前 Loop 已确认架构和规格
4. 当前 Loop 的任务与验证记录
5. 本仓库当前代码
6. 历史文档和参考材料

不得静默解决语义冲突。冲突会影响范围、数据、安全、兼容性或交付结果时，应停止相关实施，记录冲突并请求用户决策。

## 文档规则

- 活跃文档应尽量少，只在阶段真正开始时创建。
- Requirements 中维护具体 Loop 的范围、非目标和交付边界，不在 `AGENTS.md` 或状态文件中复制。
- 不提前创建后续阶段的空壳文档；满足前一阶段门禁并真正进入该阶段时再创建。
- 文档状态只允许使用 `draft`、`confirmed`、`superseded`、`archived`。
- 把文档状态改成 `confirmed` 时，必须在该文档里留痕：**谁确认的、什么时候、确认的是哪一版**（日期 + 提交号或一句话摘要）。只在对话里说过「用户同意了」不算——那句话跟着会话一起消失，留下的文档只剩一个 `confirmed`，没人分得出它确认的是当时那一版还是后来被改过的。
- 修改已确认文档时必须说明原因和影响，不得只改正文而保留旧确认状态。
- 关闭 Loop 时，应在同一次变更中归档该 Loop 的阶段文档并更新状态文件；不得留下指向已归档目录的 activeLoop。
- 关 Loop 时用的是那唯一一份状态文件 `docs/loops/status.md`，归档落在 `docs/archive/`。
- `docs/backlog.md` 是**跨 Loop 常驻**文件：不属于任何 Loop，不参与阶段门禁，不随 Loop 关闭归档。它只记录，不做决定——某一条要不要做，由下一轮 Requirements 开局时用户拍板。第一次有东西要记时才创建它。
- 代码、提交和交付说明不得把规划能力表述为已实现能力。

## 工作区与 Git

- 工作前检查 Git 状态，保留用户已有和无关改动。
- 只暂存当前任务明确涉及的路径，不使用 `git add -A`。
- 未经用户明确要求，不提交、不推送、不部署。
- 删除、覆盖、数据库变更和共享环境操作前，必须确认准确目标与影响范围。
