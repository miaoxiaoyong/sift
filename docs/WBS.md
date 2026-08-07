---
created: 2026-07-28
summary: Sift PoC 的里程碑、工作分解与验收标准
---

# Sift — 工作分解与里程碑

> 本文只做任务分解（里程碑/任务/验收/范围）。执行进展见 [STATUS.md](STATUS.md)。

> **D0.3 · 对应 DESIGN D0.10 / PRD V0.8**

本文把 [DESIGN §13](DESIGN.md) 的八个纵向切片展开为任务与验收。需求语义以 [PRD](PRD.md) 为准，结构与理由以 [DESIGN](DESIGN.md) / ADR 为准，字段级契约下沉 `specs/`，执行步骤下沉 `plans/`；本文不复制完整协议表。

执行纪律：

1. 一个里程碑的完成条件只能依赖本里程碑及已完成前置里程碑。
2. 跨片验证标明「首次可运行 / 最终闭合」，不得把后续能力写成前置片门禁。
3. DESIGN 已有完整表格时，WBS 只写「逐行实现 + 不变量 + 验收」，避免部分复制被误读为全集。
4. 自动化门禁与人工验收都是 PoC 发布条件，但证据形式不同。

---

> D0.3 规划评审处置(R1–R16 发现→里程碑落点)属规划历史,见 [STATUS.md](STATUS.md) 与各 [`reviews/`](reviews/);本文只登记工作包分解


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

- ADR-010 决策 6 前已增加 ADR-013 名称修订指针
- 新规范名称确定为 `attempt_resolution`，V0 枚举为 `reject | retry_after_absence`
- 创建单 Go module 与三个命令：`siftd`、`sift`、`sift-agent-wrapper`

### 任务

#### 1.1 Decode gateway、schema 与 CI

- 单一 decode gateway，调用方显式选 `closed` 或 `open-envelope`
- 配置、LLM 输出、socket 请求用 `closed`；Forge envelope 用 `open-envelope`，必需语义仍 fail closed
- 结构体生成 JSON Schema 并入 git；schema 漂移使 CI 失败（`tools/schema/cmd/contracts` + CI `schema-drift` job）
- V14 golden tests 覆盖缺失字段、额外字段、类型/枚举变型
- 从本片起在 CI 构建 darwin/linux × arm64/amd64，保持 `CGO_ENABLED=0`（`build-matrix` job）

#### 1.2 SQLite、事件与迁移

- `modernc.org/sqlite`，WAL、foreign keys、busy timeout，写连接池上限 1
- 前向迁移与 `schema_version`；数据库版本较新时拒启
- 按 DESIGN §7 数据组落当前投影、attempt、只追加事件、Intake、outbox、校准/Brain trace、认证、预算、配置快照（`migrations/0001_initial_schema.sql`）
- attempt 数据包含 `attempt_resolution` 与独立隔离标记；隔离期 worktree 不清理、不复用（attempts 表 `attempt_resolution`/`isolation_state`）
- Interrupt 数据包含生成键、nonce、投递状态、升级计数及 `superseded_by_fact | superseded_by_decision` 等关闭原因（interrupts/interrupt_deliveries 表）
- 事件与投影同事务；事件存储无 update/delete 业务入口

#### 1.3 状态机与事务核心

