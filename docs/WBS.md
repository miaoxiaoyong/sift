---
status: active
created: 2026-07-28
summary: Sift PoC 的里程碑、工作分解与验收标准
---

# Sift — 工作分解与里程碑

> **D0.3 · 对应 DESIGN D0.10 / PRD V0.8**
>
> 四份 D0.1 评审与 D0.2 独立复评均已处置；[D0.3 定向复核](reviews/2026-07-28-wbs-review-pi-gpt-5.6-sol-02.md)通过，状态为 `active`。

本文把 [DESIGN §13](DESIGN.md) 的八个纵向切片展开为任务与验收。需求语义以 [PRD](PRD.md) 为准，结构与理由以 [DESIGN](DESIGN.md) / ADR 为准，字段级契约下沉 `specs/`，执行步骤下沉 `plans/`；本文不复制完整协议表。

执行纪律：

1. 一个里程碑的完成条件只能依赖本里程碑及已完成前置里程碑。
2. 跨片验证标明「首次可运行 / 最终闭合」，不得把后续能力写成前置片门禁。
3. DESIGN 已有完整表格时，WBS 只写「逐行实现 + 不变量 + 验收」，避免部分复制被误读为全集。
4. 自动化门禁与人工验收都是 PoC 发布条件，但证据形式不同。

---

## 评审处置对账

评审原文保持只读：

- [codex-gpt5](reviews/2026-07-28-wbs-review-codex-gpt5.md)
- [cursor-opus5](reviews/2026-07-28-wbs-review-cursor-opus5.md)
- [glm-5.2](reviews/2026-07-28-wbs-review-glm-5.2.md)
- [kimi-code](reviews/2026-07-28-wbs-review-kimi-code-01.md)
- [pi-gpt-5.6-sol · D0.2 独立复评](reviews/2026-07-28-wbs-review-pi-gpt-5.6-sol-01.md)
- [pi-gpt-5.6-sol · D0.3 定向复核](reviews/2026-07-28-wbs-review-pi-gpt-5.6-sol-02.md)

| # | 合并后的发现 | 处置 | 落点 |
|---|-------------|------|------|
| R1 | Brain T1–T7、调用壳与 `specs/brain.md` 整块缺失 | 调用壳、T1/T2 在 M1；T3/T5 在 M4；T4/T6/T7 在 M5；统一 trace、预算与兜底契约贯穿 | M1 §1.7、M4 §4.2、M5 §5.1 |
| R2 | `startup_stall` 仲裁、隔离与 retry 两段式未分解，迟到事实描述错误 | M3 落隔离、唯一仲裁函数、最小 Interrupt 发射核心；M5 落 Command、retry 请求/结果两段式与 ADR-013 原子事务 | M3 §3.5–3.7、M5 §5.4 |
| R3 | M1–M4 验收依赖后续切片，形成环 | 骨架链不引入临时 Gate；V5/V10a/V9 分阶段；M3 前移唯一 Interrupt 发射核心，M5 扩展而不另建入口 | 各里程碑验收、验收权威表 |
| R4 | 配置/策略生命周期、敏感配置不热加载、两级启动探测和 Agent 定义校验缺失 | 新增硬约束；M1 实现全局配置生命周期与探测分级；M4 实现有效策略组装 | H16、M1 §1.4、M4 §4.1 |
| R5 | `max_escalations` 上限统一落 `hold` 与 PRD 冲突 | 改为按 reason 确定性映射到 `auto_reject` 或 `hold`；`startup_stall` 强制 `hold` | M5 §5.3 |
| R6 | Ledger 缺人类结果写入，认证投影与指标无数据源 | M4 落 Ledger 写入 API/认证投影；M5 的 Command 同事务写人类决定、语义原料并更新投影；补 PRD §10.2 全部九项指标与延迟指标 | M4 §4.4、M5 §5.5–5.7 |
| R7 | launcher、版本握手、`SIFT_RUN_DIR`、单实例和零网络监听缺失 | M1 定控制协议版本、单实例与零 listener；M3 落 launcher、同版本 wrapper 解析与 `SIFT_RUN_DIR`；M8 验发布归档 | M1 §1.5、M3 §3.1、M8 |
| R8 | 状态机缺 `queued → failed`、`waiting_human → done` | 已按 PRD §4.5 的既有事实收敛要求补回 PRD §4.1，WBS V1/V11 覆盖 | PRD §4.1、M1 §1.3、M2 §2.5 |
| R9 | V5、V9、V10a、V11、V12 与 A3/A5 归属漂移 | 设单一验收权威表，标注首跑与最终闭合；里程碑表只链接对应阶段 | §自动化门禁、§人工验收 |
| R10 | 恢复矩阵、V4 清单部分复制且遗漏 | 不再复制行级全集；要求逐行实现 DESIGN §10.1 / §12，并单列最易丢的不变量 | M3 §3.4、M6 |
| R11 | ADR-012 只有资格报告，没有恢复门控 | M3 默认未验证并禁止含糊状态自动 retry；M6 构造脱组；M7 对真实 Agent/version 取证 | M3 §3.4、M6、M7 §7.3 |
| R12 | M8 新增“稳定运行 ≥3 天”无来源 | 删除；不新增未标注来源的发布硬门槛 | M8 前置 |
| R13 | doctor 缺隔离、hooks 指纹和策略漂移；M7 双平台任务重复 | 补齐 doctor 接线；合并为一次正式 PoC 取证 | M3 §3.8、M4 §4.1、M7 §7.2、M8 §8.5 |
| R14 | ADR-010 旧名未处理 | 已按 DESIGN §14.14 仅增加 ADR-013 修订指针；历史正文保留旧名，新文档只用 `attempt_resolution` | ADR-010 决策 6 前 |
| R15 | D0.2 仍有 Gate→Interrupt、V11→指标两处后向依赖；CLI/doctor、通用 Command、软豁免与手工合并校准未闭合 | M3 发射器改为全 reason 泛型核心；V11 指标闭合移至 M5；补齐四项工作包与验收 | M1 §1.5、M3 §3.6、M4 §4.3–4.4、M5 §5.4/§5.7 |
| R16 | D0.3 定向复核 | 前序 F1–F6、N1–N2 全部关闭，无新阻断；允许进入 M1 specs | D0.3 定向复核 |


---

## 全局硬约束

| # | 约束 | 来源 |
|---|------|------|
| H1 | 状态转移、门禁通断、合并、指令解析均由确定性代码执行；LLM 只产出 recommendation | PRD A1 |
| H2 | `transition(runId, event, expected)` 是唯一 Run 状态写入口 | DESIGN §6.2 |
| H3 | Gate 是无 IO、无时钟的纯函数；输入先冻结 | DESIGN §8.5 |
| H4 | 影子门禁随 Gate 上线即常驻，每次调用都记录，无开关 | PRD §3.4、DESIGN §8.5 |
| H5 | Gate、影子门禁、认证投影、回放导出、Change 创建在 M4 同片交付 | DESIGN §13 |
| H6 | 双 socket 与「端点 + 凭据 → 动词子集」在 M1 定形 | DESIGN §3.2、§13 |
| H7 | `CGO_ENABLED=0` 四组合构建从 M1 进入 CI | DESIGN §13 |
| H8 | policy/context 从 base 读；不存在读取 worktree policy/context 的函数 | PRD §13.1、DESIGN §8.4 |
| H9 | 驱动性 forge 事件必须取得 actor 并通过 allowlist，取不到即忽略 | PRD §9.2 |
| H10 | token/API/注意力三类预算各有唯一收费口；注意力不可借支 | DESIGN §9.2 |
| H11 | 开放数值均为配置项，`specs/config.md` 提供确定性默认值 | DESIGN §14.2 |
| H12 | `.sift/**` 与 CI 配置是不可豁免硬护栏 | PRD §5.4 |
| H13 | merge 必须由 forge 远端比较 expected head；能力探测不能兑现时结构性禁用自动合并，不得降级为无条件 merge | ADR-011 |
| H14 | `process-group-verified` 只是 Agent/version 资格结论，不代表 TM6 或沙箱闭合 | ADR-012、DESIGN §14.14 |
| H15 | 新增设计主题前先拆分已越过提醒线的 DESIGN；WBS/spec 不继续向 DESIGN 堆字段契约 | DESIGN §14.14 |
| H16 | allowlist、配额、Agent 定义、全局缺省策略启动期一次读入并指纹校验；运行期磁盘变化告警且拒绝生效，修改须重启 | PRD §13.1、DESIGN §9.1 |
| H17 | `startup_stall` 的 resolution、隔离、迟到事实与 retry 结果必须共享一个 CAS 仲裁实现；终态不得冒充执行体已消失 | ADR-013、DESIGN §10.1 |

---

## 里程碑与模块总览

| 里程碑 | 主要产出 | 新增/扩展的 PRD 模块 |
|--------|----------|----------------------|
| M1 骨架 | Go/SQLite、状态机、outbox、控制面、配置、Brain 壳/T1/T2、fake 骨架链 | Brain、CLI |
| M2 Forge | GitHub/GitLab 适配、Intake、actor 闸门、API 预算 | Forge、Intake |
| M3 Runtime | process backend、wrapper handoff、恢复、worktree、泛型 Interrupt 发射核心 | Runtime、Attention（确定性核心） |
| M4 门禁 | 有效策略、T3/T5、Gate、Shadow Gate、Ledger、认证、回放、Change 创建 | Gate、Ledger、Brain |
| M5 注意力 | T4/T6/T7、Interrupt 全功能、Command、Report、Channel、指标 | Attention、Command、Report、Brain、CLI |
| M6 tmux | 第二后端与全恢复矩阵 | Runtime |
| M7 真实链 | 真实 Agent、双 forge、并发、资格与凭证 spike | 全模块集成 |
| M8 发布 | 单归档、四组合冒烟、托管、升级、Homebrew | CLI/运维交付 |

---

## M1：骨架

### 前置

- [x] ADR-010 决策 6 前已增加 ADR-013 名称修订指针
- [x] 新规范名称确定为 `attempt_resolution`，V0 枚举为 `reject | retry_after_absence`
- [x] 创建单 Go module 与三个命令：`siftd`、`sift`、`sift-agent-wrapper`（`go.mod` + `cmd/{siftd,sift,sift-agent-wrapper}`，#8）

### 任务

#### 1.1 Decode gateway、schema 与 CI

- [x] 单一 decode gateway，调用方显式选 `closed` 或 `open-envelope`（`internal/schema`；调用方传 `schema.Closed`/`schema.OpenEnvelope`）
- [x] 配置、LLM 输出、socket 请求用 `closed`；Forge envelope 用 `open-envelope`，必需语义仍 fail closed（gateway 与两种模式已落；config/LLM/socket 已接 `schema.Closed`、Brain provider envelope 接 `schema.OpenEnvelope`，各消费者类型随各自切片接线；#9 + #16/#18/#20）
- [x] 结构体生成 JSON Schema 并入 git；schema 漂移使 CI 失败（`internal/schema/genschema` + CI `schema-drift` job）
- [x] V14 golden tests 覆盖缺失字段、额外字段、类型/枚举变型（`internal/schema/contract_test.go`）
- [x] 从本片起在 CI 构建 darwin/linux × arm64/amd64，保持 `CGO_ENABLED=0`（`build-matrix` job）

#### 1.2 SQLite、事件与迁移

- [x] `modernc.org/sqlite`，WAL、foreign keys、busy timeout，写连接池上限 1（`internal/storage/storage.go`：`SetMaxOpenConns(1)`、PRAGMA 验证）
- [x] 前向迁移与 `schema_version`；数据库版本较新时拒启（`internal/storage/migrate.go`：`ErrDatabaseTooNew`/`ErrMigrationMismatch` + checksum）
- [x] 按 DESIGN §7 数据组落当前投影、attempt、只追加事件、Intake、outbox、校准/Brain trace、认证、预算、配置快照（`migrations/0001_initial_schema.sql`）
- [x] attempt 数据包含 `attempt_resolution` 与独立隔离标记；隔离期 worktree 不清理、不复用（attempts 表 `attempt_resolution`/`isolation_state`）
- [x] Interrupt 数据包含生成键、nonce、投递状态、升级计数及 `superseded_by_fact | superseded_by_decision` 等关闭原因（interrupts/interrupt_deliveries 表）
- [x] 事件与投影同事务；事件存储无 update/delete 业务入口（§13 append-only triggers；#12）

#### 1.3 状态机与事务核心