- 实现 PRD §4.1 当前完整转移图，含外部事实的 `queued → failed` 与 `waiting_human → done`
- `Recommendation → DomainCommand → Transition` 类型隔离；只有 `transition()` 写 Run 状态
- CAS 拒绝过期命令；非法转移报错并记审计事件（`ErrRejectedStale` + `auditIllegalTransition`）
- transactional outbox、稳定 operation key、提交唤醒与退避框架
- 三组具名调度器与提交唤醒生产接线：`siftd` 分别驱动 Intake / Supervisor / Outbox；事务提交经 `DB.SetOutboxWakeup` 立即推进 outbox，独立时钟只在 `startSchedulers` 集中创建。`cmd/siftd/main_test.go` 以 production wiring factory 逐边验证 Intake 的未到期 `NextPollAtMS` skip、Supervisor/Outbox 不串联，且 outbox startup sweep drained 后 `EnqueueOperation` / `EmitInterrupt` 经 commit wake 到真实 comment worker；该生产接线已由 [#302](https://github.com/miaoxiaoyong/sift/issues/302) 的 rereview-2 PASS 核销。`internal/storage/scheduler_test.go` 只覆盖 storage seam 的并发 wake 收敛，不声称 production 职责/步频证据。此前 `scheduler.go` 的 Intake/Reconciler/Supervisor 仅为骨架，不是生产步频证据
- V1 与 V2 核心崩溃注入；当前已实现的状态、Forge Run/receipt、Task Spec、Brain trace/token、outbox claim/completion 写入族均以末写入点 abort 注入验证全有或全无；项目健康、Forge 收费、Interrupt 推进与 delivery 在各自写端口实现时补入同一门禁，不得以 schema 代替崩溃证据。M6 继续闭合的 V2-12/V2-13/V2-15 具名证据见 [`docs/testing/runtime-matrix.md`]：`TestV2InterruptFivePartCrashMatrix`、`TestV2RetryProbeSuccessCrashMatrix`、`TestHookCrashReplayRecordsOneStableDrift` / `TestHookRecheckCrashReplayReceiptIsAtomicWithTerminalResult`。

#### 1.4 配置与启动生命周期

- 统一 `SIFT_HOME` 路径解析，默认 `~/.sift/`
- 全局配置 schema、零配置默认值、Agent 定义 schema 与至少两个 Agent 配置的校验能力
- 敏感配置启动期一次读取并保存指纹；运行期变更只告警、不生效
- 调度硬护栏：未知 Agent 拒绝、按 Agent 的 `max_concurrent`、需要时的项目互斥
- 启动探测分级框架：进程级失败拒启；项目级失败只隔离该项目并产生一次告警
- V12：不提供任何可选配置也能启动并调度；默认值表缺项即失败

#### 1.5 控制面与进程边界

- `siftd.sock` 与 `run.sock` 分离；operator capability 只授权运维端点
- RPC envelope 携协议/二进制版本；主版本不一致拒绝
- 单实例互斥，第二个 daemon 明确拒启
- 只创建 Unix socket，不创建 TCP/UDP listener；Linux 集成测试枚举本进程 socket inode 与 `/proc/self/net/{tcp,tcp6,udp,udp6}`，严格断言零 TCP/UDP listener
- V10a 首段：无 operator token 的运维请求被拒、`run.sock` 无运维动词、run token 不能调用 wrapper handoff 动词
- V10b：V0 以 Agent 身份读取 operator token 并调用运维 RPC 预期成功，同时 `doctor` 必须报告此未闭合边界
- 实现薄 CLI 的 `ps/logs/worktree/doctor` 与 `kill/retry` 请求壳；所有运维命令只走 daemon，不直连 DB（`cmd/sift/main.go`）
- daemon 不可用时只允许明确标记为 offline 的只读诊断；`kill/retry` 等写操作拒绝，绝不离线改库（`sift doctor --offline` + `OperatorRequest` 失败拒绝）
- `doctor` 基线检查 runtime、SQLite、Agent CLI、相关 forge CLI 登录/版本、按配置启用的 tmux、目录/socket 权限，且 CLI 进程退出状态映射 doctor `exit_code` 0/1/2；后续片增补策略、hooks、积压与安全姿态

#### 1.6 Reconciler 与 fake 骨架链

- fake Forge、fake Agent、fake Brain provider 实现与真实端口同契约
- M1 骨架链：fake Issue → T1/T2 → queued → fake attempt 完成证据 → 注入 fake forge「Change 已合并」事实 → done
- M1 **不实现临时 Gate、不创建 Change、不保留旁路裁定**；M4 接入 Gate/Create Change 后替换测试夹具
- 将 fake 骨架链作为 V9 的首段 CI 测试，而非手工验证
- 事件时间戳覆盖「可信触发标签观测 → Agent started」，为 P50 指标留 day-1 数据

#### 1.7 Brain 调用壳、T1/T2 与 Task Spec

- 编写 `specs/brain.md` 的统一调用与 T1/T2 契约
- 本机 agent CLI 调用壳：stdin/临时文件输入 → schema 校验 → 同 prompt 重试一次 → 逐触点确定性兜底
- 提示词与 schema 版本化并入 git；调用身份与各触点作用域以 `specs/brain.md`、`specs/storage.md` §10.1 为准
- 每次调用按 `specs/storage.md` §10.1 持久化 call/attempt（`brain_calls` 一次终结 + 有序 `brain_attempts`）；具体调用、兜底与 Gate 关联契约由 `specs/brain.md` 定义（`ReserveBrainCall`/`RecordBrainAttempt`/`FinalizeBrainCall`）
- T1：Issue 体检，失败兜底为直接入队（`T1Contract` + `T1FallbackOutput` = ready 直接入队）
- T2：生成 kind/agent/goals/开工前审批建议；失败兜底为人工分派（`T2Contract`，fallback 留待人工分派）
- 组装 Task Spec（Description + Goals + Guardrails + Context）；Context 从 base/全局/任务附注组合
- token 收费口只在调用壳；超限后所有触点走各自兜底并产生告警事件
- replay JSONL 单条 `brain_call` record 内携有序 attempts
- M1 fake 链使用 fake provider 的合法 T2 输出；真实 CLI 壳通过 fixture/子进程测试

> Intake 澄清/确认评论的 crash marker 收敛与旧 generation 回复仲裁迁至 M2 §2.3/§2.5：两者依赖 M2 的真实 Forge comment worker、回复 receipt 消费与 `PersistIntakeDecision` 写端口。M1 只保留已实现、可独立验证的 Brain replay JSONL，不以 schema 或 outbox 通用框架冒充 Intake 实现。

### 先写 spec

- `specs/storage.md`
- `specs/control-plane.md`
- `specs/config.md`（含全部确定性默认值、Agent 定义、路径与启动探测分级）
- `specs/outbox.md`
- `specs/brain.md`（调用壳、T1/T2；后续触点随片增补）

### M1 门禁

- V1、已实现写端口的 V2 核心、V9 骨架段、V10a 端点段、V10b、V12、V14 通过
- Brain 测试夹具与门禁对账：真实子进程 fixture 覆盖 schema 失败后同 prompt 重试、冻结 prompt 与逐 attempt token 收费；fake provider 覆盖逐触点兜底与 trace 恢复
- V15 四组合构建段通过
- 第二实例拒启且进程无网络 listener
- 敏感配置磁盘漂移不热生效；零配置启动通过

---

## M2：Forge 与 Intake

### 前置

- M1 门禁通过

### 任务

#### 2.1 Forge 端口与双平台适配

- 逐项实现 PRD §5.2 最小动词集；签名与中性类型由 `specs/forge.md` 唯一定义
- GitHub/GitLab 差异在边界归一；不确定语义输出 `unknown`，不得猜测
- 评论与标签事件的 actor 为必需语义；缺失时适配器 fail closed
- argv 数组执行 `gh/glab api`，禁止 shell 拼接
- 统一错误为 `Transient | RateLimited | AuthOrCapability | ContractViolation | SemanticConflict`

#### 2.2 Change/merge 副作用契约

- Change marker 跨 open/closed/merged 全状态查找；同 base/head 无匹配 marker 时返回冲突，绝不接管
- merge 端口接受 expected head，适配器映射为远端原子条件检查
- 能力探测不能证明条件合并时，将该项目 `auto_merge` capability 置为不可用；不得只告警后继续
- V3/V7 覆盖 marker、冲突、stale head 与能力缺失禁用

#### 2.3 Intake、T1 接线与反向同步

- 每项目独立自适应轮询，游标在整批持久化后推进；幂等键 ``
- 触发标签事件先回溯 actor；不可信作者被可信 actor 触发时强制开工前审批
- T1 接在归一 Issue 后；T1 不可用时直接入队
- 实现 `intake_items` 投影/CAS、回复 receipt 消费与 `PersistIntakeDecision` 写端口；澄清/重复确认评论与 intake 状态变更由同一领域事务创建 outbox operation
- 澄清/确认评论在「远端成功、本地提交前崩溃」后按 marker 查询收敛，不重复发送；覆盖真实 Forge comment worker 的崩溃重放测试
- 回复按当前 clarification generation 仲裁；旧 generation 回复只追加审计事件，不推进 intake 状态
- 逐项实现 PRD §4.5 反向同步；事实观测不套 actor 鉴权，移除触发标签必须鉴权
- 运行期 `AuthOrCapability` 只隔离对应项目并告警一次；健康项目继续调度

#### 2.4 Forge API 预算

- API 调用只在 Forge 适配层收费；接近/达到上限时降为慢轮询并告警
- reset、退避与预算状态持久化；不得在 M5 再建第二收费口

#### 2.5 契约与事实收敛测试

- 双平台 fixture 跑同一契约套件：分页、actor 缺失、限流、平台差异、marker、merge CAS
- intake 评论 worker 的远端成功/本地提交前崩溃重放不重复发送；旧 generation 回复只审计、不推进状态
- V11 首段：fake/fixture 中让 `waiting_human` Run 的 Change 被外部合并，断言 `done + gate_bypassed`
- V11 后续闭合 Gate/审计/Ledger 分类与指标分母

### 先写 spec

- [`specs/forge.md`]

### M2 门禁

- V3 通过；V7 的 Forge/marker/CAS 部分通过
- 条件合并能力缺失时 `auto_merge` 被结构性禁用
- actor 缺失事件被忽略；坏项目不影响健康项目
- Intake crash marker 与旧 generation 回复仲裁测试通过
- V11 外部事实收敛首段通过

---

## M3：Runtime 与启动停滞安全闭环

### 前置

- M2 门禁通过

### 任务

#### 3.1 ExecutionBackend、launcher 与 wrapper

- `process` backend 只负责启动 wrapper；Agent 恒由 wrapper 直接 spawn 到其进程组
- Agent 启动只经一个 launcher 函数，V0 为恒等实现；不得绕过该接缝
- daemon 只从自身安装目录解析同版本 wrapper，不从 `PATH` 猜；wrapper/daemon 主版本不一致拒绝
- Agent 环境只注入非机密 `SIFT_RUN_DIR`；bootstrap/run token 不进 argv 或环境变量
- 逐条实现 DESIGN §8.4 wrapper 契约，控制文件用 temp + fsync + rename


#### 3.2 Spawn handoff 与控制面最终接线

- 逐步实现 ADR-010 的 operation lease、acquire/session、permit、spawning handoff、started 证据
- wrapper 不写 DB；permit 的重放不得再次进入 spawn
- `spawning` 期间不可换 owner；fencing 不能替代旧执行者消失证明
- V10a wrapper 段：跨 instance/session/permit/generation 使用全部拒绝


#### 3.3 Worktree 与成功证据

- 每 Run 独立 worktree；policy/context 只从 base 读
- Sift 的 git 命令强制 `-c core.hooksPath=/dev/null`
- 仅成功且身份一致的 `result.json`、冻结 final head、有提交三者齐备时产生“可创建 Change”领域事实；实际创建在 M4
- 失败 attempt 与中间提交不得产生 Change 操作


#### 3.4 恢复矩阵与资格门控

- 逐行实现并逐行测试 DESIGN §10.1 完整恢复矩阵；M6 双后端具名清单见 [`docs/testing/runtime-matrix.md`] R01–R19，使用 `TestRecoveryRowsBackendParameterized`。
- 恢复扫描先于启动 operation lease 回收
- 凡执行体可能存活却要判 orphaned，必须先走同一受控终止流程
- 进程身份至少校验 PID + 启动时间 + 可执行路径 + control nonce；不得向不确定 PID 发信号
- attempt 所用 Agent/version 未标 `process-group-verified` 时，不把“进程组消失”当充分证明，不自动 retry，保持隔离并转人工
- 多 wrapper 竞争、旧 generation 苏醒、heartbeat 过期、后端会话与 wrapper 不一致均按 DESIGN 矩阵收敛并记安全事件


#### 3.5 attempt_resolution、隔离与唯一仲裁

- 隔离是独立投影：即使 Run 已 `failed`，未证明执行体消失前 worktree 不回收、不复用
- 实现一个仲裁函数，供 `claim:started`、恢复补 started、迟到 `result.json`、Interrupt 指令四个入口调用
- resolution 未落定且合法启动事实先到：attempt → running、Run `waiting_human → running`、Interrupt 以 `superseded_by_fact` 关闭并接管监督；**不得继续终止正常执行体**
- `attempt_resolution=reject | retry_after_absence` 先落定：迟到事实不推进旧 Run，登记身份、返回 `superseded_by_decision` 并受控终止旧执行体
- 自动 escalate/hold 不写 resolution，事实优先窗口保持开放


#### 3.6 Attention 泛型单一发射器核心

- 在 M3 建立此后唯一的 Interrupt 发射入口；M4/M5 只能调用或扩展渲染/调度，不能新建第二入口
- 入口从第一天支持 PRD 全部 reason 的最小确定性契约：reason/min_modality、互斥 options（≤4）、fallback headline/brief/links、expires/on_expire 与 severity 映射；T4 不可用时也能生成合法对象
- 每类故障有带 domain/version/reason 的稳定生成键并受唯一约束；`startup_stall` 使用 `(run_id, attempt_no, generation, cause=startup_stall)`，诊断分类不拆键
- Run 转移、Interrupt、注意力记账、事件、发布 operation 五件事同事务
- M3 使用已有 forge 评论与确定性 fallback 作为可见发布面；T4/T6、Channel、critical 熔断在 M5 增补
- 受控终止无法证明消失时生成一条 `startup_stall`、Run 转 `waiting_human`、attempt 保持隔离；不得静默停在 queued


#### 3.7 受控终止

- 恢复、operator kill/retry、超时共用：身份确认 → 有界信号升级 → 复核消失
- 确认消失后的结局按来源区分：恢复按重试策略、retry 新建 attempt、kill 不新建且 Run failed；双后端证据见 `TestV4KillRetryBackends`、`TestTerminationKillAfterAbsenceFailsWithoutNewAttempt`、`TestTerminationRetryAfterAbsenceCreatesNewAttempt`。
- 未确认消失统一进入 §3.6；人的后续 retry/reject/hold 在 M5 接通


#### 3.8 hooks 与 doctor

- hooks 指纹覆盖 `.git/config`、`core.hooksPath` 值和最终目录内容；Agent 结束后复核——#848 `365a9fa` 接通生产基线与完成复核，崩溃重放证据见 `TestHookCrashReplayRecordsOneStableDrift` / `TestHookRecheckCrashReplayReceiptIsAtomicWithTerminalResult`。
- `doctor` 报 hooks 漂移、隔离 attempt/未回收 worktree、process-group 资格与 `unsafe-local`


### 先写/增补 spec

- `specs/control-plane.md`：acquire/permit/started 完整字段与版本握手
- `specs/storage.md`：resolution、隔离、关闭原因
- `specs/config.md`：启动 lease/等待/终止/复核/Report 退避默认值
- `specs/interrupt.md`：先落全部 reason 的最小确定性契约与 `startup_stall` 特殊规则

### M3 门禁

- V4 的 **M3 process 首跑段**通过；完整双后端矩阵仍按验收权威表在 M6 最终闭合
- V5a：base/worktree 读取源通过；硬护栏 V5b 留 M4
- V10a wrapper 凭据部分通过
- 每个 PRD reason 均能在无 T4/T6 时生成结构合法、可发布的 fallback Interrupt（`interruptTemplates`/`renderInterrupt` golden/vector 契约）
- 同一 startup_stall 并发发现只生成一条 Interrupt、扣一次配额、保留一条可重放发布 operation（generation-key 唯一冲突按幂等回放收敛）
- 无法证明消失时系统可见且 worktree 保持隔离；本片不要求 M5 的人工 retry 两段式

---

## M4：Gate、Shadow Gate、Ledger 与回放

### 前置

- M3 门禁通过
- 本片开始前先完成下列 spec，而不是把 spec 完成列为本片结束条件

### 任务

#### 4.1 Policy 与有效策略组装

- 编写 `specs/policy.md`；项目 policy 经 closed schema 校验，失败只隔离该项目
- Gate 外组装：base policy ∪ 全局缺省 → 按认证与 forge capability 剔除未获资格的提权项
- `auto_merge` 同时要求配置、类别认证、远端 expected-head CAS capability；缺一即关闭
- 有效策略 hash 与 certification version 进入冻结输入
- `doctor` 横向比较项目策略并标记漂移；Gate 本身不读取配置/认证/文件

#### 4.2 Brain T3/T5

- 在 `specs/brain.md` 增 T3/T5 schema、提示词版本与兜底
- T3 输出风险分与风险点；失败/超预算视为高风险
- T5 分类 flaky/真实失败/基础设施问题；失败兜底为 HITL
- 两触点复用 M1 调用壳、token 收费与 trace；输出来源/版本进入 Gate 快照

#### 4.3 Gate、Shadow Gate 与 Change 创建

- 编写 `specs/gate.md`；`gate(changeFacts, effectivePolicy, riskScore)` 保持纯函数
- `gate_input_hash` 摘要整份规范化快照；缓存键仅 `(gate_input_hash, gate_version)`
- 默认硬护栏、Checks、review policy、auto merge 顺序按 PRD §5.4
- 软护栏豁免默认仅本 Run 本次命中；“记住”必须是独立显式选项，并形成可审计的仓库 policy 例外变更
- 硬护栏永远不进入一次性/记住豁免路径；测试同时覆盖两类软豁免与硬护栏拒绝
- Gate 每次调用强制写快照与影子预判，无配置开关；行为测试断言每次调用新增 calibration 行
- 需要 HITL 时，预判与 M3 发射器的 Interrupt 五件事同事务
- 仅消费 M3 的“可创建 Change”事实；创建操作使用 marker 并持久化远端 ID（`gate.EnqueueCreateChange` / `forgeworker.ChangeWorker`）

#### 4.4 Ledger、人类结果接口与认证投影

- 编写 `specs/ledger.md`，覆盖 Gate 预判、人类决定、路径/文件类型、护栏、Issue 作者、打扰特征及自然语言原料
- 提供确定性 `recordHumanDecision` 应用入口；M5 Command 只调用它，不另写账本（`storage.RecordHumanDecision`）
- 人类结果、校准样本与认证投影增量在同事务提交（`storage.RecordHumanDecision`）
- 认证按任务类别计算漏放、误拦、总样本与负样本绝对数；输出只有类别布尔与证据摘要（`storage.Certification`）
- forge 手工合并在已有 Gate 预判时调用 `recordHumanDecision`：把手工合并记为人的实际决定、保留校准样本并附 `gate_bypassed`（`external_decision_bindings` + `storage.RecordHumanDecision`）
- `gate_bypassed` 不进入 Sift 自发合并的误放行率分母，但作为独立绕过样本保留（认证仅聚合 settled calibration）

#### 4.5 回放集

- `SELECT → JSONL` 导出当时冻结的 Gate 快照，不拼当前数据（`storage.ExportReplayJSONL`）
- 导出独立 Brain trace；仅真实参与 Gate 输入时携可空 snapshot ID（`brain_gate_input_links` + Brain JSONL v2）
- 导出后用同一 Gate 函数/Brain trace runner 重放；可量化漏放/误拦变化

### 先写 spec

- `specs/policy.md`
- `specs/gate.md`
- `specs/ledger.md`
- 增补 `specs/brain.md`（T3/T5）

### M4 门禁


- V5b：`.sift/**`、CI 配置、head 变化 fail closed；A5/A6 在本片闭合
- T3/T5 正常输出与确定性兜底均被版本化并进入 trace/Gate 快照
- V6：纯函数、cache miss、每次 Gate 必有校准记录、导出重放通过
- V7：Change marker 与 merge stale/no-op 全链通过
- V11 审计段：等待 Gate/HITL 时外部合并 → done + gate_bypassed，并写入人类决定/校准分类；指标查询分母留 M5
- Gate/Shadow/认证/回放/Change 创建五项同时可用，无延后项

---

## M5：Attention、Command、Report、Brain 与指标

### 前置

- M4 门禁通过

### 任务

#### 5.1 Brain T4/T6/T7 与 A7 防火墙

- T4/T6/T7 调用壳与验收矩阵已由 rereview-6 PASS 核销；这不表示生产接线或 M5 已实现
- T4 生成 headline/brief/options；失败兜底为裸链接 + 原始状态
- T6 只建议时机/通道，失败按 severity 确定性阈值；任何结果仍经过发射器配额
- T7 只生成 policy 提案或 context 草稿，二者都不自动生效
- 测试 T7/历史数据不能放松单条 Gate、不能抑制单条 HITL
- 三触点在生产路径复用统一调用壳并写 trace

#### 5.2 Interrupt 全功能与 Channel

- 先写 [`specs/channel.md`]：冻结首个 webhook Channel、delivery/operation key、Attention sealed payload 接缝与 Forge 失败兜底；实现项保持未勾
- 复用 M3 已支持全部 reason 的唯一发射器，接入 T4/T6、Channel、调度与 critical 熔断；不得新增 reason 专用旁路
- LLM 只能建议 severity 降级；`min_modality: visual` renderer 拒绝语音路径
- 实现首个 Channel；连续失败 N 次转 forge 告警评论，并在 ps/doctor 显示
- 一次 Interrupt 只收费一次；升级重推不重复收费

#### 5.3 超时与升级


- Supervisor tick 扫描 `expires_at/on_expire`
- 达 `max_escalations` 后 severity 封顶，并按 reason 的确定性映射进入 `auto_reject` 或 `hold`
- `startup_stall` 禁止配置 `auto_reject`，达上限强制 `hold`，且不写 resolution
- 两类上限结局分别做状态机测试；每次升级轮换 nonce
- critical 熔断在发射器内生效，非 critical 超额合批且不可借支

#### 5.4 Command 与 startup_stall 两段式


- actor 鉴权 → 严格语法 → nonce/current Interrupt/options 校验 → DomainCommand + 回执 outbox
- 逐项实现 PRD §7.1 `/sift approve | reject | retry | hold | ask` 与审批标签的 reason-specific 确定性效果；不在当前 options 内一律拒绝
- `/sift ask <文本>` 同事务写当前 Run 的任务层澄清与 Ledger 语义原料，并按当前 Interrupt 契约继续；不得自动升格项目/全局 Context
- 通用 reject/retry/hold/approve 均经唯一 transition 与 outbox，不直接写状态；标签和评论路径共用鉴权/幂等实现
- `startup_stall` 只提供 retry/reject/hold，approve 必须拒绝
- retry 请求不关闭 Interrupt、不写 resolution；探测在途拒绝新指令并回复“已在探测中”
- 探测失败复用同一 Interrupt、升级计数、轮换 nonce；达到上限 hold
- 探测成功以 ADR-013 单一 CAS 事务提交：消失证据、旧 attempt 终结、`retry_after_absence`、解除隔离、关闭 Interrupt、Run → queued、新 attempt/claim、启动与回执 operation、事件
- reject 写 `attempt_resolution=reject`、Run failed，但隔离保持到执行体被证明消失
- 所有动作复用 M3 唯一仲裁函数；覆盖事实先到/决定先到/探测在途事实到达
- `/sift reject` 原因与 `/sift ask` 文本随人类决定写入 Ledger，不丢语义原料

#### 5.5 Report


- `sift report` 只连 `run.sock`，从 `SIFT_RUN_DIR/control.json` 读 run token
- running 接受；spawning 返回有界可重试 `not_ready`；跨 Run/过期 attempt 永久拒绝
- 进度/goal/blocker/完成声明只写事件，不直接转状态
- 确定性去重、令牌桶与每 Run Interrupt 子配额；触顶只生成一次异常处置

#### 5.6 三类预算集成


- token 预算沿用 M1 Brain 收费口；API 预算沿用 M2 Forge 收费口；注意力沿用 M3 发射器收费口
- 本片只做三者并存与降级集成，不复制收费实现
- token/API 降级不得突破注意力配额

#### 5.7 指标、CLI 与时间线


- 从事件流/Ledger 确定性派生 PRD §10.2 全部指标（当前九项）：加权打扰/已合并 Change、误放行率、门禁绕过率、Gate 漏放/误拦、HITL 率、配额消耗、分派准确率、LLM 成本
- reason 耗时权重为配置项；响应间隔只作调度特征，不作人类分钟数
- `gate_bypassed` 排除误放行率分母的查询测试
- `sift ps` 显示 Run/attempt、今日注意力余量、隔离与推送故障；`logs` 提供 Run 原始日志；事件时间线可查
- 输出「触发标签 → started」延迟分布；真实 P50 < 60s 在 M7 验收

### 先写/增补 spec

- `specs/interrupt.md`
- `specs/command.md`
- `specs/report.md`
- 增补 `specs/brain.md`
- 增补 `specs/config.md`（配额、熔断、reason 上限去向、指标权重）
- [`specs/channel.md`]

### M5 门禁


- V2 retry 成功原子事务段、V4 人工态交错段通过
- T4/T6 各自兜底、T7 两类只读提案及 A7 防火墙测试通过
- V8、V10a Command/Report 段、V13 通过
- `startup_stall` approve/auto_reject 均被拒；retry 两段式与迟到事实仲裁通过
- 九项指标可查询，V11 指标段闭合：`gate_bypassed` 不进入误放行率分母且进入门禁绕过率
- 全 fake 端到端链成为 V9 的自动化完整段

---

## M6：tmux 与完整故障矩阵


### 前置

- M5 门禁通过

### 任务与门禁

- V4 双后端完整矩阵与独立阶段门报告；真实资格、真实双 Forge、手机与发布证据属于 M7/M8。

---

## M7：真实 Agent 与 PoC 取证

### 前置

- M6 门禁通过

### 任务

#### 7.1 Agent 与资格

- 至少两个 Agent 定义通过配置校验，其中一个真实跑通
- 真实 Agent 可使用 Task Spec 与 `sift report`
- 按 Agent CLI + 版本执行进程拓扑资格测试；结果进入 doctor

#### 7.2 双平台与人工验收

- GitHub、GitLab 各一个真实项目完成摄入 → Brain → Runtime → Gate → HITL/merge
- 正式留存一次手机端审批证据、一次真实 HITL 往返、一次负样本
- 真实运行 ≥3 个并行 Run，触及注意力上限并证明合批，不把 V8 模拟结果冒充此证据
- 测量真实「可信打标 → Agent started」，验证 P50 < 60s

#### 7.3 凭证形态 spike

- 对首批 Agent 分别实证 macOS/Linux 凭证存储与可挂载性
- 结论按 OS 分行；macOS 不成立不自动否定 Linux 方向
- 若触发 ADR-007 证伪条件，先追加 ADR，不在 plan 中静默换方向

### M7 人工门禁

- A1–A4、A7–A9 的对应证据齐全
- A8：两个定义通过、一个真实闭环
- 双平台正式证据与凭证/资格矩阵归档

---

## M8：发布

### 前置

- M7 门禁通过

### 任务

#### 8.1 发布归档与升级

- GoReleaser 产出同版本三二进制单归档、manifest、校验和
- 四组合运行安装、版本握手、SQLite、双 socket、wrapper handoff 冒烟
- 安装到版本目录，校验后原子切换 `current`；禁止逐文件覆盖
- CLI/daemon/wrapper 主版本不一致拒绝并由 doctor 报错

#### 8.2 托管与安装渠道

- launchd user agent、systemd user unit、无 systemd foreground fallback
- 崩溃自启与原子升级后重启
- Homebrew tap 与 Release 归档两条安装路径

#### 8.3 干净机验收与文档

- 干净 macOS 与 systemd Linux 各从发布归档安装并跑通完整闭环/恢复
- 四种 OS/架构组合均保留安装与二进制冒烟证据
- `dev/release.md`：manifest、构建矩阵、CGO 门禁、升级与干净机流程
- `guides/installation.md` 与 `runbooks/troubleshooting.md`

#### 8.4 doctor 最终姿态

- 报 `unsafe-local` 与 TM6 每条未闭合暴露面，不把资格写成沙箱闭合
- 按 DESIGN §8.10 全量检查 runtime、SQLite、Agent CLI/资格、相关 forge CLI、可选 tmux、policy schema/漂移、hooks、目录/socket 权限、版本、隔离、outbox 与推送故障
- macOS/Linux 安全姿态分别呈现

### M8 门禁

- V15 全部通过
- A10 两台干净机证据齐全
- 原子升级不丢数据库状态，较新 schema 拒绝旧 daemon

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
| `specs/channel.md` | M5（首个 webhook Channel 与失败兜底） | 随 Channel/Attention/outbox 实现同步 |
| `specs/policy.md` | M4 | 随 Gate 策略同步 |
| `specs/gate.md` | M4 | 随 Gate/回放同步 |
| `specs/ledger.md` | M4 | M5 Command/指标同步 |
| `specs/command.md` | M5 | 随指令语法同步 |
| `specs/report.md` | M5 | 随上报契约同步 |
| `dev/release.md` | M8 | 随发布链同步 |