- [x] 实现 PRD §4.1 当前完整转移图，含外部事实的 `queued → failed` 与 `waiting_human → done`（`legalTransition`；`TestV1RunTransitionGraphAndCAS`）
- [x] `Recommendation → DomainCommand → Transition` 类型隔离；只有 `transition()` 写 Run 状态（`internal/storage/transition.go`）
- [x] CAS 拒绝过期命令；非法转移报错并记审计事件（`ErrRejectedStale` + `auditIllegalTransition`）
- [x] transactional outbox、稳定 operation key、提交唤醒与退避框架（`internal/storage/outbox.go` + `wakeOutbox` + `BackoffPolicy`）
- [x] 三组具名调度器与提交唤醒生产接线：`siftd` 分别驱动 Intake / Supervisor / Outbox；事务提交经 `DB.SetOutboxWakeup` 立即推进 outbox，独立时钟只在 `startSchedulers` 集中创建。`cmd/siftd/main_test.go` 以 production wiring factory 逐边验证 Intake 的未到期 `NextPollAtMS` skip、Supervisor/Outbox 不串联，且 outbox startup sweep drained 后 `EnqueueOperation` / `EmitInterrupt` 经 commit wake 到真实 comment worker；该生产接线已由 [#302](https://github.com/miaoxiaoyong/sift/issues/302) 的 [rereview-2 PASS](reviews/2026-07-29-m5-scheduler-wakeup-302-rereview-2-pi-gpt-5.6-sol.md) 核销。`internal/storage/scheduler_test.go` 只覆盖 storage seam 的并发 wake 收敛，不声称 production 职责/步频证据。此前 `scheduler.go` 的 Intake/Reconciler/Supervisor 仅为骨架，不是生产步频证据
- [ ] V1 与 V2 核心崩溃注入；当前已实现的状态、Forge Run/receipt、Task Spec、Brain trace/token、outbox claim/completion 写入族均以末写入点 abort 注入验证全有或全无（`TestV1RunTransitionGraphAndCAS`、`TestV2TransitionCrashAtomicity`、`TestV2CurrentWritePortsCrashAtomicity`）；项目健康、Forge 收费、Interrupt 推进与 delivery 在各自写端口实现时补入同一门禁，不得以 schema 代替崩溃证据

#### 1.4 配置与启动生命周期

- [x] 统一 `SIFT_HOME` 路径解析，默认 `~/.sift/`（`internal/config/home.go`）
- [x] 全局配置 schema、零配置默认值、Agent 定义 schema 与至少两个 Agent 配置的校验能力（`internal/config`：closed 契约、`DefaultConfig`、多 Agent 唯一性校验）
- [x] 敏感配置启动期一次读取并保存指纹；运行期变更只告警、不生效（`Load` + `Fingerprint` + `DriftChecker` warn-only）
- [x] 调度硬护栏：未知 Agent 拒绝、按 Agent 的 `max_concurrent`、需要时的项目互斥（`internal/config/guard.go`）
- [x] 启动探测分级框架：进程级失败拒启；项目级失败只隔离该项目并产生一次告警（`internal/config/probe.go`）
- [x] V12：不提供任何可选配置也能启动并调度；默认值表缺项即失败（`config_test.go` 两种场景 + 全默认断言）

#### 1.5 控制面与进程边界

- [x] `siftd.sock` 与 `run.sock` 分离；operator capability 只授权运维端点（`internal/controlplane/server.go` + `TestV10aEndpointCapabilitiesAndSockets`）
- [x] RPC envelope 携协议/二进制版本；主版本不一致拒绝（`protocol.go` `validateEnvelope`：`ProtocolMajor`/`ClientVersion` 主版本校验）
- [x] 单实例互斥，第二个 daemon 明确拒启（`acquireLock` flock + `TestSecondDaemonRefusesLock`）
- [x] 只创建 Unix socket，不创建 TCP/UDP listener；Linux 集成测试枚举本进程 socket inode 与 `/proc/self/net/{tcp,tcp6,udp,udp6}`，严格断言零 TCP/UDP listener（`TestV10ZeroNetworkListeners`）
- [x] V10a 首段：无 operator token 的运维请求被拒、`run.sock` 无运维动词、run token 不能调用 wrapper handoff 动词（`TestV10aEndpointCapabilitiesAndSockets`）
- [x] V10b：V0 以 Agent 身份读取 operator token 并调用运维 RPC 预期成功，同时 `doctor` 必须报告此未闭合边界（`TestV10bUnsafeLocalAttackReproduces` 以同 UID Agent 读取 token 后成功调用 `ops.doctor`，严格断言 `unsafe-local` + `operator-token-readable-by-agent`；M8 最终闭合）
- [x] 实现薄 CLI 的 `ps/logs/worktree/doctor` 与 `kill/retry` 请求壳；所有运维命令只走 daemon，不直连 DB（`cmd/sift/main.go`）
- [x] daemon 不可用时只允许明确标记为 offline 的只读诊断；`kill/retry` 等写操作拒绝，绝不离线改库（`sift doctor --offline` + `OperatorRequest` 失败拒绝）
- [x] `doctor` 基线检查 runtime、SQLite、Agent CLI、相关 forge CLI 登录/版本、按配置启用的 tmux、目录/socket 权限，且 CLI 进程退出状态映射 doctor `exit_code` 0/1/2（config.md §7；`cmd/sift/main_test.go` 覆盖 online/offline）；后续片增补策略、hooks、积压与安全姿态

#### 1.6 Reconciler 与 fake 骨架链

- [x] fake Forge、fake Agent、fake Brain provider 实现与真实端口同契约（[`internal/forge`](../internal/forge)、[`internal/attempt`](../internal/attempt)、Brain fake provider 自 #20）
- [x] M1 骨架链：fake Issue → T1/T2 → queued → fake attempt 完成证据 → 注入 fake forge「Change 已合并」事实 → done（[`internal/skeleton`](../internal/skeleton)）
- [x] M1 **不实现临时 Gate、不创建 Change、不保留旁路裁定**；M4 接入 Gate/Create Change 后替换测试夹具
- [x] 将 fake 骨架链作为 V9 的首段 CI 测试，而非手工验证（`internal/skeleton/chain_test.go`）
- [x] 事件时间戳覆盖「可信触发标签观测 → Agent started」，为 P50 指标留 day-1 数据

#### 1.7 Brain 调用壳、T1/T2 与 Task Spec

- [x] 编写 `specs/brain.md` 的统一调用与 T1/T2 契约
- [x] 本机 agent CLI 调用壳：stdin/临时文件输入 → schema 校验 → 同 prompt 重试一次 → 逐触点确定性兜底（`internal/brain/shell.go` + `provider.go` `SubprocessProvider`）
- [x] 提示词与 schema 版本化并入 git；调用身份与各触点作用域以 `specs/brain.md`、`specs/storage.md` §10.1 为准（`assets.go` `PromptVersion`/`OutputSchemaVersion`）
- [x] 每次调用按 `specs/storage.md` §10.1 持久化 call/attempt（`brain_calls` 一次终结 + 有序 `brain_attempts`）；具体调用、兜底与 Gate 关联契约由 `specs/brain.md` 定义（`ReserveBrainCall`/`RecordBrainAttempt`/`FinalizeBrainCall`）
- [x] T1：Issue 体检，失败兜底为直接入队（`T1Contract` + `T1FallbackOutput` = ready 直接入队）
- [x] T2：生成 kind/agent/goals/开工前审批建议；失败兜底为人工分派（`T2Contract`，fallback 留待人工分派）
- [x] 组装 Task Spec（Description + Goals + Guardrails + Context）；Context 从 base/全局/任务附注组合（`internal/brain/taskspec.go`）
- [x] token 收费口只在调用壳；超限后所有触点走各自兜底并产生告警事件（`Shell.Call` 唯一收费口 + preflight fallback）
- [x] replay JSONL 单条 `brain_call` record 内携有序 attempts（`ExportBrainCallsJSONL` + `TestExportBrainCallsJSONL`）
- [x] M1 fake 链使用 fake provider 的合法 T2 输出；真实 CLI 壳通过 fixture/子进程测试（`FakeProvider.ValidT2ResultText` + `TestShellWithRealCLIFixture`）

> Intake 澄清/确认评论的 crash marker 收敛与旧 generation 回复仲裁迁至 M2 §2.3/§2.5：两者依赖 M2 的真实 Forge comment worker、回复 receipt 消费与 `PersistIntakeDecision` 写端口。M1 只保留已实现、可独立验证的 Brain replay JSONL，不以 schema 或 outbox 通用框架冒充 Intake 实现。

### 先写 spec

- [x] `specs/storage.md`
- [x] `specs/control-plane.md`
- [x] `specs/config.md`（含全部确定性默认值、Agent 定义、路径与启动探测分级）
- [x] `specs/outbox.md`
- [x] `specs/brain.md`（调用壳、T1/T2；后续触点随片增补）

### M1 门禁

- [x] V1、已实现写端口的 V2 核心、V9 骨架段、V10a 端点段、V10b、V12、V14 通过（`TestV1RunTransitionGraphAndCAS`/`TestV2*`/`TestV9FirstSegmentSkeletonChain`/`TestV10aEndpointCapabilitiesAndSockets`/`TestV10bUnsafeLocalAttackReproduces`/`TestV12Scenario*` + `TestV12ZeroConfigStartsDaemon`/`TestV14*`；`CGO_ENABLED=0 go test ./...` 绿）
- [x] Brain 测试夹具与门禁对账：真实子进程 fixture 覆盖 schema 失败后同 prompt 重试、冻结 prompt 与逐 attempt token 收费（`TestShellWithRealCLIFixture`）；fake provider 覆盖逐触点兜底与 trace 恢复（`TestShellInvalidThenFallback`/`TestShellRecovery`/`TestShellZeroUsageNoCharge`）
- [x] V15 四组合构建段通过（CI `build-matrix` job darwin/linux × amd64/arm64 `CGO_ENABLED=0`，`check` job 同样以 `CGO_ENABLED=0 go vet ./...` 和 `go test ./...` 运行）
- [x] 第二实例拒启且进程无网络 listener（`TestSecondDaemonRefusesLock` + Linux `TestV10ZeroNetworkListeners`）
- [x] 敏感配置磁盘漂移不热生效；零配置启动通过（`internal/config` DriftChecker warn-only + V12 两种场景）

---

## M2：Forge 与 Intake

### 前置

- [x] M1 门禁通过（[第二次定向复审 PASS WITH NOTES](reviews/2026-07-29-s1-m1-rereview-2-pi-gpt-5.6-sol.md)）

### 任务

#### 2.1 Forge 端口与双平台适配

- [x] 逐项实现 PRD §5.2 最小动词集；签名与中性类型由 `specs/forge.md` 唯一定义
- [x] GitHub/GitLab 差异在边界归一；不确定语义输出 `unknown`，不得猜测
- [x] 评论与标签事件的 actor 为必需语义；缺失时适配器 fail closed
- [x] argv 数组执行 `gh/glab api`，禁止 shell 拼接
- [x] 统一错误为 `Transient | RateLimited | AuthOrCapability | ContractViolation | SemanticConflict`

#### 2.2 Change/merge 副作用契约

- [x] Change marker 跨 open/closed/merged 全状态查找；同 base/head 无匹配 marker 时返回冲突，绝不接管
- [x] merge 端口接受 expected head，适配器映射为远端原子条件检查
- [x] 能力探测不能证明条件合并时，将该项目 `auto_merge` capability 置为不可用；不得只告警后继续
- [x] V3/V7 覆盖 marker、冲突、stale head 与能力缺失禁用

#### 2.3 Intake、T1 接线与反向同步

- [x] 每项目独立自适应轮询，游标在整批持久化后推进；幂等键 `(forge, project, issue_id)`
- [x] 触发标签事件先回溯 actor；不可信作者被可信 actor 触发时强制开工前审批
- [x] T1 接在归一 Issue 后；T1 不可用时直接入队
- [x] 实现 `intake_items` 投影/CAS、回复 receipt 消费与 `PersistIntakeDecision` 写端口；澄清/重复确认评论与 intake 状态变更由同一领域事务创建 outbox operation
- [x] 澄清/确认评论在「远端成功、本地提交前崩溃」后按 marker 查询收敛，不重复发送；覆盖真实 Forge comment worker 的崩溃重放测试
- [x] 回复按当前 clarification generation 仲裁；旧 generation 回复只追加审计事件，不推进 intake 状态
- [x] 逐项实现 PRD §4.5 反向同步；事实观测不套 actor 鉴权，移除触发标签必须鉴权
- [x] 运行期 `AuthOrCapability` 只隔离对应项目并告警一次；健康项目继续调度

#### 2.4 Forge API 预算

- [x] API 调用只在 Forge 适配层收费；接近/达到上限时降为慢轮询并告警
- [x] reset、退避与预算状态持久化；不得在 M5 再建第二收费口

#### 2.5 契约与事实收敛测试

- [x] 双平台 fixture 跑同一契约套件：分页、actor 缺失、限流、平台差异、marker、merge CAS
- [x] intake 评论 worker 的远端成功/本地提交前崩溃重放不重复发送；旧 generation 回复只审计、不推进状态
- [x] V11 首段：fake/fixture 中让 `waiting_human` Run 的 Change 被外部合并，断言 `done + gate_bypassed`
- [ ] V11 在 M4 闭合 Gate/审计/Ledger 分类，在 M5 闭合指标分母

### 先写 spec

- [x] [`specs/forge.md`](specs/forge.md)（[字段级评审 PASS](reviews/2026-07-29-forge-review-pi-gpt-5.6-sol.md)）

### M2 门禁

- [x] V3 通过；V7 的 Forge/marker/CAS 部分通过（[第四次定向复审 PASS WITH NOTES](reviews/2026-07-29-s2-m2-rereview-4-pi-gpt-5.6-sol.md)）
- [x] 条件合并能力缺失时 `auto_merge` 被结构性禁用
- [x] actor 缺失事件被忽略；坏项目不影响健康项目
- [x] Intake crash marker 与旧 generation 回复仲裁测试通过
- [x] V11 外部事实收敛首段通过

---

## M3：Runtime 与启动停滞安全闭环

### 前置

- [x] M2 门禁通过（[第四次定向复审 PASS WITH NOTES](reviews/2026-07-29-s2-m2-rereview-4-pi-gpt-5.6-sol.md)）

### 任务

#### 3.1 ExecutionBackend、launcher 与 wrapper

- [x] `process` backend 只负责启动 wrapper；Agent 恒由 wrapper 直接 spawn 到其进程组
- [x] Agent 启动只经一个 launcher 函数，V0 为恒等实现；不得绕过该接缝
- [x] daemon 只从自身安装目录解析同版本 wrapper，不从 `PATH` 猜；wrapper/daemon 主版本不一致拒绝
- [x] Agent 环境只注入非机密 `SIFT_RUN_DIR`；bootstrap/run token 不进 argv 或环境变量
- [x] 逐条实现 DESIGN §8.4 wrapper 契约，控制文件用 temp + fsync + rename

> 证据（PR #109 / #107 及后续 P1 关闭链）：`internal/runtime/runtime.go` 的 `ProcessBackend` 仅 spawn wrapper，`Launcher`/`DirectLauncher` 是唯一 Agent 启动接缝，环境仅含 `SIFT_RUN_DIR`；`internal/wrapper/wrapper.go` 的生产状态机完成 bootstrap 读后删除、acquire/permit/started、control/heartbeat/result、信号转发与进程组回收；`internal/runtime/files.go` 的控制文件走 temp+fsync+rename+目录 sync、`0600`。`TestProductionWrapperKeepsAgentInWrapperProcessGroup`、`TestProductionWrapperReplaysLostPermitResponseWithSameParameters` 与 `TestLaunchWorkerWrapperCrashSuite` 覆盖生产拓扑、重放和崩溃链。

#### 3.2 Spawn handoff 与控制面最终接线

- [x] 逐步实现 ADR-010 的 operation lease、acquire/session、permit、spawning handoff、started 证据
- [x] wrapper 不写 DB；permit 的重放不得再次进入 spawn
- [x] `spawning` 期间不可换 owner；fencing 不能替代旧执行者消失证明
- [x] V10a wrapper 段：跨 instance/session/permit/generation 使用全部拒绝

> 证据（PR #115）：`internal/storage/handoff.go`——`AcquireLaunchClaim`/`PermitSpawn`/`ConfirmStarted` 三个 DB-only 写端口串联 operation lease + acquire/session（pending→`starting`）、一次性不可换 permit（`starting`→`spawning`）、started 证据（`spawning`→`running`），均只校验 wrapper 身份（PID+started_at_ms+executable+pgid），不依赖 fencing；permit 重放对相同元组为提交幂等 no-op、换 permit/owner 返 `ErrHandoffConflict`，`spawning` 期换 wrapper 即拒。`internal/controlplane/handoff.go`——`claim.acquire`/`claim.permit_spawn`/`claim.started` 三动词按 auth kind 与 `onlyKeys` 严格分诊，wrapper 仅经 RPC 调用，不写 DB。`internal/runtime/handoff.go`——`PermitGate.SpawnOnce` 在 wrapper 本地一次性消费 permit，重放不得再次进入 OS spawn。跨 instance/session/permit/generation 全部映射为 `unauthorized`/`stale`/`conflict`；#144/PR #146 起这些拒绝经 `handoffFailure`→`RecordHandoffSecurityEvent` 记 `security.handoff_rejected` 安全事件（证据见 §3.4）。覆盖 `TestHandoffPermitReplayAndStartedEvidence`。后续 P1 关闭链已将这些端口接入生产 launch worker/wrapper，并以 `TestLaunchWorkerWrapperCrashSuite`、`TestLaunchWorkerKilledAtHandoffBoundaries`、`TestProductionWrapperReplaysLostPermitResponseWithSameParameters` 与 paused recovery matrix 覆盖跨崩溃 handoff；完整双后端恢复矩阵仍按 §3.4/M6 单独跟踪。

#### 3.3 Worktree 与成功证据

- [x] 每 Run 独立 worktree；policy/context 只从 base 读
- [x] Sift 的 git 命令强制 `-c core.hooksPath=/dev/null`
- [x] 仅成功且身份一致的 `result.json`、冻结 final head、有提交三者齐备时产生“可创建 Change”领域事实；实际创建在 M4
- [x] 失败 attempt 与中间提交不得产生 Change 操作

> 证据（PR #118）：`internal/worktree/worktree.go`——`Manager.Create` 按 `root/<runID>/<attemptNo>` 建 per-run worktree，`ReadBaseFile` 恒走 `git show <base>:<name>` 从 base 读，worktree 内被改写的 policy/context 不生效；`command()` 在每条 git argv 前置 `-c core.hooksPath=/dev/null`（含 create/remove/read/rev-parse）；`EvaluateSuccess` 要求 exit 0 且无 signal、非空 digest、agent 身份一致、冻结 final head（rev-parse 重校验一致）、`rev-list --count ≥ 1` 四者齐备才返回 `ReadyChange`（领域事实，实际 Change 创建留 M4），任一缺失返 `ErrEvidenceRejected`/`ErrNoCommit`——失败 attempt（exit≠0/有 signal）与中间提交（final head 不匹配）均被拒、不产生 Change 操作。覆盖 `TestManagerCreatesIsolatedWorktreeAndReadsBaseOnly`、`TestEvaluateSuccessRequiresMatchingIdentityHeadAndCommit`。注：成功事实尚未接 Gate/Create Change（M4）；worktree 回收随 §3.5 隔离投影闭合，对应项保持 `[ ]`。

#### 3.4 恢复矩阵与资格门控

- [ ] **逐行实现并逐行测试 DESIGN §10.1 完整恢复矩阵**；本文不复制行级全集
- [x] 恢复扫描先于启动 operation lease 回收
- [x] 凡执行体可能存活却要判 orphaned，必须先走同一受控终止流程
- [x] 进程身份至少校验 PID + 启动时间 + 可执行路径 + control nonce；不得向不确定 PID 发信号
- [x] attempt 所用 Agent/version 未标 `process-group-verified` 时，不把“进程组消失”当充分证明，不自动 retry，保持隔离并转人工
- [ ] 多 wrapper 竞争、旧 generation 苏醒、heartbeat 过期、后端会话与 wrapper 不一致均按 DESIGN 矩阵收敛并记安全事件

> 证据（PR #129）：`internal/daemon/termination.go`——唯一 `TerminationCoordinator` 是恢复 / 超时 / operator kill·retry 三源到受控终止的应用层桥：`Recover`（启动期、先于 `daemon.Assemble` 起任何 worker）、`Timeout`（supervisor tick 扫持久化 heartbeat 事实，`StaleHeartbeatAttempts`）、`Operator`（`ops.kill`/`ops.retry`）三入口汇入同一私有 `terminate` → `Terminator.Terminate`（§3.7）→ `RecordTerminationObservation`（`internal/storage/termination.go`）。故凡「执行体可能存活却要判 orphaned」一律先经同一受控终止流程，第 3 项勾选。`internal/controlplane/server.go` 经 `SetOperatorAction` 把 kill/retry 限定在 daemon 拥有的回调上、CLI 仍不获 DB 句柄；`RecoveryAttempts` 显式不限 `runs.status`（`failed` Run 仍可持有隔离的存活执行体），覆盖 `TestRecoveryAttemptsIncludesNonterminalAttemptRegardlessOfRunState`、`TestOperatorKillAndRetryDelegateToTerminationCoordinator`。第 5 项已勾：`terminate` 仅在 `result.Absent` 且 `ProcessGroupVerified(agentID)` 为真时才认进程组消失为充分证明，而 `cmd/siftd/main.go` 未注入该谓词（nil），故任何 Agent/version 的进程组消失都不当充分证明、不自动 retry，落 `process_group_unverified` 诊断并经 §3.6 转 `waiting_human` + 隔离。
>
> **未完成（不得读作端到端）**：①完整恢复矩阵逐行（DESIGN §10.1 全行）——定向行已由 #144/PR #146 交付：多 wrapper 竞争、旧 generation 苏醒经 `internal/controlplane/handoff.go` 的 `handoffFailure` 调 `RecordHandoffSecurityEvent`（`internal/storage/security.go`，`security.handoff_rejected` 事件、disposition `conflict`/`stale`/`unauthorized`、不存凭据，覆盖 `TestRecordHandoffSecurityEventDoesNotPersistCredentials`）记安全事件；`Recover` 经 `ownerIsLive`（`Inspector.Observe` 校验完整 wrapper 身份）保留 starting/spawning 及 heartbeat 新鲜的 running owner、stale heartbeat 经 `heartbeatStale` 路由进同一 `terminate`（覆盖 `TestRecoverKeepsLiveStartingOwner`、`TestRecoverRoutesStaleHeartbeatThroughTermination`），第 6 项定向行（竞争/generation 苏醒/heartbeat 过期）已交。但全集未齐——`pending` re-dispatch 已由 `RecoverStartup` + `ApplyStartupRecoveryAction(redispatch)` 及 `TestRecoverStartupRedispatchesPendingAttemptBeforeOpeningBarrier` 闭合；后端会话存活观测（§10.1「后端会话在/wrapper 不在」「wrapper 在/后端会话不在」）依赖尚未实现的 tmux 后端，按验收权威表留 M6，故第 1/6 项保持 `[ ]` 表示完整双后端矩阵尚未最终闭合。②boot recovery lease barrier 已由 #138 闭合：`StartDaemonBoot` 为每次启动写入一个 recovery completion 为空的新 boot，`cmd/siftd/main.go` 在 `Recover` 成功后才调用 `CompleteStartupRecovery`；`ClaimLaunchOperation` 在同一事务内验证该 boot 的 completion，再租约认领（含过期 lease 回收）。普通 outbox claim 与 kind-scoped claim 均拒绝 `launch_agent`，不能旁路屏障。覆盖 `TestLaunchClaimWaitsForCurrentBootRecoveryBarrier`。③Linux 平台身份探测已由 #133/PR #135 闭合：生产 `Inspector` 为 `runtime.PlatformProcessInspector`——Linux 经 procfs + owner-only `control.json` 独立重建 PID/启动时间/可执行路径/PGID/control nonce hash，任一证据缺失/失配即 live-but-incomplete、`Terminator` fail-closed **不发信号**；Darwin 无 native inspector，统一身份未知走同一冻结路径。第 4 项勾选。另注：第 5 项 `ProcessGroupVerified` 真值分支生产仍不可达——`cmd/siftd/main.go` 未注入该谓词（nil），即便 Linux 上 `Terminate` 已能证消失，协调器仍映射 `process_group_unverified`、确认消失/自动 retry 仍仅测试覆盖；真实资格判定谓词（按 Agent CLI + 版本）属 M6/M7。
>
> **P1 定向结案（#201 / PR #202，PASS WITH NOTES）**：[第八次定向复审](reviews/2026-07-29-m3-rereview-8-pi-gpt-5.6-sol.md) 确认 paused recovery matrix 已用 outer supervisor、持久 execution wrapper 与 Agent 三面身份、恢复前后存活/空区间及持久 owner/claim/session/permit 投影，关闭 P1-1g5/g6 的“至多一个 owner”证据缺口；精确同步、真实 `SIGSTOP`、生产 `RecoverStartup` 路径和 marker 稳定性未回退。该结论关闭 M3 process 段的最后 P1，并支持 §3.1/§3.2 与 M3 V4 首跑门禁勾选；它**不等于完整 V4**：backend session liveness、生产 `ProcessGroupVerified`、DESIGN §10.1 双后端最终矩阵及 hooks 基线写入/自动复核（§3.8）仍分别留 M6/M7 或后续片。阶段门禁裁决见 [M3 phase gate](reviews/2026-07-29-m3-phase-gate-pi-gpt-5.6-sol.md)。

#### 3.5 attempt_resolution、隔离与唯一仲裁

- [x] 隔离是独立投影：即使 Run 已 `failed`，未证明执行体消失前 worktree 不回收、不复用
- [x] 实现一个仲裁函数，供 `claim:started`、恢复补 started、迟到 `result.json`、Interrupt 指令四个入口调用
- [x] resolution 未落定且合法启动事实先到：attempt → running、Run `waiting_human → running`、Interrupt 以 `superseded_by_fact` 关闭并接管监督；**不得继续终止正常执行体**
- [x] `attempt_resolution=reject | retry_after_absence` 先落定：迟到事实不推进旧 Run，登记身份、返回 `superseded_by_decision` 并受控终止旧执行体
- [x] 自动 escalate/hold 不写 resolution，事实优先窗口保持开放

> 证据（PR #125 / #124 / #141）：`internal/storage/attempt_race.go`——唯一 `ResolveAttemptRace` 是执行事实与 `attempt_resolution` 的单一线性化点，**显式不释放隔离**。事实先到 / 决定先到交错行为同前；`retry_after_absence` 由 `RecordTerminationObservation`（Source=retry、absence 已确认）写入。覆盖 `TestResolveAttemptRaceFactWinsWhileFrozen`、`TestResolveAttemptRaceDecisionAbsorbsLateFact`。
>
> **接线状态（PR #141）**：四入口中 `claim:started`、恢复补 started、迟到 `result.json` 已接——`internal/daemon/termination.go` 将 recovery-started / late result 事实汇入 `ResolveAttemptRace`；decision-first 路径可触发受控终止旧执行体。Interrupt 指令（M5 §5.4 `/sift reject|retry|...`）仍未实现，人工决定入口留 M5。第 4 项勾选（运行时迟到事实路径已达）；第 5 项仍为仲裁函数不变量。

#### 3.6 Attention 泛型单一发射器核心

- [x] 在 M3 建立此后唯一的 Interrupt 发射入口；M4/M5 只能调用或扩展渲染/调度，不能新建第二入口
- [x] 入口从第一天支持 PRD 全部 reason 的最小确定性契约：reason/min_modality、互斥 options（≤4）、fallback headline/brief/links、expires/on_expire 与 severity 映射；T4 不可用时也能生成合法对象
- [x] 每类故障有带 domain/version/reason 的稳定生成键并受唯一约束；`startup_stall` 使用 `(run_id, attempt_no, generation, cause=startup_stall)`，诊断分类不拆键
- [x] Run 转移、Interrupt、注意力记账、事件、发布 operation 五件事同事务
- [x] M3 使用已有 forge 评论与确定性 fallback 作为可见发布面；T4/T6、Channel、critical 熔断在 M5 增补
- [x] 受控终止无法证明消失时生成一条 `startup_stall`、Run 转 `waiting_human`、attempt 保持隔离；不得静默停在 queued

> 证据（PR #111 / #108）：`internal/storage/interrupt.go`——唯一 `EmitInterrupt` 创建端口、七 reason 模板与确定性渲染（headline/brief/options≤4/min_modality/links）、`interruptGenerationKey` 唯一键（`startup_stall` 固定 `cause=startup_stall`，诊断分类不拆键）、按 generation_key 去重、单事务内 Run→`waiting_human` + 注意力扣费 + `interrupts` + `interrupt.emitted` 事件 + `forge_comment` operation/delivery 五件事。注：第 6 项的 `EmitInterrupt` 接线已由 PR #122 落地——`internal/storage/termination.go` 的 `RecordTerminationObservation` 在 `Absent=false` 时调用 `EmitInterrupt(startup_stall)`，复用本端口已有的 Run→`waiting_human` + 隔离冻结 + 注意力扣费 + 发布 operation 同事务（覆盖 `TestTerminationUnconfirmedFreezesAndMakesStartupStallVisible`）。触发该路径的 `Terminator.Terminate` 经 §3.7 `TerminationCoordinator`（`Recover`/`Timeout`/`Operator`，PR #129）已生产可达——Darwin 生产 inspector fail-closed（身份未知）恒判未证消失，Linux 虽自 PR #135 起能证消失，但因 `ProcessGroupVerified` 谓词未注入（nil）仍映射 `process_group_unverified`，故恢复/超时/operator 三源的非终态 attempt 在两平台一律经此 `Absent=false` 路径落 `startup_stall` 而非静默停在 queued；第 6 项与门禁「无法证明消失时系统可见且 worktree 隔离」勾选。

#### 3.7 受控终止

- [x] 恢复、operator kill/retry、超时共用：身份确认 → 有界信号升级 → 复核消失
- [ ] 确认消失后的结局按来源区分：恢复按重试策略、retry 新建 attempt、kill 不新建且 Run failed
- [x] 未确认消失统一进入 §3.6；人的后续 retry/reject/hold 在 M5 接通

> 证据（PR #122 / #120 / #129）：`internal/runtime/termination.go`——唯一终止入口 `Terminator.Terminate` 落「身份确认 → 有界信号升级（TERM→KILL，按 grace）→ 复核消失」核心：先 `Observe` 校验完整身份（PID+启动时间+可执行路径+PGID+control nonce hash，`subtle.ConstantTimeCompare` 常量时间比对），身份不符即 `TerminationIdentityUnknown` 且**绝不发信号**（拒绝把 PID 复用当消失证明），有界 recheck 复核；`UnixProcessSignaler.SignalGroup(-pgid)` 对进程组发信号，wrapper 内 agent 后代一并终止。`internal/storage/termination.go`——`RecordTerminationObservation` 是三源共享的持久化端口，`Source∈{recovery,retry,kill}`：`Absent=true` 时释放隔离并按来源分诊结局（kill→Run `failed`/`operator_kill` 不建新 attempt；recovery/retry 在 `retry_count+1<max_attempts` 时建 pending 新 attempt 并补 launch operation，耗尽则 `attempts_exhausted` failed），落 `termination.absence_confirmed` 事件；`Absent=false` 时走 §3.6 `EmitInterrupt(startup_stall)` + 隔离冻结。覆盖 `TestTerminatorSignalsOnlyVerifiedIdentityAndProvesAbsence`/`TestTerminatorNeverSignalsReusedOrUncertainPID`/`TestTerminatorEscalatesAndFailsClosedWhenGroupRemains`、`TestTerminationUnconfirmedFreezesAndMakesStartupStallVisible`/`TestTerminationKillAfterAbsenceFailsWithoutNewAttempt`/`TestTerminationRetryAfterAbsenceCreatesNewAttempt`。**调用接线（PR #129，闭合原「端口已有、调用未接」）**：`internal/daemon/termination.go` 的 `TerminationCoordinator` 是三源到受控终止的唯一应用层桥——`Recover`（启动期先于 worker）、`Timeout`（supervisor tick 的 stale heartbeat）、`Operator`（`ops.kill`/`ops.retry` 经 `controlplane.SetOperatorAction`）汇入同一 `terminate` → `Terminator.Terminate` → `RecordTerminationObservation`，故 §3.7 的共享调用桥已接；未确认分支生产可达，第 1/3 项勾选，确认消失分诊因下述资格谓词缺口保持 `[ ]`；覆盖 `TestOperatorKillAndRetryDelegateToTerminationCoordinator`、`TestRecoveryAttemptsIncludesNonterminalAttemptRegardlessOfRunState` 及上列 Terminator/termination 测试。
>
> **未完成（不得读作端到端）**：生产 `Inspector` 自 PR #135 起为 `runtime.PlatformProcessInspector`——Linux 经 procfs + owner-only `control.json` 独立重建身份，可发信号并证消失；Darwin fail-closed（身份未知、不发信号）。**但** `cmd/siftd/main.go` 未注入 `ProcessGroupVerified` 谓词（nil），故协调器即便在 Linux 拿到 `result.Absent=true` 也映射为 `process_group_unverified` 诊断、`cmd.Absent` 仍为假——确认消失分支（第 2 项按来源分诊结局、`termination.absence_confirmed` 事件、自动 retry）生产仍不可达，三源的非终态 attempt 在两平台一律经 `Absent=false` 走 §3.6 `startup_stall` 兜底（Linux 是「资格谓词缺失」所致，Darwin 是「无 native 探测」所致）。§3.4 第 4 项（平台进程身份）已闭合；完整恢复矩阵（§3.4 第 1/6 项）仍留 M6；真实资格判定谓词（按 Agent CLI + 版本）属 M6/M7；第 3 项「人的后续 retry/reject/hold 在 M5 接通」仍属 M5 §5.4，本片不闭合。

#### 3.8 hooks 与 doctor

- [ ] hooks 指纹覆盖 `.git/config`、`core.hooksPath` 值和最终目录内容；Agent 结束后复核（`internal/hooks`；doctor 对当前基线做漂移报告）
- [x] `doctor` 报 hooks 漂移、隔离 attempt/未回收 worktree、process-group 资格与 `unsafe-local`

> 证据（PR #134）：`internal/hooks/fingerprint.go`——`Capture` 覆盖 `.git/config` 摘要、`core.hooksPath` 取值、effective hooks 目录与目录内容摘要并合成 `Digest`（`TestCaptureIncludesConfigHooksPathAndDirectory`）。`internal/controlplane/doctor.go`——`hookChecks` 经 `hooks.Capture` 取当前指纹、与 `project_hook_baselines` 基线比对报漂移（absent/drifted/match），`processGroupChecks` 对每个 Agent 报 `process-group-unverified`，`attemptChecks` 列 `frozen` 或非终态 attempt（含 worktree 路径），`unsafeLocalCheck` 保留 V10b 边界；`internal/storage/doctor.go` `ReadDoctorState` 只读投影、从不写/迁移。
>
> **缺口（诚实标注）**：`project_hook_baselines` 在生产无写入方——仅有 reader（`ReadDoctorState`）与 schema/索引，无任何代码在项目启用或 attempt 起止时捕获并落基线，故 doctor 在生产恒报「baseline is absent」、漂移比对尚无基线可比。「Agent 结束后复核」当前仅由 doctor 这一 operator 触发路径承载，并无 attempt 完成后自动重捕的接线。基线写入与自动复核属已知缺口，不读作 §3.8 已端到端闭合；M3 门禁不依赖 hooks 漂移生效（V5b 硬护栏留 M4）。

### 先写/增补 spec

- [x] `specs/control-plane.md`：acquire/permit/started 完整字段与版本握手（PR #94 / #88——§3.4 版本握手、§4.2–4.3 acquire/permit/started；PR #135 增补 §5.4 `control_nonce_hash` 与 §6 Linux/Darwin 进程身份契约）
- [x] `specs/storage.md`：resolution、隔离、关闭原因（PR #95 / #89——`attempt_resolution`/隔离独立投影/`close_reason`；行级实现见 §3.5）
- [x] `specs/config.md`：启动 lease/等待/终止/复核/Report 退避默认值（PR #91 / #90——`spawn_operation_lease_ttl`/`starting_permit_timeout`/`spawning_started_timeout`/终止 grace/`absence_recheck_*`/`not_ready` 退避）
- [x] `specs/interrupt.md`：先落全部 reason 的最小确定性契约与 `startup_stall` 特殊规则（[三次字段评审 PASS WITH NOTES](reviews/2026-07-29-interrupt-rereview-2-pi-gpt-5.6-sol.md)）

### M3 门禁

- [x] V4 的 **M3 process 首跑段**（backend、handoff、恢复安全不变量、受控终止与未验证资格 fail-closed 门控）通过；完整双后端矩阵仍按验收权威表在 M6 最终闭合（[M3 phase gate PASS WITH NOTES](reviews/2026-07-29-m3-phase-gate-pi-gpt-5.6-sol.md)）
- [x] V5a：base/worktree 读取源通过；硬护栏 V5b 留 M4（`TestManagerCreatesIsolatedWorktreeAndReadsBaseOnly` 覆盖 base-only 的 policy/context 读取与 worktree 改写隔离）
- [x] V10a wrapper 凭据部分通过
- [x] 每个 PRD reason 均能在无 T4/T6 时生成结构合法、可发布的 fallback Interrupt（`interruptTemplates`/`renderInterrupt` golden/vector 契约）
- [x] 同一 startup_stall 并发发现只生成一条 Interrupt、扣一次配额、保留一条可重放发布 operation（generation-key 唯一冲突按幂等回放收敛）
- [x] 无法证明消失时系统可见且 worktree 保持隔离；本片不要求 M5 的人工 retry 两段式

---

## M4：Gate、Shadow Gate、Ledger 与回放

### 前置

- [x] M3 门禁通过（[phase gate PASS WITH NOTES](reviews/2026-07-29-m3-phase-gate-pi-gpt-5.6-sol.md)；不代表完整 V4/M6 已通过）
- [x] 本片开始前先完成下列 spec，而不是把 spec 完成列为本片结束条件

> 规格对账证据：[`policy.md`](specs/policy.md) 已由 [PR #221](https://github.com/miaoxiaoyong/sift/pull/221) 字段级评审转为 `active`；[`ledger.md`](specs/ledger.md) 已由 [PR #232](https://github.com/miaoxiaoyong/sift/pull/232) P1 复审转为 `active`；[`brain.md`](specs/brain.md) 的 T3/T5 已由 [PR #228](https://github.com/miaoxiaoyong/sift/pull/228) 字段级评审补齐并保持 `active`；[`gate.md`](specs/gate.md) 已由 [PR #238](https://github.com/miaoxiaoyong/sift/pull/238) [二次定向复审](reviews/2026-07-29-gate-rereview-2-pi-gpt-5.6-sol.md)转为 `active`。以下仅对账规格完成，M4 实现项仍待交付。

### 任务

#### 4.1 Policy 与有效策略组装

- [x] 编写 `specs/policy.md`；项目 policy 经 closed schema 校验，失败只隔离该项目
- [x] Gate 外组装：base policy ∪ 全局缺省 → 按认证与 forge capability 剔除未获资格的提权项（`internal/policy`）
- [x] `auto_merge` 同时要求配置、类别认证、远端 expected-head CAS capability；缺一即关闭（`internal/policy`）
- [x] 有效策略 hash 与 certification version 进入冻结输入（`internal/policy.FrozenInput`）
- [x] `doctor` 横向比较项目策略并标记漂移；Gate 本身不读取配置/认证/文件（`internal/controlplane/doctor.go`）

#### 4.2 Brain T3/T5

- [x] 在 `specs/brain.md` 增 T3/T5 schema、提示词版本与兜底
- [x] T3 输出风险分与风险点；失败/超预算视为高风险
- [x] T5 分类 flaky/真实失败/基础设施问题；失败兜底为 HITL
- [x] 两触点复用 M1 调用壳、token 收费与 trace；输出来源/版本进入 Gate 快照

#### 4.3 Gate、Shadow Gate 与 Change 创建

- [x] 编写 `specs/gate.md`；`gate(changeFacts, effectivePolicy, riskScore)` 保持纯函数
- [x] `gate_input_hash` 摘要整份规范化快照；缓存键仅 `(gate_input_hash, gate_version)`（`internal/gate`：`CanonicalInput`/`Cache.EvaluateCached`；持久化 evaluation 仍随本节 Shadow 事务项闭合）
- [x] 默认硬护栏、Checks、review policy、auto merge 顺序按 PRD §5.4（`internal/gate.Evaluate` + `TestEvaluateOrderingAndShadow`）
- [x] 软护栏豁免默认仅本 Run 本次命中；“记住”必须是独立显式选项，并形成可审计的仓库 policy 例外变更（`internal/gate.Exemption` / `EffectivePolicyV1.protected_paths.soft_exceptions`）
- [x] 硬护栏永远不进入一次性/记住豁免路径；测试同时覆盖两类软豁免与硬护栏拒绝（`internal/gate.TestSoftExemptionIsBoundToPaths` / `TestEvaluateOrderingAndShadow`）
- [x] Gate 每次调用强制写快照与影子预判，无配置开关；行为测试断言每次调用新增 calibration 行（`storage.RecordGateEvaluation` / `TestRecordGateEvaluationAlwaysAppendsCalibration`）
- [x] 需要 HITL 时，预判与 M3 发射器的 Interrupt 五件事同事务（`storage.RecordGateEvaluationAndEmitInterrupt` / `TestGateHITLIsAtomicWithCalibration`）
- [x] 仅消费 M3 的“可创建 Change”事实；创建操作使用 marker 并持久化远端 ID（`gate.EnqueueCreateChange` / `forgeworker.ChangeWorker`）

#### 4.4 Ledger、人类结果接口与认证投影

- [x] 编写 `specs/ledger.md`，覆盖 Gate 预判、人类决定、路径/文件类型、护栏、Issue 作者、打扰特征及自然语言原料
- [x] 提供确定性 `recordHumanDecision` 应用入口；M5 Command 只调用它，不另写账本（`storage.RecordHumanDecision`）
- [x] 人类结果、校准样本与认证投影增量在同事务提交（`storage.RecordHumanDecision`）
- [x] 认证按任务类别计算漏放、误拦、总样本与负样本绝对数；输出只有类别布尔与证据摘要（`storage.Certification`）
- [x] forge 手工合并在已有 Gate 预判时调用 `recordHumanDecision`：把手工合并记为人的实际决定、保留校准样本并附 `gate_bypassed`（`external_decision_bindings` + `storage.RecordHumanDecision`）
- [x] `gate_bypassed` 不进入 Sift 自发合并的误放行率分母，但作为独立绕过样本保留（认证仅聚合 settled calibration）

#### 4.5 回放集

- [x] `SELECT → JSONL` 导出当时冻结的 Gate 快照，不拼当前数据（`storage.ExportReplayJSONL`）
- [x] 导出独立 Brain trace；仅真实参与 Gate 输入时携可空 snapshot ID（`brain_gate_input_links` + Brain JSONL v2）
- [x] 导出后用同一 Gate 函数/Brain trace runner 重放；可量化漏放/误拦变化（`internal/replay`）

### 先写 spec

- [x] `specs/policy.md`
- [x] `specs/gate.md`
- [x] `specs/ledger.md`
- [x] 增补 `specs/brain.md`（T3/T5）

### M4 门禁

> **阶段门禁评审（#256）：[FAIL](reviews/2026-07-29-m4-phase-gate-pi-gpt-5.6-sol.md)。** 当前组件 API 与定向测试已存在，但生产 Gate 纵向链、`merge_change` producer/worker、外部合并到 Ledger 的接线及阶段级组合证据仍缺失；以下六项均无充分证据勾选，M5 前置保持未通过。
>
> **P1 关闭后定向复审（#263）：[FAIL](reviews/2026-07-29-m4-phase-gate-rereview-pi-gpt-5.6-sol.md)。** PR #259–#262 已补生产形 Gate/merge/external-audit/replay 主体，但 policy 读取错误仍可退化为 missing，V7 没有 B 重过 Gate及成功终态重读，reverse-sync 会把 Sift merge 错记为人工绕过，阶段测试也以 seed 跳过 Change 创建与认证结算；六项继续不勾，M5 前置不开放。
>
> **第五次定向复审（#279）：[FAIL](reviews/2026-07-29-m4-phase-gate-rereview-5-pi-gpt-5.6-sol.md)。** PR #278 已补足 policy alert 幂等、T3/T5 fallback snapshot association 与 V6 cache-miss vectors，前三项据证据勾选；V7 的精确 CLI merge SHA/marker recovery replay及 V11 的 Forge 事实优先收敛仍未闭合，M5 前置不开放。
>
> **第六次定向复审（#283）：[PASS WITH NOTES](reviews/2026-07-29-m4-phase-gate-rereview-6-pi-gpt-5.6-sol.md)。** PR #282 已补双平台精确 merge SHA、真实 Change marker recovery replay，以及 `queued/running` 与 exact binary/inconclusive/missing/ambiguous binding 的 facts-first V11 矩阵；后三项据证据勾选，M4 门禁通过并开放 M5 前置。PRD 状态图同步与并行时序 flake 属非阻断注记。

- [x] V5b：`.sift/**`、CI 配置、head 变化 fail closed；A5/A6 在本片闭合
- [x] T3/T5 正常输出与确定性兜底均被版本化并进入 trace/Gate 快照
- [x] V6：纯函数、cache miss、每次 Gate 必有校准记录、导出重放通过
- [x] V7：Change marker 与 merge stale/no-op 全链通过
- [x] V11 审计段：等待 Gate/HITL 时外部合并 → done + gate_bypassed，并写入人类决定/校准分类；指标查询分母留 M5
- [x] Gate/Shadow/认证/回放/Change 创建五项同时可用，无延后项

---

## M5：Attention、Command、Report、Brain 与指标

### 前置

- [x] M4 门禁通过（[第六次定向复审 PASS WITH NOTES](reviews/2026-07-29-m4-phase-gate-rereview-6-pi-gpt-5.6-sol.md)）

### 任务

#### 5.1 Brain T4/T6/T7 与 A7 防火墙

- [x] T4/T6/T7 调用壳与验收矩阵已由 [rereview-6 PASS](reviews/2026-07-29-m5-brain-t4-t6-t7-impl-406-rereview-6-pi-gpt-5.6-sol.md) 核销；这不表示生产接线或 M5 已实现
- [x] T4 生成 headline/brief/options；失败兜底为裸链接 + 原始状态（生产接纳已由 [#706 PASS WITH NOTES](reviews/2026-07-30-m5-t4-emit-interrupt-706-rereview-pi-deepseek-v4-pro.md) 核销：T4 canonical trace 与 fallback renderer 逐字节持久化、`invalid_output` 兜底写原始 trace envelope；注记仅涉 Report 配额路径覆盖完整性，不表示 T6/T7 接线或 M5 已实现）
- [x] T6 只建议时机/通道，失败按 severity 确定性阈值；任何结果仍经过发射器配额（生产接线已由 [#721 PASS](reviews/2026-07-30-m5-t6-emit-interrupt-721-rereview-pi-deepseek-v4-pro.md) 核销：事务外 `Shell.Call(T6)` 镜像 T4、一档降级且 high/critical 强制 immediate、三层确定性兜底写 Brain trace，任何结果仍进唯一发射器配额；不表示 T7 接线、调度、critical 熔断或 M5 已实现）
- [x] T7 只生成 policy 提案或 context 草稿，二者都不自动生效（I5 T7/A7 防火墙已由 [#733 PASS](reviews/2026-07-30-m5-t7-a7-firewall-rereview-733-pi-deepseek-v4-pro.md) 核销：`SaveProposalDraft` 是唯一写口、仅 INSERT 且 status 恒为 `pending_human_approval`，`BEFORE UPDATE/DELETE` 触发器强制 append-only；无 outbox/budget/Gate/Interrupt/状态转移副作用，policy 与 context 两种 `proposal_kind` 均覆盖，fallback 一律不创建草稿；T7 生产调用器未接线）
- [x] 测试 T7/历史数据不能放松单条 Gate、不能抑制单条 HITL（I5 T7/A7 防火墙已由 [#733 PASS](reviews/2026-07-30-m5-t7-a7-firewall-rereview-733-pi-deepseek-v4-pro.md) 核销：`internal/gate/` 非测试代码零引用 `proposal_drafts`/T7，`Evaluate` 是冻结输入纯函数；`TestA7GateVerdictAndDigestInvariantUnderT7Proposal` 证明草稿在场时 verdict 与 digest 逐字节不变，`TestA7HITLNotSuppressedByT7Proposal` 证明单条 HITL 仍以原 severity 发出、未产生额外 Interrupt/outbox）
- [ ] 三触点在生产路径复用统一调用壳并写 trace（T4/T6 生产接线已由 #706/#721 核销；T7 生产调用器「周期聚合 → T7 → 持久化草稿」未接线，超出 I5 范围，留后续波次——见 [#733 备考](reviews/2026-07-30-m5-t7-a7-firewall-rereview-733-pi-deepseek-v4-pro.md)）

#### 5.2 Interrupt 全功能与 Channel

- [x] 先写 [`specs/channel.md`](specs/channel.md)：冻结首个 webhook Channel、delivery/operation key、Attention sealed payload 接缝与 Forge 失败兜底（[字段评审 PASS WITH NOTES](reviews/2026-07-29-m5-channel-field-rereview-3-pi-gpt-5.6-sol.md)；`active` 不表示 Channel 已实现；outbox ASCII 图缺 terminal reclaim 分支属非阻断注记）；实现项保持未勾
- [x] 复用 M3 已支持全部 reason 的唯一发射器，接入 T4/T6、Channel、调度与 critical 熔断；不得新增 reason 专用旁路（T4 经 [#706](reviews/2026-07-30-m5-t4-emit-interrupt-706-rereview-pi-deepseek-v4-pro.md)、T6 经 [#721](reviews/2026-07-30-m5-t6-emit-interrupt-721-rereview-pi-deepseek-v4-pro.md)、Channel 经 [#715](reviews/2026-07-30-m5-channel-webhook-worker-715-rereview-pi-kimi-k3-sol.md)/[#782](reviews/2026-07-30-m5-channel-ops-ps-doctor-endpoint-782-rereview-pi-deepseek-v4-pro.md)、critical 经 [#779 PASS](reviews/2026-07-30-m5-critical-fuse-779-rereview-pi-deepseek-v4-pro.md)/#777/PR #778、调度合取经 [#786 PASS WITH NOTES](reviews/2026-07-30-m5-emitinterrupt-scheduling-786-rereview-pi-deepseek-v4-pro.md)/PR #787：`scheduling_conjunct_test.go` 证 EmitInterrupt→`SupervisorInterruptTick`→`AdvanceInterrupt`→`PrepareDueAttentionBatches` 唯一生产路径，batch/immediate/next_window/跨重启向量 `-race`/`count≥3` PASS；P2：`CallT6` Availability 仍硬编码 `unknown` 不阻断合取。**不读作** M5 已实现；§5.2 收费项/Command 残留仍开）
- [x] LLM 只能建议 severity 降级；`min_modality: visual` renderer 拒绝语音路径（[#798 Sol PASS](reviews/2026-07-30-m5-llm-severity-visual-reject-798-rereview-pi-deepseek-v4-pro.md) / PR #799 merge `926e5b5`：导出唯一 `Severity(base, suggestedDowngrade)` 入口；T4/T6 不能设定/升级 severity；`min_modality=visual` → held/`no_compatible_channel` 不调 T6、不落 voice；并发/重放单 charge + 一级降级。非阻断 P2 见复审。**不读作** once-charge 全生命周期或 M5 已实现）
- [x] 实现首个 Channel；连续失败 N 次转 forge 告警评论，并在 ps/doctor 显示（[#715 PASS WITH NOTES](reviews/2026-07-30-m5-channel-webhook-worker-715-rereview-pi-kimi-k3-sol.md) 闭合 production sealer exact 向量、阈值 `forge_alert(channel_failure)` 与同 key/bytes response-loss replay；[#782 Sol PASS](reviews/2026-07-30-m5-channel-ops-ps-doctor-endpoint-782-rereview-pi-deepseek-v4-pro.md) / PR #783 闭合 `ops.ps`/`ops.doctor` 端点级跨重启验收。非阻断 P2：`interrupt_deliveries` 单条路径端点未验、seed helper 克隆等。**不读作** §6.6 完整 failure-episode 矩阵或 M5 已实现；EmitInterrupt 接入行已由 #786 Sol PASS WITH NOTES / PR #787 勾选；不读作 M5 已实现）
- [ ] 一次 Interrupt 只收费一次；升级重推不重复收费（[#711 PASS WITH NOTES](reviews/2026-07-30-m5-advance-interrupt-711-rereview-pi-deepseek-v4-pro.md) 在 storage 层证实升级各步保持单笔 admission/charge、不新增 member/authority/channel operation；全生命周期计费纪律仍待 Command/Channel redelivery 证据闭合。command_ack 发布 worker 已由 [#803](https://github.com/miaoxiaoyong/sift/issues/803) / [#804 Sol PASS](reviews/2026-07-30-m5-command-ack-worker-803-rereview-pi-deepseek-v4-pro.md) 接线，但本项不据此勾选：worker 仅扣 Forge API 预算、不涉注意力 once-charge，且 `gate_re_evaluation` Complete worker 已由 [#807](https://github.com/miaoxiaoyong/sift/issues/807) / [#808 Sol PASS](reviews/2026-07-30-m5-gate-re-evaluation-complete-807-rereview-pi-deepseek-v4-pro.md) 接线（§8.1 部分矩阵）；ready/merge→`merge_change` 已由 [#813](https://github.com/miaoxiaoyong/sift/issues/813) 接线；failed 臂 `failure_review` 已由 [#815](https://github.com/miaoxiaoyong/sift/issues/815) / [#816](https://github.com/miaoxiaoyong/sift/pull/816) 接线；七条 HITL verdict→Interrupt 后继已由 [#817](https://github.com/miaoxiaoyong/sift/issues/817) / [#818](https://github.com/miaoxiaoyong/sift/pull/818) / [#819](https://github.com/miaoxiaoyong/sift/pull/819) / [#820 Sol PASS](https://github.com/miaoxiaoyong/sift/pull/820) 接线（`GateReEvaluationInterruptV1` seam + code_review digest + seam replay）；`retry_checks`/flaky_retry→`rerun_checks` 后继已由 [#822](https://github.com/miaoxiaoyong/sift/issues/822) 接线（migration 0057 + `OperationRerunChecks`/`RerunChecksOperationKey` + §8.2 `check_rerun_consumptions`）；**Forge `RerunCheck` worker / §8.5 request-start 执行仍 deferred**；probe 进程检查 worker 已由 [#810](https://github.com/miaoxiaoyong/sift/issues/810) / [#811 Sol PASS](reviews/2026-07-30-m5-probe-process-check-810-rereview-pi-deepseek-v4-pro.md) 接线——`internal/daemon/probe.go` 在 supervisor tick 扫描 pending|running `attempt_probes`、pending→running CAS（崩溃可续跑）、事务外观测 wrapper 进程（复用 `ProcessInspector` + 有界 absence recheck、不发信号）、仅经唯一 `ApplyRetryProbeResult` 提交；但本项仍不据此勾选：worker 不涉注意力 once-charge）

#### 5.3 超时与升级

> [#711 PASS WITH NOTES](reviews/2026-07-30-m5-advance-interrupt-711-rereview-pi-deepseek-v4-pro.md) 在 storage 端口闭合了升级行为矩阵：severity 封顶（升级复用冻结 downgrade、从不达 critical）、reason 确定性映射（`startup_stall` 强制 hold 且不可 `auto_reject`）、两类上限结局状态机测试与每次升级轮换 nonce。[#727 PASS](reviews/2026-07-30-m5-advance-interrupt-727-rereview-pi-deepseek-v4-pro.md) 闭合 I4 `SupervisorInterruptTick` 生产 seam（siftd 唯一接线、expiry/dispatch 双谓词、stale CAS 双层吞错、escalate→redeliver 双 tick 四测试 3/3 无 flake），升级行为经生产 seam 再验证，据此勾选首项。中段各项仍按 §5.2 收费项保守约定保持未勾（待全生命周期 Command/Channel redelivery 证据）。[#779 PASS](reviews/2026-07-30-m5-critical-fuse-779-rereview-pi-deepseek-v4-pro.md) / #777 / PR #778（merge `54f0191`）闭合末项 critical 熔断与非 critical 超额合批/不可借支。

- [x] Supervisor tick 扫描 `expires_at/on_expire`（[#727 PASS](reviews/2026-07-30-m5-advance-interrupt-727-rereview-pi-deepseek-v4-pro.md)：`SupervisorInterruptTick` 生产 seam + expiry/escalate 四测试 3/3 无 flake）
- [x] 达 `max_escalations` 后 severity 封顶，并按 reason 的确定性映射进入 `auto_reject` 或 `hold`（[#711 PASS WITH NOTES](reviews/2026-07-30-m5-advance-interrupt-711-rereview-pi-deepseek-v4-pro.md) / #705 / PR #710 `871d719`/`9095f91`：`advance_interrupt.go` 升级前 `escalation>=max` 短路——`on_max==auto_reject` 且非 startup_stall 走 `closeExpiredInterrupt`，否则 `holdAdvance("max_escalations")`；升级路径复用冻结 downgrade、从不达 critical；`TestAdvanceInterruptExpiryAndMaxOutcomeMatrix`/`TestAdvanceInterruptReasonOutcomeMatrix`/`TestAdvanceInterruptEscalationCountsReuseDowngrade` 逐格 state/authority/accounting + stale CAS 全绿）
- [x] `startup_stall` 禁止配置 `auto_reject`，达上限强制 `hold`，且不写 resolution（[#711 PASS WITH NOTES](reviews/2026-07-30-m5-advance-interrupt-711-rereview-pi-deepseek-v4-pro.md) / #705 / PR #710 `871d719`/`9095f91`：`EmitInterrupt` 校验拒绝 startup_stall + `auto_reject`（on_expire/on_max）；达上限 `reason != startup_stall` 才走 auto_reject，否则强制 `holdAdvance("max_escalations")`；`holdAdvance` 仅置 held/dispatch/version+1、不写 `attempt_resolution`；`TestAdvanceInterruptStartupStallAtLimitHoldsRatherThanAutoRejecting`/`TestAdvanceInterruptReasonOutcomeMatrix` 证 startup_stall 达上限 hold 且 emit 层拒 auto_reject）
- [x] 两类上限结局分别做状态机测试；每次升级轮换 nonce（[#711 PASS WITH NOTES](reviews/2026-07-30-m5-advance-interrupt-711-rereview-pi-deepseek-v4-pro.md) / #705 / PR #710 `871d719`/`9095f91`：`TestAdvanceInterruptExpiryAndMaxOutcomeMatrix`（max hold/max auto_reject 逐格）/`TestAdvanceInterruptReasonOutcomeMatrix`（reason×on-max 表）分别闭合两类上限结局；每次升级 `newNonce:=newID()` 轮换 `nonce`/`nonce_issued_at_ms`，`TestAdvanceInterruptEscalationCountsReuseDowngrade` 逐级断言 nonce 轮换、封顶 hold 不轮换）
- [x] critical 熔断在发射器内生效，非 critical 超额合批且不可借支（[#779 PASS](reviews/2026-07-30-m5-critical-fuse-779-rereview-pi-deepseek-v4-pro.md) / #777 / PR #778：`admitCriticalTx` 仅经 `EmitInterrupt`/`AdvanceInterrupt`；`chargeAttentionTx` CAS 零行重读证明超额才 `quota_batched`；`critical_fuse_test.go` 窗口/并发/global>per-Run/`quota_batched→critical`/不借支向量 `-race` 三连 PASS）

#### 5.4 Command 与 startup_stall 两段式

> [Command bootstrap #739 PASS WITH NOTES](reviews/2026-07-30-m5-command-bootstrap-739-rereview-pi-deepseek-v4-pro.md) / #738（commit `4c2a12e`）落地单一写口 `ApplyCommandEvent`。[#789 Sol PASS](reviews/2026-07-30-m5-command-reason-specific-effects-789-rereview-pi-deepseek-v4-pro.md) / PR #790（merge `d8873c3`）闭合非 startup retry + guardrail/code_review approve。[#794 Sol PASS](reviews/2026-07-30-m5-command-agent-blocked-ask-794-rereview-pi-deepseek-v4-pro.md) / PR #795 闭合 `agent_blocked` `/sift ask` 全契约。[#796 Sol PASS](reviews/2026-07-30-m5-probe-failure-escalate-hold-796-rereview-pi-deepseek-v4-pro.md) / PR #797 闭合 probe 失败 → AdvanceInterrupt 升级计数 → 达上限 hold（below-cap 回 `batched` 可升级；at-cap 冻结 capped hold；startup_stall 永不 auto_reject）。ack 发布 worker 已由 [#803](https://github.com/miaoxiaoyong/sift/issues/803) / [#804 Sol PASS](reviews/2026-07-30-m5-command-ack-worker-803-rereview-pi-deepseek-v4-pro.md)（merge `0657f29`）接线：`forgeworker.CommandAckWorker` 镜像 CommentWorker 的 marker-then-send（outbox.md §5），从 append-only `command_receipts`+`projects` 解析不可变 target/forge 路由（command.md §6.1），崩溃/响应丢失经 marker 收敛不双发；`gate_re_evaluation` Complete worker 已由 [#807](https://github.com/miaoxiaoyong/sift/issues/807) / [#808 Sol PASS](reviews/2026-07-30-m5-gate-re-evaluation-complete-807-rereview-pi-deepseek-v4-pro.md)（merge `ab76e57`）接线：storage §8.1 终结协议 succeeded/failed/conflict 闭合矩阵 + `forgeworker.GateReEvaluationWorker` claim→assemble→Evaluate→Complete + siftd `OutboxTick`；ready/merge→`merge_change` 已由 [#813](https://github.com/miaoxiaoyong/sift/issues/813) 接线；failed 臂 `failure_review` 已由 [#815](https://github.com/miaoxiaoyong/sift/issues/815) / [#816](https://github.com/miaoxiaoyong/sift/pull/816) 接线；七条 HITL verdict→Interrupt 后继已由 [#817](https://github.com/miaoxiaoyong/sift/issues/817) / [#818](https://github.com/miaoxiaoyong/sift/pull/818) / [#819](https://github.com/miaoxiaoyong/sift/pull/819) / [#820 Sol PASS](https://github.com/miaoxiaoyong/sift/pull/820) 接线；`retry_checks`/flaky_retry→`rerun_checks` 后继已由 [#822](https://github.com/miaoxiaoyong/sift/issues/822) 接线（migration 0057 + `OperationRerunChecks`/`RerunChecksOperationKey` + §8.2 `check_rerun_consumptions`）；**Forge `RerunCheck` worker / §8.5 request-start 执行仍 deferred**；probe 进程检查 worker 已由 [#810](https://github.com/miaoxiaoyong/sift/issues/810) / [#811 Sol PASS](reviews/2026-07-30-m5-probe-process-check-810-rereview-pi-deepseek-v4-pro.md)（merge `fa70334`）接线（`ProbeProcessCheckCoordinator` 走 supervisor tick、仅观测不信号、不经 outbox、经唯一 finalizer）；这不读作 once-charge 全生命周期或 M5 已实现。

- [x] actor 鉴权 → 严格语法 → nonce/current Interrupt/options 校验 → DomainCommand + 回执 outbox（[#739](reviews/2026-07-30-m5-command-bootstrap-739-rereview-pi-deepseek-v4-pro.md) / #738：`ApplyCommandEvent` 单一写口串联 `lookupCommandReceiptTx` 去重 → `NewAuthorizer` 鉴权 → `ParseCommand` 严格语法 → 不可变 target/current Interrupt/nonce/options 校验 → 效果 + 事件 + receipt + ack outbox。回执 outbox op 已在事务内创建并持久化；ack 发布回 Forge 的 worker 已由 [#803](https://github.com/miaoxiaoyong/sift/issues/803) / [#804 Sol PASS](reviews/2026-07-30-m5-command-ack-worker-803-rereview-pi-deepseek-v4-pro.md) 接线（`CommandAckWorker` claim→解析路由→marker 收敛→`CommentTarget`→`CompleteOutboxAttempt`，崩溃不双发）——不读作 once-charge 或 M5 已实现）
- [x] 逐项实现 PRD §7.1 `/sift approve | reject | retry | hold | ask` 与审批标签的 reason-specific 确定性效果；不在当前 options 内一律拒绝（[#789 Sol PASS](reviews/2026-07-30-m5-command-reason-specific-effects-789-rereview-pi-deepseek-v4-pro.md) / #790：`guardrail_violation`/`code_review` approve 写 `command_effects` 并入队 `gate_re_evaluation`；`merge_conflict`/`failure_review(gate_recheck)` retry 同路径；`failure_review(new_attempt)`/`agent_blocked` retry 经 terminalize+spawn；`design_approval` approve 与通用 reject/hold/ask 保持 #739；optionAllowed 拒绝非 options；`gate_re_evaluation` Complete worker 已由 [#807](https://github.com/miaoxiaoyong/sift/issues/807) / [#808 Sol PASS](reviews/2026-07-30-m5-gate-re-evaluation-complete-807-rereview-pi-deepseek-v4-pro.md) 接线——ops 可 claim→Complete→terminal（succeeded 无后继 verdict、failed 结果 union terminal event + Run bump、conflict 替换头均已闭合；ready/merge→`merge_change` 后继已由 [#813](https://github.com/miaoxiaoyong/sift/issues/813) 接线；**failed 臂 `failure_review` Interrupt 后继（thin）** 已接线：`completeGateReEvalFailedTx` 同事务 `failure_review` + frozen `failure_review_attempt(gate_recheck)`（`emitInterruptInExistingTx`）；**七条 HITL verdict→Interrupt 后继** 已由 [#817](https://github.com/miaoxiaoyong/sift/issues/817) / [#818](https://github.com/miaoxiaoyong/sift/pull/818) / [#819](https://github.com/miaoxiaoyong/sift/pull/819) / [#820 Sol PASS](https://github.com/miaoxiaoyong/sift/pull/820) 接线（closed `GateReEvaluationInterruptV1` + migration 0055/0056 + code_review digest）；`retry_checks`/flaky_retry→`rerun_checks` 后继已由 [#822](https://github.com/miaoxiaoyong/sift/issues/822) 接线（migration 0057 + `OperationRerunChecks`/`RerunChecksOperationKey` + §8.2 `check_rerun_consumptions`；后继可 claim、同事务消费额、replay 不双发）；**Forge `RerunCheck` worker / §8.5 request-start 执行仍 deferred**；不涉 once-charge、不读作 §8.1 全矩阵或 M5 完成））
- [x] `/sift ask <文本>` 同事务写当前 Run 的任务层澄清与 Ledger 语义原料，并按当前 Interrupt 契约继续；不得自动升格项目/全局 Context（[#794 Sol PASS](reviews/2026-07-30-m5-command-agent-blocked-ask-794-rereview-pi-deepseek-v4-pro.md) / PR #795：`commandAskTx`→`commandAgentBlockedAskTx` 同事务 `recordHumanDecisionTx(DecisionAsk+SemanticMaterial)` + `insertClarificationTaskSpecTx`（append-only、`source_event_id`=command event）+ `closeInterruptTx(responded)` + `spawnNextAttemptTx` terminalize/next claim/launch + `waiting_human→queued`；`TestApplyCommandAgentBlockedAskFullContract` 证无 Context/`proposal_drafts` 升格、crash/replay 幂等；`-race` PASS）
- [x] 通用 reject/retry/hold/approve 均经唯一 transition 与 outbox，不直接写状态；标签和评论路径共用鉴权/幂等实现（#739/#738：所有效果经 `d.transition()` + `recordHumanDecisionTx` + `writeCommandAckOpTx`/`writeProbeAckOpTx`，无第二条状态/Ledger/outbox 路径；标签 approve 与评论 approve 同走 `commandApproveTx`；去重口诀 `lookupCommandReceiptTx` 检查 `(project_id,event_kind,remote_event_id)`）
- [x] `startup_stall` 只提供 retry/reject/hold，approve 必须拒绝（#739/#738：`EmitInterrupt` 为 startup_stall 设 `options=["retry","reject","hold"]`；`Compile` optionAllowed 拒绝 approve → `rejected_option`，`TestCompileStartupStallApproveRejected` 验证）
- [x] retry 请求不关闭 Interrupt、不写 resolution；探测在途拒绝新指令并回复“已在探测中”（#739/#738：`startupStallRetryRequestTx` CAS 旋转 nonce + `dispatch_state=probe_in_progress`、不关 Interrupt/不写 resolution/不发 ack；`Compile` 检 `DispatchProbeInProgress` → `probe_in_progress`；`TestApplyCommandStartupStallRetryRequestPendingNoAck` + `...ProbeInProgressRejects` 验证）
- [x] 探测失败复用同一 Interrupt、升级计数、轮换 nonce；达到上限 hold（[#796 Sol PASS](reviews/2026-07-30-m5-probe-failure-escalate-hold-796-rereview-pi-deepseek-v4-pro.md) / PR #797：`probeFailedTx` below-cap 回 `batched`（可经唯一 `AdvanceInterrupt`/expiry 升级）+ at-cap 冻结 capped hold（`held`/`max_escalations`，无 auto_reject/resolution）；`TestStartupStallProbeFailureEscalatesToCapHold` / `...AtCapAppliesFrozenCappedHold` / `...StateIsEscalateableByDirectAdvance` `-race`/`count=3` PASS；非阻断 P2：未对称调用 `excludeStaleBatchMembersTx`。probe 进程检查 worker 已由 [#810](https://github.com/miaoxiaoyong/sift/issues/810) / [#811 Sol PASS](reviews/2026-07-30-m5-probe-process-check-810-rereview-pi-deepseek-v4-pro.md) 接线（supervisor tick、仅观测不信号、唯一 finalizer、`-race` 关键包绿）——不读作 M5 已实现）
- [x] 探测成功以 ADR-013 单一 CAS 事务提交：消失证据、旧 attempt 终结、`retry_after_absence`、解除隔离、关闭 Interrupt、Run → queued、新 attempt/claim、启动与回执 operation、事件（#739/#738：`probeSucceededTx` 单事务 7 步全或全无，任一 CAS 失败回滚；`TestApplyRetryProbeResultSuccessClosesAndQueues` 验证）
- [x] reject 写 `attempt_resolution=reject`、Run failed，但隔离保持到执行体被证明消失（#739/#738：`commandRejectTx` 经 `transition` → `RunFailed(human_reject)` + `attempt_resolution=reject`（write-once NULL→reject）+ close/responded，isolation 保持 frozen；`TestApplyCommandStartupStallRejectFailsRunHoldsIsolation` 验证）
- [x] 所有动作复用 M3 唯一仲裁函数；覆盖事实先到/决定先到/探测在途事实到达（#739/#738：Command 不直接调 `ResolveAttemptRace`，但写 `attempt_resolution` marker——reject→`reject`、probe 成功→`retry_after_absence`、retry 请求不写（ADR-013 留事实窗口）；M3 race 据此返回 `superseded_by_decision`/`superseded_by_fact`/`running`，三场景覆盖）
- [x] `/sift reject` 原因与 `/sift ask` 文本随人类决定写入 Ledger，不丢语义原料（#739/#738：`commandRejectTx`/`commandAskTx` → `recordHumanDecisionTx` 携 `SemanticMaterial`（reject reason / ask text）；`TestApplyCommandReject` 验证 semantic_material=1，`TestApplyCommandAskWritesSemanticMaterial` 验证 “what next?” 持久化）

#### 5.5 Report

> [Report bootstrap #755 PASS](reviews/2026-07-30-m5-report-runsock-rereview-755-pi-deepseek-v4-pro.md) / #745 / PR #754（merge `9da6499`）闭合 wave-2 Report 写入端口：`sift report` 只 dial `run.sock`、只读 `SIFT_RUN_DIR/control.json`（owner/mode/symlink 防护）；running 接受；spawning 返回携带 closed `RetryPolicy` 的有界 `not_ready`（不消费 token）；跨 Run token→`unauthorized`、错 generation→`stale`、finished/其余非 running→永久 `conflict`；`progress`/`goal`/`blocker`/`completed` 只写事件（+ receipts；blocker 复用 `EmitInterrupt`），不直接改 `runs.status`；两层去重 + 令牌桶 + 每 Run Interrupt 子配额，触顶 at-most-once。四项关键验收据此勾选。非阻断 [P2]：`agent_log_ref` 链接合法性诊断未接线（系统生成 worktree 路径常规不可触发）。本片不闭合预算 §5.6、指标 §5.7；critical 熔断与 Channel `ops.ps`/`ops.doctor` 端点级验收已另片闭合；不读作 M5 已实现。

- [x] `sift report` 只连 `run.sock`，从 `SIFT_RUN_DIR/control.json` 读 run token（[#755](reviews/2026-07-30-m5-report-runsock-rereview-755-pi-deepseek-v4-pro.md) / #745：`runReport`→`ReadControlFile`；`RunReportRequest` 只 dial `run.sock`；operator token 在 run.sock 被拒）
- [x] running 接受；spawning 返回有界可重试 `not_ready`；跨 Run/过期 attempt 永久拒绝（#755/#745：`checkReportBinding`/`assertReportBindingTx`；CLI `BackoffDelays` + policy drift fail-closed；unauthorized/stale/conflict 映射）
- [x] 进度/goal/blocker/完成声明只写事件，不直接转状态（#755/#745：`recordSimpleReport`/`recordBlockerReport` 无 `runs.status` 写入；`completed` 后 status 仍为 running）
- [x] 确定性去重、令牌桶与每 Run Interrupt 子配额；触顶只生成一次异常处置（#755/#745：key/digest + `dedupe_window`；CAS 令牌桶；`report_quota_exhaustions` UNIQUE 日桶；crash-replay/concurrency 测试）

#### 5.6 三类预算集成

> [三类预算共存 #763 PASS](reviews/2026-07-30-m5-three-budget-coexistence-763-rereview-pi-deepseek-v4-pro.md) / #760 / PR #762（merge `9e26f6f`）闭合 wave-2 三预算集成片：token 经 Brain `RecordBrainAttempt`（`brain.go:198`、brain.md §6）、API 经 Forge `ChargeForgeAPICall`（`forgebudget.go:95`、forge.md §9）、注意力经 `EmitInterrupt`（interrupt.md §1）三个既有唯一收费口并存；`three_budget_coexistence_test.go` 仅新增测试、零生产代码改动，无第二收费口；`TestTokenDegradeDoesNotBreakAttentionQuota` 与 `TestForgeAPIDegradeDoesNotBreakAttentionQuota` 验证 token/API 各自降级仍只经 `EmitInterrupt` 计注意力、不突破配额。三项关键验收据此勾选。本片不闭合指标 §5.7；critical 熔断与 Channel `ops.ps`/`ops.doctor` 端点级验收已另片闭合；不读作 M5 已实现。

- [x] token 预算沿用 M1 Brain 收费口；API 预算沿用 M2 Forge 收费口；注意力沿用 M3 发射器收费口（[#763](reviews/2026-07-30-m5-three-budget-coexistence-763-rereview-pi-deepseek-v4-pro.md) / #760：共存用例经 `db.RecordBrainAttempt`/`db.ChargeForgeAPICall`/`db.EmitInterrupt` 三收费口；唯一收费口 `brain.go:198`/`forgebudget.go:95`/`interrupt.go:437`；brain.md §6、forge.md §9、interrupt.md §1）
- [x] 本片只做三者并存与降级集成，不复制收费实现（#763/#760：PR #762 仅新增 `three_budget_coexistence_test.go`，零生产代码改动，无重复收费实现）
- [x] token/API 降级不得突破注意力配额（#763/#760：`TestTokenDegradeDoesNotBreakAttentionQuota` + `TestForgeAPIDegradeDoesNotBreakAttentionQuota` 各 3/3 PASS 含 `-race`；注意力计数只写于 `EmitInterrupt` 内部；`internal/storage/` + `internal/brain/` 包全绿）

#### 5.7 指标、CLI 与时间线

> [指标/CLI/时间线 bootstrap #767 round-2 PASS](reviews/2026-07-30-m5-metrics-cli-timeline-767-rereview-2-pi-deepseek-v4-pro.md) / 并行 [#770 PASS WITH NOTES](reviews/2026-07-30-m5-rereview-metrics-cli-timeline-bootstrap-770-pi-deepseek-v4-pro.md) / #767 / PR #769（merge `0b63a6c`）+ fix PR #774（merge `adbf11f`）闭合 wave-2 §5.7 只读派生面：`ops.metrics`/`ops.timeline` + `sift metrics`/`timeline`/`ps`/`logs`；九项 PRD §10.2 指标经 `internal/storage/metrics.go` 从事件流/Ledger/预算表确定性派生，缺数据处 coverage 失败闭合（绝不发明数字）；北极星权重取 Run 冻结 `config_snapshot`，响应间隔不作人类分钟数；`TestV11GateBypassExcludedFromFalseRelease` 证实 `gate_bypassed` 排除误放行率分母并进入门禁绕过率；触发→started 延迟分布可查（真实 P50 < 60s 留 M7）。#774 关闭 Sol round-1 P0/P1（项目作用域 `weightedAttention` 双 WHERE、零延迟样本、`TestMetricsProjectScoped`）。五项关键验收据此勾选。诚实缺口（非阻断）：误放行率分子在 revert/fix 事件写入前恒为 0；分派准确率为结构性 100%；P2=`RunTimeline.HasMore` 无作用域 COUNT、`llmCost` 忽略 `ProjectID`。本片不闭合 M5 门禁；Channel `ops.ps`/`ops.doctor` 端点级验收已由 [#782 Sol PASS](reviews/2026-07-30-m5-channel-ops-ps-doctor-endpoint-782-rereview-pi-deepseek-v4-pro.md) / #783 闭合；不读作 M5 已实现。

- [x] 从事件流/Ledger 确定性派生 PRD §10.2 全部指标（当前九项）：加权打扰/已合并 Change、误放行率、门禁绕过率、Gate 漏放/误拦、HITL 率、配额消耗、分派准确率、LLM 成本（[#767 r2](reviews/2026-07-30-m5-metrics-cli-timeline-767-rereview-2-pi-deepseek-v4-pro.md) / [#770](reviews/2026-07-30-m5-rereview-metrics-cli-timeline-bootstrap-770-pi-deepseek-v4-pro.md)：`Metrics()` 九项 + `TestOpsMetricsCoversNineSeries` / `TestMetricsEmptyIsHonest` / `TestMetricsProjectScoped`；误放行分子失败闭合为 0）
- [x] reason 耗时权重为配置项；响应间隔只作调度特征，不作人类分钟数（#767/#770：`reasonWeight()`→`config_snapshots`；`TestMetricsWeightedAttentionUsesFrozenWeights`；coverage 声明响应间隔不计入）
- [x] `gate_bypassed` 排除误放行率分母的查询测试（#767/#770：`TestV11GateBypassExcludedFromFalseRelease`；分母仅 `merge_change` succeeded outbox）
- [x] `sift ps` 显示 Run/attempt、今日注意力余量、隔离与推送故障；`logs` 提供 Run 原始日志；事件时间线可查（#767/#770：`RunPS`/`handleOpsLogs`/`RunTimeline` + online CLI 测试；Channel 故障端点级跨重启验收经 [#782 Sol PASS](reviews/2026-07-30-m5-channel-ops-ps-doctor-endpoint-782-rereview-pi-deepseek-v4-pro.md) / PR #783 闭合）
- [x] 输出「触发标签 → started」延迟分布；真实 P50 < 60s 在 M7 验收（#767/#770：`TriggerStartedLatency` + `TestTriggerStartedLatencyDistribution` / `TestTriggerStartedLatencyZeroAllowed`；coverage 显式留 M7）

### 先写/增补 spec

- [x] `specs/interrupt.md`（[字段评审 PASS](reviews/2026-07-29-m5-interrupt-field-rereview-6-pi-gpt-5.6-sol.md)）
- [x] `specs/command.md`（[字段评审 PASS](reviews/2026-07-29-m5-command-field-rereview-9-pi-gpt-5.6-sol.md)；覆盖鉴权、语法、nonce/options、回执、Ledger/transition 与 startup_stall 两段式）
- [x] `specs/report.md`（[字段评审 PASS](reviews/2026-07-29-m5-report-field-rereview-6-pi-gpt-5.6-sol.md)）
- [x] 增补 `specs/brain.md`（T4/T6/T7；[字段级评审 PASS WITH NOTES](reviews/2026-07-29-m5-brain-t4-t6-t7-field-review-pi-gpt-5.6-sol.md)）
- [x] 增补 `specs/config.md`（配额、熔断、reason 上限去向、指标权重）
- [x] [`specs/channel.md`](specs/channel.md)（[字段评审 PASS WITH NOTES](reviews/2026-07-29-m5-channel-field-rereview-3-pi-gpt-5.6-sol.md)；`active` 不表示 Channel 已实现）

### M5 门禁

- [ ] V2 retry 成功原子事务段、V4 人工态交错段通过
- [ ] T4/T6 各自兜底、T7 两类只读提案及 A7 防火墙测试通过（T7 两类只读提案与 A7 防火墙的测试证据已由 [#733 PASS](reviews/2026-07-30-m5-t7-a7-firewall-rereview-733-pi-deepseek-v4-pro.md) 闭合；本项仍随 M5 整体门禁未过保持未勾）
- [ ] V8、V10a Command/Report 段、V13 通过
- [ ] `startup_stall` approve/auto_reject 均被拒；retry 两段式与迟到事实仲裁通过
- [ ] 九项指标可查询，V11 指标段闭合：`gate_bypassed` 不进入误放行率分母且进入门禁绕过率
- [ ] 全 fake 端到端链（含 Gate、Interrupt、Command、merge）成为 V9 的自动化完整段

---

## M6：tmux 与完整故障矩阵

### 前置

- [ ] M5 门禁通过

### 任务与门禁

- [ ] tmux 只承载 wrapper；Agent 仍是 wrapper 直接子进程并留在其进程组
- [ ] PTY 由 wrapper 分配并中继到 pane/agent.log；tmux 会话不是事实源
- [ ] process/tmux 跑同一 V2/V4 套件
- [ ] **逐项执行 DESIGN §12 V4 全矩阵**，含同代双 wrapper、permit 重放/暂停、证据缺失、PID/PGID 复用、人工态四组交错、四发现者并发
- [ ] 构造脱组后代：标 `process-group-unverified`，禁止含糊状态自动 retry
- [ ] 两后端均证明：旧 wrapper/组未确认消失前无新 owner；kill 不生新 attempt，retry 只生一个
- [ ] `sift attach <run>` 只提供观察，不参与裁定

---

## M7：真实 Agent 与 PoC 取证

### 前置

- [ ] M6 门禁通过

### 任务

#### 7.1 Agent 与资格

- [ ] 至少两个 Agent 定义通过配置校验，其中一个真实跑通
- [ ] 真实 Agent 可使用 Task Spec 与 `sift report`
- [ ] 按 Agent CLI + 版本执行进程拓扑资格测试；结果进入 doctor

#### 7.2 双平台与人工验收

- [ ] GitHub、GitLab 各一个真实项目完成摄入 → Brain → Runtime → Gate → HITL/merge
- [ ] 正式留存一次手机端审批证据、一次真实 HITL 往返、一次负样本
- [ ] 真实运行 ≥3 个并行 Run，触及注意力上限并证明合批，不把 V8 模拟结果冒充此证据
- [ ] 测量真实「可信打标 → Agent started」，验证 P50 < 60s

#### 7.3 凭证形态 spike

- [ ] 对首批 Agent 分别实证 macOS/Linux 凭证存储与可挂载性
- [ ] 结论按 OS 分行；macOS 不成立不自动否定 Linux 方向
- [ ] 若触发 ADR-007 证伪条件，先追加 ADR，不在 plan 中静默换方向

### M7 人工门禁

- [ ] A1–A4、A7–A9 的对应证据齐全
- [ ] A8：两个定义通过、一个真实闭环
- [ ] 双平台正式证据与凭证/资格矩阵归档

---

## M8：发布

### 前置

- [ ] M7 门禁通过

### 任务

#### 8.1 发布归档与升级

- [ ] GoReleaser 产出同版本三二进制单归档、manifest、校验和
- [ ] 四组合运行安装、版本握手、SQLite、双 socket、wrapper handoff 冒烟
- [ ] 安装到版本目录，校验后原子切换 `current`；禁止逐文件覆盖
- [ ] CLI/daemon/wrapper 主版本不一致拒绝并由 doctor 报错

#### 8.2 托管与安装渠道

- [ ] launchd user agent、systemd user unit、无 systemd foreground fallback
- [ ] 崩溃自启与原子升级后重启
- [ ] Homebrew tap 与 Release 归档两条安装路径

#### 8.3 干净机验收与文档

- [ ] 干净 macOS 与 systemd Linux 各从发布归档安装并跑通完整闭环/恢复
- [ ] 四种 OS/架构组合均保留安装与二进制冒烟证据
- [ ] `dev/release.md`：manifest、构建矩阵、CGO 门禁、升级与干净机流程
- [ ] `guides/installation.md` 与 `runbooks/troubleshooting.md`

#### 8.4 doctor 最终姿态

- [ ] 报 `unsafe-local` 与 TM6 每条未闭合暴露面，不把资格写成沙箱闭合
- [ ] 按 DESIGN §8.10 全量检查 runtime、SQLite、Agent CLI/资格、相关 forge CLI、可选 tmux、policy schema/漂移、hooks、目录/socket 权限、版本、隔离、outbox 与推送故障
- [ ] macOS/Linux 安全姿态分别呈现

### M8 门禁

- [ ] V15 全部通过
- [ ] A10 两台干净机证据齐全
- [ ] 原子升级不丢数据库状态，较新 schema 拒绝旧 daemon

---

## 自动化门禁权威归属

本表是 V1–V15 的唯一归属定义；里程碑内只引用阶段，不另立冲突版本。

| 门禁 | 首次可运行 | 最终闭合 | 持续回归重点 |
|------|------------|----------|--------------|
| V1 状态机 | M1 | M1 | 全转移图、外部事实、非法转移 |
| V2 事务/崩溃 | M1 核心事务 | M6 | M3 handoff、M4 Gate/Interrupt、M5 retry 原子事务 |
| V3 Forge 契约 | M2 | M2 | 双平台、actor、marker、merge CAS |
| V4 Runtime 故障 | M3 process 段 | M6 双后端；M7 真实资格 | 完整 DESIGN §12 矩阵 |
| V5 Gate 安全 | M3：V5a 读取源 | M4：V5b 硬护栏 | base/context、`.sift/**`、CI、head |
| V6 Gate 回放 | M4 | M4 | 每次 Gate 必记录、cache miss、导出重放 |
| V7 幂等 | M2 Forge 段 | M4 | 各副作用逐类语义 |
| V8 预算/调度 | M5 | M5 | 配额、合批、公平、outbox 延迟 |
| V9 端到端 | M1 骨架 fake 段 | M5 完整 fake；M7 真实低频 | 双平台真实链为人工证据 |
| V10a 授权 | M1 端点段 | M5 | M3 wrapper 凭据、M5 Command/Report |
| V10b 未闭合暴露面 | M1 | M8 doctor 最终呈现 | V0 攻击复现预期成功 |
| V11 手工合并冲突 | M2 事实收敛段 | M5 | M4 闭合 Gate/审计/Ledger 分类；M5 闭合指标分母 |
| V12 零配置启动 | M1 | M1 | 所有新增默认值持续纳入 |
| V13 critical 熔断 | M5 | M5 | 洪水合批、不借支 |
| V14 边界解码 | M1 | M1 | closed/open-envelope/schema 漂移 |
| V15 跨平台发布 | M1 构建段 | M8 | 四组合运行、两 OS 完整恢复、托管/升级 |

---

## 人工验收权威归属

| # | PRD §10.1 成功标准 | 自动化前置 | 正式证据 |
|---|-------------------|------------|----------|
| A1 | 真实 Issue → 合并 Change | V9 | M7 |
| A2 | GitHub/GitLab 各摄入到 Gate | V3/V9 | M7 |
| A3 | ≥3 Run 并发且配额合批 | V8（M5） | M7 真实并发 |
| A4 | 至少一个负样本被拦 | V5/V6 | M7 |
| A5 | `.sift/**`/CI 硬护栏不可豁免 | V5b（M4） | M4 自动化即正式证据 |
| A6 | Sift 不合并硬护栏违规 | V5b/V6/V7 | M4 自动化即正式证据 |
| A7 | 推送 + 一条 forge 指令审批，含手机 | V10a | M7 |
| A8 | ≥2 Agent 定义通过、其中 1 个真实跑通 | M1 配置测试 | M7 |
| A9 | kill siftd 恢复无幽灵、游标不丢 | V4 | M7 可追加真实记录，自动门禁不得缺 |
| A10 | 干净 macOS/systemd Linux 安装跑通 | V15 | M8 |

---

## 派生文档归属

| 文档 | 首次完成里程碑 | 后续同步 |
|------|----------------|----------|
| `specs/storage.md` | M1 | M2 Intake 投影/写端口；M3 resolution/隔离；M4 Ledger |
| `specs/control-plane.md` | M1 | M3 wrapper；M5 Report |
| `specs/config.md` | M1 | M3 时限；M5 预算/权重；M8 托管模板 |
| `specs/outbox.md` | M1 | M2 Forge worker、Intake 评论 marker/CAS；M3 启动；M5 发布/回执 |
| `specs/brain.md` | M1（壳/T1/T2、Brain replay） | M2 T1 Intake 接线/回复仲裁；M4 T3/T5；M5 T4/T6/T7 |
| `specs/forge.md` | M2 | 随适配器契约同步 |
| `specs/interrupt.md` | M3（全部 reason 最小契约） | M5 T4/T6、Channel、调度/renderer |
| `specs/channel.md` | M5（active；首个 webhook Channel 与失败兜底；[字段评审 PASS WITH NOTES](reviews/2026-07-29-m5-channel-field-rereview-3-pi-gpt-5.6-sol.md)） | 随 Channel/Attention/outbox 实现同步 |
| `specs/policy.md` | M4 | 随 Gate 策略同步 |
| `specs/gate.md` | M4 | 随 Gate/回放同步 |
| `specs/ledger.md` | M4 | M5 Command/指标同步 |
| `specs/command.md` | M5 | 随指令语法同步 |
| `specs/report.md` | M5 | 随上报契约同步 |
| `dev/release.md` | M8 | 随发布链同步 |

---

## 自查结果

- [x] 六份 WBS 评审/复核的发现均在「评审处置对账」中有结论与落点
- [x] PRD 十个模块均有明确工作包
- [x] DESIGN §15 所列待写 spec/dev 文档均有里程碑归属
- [x] M1–M8 的门禁不依赖未完成的后续片；跨片测试均标首跑/最终闭合
- [x] V1–V15、A1–A10 均能追溯到实现任务和证据
- [x] Brain T1–T7、调用壳、trace、兜底、token 收费完整分片
- [x] ADR-013 的 resolution、隔离、事实仲裁、retry 两段式和原子结果事务均有任务与测试
- [x] `max_escalations` 按 reason 收敛，`startup_stall` 强制 hold
- [x] 配置不热加载、两级探测、有效策略、Agent 校验与 V12 已落地
- [x] Ledger 人类结果、语义原料、认证投影、九项指标与 P50 测量已落地
- [x] launcher、版本握手、`SIFT_RUN_DIR`、单实例、零网络监听与 V15 有被测实现
- [x] 已删除无来源的“三天稳定运行”发布门槛

- [x] D0.2 复评 F1/F2 后向依赖已消除，F3–F6 已进入任务、spec 与验收

**结论：D0.3 已关闭 D0.2 独立复评全部发现，并通过定向复核；WBS 进入 `active`。**

---

_D0.3 | 2026-07-28 | active / 定向复核通过 | 对应 DESIGN D0.10 / PRD V0.8_
