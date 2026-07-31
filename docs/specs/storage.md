---
status: active
created: 2026-07-28
summary: SQLite 表、事务、迁移与审计存储契约
---

# 存储规格

本文定义 `sift.db` 的表、字段、约束、事务边界与迁移行为，是状态机、恢复、outbox、回放和审计的共同存储契约。

结构来源：[DESIGN §6.2–§7、§8.4–§8.7、§10](../DESIGN.md)，[ADR-002](../decisions/002-reconciler-and-single-transition-entry.md)、[ADR-003](../decisions/003-transactional-outbox.md)、[ADR-004](../decisions/004-gate-as-pure-function.md)、[ADR-009](../decisions/009-tech-stack-go.md)、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)，[WBS M1 §1.2–§1.3、M3 §3.4–§3.5](../WBS.md)。

## 评审处置

首次评审：[2026-07-28-specs-review-pi-k3.md](../reviews/2026-07-28-specs-review-pi-k3.md)；定向复评：[2026-07-28-storage-rereview-pi-gpt-5.6-sol.md](../reviews/2026-07-28-storage-rereview-pi-gpt-5.6-sol.md)。

| 发现 | 处置 |
|------|------|
| S1 Brain trace 身份域错误 | 改为 intake/run/aggregate scope + subject key，按 T1–T7 约束可空 Run/attempt |
| S2 写端口覆盖不全 | 补项目、Task Spec、Forge 收费、Interrupt 推进与 delivery 等唯一写端口归属 |
| S3 hooks 无持久基线 | 新增 `project_hook_baselines` 当前投影与安全事件规则 |
| S4 Report burst 无法表达 | 新增整数令牌桶；critical fuse 改为 append-only entry 的滑动窗口查询 |
| S5 字段与导出细节 | 统一 schema version/FK/原因枚举，明确 probe、manual Run、close reason、回放 JSONL 与 immutable payload trigger |

S1–S5 已处置；定向复评通过，本规格转为 `active`。后续字段变更须与迁移、写端口和派生测试同步。

Brain 字段级评审 [2026-07-28-brain-review-pi-gpt-5.6-sol.md](../reviews/2026-07-28-brain-review-pi-gpt-5.6-sol.md) 的交叉补丁：B1 将单行 `brain_traces` 拆为 `brain_call_counters`/`brain_calls`/`brain_attempts`（§10.1）；B2 新增 pre-Run intake 投影（§7.5–§7.6）与对应写端口（§11）；B4 明确 token 发起前阈值 + 事后越界 post-charge 对 §9.1 通用 CAS 的例外（§9.1）。

## 1. 存储不变量

1. 单库：`$SIFT_HOME/sift.db`，SQLite WAL，`modernc.org/sqlite`，不引 ORM。
2. `siftd` 是唯一业务写者；`sift` 与 `sift-agent-wrapper` 不直连数据库。
3. 写连接池 `MaxOpenConns=1`；读连接不得升级为写事务。
4. `runs.status` 只能由存储端口的 `transition()` 路径更新。代码中不存在通用 `UpdateRun` 或暴露裸事务的接口。
5. 状态投影、append-only 事件、必要 outbox、预算/游标/幂等记录在同一事务提交。
6. 外部 IO、进程等待、文件扫描、LLM 和 forge 调用不得发生在数据库事务内。
7. 时间由应用层注入；领域与存储逻辑不得在事务中自行读取系统时钟。
8. 所有 JSON 列使用 [`config.md`](config.md) §4 定义的 canonical JSON；写入前必须经过对应 schema。
9. 审计与回放数据 V0 不做业务删除。worktree/日志清理不得删除 Run、事件、Gate、Ledger 或 outbox 历史。
10. `attempt_resolution` 是唯一规范名称；V0 枚举仅 `reject | retry_after_absence`。
11. attempt 隔离是独立于 Run 状态和 attempt phase 的当前投影：Run 终态、attempt 终结、Interrupt 关闭均不隐含解除隔离；冻结 worktree 不得清理或分配给任何 attempt。
12. 只有持久化的执行体消失证据，或显式的 operator 强制清理安全事件，才能解除隔离；仅凭 lease/heartbeat/等待超时、PID 不存在或 Run 终态均不够。
13. 合法启动/结果事实与 `attempt_resolution` 只在 private `resolveAttemptRaceTx` 内仲裁；public `ApplyCommandEvent`、recovery 和 probe ports 不得各自实现近似事务。
14. 每次 daemon boot 都有存储内恢复屏障。该 boot 的恢复扫描完成前，任何 worker 都不得 claim 或 reclaim `launch_agent` operation；非启动 operation 不受此屏障阻塞。
15. 恢复扫描覆盖全部非终态 attempt 与全部未完成 `launch_agent` operation，不以 Run 状态过滤；外部文件/进程观测在事务外完成，只有确定性恢复动作进入写事务。

## 2. SQLite 打开契约

每个连接必须设置并验证：

```text
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = FULL;
PRAGMA temp_store = MEMORY;
```

初始化写连接额外执行并验证：

```text
PRAGMA journal_mode = WAL;
PRAGMA wal_autocheckpoint = 1000;
```

任一关键 PRAGMA 未生效即拒绝启动。数据库文件模式为 `0600`，父目录为 `0700`。

### 2.1 基础类型

| 逻辑类型 | SQLite | 契约 |
|----------|--------|------|
| ID | `TEXT` | 32 位小写十六进制，由 `crypto/rand` 生成；不可承载业务语义 |
| 时间 | `INTEGER` | Unix epoch 毫秒，UTC；可空时间用 `NULL` |
| boolean | `INTEGER` | 仅 `0/1`，带 CHECK |
| hash | `TEXT` | SHA-256 小写十六进制 64 位 |
| enum | `TEXT` | 每列带 CHECK；未知值不得入库 |
| JSON | `TEXT` | canonical JSON；schema version 另列 |
| forge id | `TEXT` | 原样字符串，不假设数字 |

列说明显式写 `NULL` 才允许空值；未写 `NULL` 的列均须在 DDL 声明 `NOT NULL`。SQLite rowid 表不会自动令组合主键各列 `NOT NULL`，因此本规格所有组合主键列还必须显式声明 `NOT NULL`，不得依赖 PRIMARY KEY 的隐含行为。

数据库内不存 capability 明文。operator token 只在文件和 daemon 内存；run token、bootstrap nonce、wrapper session、spawn permit 只存 SHA-256 hash。用于外部公开指令防重放的 Interrupt nonce 可以明文存储，因为它会出现在 forge 评论中，不作为秘密。

## 3. 迁移

### 3.1 `schema_migrations`

| 列 | 类型 | 约束 |
|----|------|------|
| `version` | INTEGER | PK，正整数，严格递增 |
| `name` | TEXT | NOT NULL，唯一迁移名 |
| `checksum` | TEXT | NOT NULL，迁移文件 SHA-256 |
| `applied_at_ms` | INTEGER | NOT NULL |
| `binary_version` | TEXT | NOT NULL |

迁移文件随二进制嵌入，命名 `NNNN_name.sql`，只允许前向执行。每个迁移在独立 `BEGIN IMMEDIATE` 事务中完成；checksum 与已应用记录不一致时拒启。数据库最高版本高于二进制支持版本时拒启，禁止尝试降级。

Command 字段迁移必须把旧 `forge_event_receipts(project_id,forge_event_id)` 唯一键改为 `(project_id,event_kind,forge_event_id)`，回填 `event_key` 为可验证的 canonical identity；无法唯一回填或发现同 key 异 digest/target 的旧行时拒启并要求人工迁移。迁移必须同时建立 command outcome/effect/target 表及 gate re-evaluation operation 的 FK/唯一索引，不能只加 nullable 列。

## 4. 配置与项目投影

### 4.1 `config_snapshots`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `config_hash` | TEXT | NOT NULL UNIQUE |
| `schema_version` | INTEGER | NOT NULL |
| `canonical_json` | TEXT | NOT NULL |
| `source_present` | INTEGER | NOT NULL boolean |
| `source_mtime_ms` | INTEGER | NULL |
| `loaded_at_ms` | INTEGER | NOT NULL |
| `binary_version` | TEXT | NOT NULL |

同一 hash 可复用既有快照。表禁止 UPDATE/DELETE。

### 4.2 `daemon_boots`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK，boot id |
| `config_snapshot_id` | TEXT | FK config_snapshots |
| `pid` | INTEGER | NOT NULL |
| `binary_version` | TEXT | NOT NULL |
| `protocol_major` | INTEGER | NOT NULL |
| `started_at_ms` | INTEGER | NOT NULL |
| `recovery_completed_at_ms` | INTEGER | NULL；本 boot 完成启动恢复扫描后只写一次 |
| `stopped_at_ms` | INTEGER | NULL；只允许专用“正常停止”一次性补写 |
| `stop_reason` | TEXT | NULL |

新 boot 行的 `recovery_completed_at_ms` 必须为空；它是 `launch_agent` claim/reclaim 的持久恢复屏障，不得沿用前一 boot 的完成标记。`CompleteStartupRecovery` 只能在全部恢复候选已收敛为确定动作（包括保持冻结并转人工）后一次性补写。`recovery_completed_at_ms` 与 `stopped_at_ms/stop_reason` 是 append record 的仅有补全字段；不得修改其他列。非正常退出保持 stop 字段为 NULL。

### 4.3 `projects`

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK，配置中的稳定 project id |
| `config_snapshot_id` | TEXT | NOT NULL FK |
| `forge_kind` | TEXT | `github \| gitlab` |
| `forge_host` | TEXT | NOT NULL |
| `forge_project_key` | TEXT | NOT NULL |
| `repo_path` | TEXT | NOT NULL |
| `enabled` | INTEGER | NOT NULL boolean |
| `health` | TEXT | `active \| isolated` |
| `isolation_reason` | TEXT | NULL 或 `config_invalid \| repo_invalid \| agent_unavailable \| forge_auth_or_capability \| policy_invalid` |
| `capabilities_json` | TEXT | NOT NULL，默认 `{}`；已证明的 expected-head CAS 写为 `{"auto_merge":true}`，缺失/false 均不得自动合并 |
| `capabilities_checked_at_ms` | INTEGER | NULL；最近一次 capability probe（包括未证明）时间 |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

唯一约束：`(forge_kind, forge_host, forge_project_key)`；启用项目的规范化 `repo_path` 唯一。项目配置变化更新当前投影并保留旧 config snapshot。

`health=isolated` 时 `isolation_reason` 必须是：`config_invalid | repo_invalid | agent_unavailable | forge_auth_or_capability | policy_invalid`；active 时必须为空。新增原因必须走 schema migration，不接受自由文本。

### 4.4 `project_hook_baselines`

每项目一行当前基线；历史变化写 events。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `project_id` | TEXT | PK、FK projects |
| `git_config_digest` | TEXT | NOT NULL |
| `core_hooks_path_value` | TEXT | NULL；未显式配置时为空 |
| `effective_hooks_path` | TEXT | NOT NULL，规范化后的最终目录 |
| `hooks_directory_digest` | TEXT | NOT NULL |
| `baseline_digest` | TEXT | NOT NULL；覆盖前四项配置/路径/目录事实 |
| `source_run_id` | TEXT | NULL FK runs；初始基线可空 |
| `source_attempt_no` | INTEGER | NULL；非空时与 source_run_id 组成 attempts 组合 FK |
| `captured_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

接入项目时建立基线；每次 Agent 结束后复核。初始来源两列同为空，attempt 来源两列同为非空。无变化只更新时间；变化时以旧 `baseline_digest` 做 CAS，更新投影并同事务追加 `hooks_drift_detected` 安全事件；按确定性严重度映射需要停 Run/HITL 时，还须同事务执行 Run transition、Interrupt、预算与 outbox，不以新值静默覆盖而不留痕。

## 5. Run 与 attempt

### 5.1 `task_spec_snapshots`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL FK runs |
| `version` | INTEGER | NOT NULL，按 Run 从 1 递增 |
| `schema_version` | INTEGER | NOT NULL |
| `canonical_json` | TEXT | NOT NULL；Description + Goals + Guardrails + Context |
| `content_digest` | TEXT | NOT NULL |
| `source_event_id` | TEXT | NULL FK events；初始 T2 组装可空，ask 更新必填 |
| `created_at_ms` | INTEGER | NOT NULL |

唯一约束 `(run_id, version)`，并声明候选键 `(run_id, id)` 供组合外键使用。`/sift ask` 不覆盖旧 Task Spec：它插入新 snapshot、更新 Run 当前指针并追加事件；历史 attempt 始终引用自己启动时的 snapshot。

### 5.2 `runs`

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `source_kind` | TEXT | `forge \| manual` |
| `project_id` | TEXT | NOT NULL FK projects；V0 的 forge/manual Run 均绑定项目 |
| `config_snapshot_id` | TEXT | NOT NULL FK config_snapshots；创建 Run 时冻结 |
| `forge_kind` | TEXT | NULL，`github \| gitlab` |
| `forge_host` | TEXT | NULL |
| `forge_project_key` | TEXT | NULL |
| `issue_id` | TEXT | NULL |
| `issue_url` | TEXT | NULL |
| `issue_author` | TEXT | NULL |
| `discussion_target_kind` | TEXT | NULL 或 `issue \| change`；manual Run 创建时冻结的 forge comment 目标类型 |
| `discussion_target_id` | TEXT | NULL；与 kind 成对存在的项目内 target ID |
| `discussion_target_url` | TEXT | NULL；与 kind/id 同次 Forge 验证后冻结的规范 URL |
| `status` | TEXT | `queued \| running \| waiting_human \| done \| failed` |
| `version` | INTEGER | NOT NULL，初值 1，每次 transition +1 |
| `kind` | TEXT | NULL，T2 的规范化 kind |
| `agent_id` | TEXT | NULL |
| `hitl_before_start` | INTEGER | NOT NULL boolean，默认 0 |
| `current_task_spec_id` | TEXT | NULL FK task_spec_snapshots；T2 完成前可空 |
| `retry_count` | INTEGER | NOT NULL，默认 0 |
| `max_attempts` | INTEGER | NOT NULL；创建时冻结有效配置 |
| `change_id` | TEXT | NULL |
| `change_url` | TEXT | NULL |
| `change_head_sha` | TEXT | NULL |
| `gate_bypassed` | INTEGER | NOT NULL boolean，默认 0 |
| `failure_reason` | TEXT | NULL 或 `closed_upstream \| change_closed \| untriggered \| hard_guardrail \| agent_exit \| attempts_exhausted \| human_reject \| hitl_expired \| operator_kill \| contract_violation \| no_change` |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |
| `completed_at_ms` | INTEGER | NULL |

约束：

- forge source 必须具有 project/forge/issue 字段，且 discussion target 三列为空；manual source 仍绑定 project/forge，issue 字段为空，并必须有三列同非空的已验证 discussion target。V0 不创建无项目、无法产出 Change 的 Run。
- manual Run 创建端口在同一创建事务前，以其 project 的 Forge `GetIssue` 或 `GetChange` 验证 discussion target 的 kind/id/url；验证的 URL 必须写入本行，不得从用户输入原样信任。该目标是预选讨论面，不改变 `issue_*` 来源字段或 `change_*` 产物字段的语义。
- discussion target 三列只能同空或同非空；一经插入不可更新。manual Run 不存在这三列即拒绝创建，故后续 Interrupt 恢复/重试无需查询漂移的 forge 对象来猜测发布位置。
- `(forge_kind, forge_host, forge_project_key, issue_id)` 在 `issue_id IS NOT NULL` 时唯一，构成 Intake 幂等键。
- `done` 必须有 `change_id`；`gate_bypassed` 不改变状态语义。
- `current_task_spec_id` 以 `(runs.id, current_task_spec_id)` 组合外键保证属于本 Run。
- `completed_at_ms` 仅在 `done/failed` 时非空；`failed → queued` retry 时清空。

索引：`(status, updated_at_ms)`、`(project_id, status)`、`change_id`。

### 5.3 `attempts`

主键 `(run_id, attempt_no)`；`attempt_no` 从 1 单调递增且不得复用。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `run_id` | TEXT | NOT NULL，FK runs ON DELETE RESTRICT |
| `attempt_no` | INTEGER | NOT NULL，PK part，正整数 |
| `phase` | TEXT | `pending \| starting \| spawning \| running \| finished \| orphaned` |
| `generation` | INTEGER | NOT NULL，正整数；换 owner 时递增 |
| `backend` | TEXT | `process \| tmux` |
| `agent_id` | TEXT | NOT NULL |
| `task_spec_snapshot_id` | TEXT | NOT NULL FK task_spec_snapshots |
| `worktree_path` | TEXT | NOT NULL |
| `branch_name` | TEXT | NOT NULL |
| `base_ref` | TEXT | NOT NULL |
| `base_sha` | TEXT | NOT NULL |
| `final_head_sha` | TEXT | NULL |
| `wrapper_pid` | INTEGER | NULL |
| `wrapper_started_at_ms` | INTEGER | NULL |
| `wrapper_executable` | TEXT | NULL |
| `wrapper_pgid` | INTEGER | NULL |
| `wrapper_instance_id` | TEXT | NULL |
| `agent_pid` | INTEGER | NULL |
| `agent_started_at_ms` | INTEGER | NULL |
| `agent_executable` | TEXT | NULL |
| `control_nonce_hash` | TEXT | NULL |
| `heartbeat_at_ms` | INTEGER | NULL |
| `result_exit_code` | INTEGER | NULL |
| `result_signal` | TEXT | NULL |
| `result_digest` | TEXT | NULL |
| `result_observed_at_ms` | INTEGER | NULL |
| `attempt_resolution` | TEXT | NULL 或 `reject \| retry_after_absence` |
| `resolution_at_ms` | INTEGER | NULL |
| `isolation_state` | TEXT | `none \| frozen`，默认 `none` |
| `isolation_reason` | TEXT | NULL 或 `startup_stall \| process_identity_unknown \| termination_unconfirmed \| process_group_unverified \| late_execution_fact` |
| `isolated_at_ms` | INTEGER | NULL |
| `isolation_released_at_ms` | INTEGER | NULL；仅 frozen→none 时写入 |
| `isolation_release_event_id` | TEXT | NULL FK events；解除时指向消失证据或 operator 强制清理安全事件 |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |
| `finished_at_ms` | INTEGER | NULL |

约束：

- wrapper 身份字段必须全空或 `pid/started/executable/pgid/instance` 全具备；Agent 身份三元组必须全空或全具备。
- `task_spec_snapshot_id` 以 `(run_id, task_spec_snapshot_id)` 组合外键保证属于本 Run。
- `running` 必须有 Agent 身份；`finished` 必须有 result，且 `result_exit_code/result_signal` 恰有一个非空；`orphaned` 不要求 result。
- resolution 与 `resolution_at_ms` 同空同非空，写入后不可修改。`reject` 只能来自人的终局拒绝；`retry_after_absence` 只能与执行体消失证据在 ADR-013 结果事务中同时写入。retry 请求、hold、escalate、超时或封顶 hold 均不得写 resolution。
- 隔离列共同构成 attempt 的独立当前投影，生命周期为 `none → frozen → none`；不得把 phase 或 Run status 作为其替代。初始 `none` 的 reason/三个隔离时间或事件字段全空；`frozen` 时 `isolation_reason/isolated_at_ms` 必填且两个 release 字段为空；解除后的 `none` 保留 reason/isolated 时间，并令 `isolation_released_at_ms/isolation_release_event_id` 同为非空，后者指向同事务写入的消失证据或强制清理安全事件。重复冻结/解除按 expected generation 与当前 isolation state 做 CAS。
- 仅 `process-group-verified` 的 Agent/version 可用“已登记 wrapper 与进程组均消失”作为自动解除证据。未验证或身份含糊时保持 `frozen` 并发 `startup_stall`；不得因 `phase=finished|orphaned` 或 Run `done|failed` 自动解除。
- `frozen` worktree 不得删除、移动、复用或作为新 attempt 的 `worktree_path`。新 attempt 创建事务必须断言目标路径没有 frozen 投影；operator 强制清理是唯一无消失证明的例外，必须先写高严重度安全事件，并明确标记不再提供单 writer 保证。
- partial unique index：每个 Run 最多一个 phase 属于 `pending/starting/spawning/running` 的 attempt。

### 5.4 `attempt_claims`

主键/外键 `(run_id, attempt_no)`，与 attempt 一一对应。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `run_id` | TEXT | NOT NULL，PK part |
| `attempt_no` | INTEGER | NOT NULL，PK part |
| `generation` | INTEGER | NOT NULL，与 attempt 相同 |
| `launch_operation_key` | TEXT | NOT NULL UNIQUE |
| `dispatch_id` | TEXT | NULL |
| `bootstrap_nonce_hash` | TEXT | NULL；launch dispatch 准备后非空 |
| `run_token_hash` | TEXT | NULL；与 bootstrap hash 同空同非空 |
| `wrapper_instance_id` | TEXT | NULL |
| `wrapper_session_hash` | TEXT | NULL |
| `spawn_permit_hash` | TEXT | NULL |
| `acquired_at_ms` | INTEGER | NULL |
| `permit_issued_at_ms` | INTEGER | NULL |
| `started_confirmed_at_ms` | INTEGER | NULL |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

约束：

- `dispatch_id/bootstrap_nonce_hash/run_token_hash` 三者同空同非空；只有 `PrepareLaunchDispatch` 可从全空 CAS 为全非空。
- 同一 wrapper instance 的 acquire 重放返回既有 session；不同 instance 不覆盖。
- permit 每 attempt/generation 最多一个，写入后不可替换。
- `spawning` 期间不得释放 claim 或递增 generation，除非受控终止已持久化消失证据。
- 换代以同一事务递增 attempt 与 claim generation，并作废旧 dispatch/session；不得只改其中一表。

### 5.5 `attempt_probes`

用于 `startup_stall` retry 的请求段/结果段，不新增 Run 状态。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL，和 attempt_no 组成 attempts 组合 FK |
| `attempt_no` | INTEGER | NOT NULL |
| `interrupt_id` | TEXT | NOT NULL FK interrupts |
| `state` | TEXT | `pending \| running \| succeeded \| failed \| superseded` |
| `expected_run_version` | INTEGER | NOT NULL |
| `expected_generation` | INTEGER | NOT NULL |
| `requested_by_event_id` | TEXT | NOT NULL FK events |
| `absence_evidence_json` | TEXT | NULL |
| `absence_evidence_digest` | TEXT | NULL |
| `created_at_ms` | INTEGER | NOT NULL |
| `started_at_ms` | INTEGER | NULL |
| `finished_at_ms` | INTEGER | NULL |

每个 Interrupt 最多一个 `pending/running` probe（partial unique index）。Supervisor tick 推进该本地持久 operation；进程观测和幂等信号发送不进入 outbox，崩溃后从 pending/running 继续。每次观测/信号结果写事件，probe 成功只表示可尝试 ADR-013 结果事务；不能在事务外关闭 Interrupt 或创建新 attempt。

## 6. Interrupt

### 6.1 `interrupts`

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL FK runs |
| `attempt_no` | INTEGER | NULL；非空时与 run_id 组成 attempts 组合 FK |
| `generation_key` | TEXT | NOT NULL UNIQUE |
| `reason` | TEXT | PRD 七种 reason |
| `severity` | TEXT | `low \| normal \| high \| critical` |
| `headline` | TEXT | NOT NULL，≤40 Unicode code points |
| `brief_markdown` | TEXT | NOT NULL |
| `options_json` | TEXT | NOT NULL；1..4 个互斥 option |
| `min_modality` | TEXT | `voice \| text \| visual` |
| `links_json` | TEXT | NOT NULL，默认 `[]` |
| `nonce` | TEXT | NOT NULL |
| `nonce_issued_at_ms` | INTEGER | NOT NULL；当前 nonce 成为有效值的时间 |
| `hold_max_duration_ms` | INTEGER | NOT NULL；创建时从 config snapshot 冻结的正毫秒数 |
| `approval_label_cutoff_position` | TEXT | NULL；正十进制远端位置；NULL 表示 nonce 初发/轮换后尚未完成 cutover |
| `version` | INTEGER | NOT NULL，初值 1；nonce/超时更新时 +1 |
| `status` | TEXT | `open \| closed` |
| `dispatch_state` | TEXT | `ready \| batched \| held \| probe_in_progress` |
| `expires_at_ms` | INTEGER | NOT NULL；自动 hold 后保留历史值，但 held 对象失去 expiry 扫描资格 |
| `expires_after_ms` | INTEGER | NOT NULL；创建时冻结 |
| `on_expire` | TEXT | `hold \| escalate \| auto_reject` |
| `on_max_escalations` | TEXT | `hold \| auto_reject`；创建时冻结 |
| `suggested_downgrade` | INTEGER | NOT NULL boolean；升级复用，不重读 T6/config |
| `channel_id` | TEXT | NULL；最终 Channel ID |
| `channel_snapshot_json` | TEXT | NULL；含 type/target_ref/capabilities/renderer，不含凭据 |
| `delivery` | TEXT | `immediate \| batch \| next_window \| held` |
| `next_dispatch_at_ms` | INTEGER | NULL；仅 `dispatch_state=ready` 时非空 |
| `held_reason` | TEXT | NULL 或 `manual \| no_compatible_channel \| channel_isolated \| batch_after_expiry \| quota_rejected \| critical_fuse \| expiry \| max_escalations` |
| `escalation_count` | INTEGER | NOT NULL，默认 0 |
| `max_escalations` | INTEGER | NOT NULL；创建时冻结配置 |
| `close_reason` | TEXT | NULL 或 `responded \| expired_auto_reject \| superseded_by_fact \| superseded_by_decision \| external_fact` |
| `closed_at_ms` | INTEGER | NULL |
| `charged_budget_entry_id` | TEXT | NULL UNIQUE FK budget_entries；仅实际 attention charge |
| `calibration_id` | TEXT | NULL UNIQUE FK calibration_entries；仅 Gate HITL 创建时不可变绑定 |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

创建、hold、expiry 与 escalation 均通过 `AdvanceInterrupt` 的 expected `version`/`nonce` CAS：创建冻结 expiry/on-max/hold/channel/delivery 快照；manual hold 只按 Command 规则重算 `expires_at_ms`，写 `dispatch_state=held, held_reason=manual, next_dispatch_at_ms=NULL`。自动 hold 也写 held/reason 并清空 next dispatch。expiry 扫描的唯一谓词是 `status=open AND dispatch_state != probe_in_progress AND (dispatch_state != held OR held_reason=manual) AND expires_at_ms <= now`；因此 timed manual hold 到期仍由 `AdvanceInterrupt` 扫描，其他 held 原因是终态调度结果而不重复命中。dispatch 扫描的唯一谓词是 `status=open AND dispatch_state=ready AND next_dispatch_at_ms <= now`。manual hold 到期 CAS 从 `(open, held, manual, expected_version, expected_nonce)` 进入冻结的 expiry 配方；成功后改为 `expiry`、升级后的 dispatch，或 close，绝不恢复旧 dispatch。每次升级原子地递增 `escalation_count`/`version`、轮换 nonce、写入 `nonce_issued_at_ms`、重算 expires/dispatch；`max_escalations=0` 直接走冻结的 on-max 去向。旧 tick、重启快照和重复请求的 CAS 失败不得产生任何 admission、delivery 或 outbox。

约束：

- `status=open` 时 close 字段为空；closed 时同为非空。
- `startup_stall` 禁止 `on_expire=auto_reject`。
- escalation 重推不新增 budget charge；关闭不退款。
- `charged_budget_entry_id` 只在首次发射实际写入 attention charge 时非空；`quota_batched` admission 的 Interrupt 必为 NULL，不能用零额或虚构 entry 充数。该列与 admission 的对应约束见 §6.3。
- 初始 nonce 的 `nonce_issued_at_ms` 等于 `created_at_ms`；每次 nonce 轮换必须同一 CAS 更新 `nonce_issued_at_ms` 并递增 version。非 nonce 更新不得改写该时间。
- `dispatch_state=held` 当且仅当 `held_reason` 非空且 `next_dispatch_at_ms` 为 NULL；`ready` 当且仅当 `held_reason` 为 NULL 且 `next_dispatch_at_ms` 非空；`batched|probe_in_progress` 时两者均为 NULL。`expires_after_ms`、`hold_max_duration_ms` 和非 NULL `next_dispatch_at_ms` 都是正整数毫秒。
- `probe_in_progress` 时拒绝新指令，但合法迟到事实仍可经仲裁入口提交。
- `EmitInterrupt`（创建）以及 `AdvanceInterrupt` / `ApplyCommandEvent`（轮换）是 nonce/cutoff 的唯一初始写者：它们在同一 expected-version/nonce CAS 中把 `approval_label_cutoff_position` 置 NULL、递增 version 并写新 nonce。随后 Forge 在事务外穷举目标的 label stream，唯一后继写者 `SetApprovalLabelCutoff(interrupt_id, expected_version, nonce, position)` 仅在该 Interrupt 仍 open、version 和 nonce 都匹配、cutoff 仍为 NULL 时以 CAS 写入最高证明位置；它不得写任何其他列。空 stream 写 sentinel `0`；无 position capability、未穷尽扫描、CAS=0、nonce/version 不符、Interrupt 已关闭或 cutoff 已非 NULL 均返回 persisted `unavailable|stale|already_cut_over`，不重扫、不轮换、不覆盖。扫描前崩溃只留下 NULL；扫描后、cutover 前崩溃重放同一 `(version,nonce,position)`，至多一次成功；cutover 后崩溃读取既有位置。所有 NULL 情况均令 Command label approval fail closed。

`close_reason` 只说明该 Interrupt 为何不再待决，不替代 attempt resolution 或 RPC disposition：

| close reason | 使用条件 |
|--------------|----------|
| `responded` | 当前合法指令本身完成关闭；包括显式 reject，或 retry probe 成功结果事务 |
| `expired_auto_reject` | 非 `startup_stall` 的超时策略合法自动拒绝 |
| `superseded_by_fact` | resolution 尚空时合法 started/result 事实先胜出并恢复监督 |
| `superseded_by_decision` | 另一已提交终局决定使本 Interrupt 失效；迟到 started/result 返回同名 disposition，但不得为了改 close reason 重写已关闭 Interrupt |
| `external_fact` | 已摄入 forge 等外部权威事实使问题失效 |

关闭原因与 `closed_at_ms` 在同一 CAS 中一次写入、之后不可改。关闭不退款；尤其 `superseded_by_fact` 仍代表真实发生过一次打扰。

### 6.2 `interrupt_deliveries`

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `delivery_id` | TEXT | NOT NULL UNIQUE；`interrupt:<interrupt_id>:<escalation_no>:<channel_id>`，Channel 单条 delivery 的不可变重放/指标身份 |
| `interrupt_id` | TEXT | NOT NULL FK interrupts |
| `surface` | TEXT | `forge_comment \| channel` |
| `channel_id` | TEXT | NULL；`channel` surface 必填 |
| `channel_snapshot_json` | TEXT | NULL；`channel` surface 必填，不得含凭据明文 |
| `interrupt_version` | INTEGER | NOT NULL；创建时冻结 |
| `nonce` | TEXT | NOT NULL；创建时冻结 |
| `escalation_no` | INTEGER | NOT NULL；创建时冻结 |
| `priority` | TEXT | `normal \| strong` |
| `operation_key` | TEXT | NOT NULL UNIQUE |
| `state` | TEXT | `pending \| delivered \| failed` |
| `attempt_count` | INTEGER | NOT NULL，默认 0 |
| `remote_ref` | TEXT | NULL |
| `last_error` | TEXT | NULL |
| `created_at_ms` | INTEGER | NOT NULL |
| `delivered_at_ms` | INTEGER | NULL |

投递执行仍由 outbox 驱动；本表只给 `ps/doctor` 一致查询面，不替代 outbox 历史。

### 6.3 `attention_admissions`、`attention_batches` 与成员

`attention_admissions` 是不可变的注意力准入事实，不以 `budget_entries` 代替。它使「扣到配额」「配额拒绝后合批」「critical 获准」和「critical 熔断」成为不同、可审计的结果。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `interrupt_id` | TEXT | NOT NULL FK interrupts |
| `run_id` | TEXT | NOT NULL FK runs；与 Interrupt 一致，供 per-Run fuse 查询 |
| `kind` | TEXT | `quota_charged \| quota_batched \| critical_admitted \| critical_fused` |
| `admission_key` | TEXT | NOT NULL UNIQUE；由 interrupt ID 与 `initial` 或首次 critical transition 组成 |
| `metric_identity` | TEXT | NOT NULL；恒为 `<interrupt_id>`，是跨 initial/critical admission 的唯一指标 lineage |
| `attention_charge_entry_id` | TEXT | NULL FK budget_entries；仅实际 charge 非空，可被升级 admission 复用 |
| `severity` | TEXT | 准入时的最终 severity |
| `quota_day` | TEXT | NULL；非 critical 初发为冻结 zone 的 `YYYY-MM-DD` |
| `day_timezone` | TEXT | NULL；规范化 IANA zone |
| `critical_source` | TEXT | NULL 或 `initial \| escalation` |
| `created_at_ms` | INTEGER | NOT NULL |

每个 Interrupt 最多一行初发 `quota_charged|quota_batched`，或一行初发 `critical_admitted|critical_fused`；首次由 high/normal 等升级至 critical 时，至多额外一行 `critical_admitted|critical_fused`，且 `critical_source=escalation`。`critical_admitted|critical_fused` 的 `critical_source` 必填，其他 kind 为 NULL。`quota_charged` 必须引用该 Interrupt 的实际 `charged_budget_entry_id`。`critical_admitted|critical_fused` 在初发有 charge 时引用该 charge，升级 admission 复用它；若初发为 `quota_batched`，两种 critical admission 的 `attention_charge_entry_id` 均为 NULL，且不得补造 charge。`quota_batched` 的两列均为 NULL。唯一 admission key 为 `<interrupt_id>:initial` 或 `<interrupt_id>:critical`，因此重复 tick、窗口边界重放只返回既有事实。两条合法 admission 共享 `metric_identity=<interrupt_id>`：已送达 `quota_batched` member 再升级为 `critical_admitted|critical_fused` 时，前者和成功 critical delivery 都保留各自 admission/delivery 审计，但指标只按该 stable lineage 计一次。两个 partial unique index `UNIQUE(interrupt_id) WHERE kind IN (quota_charged,quota_batched)` 与 `UNIQUE(interrupt_id) WHERE kind IN (critical_admitted,critical_fused)` 保证每 Interrupt 最多一次 initial decision 与最多一次 critical transition；INSERT 后禁止 UPDATE/DELETE。

`attention_batches` 是 versioned、可恢复的摘要对象，不把摘要伪造成 Interrupt/reason。它是 Channel identity 的唯一 batch authority；interrupt.md 不得定义 parallel batch tables, states, keys or prepare ports。`id` 是由其稳定 identity 生成的 batch ID，`operation_key` 由该 ID 固定导出；均不得使用当前时间、worker 或可变文本。V0 batch 禁止跨项目，且只能容纳同一已验证 Forge discussion target 的成员；不能形成该绑定的 candidate 不得入 batch。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK；daily 为 `daily:<project_id>:<zone>:<due_at_ms>:<channel_id>:<forge_kind>:<base64url(forge_host)>:<base64url(forge_project_key)>:<target_kind>:<base64url(target_id)>`；critical 为 `critical:<scope>:<scope_id>:<episode_admission_id>:<channel_id>:<forge_kind>:<base64url(forge_host)>:<base64url(forge_project_key)>:<target_kind>:<base64url(target_id)>`。`scope` 只可为 `global|run`，各 base64url 段无 padding，均不得使用 worker/时间以外的未冻结值 |
| `project_id` | TEXT | NOT NULL FK projects；所有 member 的 project 必须逐字节相同 |
| `forge_kind` / `forge_host` / `forge_project_key` | TEXT | NOT NULL；冻结的单一 Forge project identity |
| `forge_alert_target_kind` / `forge_alert_target_id` | TEXT | NOT NULL；同一 batch 所有 member 已验证且逐字节相同的 `issue|change` discussion target；唯一 failure alert 使用它 |
| `kind` | TEXT | `daily_summary \| critical_fuse` |
| `channel_id` / `channel_snapshot_json` | TEXT | NOT NULL；入批时冻结的 Channel ID / `{type,target_ref,capabilities,renderer}`，不得含 endpoint 或凭据 |
| `delivery_id` | TEXT | NOT NULL UNIQUE；`<batch_id>:publish:1`，sealed payload、operation 与 delivery projection 使用同一值 |
| `scope` / `scope_id` | TEXT | daily 为 `day` / `<zone>:<due_at_ms>`；critical 为 `global` / `global` 或 `run` / `<run_id>` |
| `quota_day` / `day_timezone` | TEXT | daily 成员可各自携带 quota day，batch 级 quota day 为 NULL；daily 的 day_timezone 必填；critical 为 NULL |
| `episode_admission_id` | TEXT | critical 必填，daily 为 NULL；首个 fused admission 标识本 episode |
| `due_at_ms` | INTEGER | NOT NULL；创建后不可改 |
| `state` | TEXT | `collecting \| sealed \| delivered \| cancelled` |
| `operation_key` | TEXT | NULL UNIQUE；sealed 时写 `attention-batch:<batch_id>:publish:1` |
| `payload_json` / `payload_digest` | TEXT | NULL；sealed 时写入，不可改 |
| `created_at_ms` / `sealed_at_ms` / `delivered_at_ms` | INTEGER | 创建必填；其余 NULL 或一次性写入 |

daily batch 的 `due_at_ms` 是 config §3.9 所定义的下一本地摘要时刻；同一规范化 zone、scheduled occurrence、冻结 Channel 和完整 Forge target（kind、host、project key、target kind、target id）只能有一个 batch，成员各自保留自己的 quota day。不同 Channel 或任一 target 字段不同必须建立不同 batch，绝不混合为一个 delivery。identity 的每个可变长字段都按 UTF-8 canonical bytes 独立 base64url 编码，因此不同 host/project key 不可能因分隔符或前缀造成碰撞。critical episode 在首个 `critical_fused` 时打开；窗口采用半开区间 `[created_at_ms, created_at_ms + window)`，因此 evidence 在 `due_at_ms` 恰好 expiry 时已不计数。episode 的 `due_at_ms` 是使当前 scope 的 admitted evidence 首次少于 limit 的最早 expiry。到期时必须在事务内重裁决：若仍饱和，则 sealing/保持旧 batch 后，以当前最早计数 evidence 的 admission ID 打开 successor episode，并为其计算新的 due_at；不得原地修改已创建 batch 的 due_at 或永久停在 collecting。只有重新裁决后低于 limit 才允许新 candidate 开新 episode。候选同时命中 global 和 per-Run 时只归 global batch；因此一个 Interrupt 绝不进入两批。

`attention_batch_members` 保存 batch 的不可重复**入批历史**与展示快照：主键 `(batch_id, interrupt_id)`，另有唯一 `member_key=<batch_id>:<interrupt_id>`；列为 `admission_id`（FK attention_admissions）、`channel_id`、`channel_snapshot_json`（两者逐字节等于 batch）、`delivery_id=<batch_id>:<interrupt_id>`（UNIQUE）、`interrupt_version`、`nonce`、`headline`、`reason`、`severity`、`links_json`、`options_json`、`joined_at_ms`、`excluded_at_ms`。入批时还必须断言 member Run 的 project 和已验证 discussion target 等于 batch 冻结列；同一 Interrupt 不能在同一 batch 有第二成员。

`attention_batch_member_authority` 是 collecting batch 唯一可变的**当前发送 authority**，主键同为 `(batch_id,interrupt_id)`，且必须引用已有 member；它保存当前 Interrupt 的 `interrupt_version`、`nonce`、`headline`、`reason`、`severity`、`links_json`、`options_json` 和 `updated_at_ms`。写入或更新只能发生在 `collecting`，并且所有展示/版本字段必须逐字节等于当时 open Interrupt；因此 repeated fuse 可在不改写 admission history 的前提下旋转 nonce。关闭或由事实 supersede 的事务在 batch 仍为 `collecting` 时必须把成员标为 excluded。到期的 `PrepareAttentionBatch` 在同一事务从 authority 重读其余成员的 `status=open AND version=authority.interrupt_version AND nonce=authority.nonce`，排除不再匹配者；有成员时冻结 sorted payload、写唯一 `channel_publish` operation 并把 batch 置 `sealed`，无成员则 `cancelled`。sealed/cancelled 后 member history、authority、payload、operation 和成功 delivery evidence 均不可改；这一定义以 sealing 为发送前的最后关闭排除边界，之后的关闭不会改写已经冻结的外部请求。

### 6.4 Command target、effect 与 outcome

`interrupt_command_targets` 是每个 Interrupt 恰好一行的不可变目标绑定：`interrupt_id` PK/FK interrupts、`publish_operation_id` NOT NULL UNIQUE FK outbox_operations（必须是初始 forge_comment operation）、`target_kind` NOT NULL (`issue|change`)、`target_id` NOT NULL、`created_at_ms` NOT NULL。目标必须与同一发布 operation 的 payload 逐字节相等。

`interrupt_command_effect_bindings` 是 reason owner 在 EmitInterrupt 五件事事务中创建的不可变一对一 binding：`interrupt_id` PK/FK、`reason`、`binding_schema_version=1`、`binding_json`、`binding_digest` UNIQUE、`created_at_ms`。`binding_json` 是 closed、tagged union，只允许这些 arms：`design_approval(task_spec_snapshot_id,run_id)`、`guardrail_violation(run_id,head_sha,rule_id,matched_paths_digest)`、`code_review(change_id,head_sha,review_policy_snapshot_digest)`、`agent_blocked(run_id,attempt_no,generation,report_id)`、`merge_conflict(change_id,head_sha,conflict_digest)`、`startup_stall(run_id,attempt_no,generation)`，以及以下两个不可互换的 `failure_review` arms：

- `failure_review_attempt(run_id,attempt_no,generation,retry_kind,change_id,head_sha,terminal_attempt_no,terminal_generation)`。两种 retry kind 都要求 `(run_id,attempt_no,generation)` 组合 FK 命中该 Interrupt 绑定的同 Run attempt。`retry_kind=gate_recheck` 时 `change_id`、`head_sha` 必填，terminal pair 必须均为 NULL，head 是 exact checked head，不能从当前 Change 查询补齐；`retry_kind=new_attempt` 时 change/head 必须均为 NULL，且 `terminal_attempt_no=attempt_no`、`terminal_generation=generation`。后者的同一组合 FK 还必须命中 `status=failed` 的 attempt；因此 Command 只能终结该 binding 所指的 failed generation，不能选择同 Run 的另一失败 attempt。
- `report_quota_failure_review(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id)`。其 `(run_id,daily_bucket_start_ms)` 必须命中 `report_quota_exhaustions`，end/security event 必须逐列相等；它没有 attempt、generation、retry、terminal 或 change/head 字段，只与 `failure_review` reason 和 `reject|hold` options 合法。

每个 arm 的 required/null 字段、组合 FK（包括 attempt/run、Change/head 和 exhaustion/security-event）及 reason/options 一致性由写端口及 CHECK 共同保证；`binding_digest=SHA-256(canonical_json(binding_json))`。拒绝 vectors 固定为：new-attempt 的错 Run、错 generation、non-`failed`、任一 terminal pair 与 binding pair 不等；attempt arm 出现 quota bucket/security-event 字段；quota arm 出现任一 attempt/generation/retry/terminal/change/head 字段；attempt arm 使用仅 quota 合法的 option 集，或 quota arm 使用 `retry|ask|approve`。未知 tag、字段缺失、额外字段、跨 reason、错配 FK 或重复 binding 一律回滚。
`command_effects` 是 `ApplyCommandEvent` 创建的不可变事实表：`id` PK、`interrupt_id` FK、`event_id` FK events、`effect_kind` (`one_time_exemption|human_review_approval`)、`run_id` FK、`change_id`、`head_sha`、`rule_id`、`matched_paths_digest`、`review_policy_snapshot_digest`、`created_at_ms`。CHECK 要求 exemption 恰有 run/head/rule/path digest，review approval 恰有 run/change/head/review-policy digest；各自 binding identity 唯一。Gate 下一份 snapshot 消费 effect，不修改历史 Gate 输入。

`command_event_outcomes` 解决 retry 的 initial/final 归属，是唯一允许 CAS 补全结局的可变投影：`id` PK、`event_key` UNIQUE、`initial_event_id` UNIQUE FK events、`final_event_id` NULL UNIQUE FK events、`state=pending|final`、`created_at_ms`、`finalized_at_ms` NULL。初始 retry 只可插入 `(state=pending,final_event_id=NULL,finalized_at_ms=NULL)`；唯一 finalizer 在同一事务插入 §6.1 指定的 final event（其 `final_for_event_id=initial_event_id`），再以 `WHERE state='pending' AND final_event_id IS NULL AND finalized_at_ms IS NULL` CAS 一次写入该 ID、`state=final` 和非 NULL finalization time。trigger 拒绝 INSERT final retry relation、pending 的 UPDATE 以外的任何 UPDATE、final 的任何 UPDATE、以及 final event/key/type 与 initial event key 不匹配。非 retry 只可同事务插入 `initial_event_id=final_event_id,state=final`。receipt 只链接 initial 也能唯一解析最终事件；任何同 key 异 digest 均为 contract violation。

### 6.5 Batch delivery 投影

每个 sealed batch 有一条 `batch_deliveries` 投影：`batch_id` PK/FK、`delivery_id` UNIQUE（逐字节等于 `<batch_id>:publish:1`）、`operation_key` UNIQUE、`state=pending|delivered|failed`、`attempt_count`、`remote_ref`、`last_error`、`created_at_ms`、`delivered_at_ms`。它与 `interrupt_deliveries` 同样只提供查询面。`CompleteOutboxAttempt` 成功时原子标记该投影、batch 和逐成员的 Ledger delivery；响应丢失的重放沿用同一 operation key 和 frozen payload，Channel 仍如实为 at-least-once。

### 6.5.1 `channel_failure_episodes`

每个 immutable `channel_publish` operation 恰有一行 durable episode，主键 `(subject_id,generation)`；单条 `subject_id` 等于 `interrupt_deliveries.delivery_id`，batch 等于 `batch_deliveries.delivery_id`，`generation` V0 固定为 `1`。列为 `subject_id`、`generation`、`consecutive_failures`（非负整数）、`state`（`open|alerted|ended_delivered|ended_failed`）、`last_error_class`、`alert_operation_key`（NULL 或唯一 `alert:channel_failure:<subject_id>:1`）、`created_at_ms`、`updated_at_ms`、`ended_at_ms`。只有 `ClaimOutboxOperation` 的 reclaim CAS 与 `CompleteOutboxAttempt` 可在各自 owner/lease CAS 同一事务中创建或更新它及 delivery/alert；reclaim 写入 `lease_expired` result 并计数，若 result 后达到 `max_attempts` 则同 CAS 终结 operation/delivery/episode（及按阈值 alert）而不建新 lease/attempt，否则才与新 lease 原子提交；已终结行不可重开。`sift ps`/`sift doctor` 直接查询此表及 outbox operation，不能由内存或 error 文本重建。

### 6.6 Channel batch and failure-episode exact vectors

The following are the sole V0 fixtures for Channel target and failure recovery. JSON is canonical under [`config.md` §4](config.md). `target_ref` is always a resolver handle, never a URL. `base64url("42")=NDI`.

**Single-target batch sealing and replay.** Two candidates `i-a` and `i-b` belong to `project-a`, have the same verified GitHub target `owner/project-a` issue `42`, and freeze `ops-slack` with `target_ref=secret_ref:SIFT_CHANNEL_OPS_SLACK`. Their identity is `daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI`; its sealed `channel_publish.body` is exactly (SHA-256 `ae3dba99e23daaf742abfeb13526da4afe0cd4ecb3b082471274e0cacfc5ac6e`):

```json
{"batch_id":"daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI","batch_kind":"daily_summary","channel":{"capabilities":["text"],"id":"ops-slack","renderer":"plain-v1","target_ref":"secret_ref:SIFT_CHANNEL_OPS_SLACK","type":"webhook"},"delivery_id":"daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1","delivery_kind":"attention_batch","due_at_ms":1785286800000,"forge_alert_target":{"forge_host":"github.com","forge_kind":"github","forge_project_key":"owner/project-a","target_id":"42","target_kind":"issue"},"members":[{"command_lines":[],"delivery_id":"daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:i-a","headline":"Agent 需要你澄清","interrupt_id":"i-a","interrupt_version":2,"links":[],"nonce":"n-a","options":[],"reason":"agent_blocked","severity":"high"},{"command_lines":[],"delivery_id":"daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:i-b","headline":"变更等待代码审阅","interrupt_id":"i-b","interrupt_version":2,"links":[],"nonce":"n-b","options":[],"reason":"code_review","severity":"high"}],"project_id":"project-a","rendered_text":"i-a: Agent 需要你澄清；i-b: 变更等待代码审阅","scope":"day","scope_id":"Asia/Shanghai:1785286800000"}
```

Concurrent inserts return this one batch/member set and `attention-batch:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1`; response-loss replay returns those same bytes and key. A candidate with another project or any different forge kind/host/project/target is not a member of this batch and cannot cause its alert to target another Run. It follows its normal single-delivery/held path. Thus cross-project batch is **not allowed**.

`attention_batches` persists the `forge_alert_target` fields above; on failure its one alert payload is closed as follows. `markdown` is rendered only from the persisted operation key, episode generation/count, persisted safe error class, delivery status, and fixed literal `Diagnostics: sift ps; sift doctor`; it is not supplied by a worker or reconstructed from current Run/Interrupt/config:

```json
{"forge_host":"github.com","forge_kind":"github","forge_project_key":"owner/project-a","markdown":"[sift alert:channel_failure:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1:1]\nChannel operation: attention-batch:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1\nEpisode generation: 1\nConsecutive failures: 3\nLatest error class: rate_limited\nStatus: generated_not_delivered\nDiagnostics: sift ps; sift doctor","purpose":"channel_failure","target_id":"42","target_kind":"issue"}
```

Its canonical payload digest is `ba180536811392f1bdf607d2afc27c42dde08d6b5d3a597e0838e705effd32f2`. No resolver result, endpoint or credential is present in either payload/digest. On secret rotation the replayed Channel operation retains `secret_ref:SIFT_CHANNEL_OPS_SLACK` and adapter resolution uses the rotated value. Missing access is `auth_or_capability`; an invalid resolved endpoint is `contract_violation`.

**Concurrent target and failure episode vectors.** Concurrent `i-a`, `i-b`, and `i-c` have the same `project-a`, `forge_kind=github`, `target_kind=issue`, `target_id=42`, zone/due/channel above. `i-a` and `i-b` freeze `forge_host=github.com`, `forge_project_key=owner/project-a`; `i-c` freezes `forge_host=git.example.com`, with that same project key. The sole result is two daily batches, never held/single-delivery: `i-a` and `i-b` are members of `daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI` with operation key `attention-batch:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1`; `i-c` is the sole member of `daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0LmV4YW1wbGUuY29t:b3duZXIvcHJvamVjdC1h:issue:NDI` with operation key `attention-batch:daily:project-a:Asia/Shanghai:1785286800000:ops-slack:github:Z2l0LmV4YW1wbGUuY29t:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1`. Thus neither batch or alert target can absorb the other host.

For critical, the complete identity grammar is the table grammar above. Exact fused inputs `i-d` and `i-e` have `scope=global`, `scope_id=global`, `episode_admission_id=admission-fused-01`, `channel_id=ops-slack`, and target `github/github.com/owner/project-a/issue/42`; both are members of `critical:global:global:admission-fused-01:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI`. Its operation key is `attention-batch:critical:global:global:admission-fused-01:ops-slack:github:Z2l0aHViLmNvbQ:b3duZXIvcHJvamVjdC1h:issue:NDI:publish:1`; a replay returns that same ID/key, not a new episode or batch.

`channel_failure_episodes` has one row per immutable Channel operation: `subject_id` is the single/batch delivery ID, `generation=1`, `consecutive_failures`, `state=open|alerted|ended_delivered|ended_failed`, `last_error_class`, `alert_operation_key` nullable, and timestamps. Its unique key is `(subject_id,generation)`. `ClaimOutboxOperation` reclaim and `CompleteOutboxAttempt` each write this row atomically with their immutable result and delivery projection.

| vector | required durable result |
|---|---|
| threshold `3`; first then second `transient` completion | count `1`, then `2`; state `open`; no alert |
| threshold `3`; lease expiry reclaim before threshold | reclaim commits immutable `lease_expired`, count increases exactly once, old completion is rejected, and new lease/attempt sees the persisted count |
| threshold `3`; lease expiry reclaim crosses threshold before attempt limit | reclaim atomically commits `lease_expired`, count `3`, `alerted`, the closed `forge_alert`, and the next lease/attempt; a restarted worker cannot create a second alert |
| `max_attempts=3`, threshold `3`; executing attempt `3` expires at reclaim | reclaim writes that attempt's immutable `retry/transient:lease_expired`, count `3`, `ended_failed`, failed operation/delivery, and exactly `alert:channel_failure:<subject_id>:1` in one CAS; it clears the lease and creates neither attempt `4` nor a new lease |
| that terminal expiry reclaim followed by the old attempt `3` completion | the stale completion is rejected and changes neither its existing `lease_expired` result, count, delivery, episode, nor alert; no new attempt is created |
| third `rate_limited` completion | count `3`; state `alerted`; exactly `alert:channel_failure:<subject_id>:1` and the frozen target payload above |
| two workers complete one lease | only owner-matching completion commits; rejected stale completion changes neither count nor alert |
| daemon restarts after count `2` | row reloads at `2`; next accepted failure creates the same threshold alert once |
| alert operation itself fails | delivery episode and its alert key remain unchanged; no `channel_failure` alert for the alert |
| `max_attempts=3`, threshold `3`, third transient result | Channel operation/delivery become `failed`, episode count `3` and `ended_failed`; alert exists and `ps`/`doctor` say generated, not delivered; no fourth Channel retry |
| success after two retryable failures (unbounded retry policy) | delivery is delivered; count resets to `0`; state `ended_delivered`; no later old completion may reopen it |

`ps`/`doctor` obtain count/state/error/alert key and alert operation state by joining these durable rows, never by reconstructing an in-memory episode.

## 7. Append-only 事件与幂等收据

### 7.1 `events`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `seq` | INTEGER | PK AUTOINCREMENT；仅作本库顺序 |
| `id` | TEXT | NOT NULL UNIQUE |
| `run_id` | TEXT | NULL FK runs |
| `attempt_no` | INTEGER | NULL |
| `project_id` | TEXT | NULL FK projects |
| `type` | TEXT | NOT NULL，稳定事件名 |
| `source` | TEXT | `system \| forge \| operator \| agent \| recovery` |
| `actor` | TEXT | NULL |
| `payload_schema_version` | INTEGER | NOT NULL |
| `payload_json` | TEXT | NOT NULL |
| `idempotency_key` | TEXT | NULL UNIQUE |
| `occurred_at_ms` | INTEGER | NOT NULL；外部事实发生时间，不明时等于 recorded |
| `recorded_at_ms` | INTEGER | NOT NULL |

`seq` 只表达本地提交顺序，不冒充 forge 全局顺序。业务连接上用 trigger 拒绝 UPDATE/DELETE。

### 7.2 `forge_cursors`

主键 `(project_id, stream)`。

| 列 | 类型 | 说明 |
|----|------|------|
| `project_id` | TEXT | NOT NULL，FK projects |
| `stream` | TEXT | NOT NULL；`issues \| issue_comments \| issue_labels \| changes \| change_comments \| checks` |
| `cursor` | TEXT | NULL |
| `etag` | TEXT | NULL |
| `since_ms` | INTEGER | NULL |
| `poll_mode` | TEXT | `idle \| active \| interrupt \| slow` |
| `next_poll_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

游标只在该批全部事件和投影持久化后推进。

### 7.3 `forge_event_receipts`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `project_id` | TEXT | NOT NULL FK projects |
| `forge_event_id` | TEXT | NOT NULL |
| `event_kind` | TEXT | NOT NULL；`forge_comment \| approval_label` |
| `event_key` | TEXT | NOT NULL；Command canonical 64-hex identity |
| `target_kind` | TEXT | `issue \| change` |
| `target_id` | TEXT | NOT NULL |
| `actor` | TEXT | NULL |
| `raw_digest` | TEXT | NOT NULL |
| `disposition` | TEXT | `accepted \| fact_observed \| ignored_untrusted_actor \| ignored_missing_actor` |
| `domain_event_id` | TEXT | NULL FK events；trusted Command 的 initial event |
| `command_outcome_id` | TEXT | NULL FK command_event_outcomes；trusted Command 的 initial/final resolution |
| `observed_at_ms` | INTEGER | NOT NULL |

唯一约束 `(project_id, event_kind, forge_event_id)` 与 `(project_id, event_key)`。迁移必须删除旧的 `(project_id, forge_event_id)` 约束；因此同项目不同 source 的相同远端 ID 不碰撞。重复投递返回既有 receipt，不新增“duplicate”行；同 key 的 digest、target、source 或 remote ID 不一致是 `contract_violation`。事实观测不因 actor 缺失被忽略；驱动事件必须有可信 actor 才能 `accepted`。

### 7.4 `report_receipts`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL，和 attempt_no 组成 attempts 组合 FK |
| `attempt_no` | INTEGER | NOT NULL |
| `report_key` | TEXT | NOT NULL |
| `report_kind` | TEXT | `progress \| goal \| blocker \| completed` |
| `payload_digest` | TEXT | NOT NULL |
| `event_id` | TEXT | NOT NULL FK events |
| `direct_interrupt_id` | TEXT | NULL UNIQUE FK interrupts；仅 blocker 直接致扰成功时非空 |
| `report_interrupt_charge_entry_id` | TEXT | NULL UNIQUE FK budget_entries；仅实际 Report 子配额 charge 时非空 |
| `received_at_ms` | INTEGER | NOT NULL |

`direct_interrupt_id` 与 `report_interrupt_charge_entry_id` 必须同空同非空；若非空，entry 必须为 `kind=report, scope=run, reason=report_agent_blocked`，且其 operation key 为 `report-interrupt-quota:<receipt_id>`。二者分别是 Report receipt 到直接 Interrupt 和 Report charge 的唯一 nullable 关联，不代表 attention charge。唯一约束 `(run_id, attempt_no, report_key)`。本表只记录已接受报告；重复请求先按该键返回既有 receipt，不消费令牌、不重复写事件；限流拒绝只记安全事件而不占用 report key。Agent 的 completed 只产生事件，不修改 Run 状态。

### 7.5 `report_quota_exhaustions`（不可变）

每个冻结 attention 日桶、每个 Run 最多一行；该表只记录 Report 子配额已满后的安全事实，不是 Agent report receipt，也不是普通领域 event。

| 列 | 类型 | 约束/说明 |
|---|---|---|
| `run_id` | TEXT | NOT NULL，FK runs |
| `daily_bucket_start_ms` | INTEGER | NOT NULL；与 `run_id` 组成主键 |
| `daily_bucket_end_ms` | INTEGER | NOT NULL；从冻结 IANA zone 的下一本地日 00:00 得出 |
| `security_event_id` | TEXT | NOT NULL UNIQUE FK events；`source=system` 的安全事件 |
| `failure_digest` | TEXT | NOT NULL；Report 专用 `failure_review` facts digest |
| `generation_key` | TEXT | NOT NULL UNIQUE；由 interrupt §5.1 独立 domain 派生 |
| `created_at_ms` | INTEGER | NOT NULL |

主键为 `(run_id,daily_bucket_start_ms)`，并以 `CHECK(daily_bucket_start_ms < daily_bucket_end_ms)` 固定半开桶。表及其索引均 append-only；数据库 trigger 拒绝 UPDATE/DELETE。`RecordReport` 以该主键的 INSERT 作为并发线性化点，不先查再写；冲突重放复用既有 `security_event_id`、digest 与 generation key。`failure_digest` 的 canonical facts、IANA/DST 边界和 `failure_review` link 以 [`interrupt.md` §5.1](interrupt.md) 为准。

### 7.6 `intake_items`（可变）

T1 pre-Run 摄入投影，回答「该 Issue 正在等什么、哪组问题已发、哪个回复可恢复处理」；语义与状态机见 [`brain.md` §7.3](brain.md)。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `project_id` | TEXT | NOT NULL FK projects |
| `forge_kind` | TEXT | `github \| gitlab` |
| `normalized_host` | TEXT | NOT NULL |
| `forge_project_key` | TEXT | NOT NULL |
| `issue_id` | TEXT | NOT NULL |
| `issue_url` | TEXT | NOT NULL |
| `issue_digest` | TEXT | NOT NULL |
| `force_hitl_before_start` | INTEGER | NOT NULL；可信 trigger actor 与不在该平台 operator allowlist 的 Issue 作者组合时为 1，后续 T2 不得清除 |
| `state` | TEXT | `pending_evaluation \| evaluating \| awaiting_clarification \| awaiting_duplicate_confirmation \| ready \| consumed` |
| `version` | INTEGER | NOT NULL，CAS |
| `latest_assessment_id` | TEXT | NULL；非空时与 id 组成 intake_assessments 组合 FK |
| `linked_run_id` | TEXT | NULL FK runs |
| `duplicate_candidate_run_id` | TEXT | NULL FK runs |
| `clarification_generation` | INTEGER | NOT NULL，默认 0 |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

唯一约束 `(forge_kind, normalized_host, forge_project_key, issue_id)`。约束：

- `consumed` 必须有 `linked_run_id`；其他 state 不得有。
- 两个 awaiting 状态必须有 `latest_assessment_id` 且 `clarification_generation >= 1`；`awaiting_duplicate_confirmation` 还必须有 `duplicate_candidate_run_id`。
- 遗留 `evaluating` 由 Brain call 的持久状态恢复（见 [`brain.md` §5](brain.md)），不靠内存超时猜测。
- `force_hitl_before_start=1` 在创建 Run 时投影到 `runs.hitl_before_start`；后续 T2 只能额外要求 HITL，不能清除该强制值。

### 7.6 `intake_assessments`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `intake_id` | TEXT | NOT NULL FK intake_items |
| `logical_call_id` | TEXT | NOT NULL FK brain_calls |
| `disposition` | TEXT | `ready \| needs_clarification \| possible_duplicate`，与 T1 output 同源 |
| `questions_json` | TEXT | NOT NULL |
| `possible_duplicate_run_id` | TEXT | NULL |
| `rationale` | TEXT | NOT NULL |
| `created_at_ms` | INTEGER | NOT NULL |

字段约束与 T1 output 互斥矩阵同源：ready 时 questions 空且 duplicate 为空；needs_clarification 时 questions 非空且 duplicate 为空；possible_duplicate 时 duplicate 非空且 questions 空。声明候选键 `(intake_id, id)` 供 intake_items 组合外键使用。

M1 冻结上述表与约束，并仅实现 fake 骨架链所需的 Forge Run/receipt 原子写入；`intake_items` 投影 CAS、`PersistIntakeBatch`/`PersistIntakeDecision` 完整写端口、回复 receipt 消费与旧 generation 审计在 [WBS M2 §2.3](../WBS.md) 交付。数据库 schema 存在不作为这些 M2 行为的实现证据。

## 8. Transactional outbox

### 8.1 `outbox_operations`

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `operation_key` | TEXT | NOT NULL UNIQUE，稳定业务键 |
| `kind` | TEXT | `forge_comment \| forge_labels \| create_change \| merge_change \| rerun_checks \| channel_publish \| launch_agent \| command_ack \| gate_re_evaluation \| forge_alert` |
| `run_id` | TEXT | NULL FK runs |
| `attempt_no` | INTEGER | NULL；非空时与 run_id 组成 attempts 组合 FK |
| `interrupt_id` | TEXT | NULL FK interrupts |
| `created_from_event_id` | TEXT | NULL FK events；Command/Gate operation 的创建事件，不可改 |
| `state` | TEXT | `pending \| executing \| retryable \| succeeded \| failed \| stale \| conflict` |
| `payload_schema_version` | INTEGER | NOT NULL |
| `payload_json` | TEXT | NOT NULL |
| `payload_digest` | TEXT | NOT NULL |
| `lease_owner` | TEXT | NULL，daemon boot/worker id |
| `lease_expires_at_ms` | INTEGER | NULL |
| `attempt_count` | INTEGER | NOT NULL，默认 0 |
| `next_attempt_at_ms` | INTEGER | NOT NULL |
| `remote_evidence_json` | TEXT | NULL |
| `remote_evidence_digest` | TEXT | NULL |
| `last_error_class` | TEXT | NULL 或 `transient \| rate_limited \| auth_or_capability \| contract_violation \| semantic_conflict` |
| `last_error_summary` | TEXT | NULL |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |
| `completed_at_ms` | INTEGER | NULL |

`executing` 必须同时有 lease owner/expiry；其他 state 不得保留有效 lease。terminal state 为 succeeded/failed/stale/conflict。payload 一经创建不可改；重试只更新执行字段。

`gate_re_evaluation` 唯一使用 `GateReEvaluationOperationV1` closed payload（`payload_schema_version=1`）：`{source_interrupt_id,source_command_event_id,source_run_version,run_id,attempt_no,generation,change_id,head_sha,gate_input_snapshot_id,gate_input_hash,gate_version,effect_binding_digest,operation_key}`。所有字段均为冻结身份；`source_run_version` 是 Command 已关闭 source Interrupt、将 Run 留在 `waiting_human` 后的版本。`(run_id,attempt_no,generation)` 必须命中 source Interrupt 的 attempt binding，且 `effect_binding_digest` 必须逐字节等于该 Interrupt 的 `interrupt_command_effect_bindings.binding_digest`；它的唯一来源是该 immutable binding，绝不由当前 Change 或 worker 猜测。`operation_key`（下称 `O`）精确为 `gate:<source_interrupt_id>:<head_sha>:reeval:1`。创建 operation 的同一 Command 事务必须令 source Interrupt 为 `closed/responded`，其 close event 就是 `source_command_event_id`；Complete **只断言**此闭合事实，绝不第二次 close 或另写 source-close event。

#### Gate re-evaluation terminal protocol v1

worker 只在事务外提交 `GateReEvaluationResultV1` 的 canonical JSON bytes：`{schema_version:1,kind,payload}`；unknown field、非 canonical bytes 或 digest 不符一律拒绝。Complete 先以 lease CAS 断言 operation 仍为 executing，且 Run 为 `(id=run_id,status=waiting_human,version=source_run_version)`、source Interrupt 为 `(id=source_interrupt_id,status=closed,close_reason=responded)`、其 Command close event 为 `source_command_event_id`。每个成功行均将 Run version 加一；任一断言、event、snapshot/evaluation、successor 或 operation state 写入失败即整体回滚。

`succeeded` payload 是闭合 `{gate_input_json,gate_input_hash,gate_version,verdict_json,verdict_digest}`：Complete 验证 input/hash/version、重算 verdict digest、insert-or-return snapshot、cache 与一条 evaluation，并在本事务分配其 ID。设该 ID 为 `E`、snapshot ID 为 `S`、result bytes digest 为 `R`。每个 succeeded terminal event 的 `idempotency_key` 的字节是 event key `K`，而 `id` 的字节是 `event:<K>`；二者不是同一列值。`source=system`、`payload_schema_version=1`，payload **逐字段**为 `{schema_version:1,operation_key:O,source_interrupt_id,source_command_event_id,gate_input_snapshot_id:S,gate_evaluation_id:E,gate_input_hash,gate_version,verdict_json,verdict_digest,result_digest:R}`。键按 canonical JSON 词典序写入；同一 `O` 的任意不同 bytes 或 successor 都是 contract violation。后续 operation 的 `created_from_event_id` 必须逐字节引用该 event `id`，即 `event:<K>`。

| Verdict | terminal event type / key | Run CAS 后态 | 唯一 successor |
|---|---|---|---|
| `failed/change_not_open` | `gate.reevaluation.failed.change_not_open` / `O:verdict:failed:change_not_open` | `failed(gate_verdict)` | 无 |
| `failed/hard_guardrail` | `gate.reevaluation.failed.hard_guardrail` / `O:verdict:failed:hard_guardrail` | `failed(gate_verdict)` | 无 |
| `wait_checks/checks_pending` | `gate.reevaluation.wait_checks.checks_pending` / `O:verdict:wait_checks:checks_pending` | `running(gate_waiting_checks)` | 无；只重观测该冻结 `change_id/head_sha` |
| `retry_checks/flaky_retry` | `gate.reevaluation.retry_checks.flaky_retry` / `O:verdict:retry_checks:flaky_retry` | `running(gate_retry_checks)` | `rerun_checks`：key `run:<run_id>:checks-rerun:<head_sha>:<check_run_id>:<retry_no>`，closed payload `{run_id,change_id,head_sha,check_run_id,retry_no,triage_source_digest,created_from_event_id}`，最后一项为 `event:<K>` |
| `hitl/checks_timeout` | `gate.reevaluation.hitl.checks_timeout` / `O:verdict:hitl:checks_timeout` | `waiting_human` | 下表 `failure_review` successor |
| `hitl/failure_review` | `gate.reevaluation.hitl.failure_review` / `O:verdict:hitl:failure_review` | `waiting_human` | 下表 `failure_review` successor |
| `hitl/guardrail_violation` | `gate.reevaluation.hitl.guardrail_violation` / `O:verdict:hitl:guardrail_violation` | `waiting_human` | 下表 `guardrail_violation` successor |
| `hitl/code_review` | `gate.reevaluation.hitl.code_review` / `O:verdict:hitl:code_review` | `waiting_human` | 下表 `code_review` successor |
| `hitl/merge_conflict` | `gate.reevaluation.hitl.merge_conflict` / `O:verdict:hitl:merge_conflict` | `waiting_human` | 下表 `merge_conflict` successor |
| `hitl/mergeability_unknown` | `gate.reevaluation.hitl.mergeability_unknown` / `O:verdict:hitl:mergeability_unknown` | `waiting_human` | 下表 `failure_review` successor |
| `hitl/input_unknown` | `gate.reevaluation.hitl.input_unknown` / `O:verdict:hitl:input_unknown` | `waiting_human` | 下表 `failure_review` successor |
| `ready/merge` | `gate.reevaluation.ready.merge` / `O:verdict:ready:merge` | `running(gate_merge_requested)` | `merge_change`：key `run:<run_id>:merge:<head_sha>`，closed payload `{run_id,change_id,expected_head_sha:head_sha,gate_input_snapshot_id:S,gate_evaluation_id:E,verdict_digest,created_from_event_id:"event:<K>"}` |
| `ready/no_auto_merge` | `gate.reevaluation.ready.no_auto_merge` / `O:verdict:ready:no_auto_merge` | `succeeded(gate_passed_no_auto_merge)` | 无 |

`triage_source_digest` is SHA-256 of the canonical frozen `GateInputV1.checks.triage.source`; `check_run_id` and `retry_no` are the verdict payload values. The normal outbox payload envelope still supplies its schema version and digest. Thus no successor reads current Change, checks, review, policy, Run version, or source Interrupt to fill a field.

The `ready/merge` `merge_change` successor payload carries the closed Gate-provenance fields listed above plus the two routing/execution fields the wired `MergeWorker` consumes (`internal/forgeworker/merge.go`): `project_id`, which drives the per-project outbox claim filter (`json_extract(payload_json,'$.project_id')`), and `method`, the Forge merge method frozen from the Gate authorization (the production reconciler uses `merge`). Both are frozen identity/authorization values, never read from current Change or Run state, so the successor remains closed.

All HITL successors use one closed `GateReEvaluationInterruptV1` input to `EmitInterrupt`: `{run_id,attempt_no,generation,reason,facts,binding_json,binding_digest,generation_key,source_interrupt_id,created_from_event_id}`. `created_from_event_id` is the succeeded event `id` bytes, `event:<K>`; `binding_digest=SHA-256(canonical_json(binding_json))`; the emitter must persist precisely that binding and may not derive another key/fact. `run_id` remains an input identity, but is not duplicated inside the `code_review` or `merge_conflict` binding arm. `failure_digest=SHA-256(canonical_json(facts))`; its generation key is the `failure_review` preimage in [`interrupt.md` §5](interrupt.md#5-生成键与发布) using frozen `(run_id,attempt_no,generation,failure_digest)`.

| successor | canonical facts and binding |
|---|---|
| `checks_timeout` | facts `{failure_class:"checks_timeout",failure_evidence_ref:<verdict.external_url>,recommended_action:"retry"}`; binding `failure_review_attempt(run_id,attempt_no,generation,gate_recheck,change_id,head_sha,NULL,NULL)` |
| `failure_review` | facts `{failure_class:"checks_"+verdict.classification,failure_evidence_ref:<verdict.external_url>,recommended_action:"retry"}`; same binding. `classification` is the closed Gate enum, so concatenation is literal ASCII, not prose. |
| `mergeability_unknown` | facts `{failure_class:"mergeability_unknown",failure_evidence_ref:"sift://event/event:<K>",recommended_action:"retry"}`; same binding. |
| `input_unknown` | facts `{failure_class:"gate_input_unknown:"+verdict.field,failure_evidence_ref:"sift://event/event:<K>",recommended_action:"retry"}`; same binding. `field` is the closed Gate payload value and is encoded as one UTF-8 string. |
| `guardrail_violation` | facts `{rule_id:verdict.rule_id,impact_scope:"matched_paths:"+verdict.matched_paths_digest,recommended_action:"approve",policy_evidence_ref:"sift://event/event:<K>"}`; binding `{run_id,head_sha,rule_id,matched_paths_digest}`; generation is the existing guardrail preimage with the snapshot `effective_policy_hash`, verdict `rule_id` and `matched_paths_digest`. |
| `code_review` | facts `{change_ref:"sift://change/"+change_id,head_sha,review_requirement:verdict.review_policy,recommended_action:"approve",diff_ref:"sift://event/event:<K>"}`; binding `{change_id,head_sha,review_policy_snapshot_digest:SHA-256(canonical_json({gate_input_hash,review_policy:verdict.review_policy}))}`; generation is the existing code-review `(change_id,head_sha)` preimage. The binding is exactly the `code_review(change_id,head_sha,review_policy_snapshot_digest)` arm in §6.4; `run_id` is carried only by the closed successor input and Interrupt row. |
| `merge_conflict` | let `conflict_digest=SHA-256(canonical_json({change_id,head_sha,mergeability:"conflicting"}))`; facts `{change_ref:"sift://change/"+change_id,head_sha,conflict_summary:"mergeability=conflicting",recommended_action:"retry",conflict_evidence_ref:"sift://event/event:<K>"}`; binding `{change_id,head_sha,conflict_digest}`; generation is the existing merge-conflict preimage. The binding is exactly the `merge_conflict(change_id,head_sha,conflict_digest)` arm in §6.4; `run_id` is carried only by the closed successor input and Interrupt row. |

`failed` has no VerdictV1. Its closed payload is `{failure_class,failure_evidence}`; `failure_class` is exactly one of `forge_read_failed | gate_input_assembly_failed | gate_contract_failed`, and `failure_evidence` is the corresponding closed object: `{stage:"get_change|get_checks|get_reviews",error_class:"transient|rate_limited|auth_or_capability",evidence_digest}`; `{code:"paths_incomplete|schema_invalid",field}`; or `{code:"input_hash_mismatch|verdict_digest_mismatch|verdict_schema_invalid"}`. Complete derives `R=SHA-256(result bytes)`, inserts type `gate.reevaluation.failed` at key `O:failed` with ID `event:O:failed` and exact payload `{schema_version:1,operation_key:O,result_digest:R,failure_class,failure_evidence}`, then derives facts `{failure_class,failure_evidence_ref:"sift://event/event:O:failed",recommended_action:"retry"}`, its failure digest/key, and the same frozen `failure_review_attempt(...gate_recheck...)` binding. Run remains `waiting_human` but version increments; the source close remains the asserted Command close.

Exact failed vectors (all JSON shown is canonical): with `run_id=run-01`, `attempt_no=1`, `generation=2`, `source_interrupt_id=int-01`, `change_id=change-01`, and `head_sha=0123456789abcdef0123456789abcdef01234567`, `O=gate:int-01:0123456789abcdef0123456789abcdef01234567:reeval:1`.

1. `forge_read_failed` result bytes are `{"kind":"failed","payload":{"failure_class":"forge_read_failed","failure_evidence":{"error_class":"transient","evidence_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","stage":"get_checks"}},"schema_version":1}` and `R=0b7d2e6f44608d3e2a03e92a41dbb95dcfe37c90dc11da883057afc23a655659`. Its event payload is `{"failure_class":"forge_read_failed","failure_evidence":{"error_class":"transient","evidence_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","stage":"get_checks"},"operation_key":"gate:int-01:0123456789abcdef0123456789abcdef01234567:reeval:1","result_digest":"0b7d2e6f44608d3e2a03e92a41dbb95dcfe37c90dc11da883057afc23a655659","schema_version":1}`; facts digest is `d669a14a2199b5531fcd0605809b740d663678a84405e5657cb167c07f10e782`, generation key is `57aea2d72c204e3014319d9f92667f2f7149eb9db2f109687d3da45d853ac005`, and binding digest is `5aeed3917cc4510f2dc72a9cde6e997dd878608fb5c4a1bed36066de1e7490d3`.
2. `gate_contract_failed` result bytes are `{"kind":"failed","payload":{"failure_class":"gate_contract_failed","failure_evidence":{"code":"verdict_digest_mismatch"}},"schema_version":1}` and `R=d5a8c1706563ff4ce16fa5419591fdbc56b7d1fd2a942ac644ea78a1f0fac978`. Its event payload is `{"failure_class":"gate_contract_failed","failure_evidence":{"code":"verdict_digest_mismatch"},"operation_key":"gate:int-01:0123456789abcdef0123456789abcdef01234567:reeval:1","result_digest":"d5a8c1706563ff4ce16fa5419591fdbc56b7d1fd2a942ac644ea78a1f0fac978","schema_version":1}`; facts digest is `e727d7d76024fa116c433eac116bff9cab030e63c4e9d27e007f61311c1c1632` and generation key is `0970ad59401f3acaf2fbfc630ea8a69137097a47197f99e44a75f1f935e407ae`; the binding bytes/digest are the same first vector.

`conflict` is closed `{replacement_head_sha,replacement_input_json,replacement_input_hash,replacement_gate_version}`. All fields are nonempty; the replacement head is Forge/head-bound, differs from `head_sha`, and `replacement_input_json` is the complete canonical [`GateInputV1`](gate.md#22-gateinputv1-闭合形态) for that head. `replacement_input_hash=SHA-256(replacement_input_json)` and `replacement_gate_version` is the Gate implementation version that will evaluate that input. Complete rejects noncanonical JSON, an invalid input, hash/version mismatch, `identity.run_id != run_id`, `identity.change_id != change_id`, or `change.head_sha != replacement_head_sha`; each is converted to the closed failed arm above. It writes type `gate.reevaluation.conflict`, key `O:conflict`, payload `{schema_version:1,operation_key:O,replacement_head_sha,replacement_input_hash,replacement_gate_version,result_digest}`.

In the same lease/CAS transaction, Complete obtains `S` by `gate_input_snapshots` insert-or-return on `replacement_input_hash`: an existing row is reusable only when its `canonical_json`, schema version and every projected §10.2 field equal the decoded input; otherwise it is a contract violation. If absent, it inserts that input and its required T3/T5 links using the normal ID allocator, and uses that newly stored row ID. Thus `gate_input_snapshot_id=S` is the sole transaction-assigned/reused value, never a worker-provided recipe or magic byte. It verifies the original operation's `effect_binding_digest` again against the source Interrupt binding and copies that exact digest into the replacement; it does not derive a new binding from replacement facts. The replacement operation's `gate_version` is exactly `replacement_gate_version`, and its input hash is exactly `replacement_input_hash`. Source identity fields retain their original values, `head_sha` is `replacement_head_sha`, and `source_run_version` is the **post-CAS** Run version. The new operation is `gate:<source_interrupt_id>:<replacement_head_sha>:reeval:1`, so it cannot self-cycle. The terminal event, Run version increment, snapshot insert/reuse and complete successor operation are one transaction; a conflict on any unique insert returns the existing byte-identical terminal result or fails as a contract violation.

Exact continuous-Complete vector (all JSON canonical): initial operation has `source_interrupt_id=int-01`, `source_command_event_id=event:gate:int-01:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:reeval:1`, `source_run_version=7`, `run_id=run-01`, `attempt_no=1`, `generation=2`, `change_id=change-01`, `head_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, `gate_input_snapshot_id=00000000000000000000000000000001`, `gate_input_hash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, `gate_version=gate/v1`, `effect_binding_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, and `operation_key=gate:int-01:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:reeval:1`; the source binding has that same digest. Before the first Complete, terminal fallback T3 call `t3-call-01` and its matching §10.2 link already exist, and §10.2 contains the replacement input row below with `id=00000000000000000000000000000002` (so insert-or-return deterministically reuses it). Its complete canonical `GateInputV1` bytes are `{"certification_rules_version":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","certification_version":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","change":{"additions":1,"base_ref":"main","changed_paths":["docs/readme.md"],"deletions":0,"files_changed":1,"head_ref":"sift/run-01","head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","is_draft":false,"mergeability":"mergeable","paths_complete":true,"review_state":"approved","state":"open"},"checks":{"conclusion":"success","external_url":"https://ci.example/runs/1","failed_jobs":[],"flaky_retries_used":0,"observed_at_ms":null,"pending_started_at_ms":null,"pending_timed_out":null,"triage":null},"effective_policy":{"auto_merge":false,"checks_pending_timeout_ms":3600000,"flaky_retry_limit":1,"protected_paths":{"hard":[".github/workflows/**",".gitlab-ci.yml",".sift/**"],"soft":[],"soft_exceptions":[]},"review_policy":"never","risky_review_threshold":1,"schema_version":1},"effective_policy_hash":"70cc93e283eaef9d52958230d0f5f785494c38cd245d9897d6ac51d8f586bb4f","identity":{"change_id":"change-01","project_id":"project-01","run_id":"run-01","task_kind":"docs"},"one_time_exemptions":[],"risk":{"rationale":"fallback","risk_points":["T3 unavailable; deterministic high-risk fallback"],"risk_score":100,"source":{"kind":"fallback","logical_call_id":"t3-call-01","reason":"provider_disabled","version":"T3/fallback/v1"}},"schema_version":1}`; its SHA-256 is `ac43ab23e60345f43df0305a58e37d58ae6644cbe2cb92619580be353057f104` (and the nested effective-policy hash is `70cc93e283eaef9d52958230d0f5f785494c38cd245d9897d6ac51d8f586bb4f`). A conflict Complete CASes Run version `7→8` and accepts result bytes `{"kind":"conflict","payload":{"replacement_gate_version":"gate/v1","replacement_head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","replacement_input_hash":"ac43ab23e60345f43df0305a58e37d58ae6644cbe2cb92619580be353057f104","replacement_input_json":"{\"certification_rules_version\":\"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\",\"certification_version\":\"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\",\"change\":{\"additions\":1,\"base_ref\":\"main\",\"changed_paths\":[\"docs/readme.md\"],\"deletions\":0,\"files_changed\":1,\"head_ref\":\"sift/run-01\",\"head_sha\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"is_draft\":false,\"mergeability\":\"mergeable\",\"paths_complete\":true,\"review_state\":\"approved\",\"state\":\"open\"},\"checks\":{\"conclusion\":\"success\",\"external_url\":\"https://ci.example/runs/1\",\"failed_jobs\":[],\"flaky_retries_used\":0,\"observed_at_ms\":null,\"pending_started_at_ms\":null,\"pending_timed_out\":null,\"triage\":null},\"effective_policy\":{\"auto_merge\":false,\"checks_pending_timeout_ms\":3600000,\"flaky_retry_limit\":1,\"protected_paths\":{\"hard\":[\".github/workflows/**\",\".gitlab-ci.yml\",\".sift/**\"],\"soft\":[],\"soft_exceptions\":[]},\"review_policy\":\"never\",\"risky_review_threshold\":1,\"schema_version\":1},\"effective_policy_hash\":\"70cc93e283eaef9d52958230d0f5f785494c38cd245d9897d6ac51d8f586bb4f\",\"identity\":{\"change_id\":\"change-01\",\"project_id\":\"project-01\",\"run_id\":\"run-01\",\"task_kind\":\"docs\"},\"one_time_exemptions\":[],\"risk\":{\"rationale\":\"fallback\",\"risk_points\":[\"T3 unavailable; deterministic high-risk fallback\"],\"risk_score\":100,\"source\":{\"kind\":\"fallback\",\"logical_call_id\":\"t3-call-01\",\"reason\":\"provider_disabled\",\"version\":\"T3/fallback/v1\"}},\"schema_version\":1}"},"schema_version":1}` (result digest `86ee95a7748f9a38cbae9851f5092f5a70a2254469bfbb7ef2793fe2b8c76b08`). It creates exactly this replacement operation payload: `{"attempt_no":1,"change_id":"change-01","effect_binding_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","gate_input_hash":"ac43ab23e60345f43df0305a58e37d58ae6644cbe2cb92619580be353057f104","gate_input_snapshot_id":"00000000000000000000000000000002","gate_version":"gate/v1","generation":2,"head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","operation_key":"gate:int-01:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb:reeval:1","run_id":"run-01","source_command_event_id":"event:gate:int-01:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:reeval:1","source_interrupt_id":"int-01","source_run_version":8}`.

The second Complete claims that exact successor, CASes `(run_id=run-01,status=waiting_human,version=8)→version=9`, and accepts the legal closed failed result `{"kind":"failed","payload":{"failure_class":"gate_contract_failed","failure_evidence":{"code":"verdict_digest_mismatch"}},"schema_version":1}` (result digest `d5a8c1706563ff4ce16fa5419591fdbc56b7d1fd2a942ac644ea78a1f0fac978`). Per the failed matrix it writes only the successor's `O:failed` event and frozen `failure_review_attempt(...,gate_recheck,change-01,bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb,NULL,NULL)` successor, retaining `waiting_human`; reusing `source_run_version=7` is rejected. This establishes the actual continuous Run version sequence `7→8→9` across two legal Complete transactions, rather than only the second operation's CAS precondition.

claim/replay returns the existing terminal result and every terminal event/evaluation/successor above; a lost lease rolls back. Worker code may not call `EmitInterrupt` or `RecordGateEvaluation`. Crash vectors before claim, after lease, before/after external reads, before complete commit, and after complete commit must prove at most one listed event, evaluation, Interrupt or operation; `rerun_checks` still uses §8.5 request-start/reclaim rules.

### 8.2 `check_rerun_consumptions`（不可变）

Gate 创建 `rerun_checks` operation 的同一事务插入一行，防止崩溃恢复或并发 Gate evaluation 重复消费 flaky retry 额度。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `run_id` | TEXT | NOT NULL FK runs |
| `head_sha` | TEXT | NOT NULL |
| `check_run_id` | TEXT | NOT NULL |
| `retry_no` | INTEGER | NOT NULL，>=1 |
| `operation_id` | TEXT | NOT NULL UNIQUE FK outbox_operations |
| `created_at_ms` | INTEGER | NOT NULL |

主键为 `(run_id, head_sha, check_run_id, retry_no)`。同一 head/check 的已消费数由这些不可变行计数；取消、conflict 或 CI 后续成功都不归还额度。

### 8.3 `outbox_attempts`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `operation_id` | TEXT | NOT NULL FK outbox_operations |
| `attempt_no` | INTEGER | NOT NULL |
| `worker_id` | TEXT | NOT NULL |
| `started_at_ms` | INTEGER | NOT NULL |

唯一约束 `(operation_id, attempt_no)`。claim operation 时插入，之后不更新。

### 8.4 `outbox_attempt_results`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `attempt_id` | TEXT | PK、FK outbox_attempts |
| `finished_at_ms` | INTEGER | NOT NULL |
| `outcome` | TEXT | `success \| retry \| failed \| stale \| conflict` |
| `error_class` | TEXT | NULL 或 `transient \| rate_limited \| auth_or_capability \| contract_violation \| semantic_conflict` |
| `error_summary` | TEXT | NULL |
| `evidence_digest` | TEXT | NULL |

每个 attempt 最多一个结果。operation claim 用 CAS：pending/retryable 且到期，或 executing 且 lease 已过期的行可被认领；后者先为旧 attempt 插入 `outcome=retry,error_class=transient,error_summary=lease_expired` result。若该 result 达到 `max_attempts`，同一 CAS 将 operation 终结为 `failed`、清除 lease，且不创建新 attempt；未达限才替换 lease owner并创建新 attempt。旧 owner 的 complete 随即 CAS 失败。

`launch_agent` 是特殊 claim：`ClaimOutboxOperation(current_boot_id, ...)` 必须在同一 SQL 语句/事务中证明 `daemon_boots.id=current_boot_id AND recovery_completed_at_ms IS NOT NULL`；先在应用内检查再 claim 不成立。恢复扫描可把旧启动 operation 收敛为 pending/retryable/succeeded/stale/conflict，但在屏障落定前同样不得取得执行 lease。daemon 在 `CompleteStartupRecovery` 提交后才启动 launch worker；崩溃产生的新 boot 因新行屏障为空而重新关闭该入口。外部动作结束后，同一事务插入 result 并 CAS 更新 operation；旧 lease owner 的结果整笔拒绝。

### 8.5 `outbox_attempt_request_starts`（不可变，`rerun_checks` 专用）

这是远端 `RerunCheck` 已可能发生的唯一 durable boundary；它不以 worker 内存、日志或 lease 推断。

| 列 | 类型 | 约束/说明 |
|---|---|---|
| `attempt_id` | TEXT | PK、FK `outbox_attempts` |
| `started_at_ms` | INTEGER | NOT NULL；写入前注入的时间 |

只允许 `outbox_operations.kind=rerun_checks` 的 attempt 插入一行；其他 kind 不得伪造该事实；该跨表约束与本表的 UPDATE/DELETE 禁止均由数据库 trigger 执行。`MarkOutboxAttemptRequestStarted(operationID, expectedLease, attemptID)` 在一个 `BEGIN IMMEDIATE` 中验证 operation 仍为 `executing` 且 owner/未过期 lease 和 attempt 均匹配，再插入本行；重复调用返回既有事实。该事务成功提交后 worker 才可调用 `RerunCheck`，提交失败则不得调用。

`ClaimOutboxOperation` 对 `rerun_checks` 的规则替代通用 reclaim：pending/retryable 到期可正常 claim；executing lease 到期时，若旧 attempt **没有**本表记录，事务为旧 attempt 插入 `retry/transient:lease_expired` result；若该 result 达到 `max_attempts`，同一 CAS 将 operation 终结为 `failed`、清除 lease，且不创建新 attempt，否则才创建新 attempt。若旧 attempt已有本表记录，事务必须为旧 attempt 插入（或确认既有）`conflict` result，将 operation CAS 为 `conflict`、清除 lease，并同事务创建/去重 `failure_review` Interrupt；不得创建新 attempt。`CompleteOutboxAttempt` 对已有本表记录的 `rerun_checks` 只接受 `success` 或 `conflict`，不接受 `retry`；调用结果不明、调用后错误或 complete CAS 丢失都走 `conflict`。因此调用前崩溃可重试，request-start 后的崩溃或 lease expiry 永远不会导致第二次远端调用。

## 9. 预算

### 9.1 `budget_counters`

主键 `(kind, scope, scope_id, bucket_start_ms)`。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `kind` | TEXT | NOT NULL；`token \| forge_api \| attention \| report` |
| `scope` | TEXT | NOT NULL；`global \| project \| run \| severity` |
| `scope_id` | TEXT | NOT NULL；global 使用 `global` |
| `bucket_start_ms` | INTEGER | NOT NULL，PK part |
| `bucket_end_ms` | INTEGER | NOT NULL |
| `limit_value` | INTEGER | NOT NULL |
| `consumed_value` | INTEGER | NOT NULL，默认 0 |
| `version` | INTEGER | NOT NULL，CAS |
| `updated_at_ms` | INTEGER | NOT NULL |

计费入口在同一事务执行 `consumed + amount <= limit` CAS。本表只承载日/小时固定桶（token、forge API、非 critical 注意力）；注意力不可借支。对非 critical attention，CAS 零行后必须在同一稳定 admission key 下重读 counter：重读仍可容纳时是竞争，有限次重试；只有 `consumed + amount > limit` 才写 `quota_batched` admission。达到重试上限、SQLite busy 以外的错误、事务错误或无法重读均整笔回滚，绝不写 quota-batched 作为存储故障的替代结局。

**token 是该通用 CAS 的唯一例外**（语义见 [`brain.md` §6](brain.md)）：token 采用「发起物理 attempt 前阈值检查 `consumed >= limit` 即拒发 + attempt 完成后按实际 usage 全额 post-charge」，post-charge 允许 counter 单次越过 limit，不执行 `consumed + amount <= limit` 预扣。token 桶为 UTC 自然日，按 attempt **开始时**冻结的 bucket 收费；usage 总和为 0 时不写 budget entry，usage 缺失不收费。收费幂等由唯一 operation key 保证，重复 key 返回原 charge。

### 9.2 `rate_limit_buckets`

Report 的 `events_per_minute + burst` 使用持久化令牌桶，不用固定窗口近似。主键 `(kind, scope_id)`。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `kind` | TEXT | NOT NULL；V0 为 `report` |
| `scope_id` | TEXT | NOT NULL；`run:<run_id>:attempt:<attempt_no>` |
| `capacity_units` | INTEGER | NOT NULL；来自 burst |
| `available_units` | INTEGER | NOT NULL，`0..capacity_units` |
| `refill_numerator` | INTEGER | NOT NULL；每周期补充 token 数 |
| `refill_period_ms` | INTEGER | NOT NULL |
| `refill_remainder` | INTEGER | NOT NULL，保存整数除法余数 |
| `last_refill_at_ms` | INTEGER | NOT NULL |
| `version` | INTEGER | NOT NULL，CAS |

补充计算只用整数：`elapsed_ms * refill_numerator + refill_remainder` 除以 `refill_period_ms`，商加入 available（封顶 capacity），余数写回。消费与 `report_receipts/events` 在同一事务完成。

### 9.3 `budget_entries`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `kind` | TEXT | 同 counters |
| `scope` | TEXT | 同 counters |
| `scope_id` | TEXT | NOT NULL |
| `bucket_start_ms` | INTEGER | NOT NULL |
| `amount` | INTEGER | NOT NULL，正整数 |
| `reason` | TEXT | NOT NULL |
| `run_id` | TEXT | NULL FK runs |
| `operation_key` | TEXT | NOT NULL UNIQUE；防重复收费 |
| `created_at_ms` | INTEGER | NOT NULL |

Interrupt 升级重推复用原 charge，不新增 entry；Interrupt 关闭不退款。非 critical 日配额 entry 使用 `kind=attention, scope=severity, scope_id=<severity>`；Report 致扰子配额使用 `kind=report, scope=run, scope_id=<run_id>`。critical 不写日配额 counter，但首次 critical 发射仍写 `kind=attention, scope=severity, scope_id=critical` entry，并令 `bucket_start_ms=created_at_ms`；high→critical 升级复用其原 entry。`budget_entries` 不反向保存 Interrupt FK；Interrupt 通过不可变 `charged_budget_entry_id` 指向实际 charge，避免循环外键。

critical fuse 的唯一权威计数（整数毫秒）是 `attention_admissions.kind=critical_admitted AND created_at_ms > now-window`，分别按全局和 `run_id` 查询；这正是 evidence 生命周期半开区间 `[created_at_ms, created_at_ms + window)`：`now=t+window-1ms` 计入 `t`，`now=t+window` 和 `t+window+1ms` 均不计入。`critical_fused` 是拒绝/episode 证据，绝不计入名额。`EmitInterrupt` 对初发 critical、`AdvanceInterrupt` 对升级首次 critical 各自在同一 CAS 事务中：检查两个窗口 → 至多插入一次 admission → 写/复用 charge、Interrupt/升级事件及所需 batch member。任何重放、旧 version 或已有 critical admission 都返回原事实，不重新占名额。

## 10. Brain、Gate、校准与 Ledger

这些表在 M1 建立稳定身份和不可变边界；T3–T7、Gate 与 Ledger 的字段扩展分别随对应 spec 迁移，但不得改变本节关联关系。

### 10.1 Brain 调用：`brain_call_counters` / `brain_calls` / `brain_attempts`

一次 logical call 拆为三表：mutable counter 负责发号，call 行承载冻结输入与一次性终结，attempt 行承载每个物理/preflight 结果。调用流程与恢复语义见 [`brain.md` §3/§5](brain.md)。

#### `brain_call_counters`（可变）

主键 `(scope, subject_key, touchpoint)`，三列均 NOT NULL。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `scope` | TEXT | `intake \| run \| aggregate` |
| `subject_key` | TEXT | NOT NULL |
| `touchpoint` | TEXT | `T1`..`T7` |
| `next_call_seq` | INTEGER | NOT NULL，正整数 |
| `version` | INTEGER | NOT NULL，CAS |
| `updated_at_ms` | INTEGER | NOT NULL |

`ReserveBrainCall` 在 `BEGIN IMMEDIATE` 中递增并以旧值插入 call 行，禁止 `max()+1`。

#### `brain_calls`（single-finalize）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK，logical_call_id |
| `scope` | TEXT | `intake \| run \| aggregate` |
| `subject_key` | TEXT | NOT NULL；该 scope 下的稳定调用主体 |
| `project_id` | TEXT | NULL FK projects |
| `run_id` | TEXT | NULL FK runs |
| `attempt_no` | INTEGER | NULL；与 run_id 组成 attempts 组合 FK |
| `touchpoint` | TEXT | `T1`..`T7` |
| `call_seq` | INTEGER | NOT NULL，正整数 |
| `prompt_version` | TEXT | NOT NULL |
| `output_schema_version` | INTEGER | NOT NULL |
| `input_json` | TEXT | NOT NULL |
| `input_digest` | TEXT | NOT NULL |
| `status` | TEXT | `running \| valid \| fallback` |
| `selected_attempt_no` | INTEGER | NULL；非空时与 id 组成 brain_attempts 组合 FK |
| `fallback_reason` | TEXT | NULL |
| `validated_output_json` | TEXT | NULL |
| `started_at_ms` | INTEGER | NOT NULL |
| `finished_at_ms` | INTEGER | NULL |

唯一约束 `(scope, subject_key, touchpoint, call_seq)`。约束：

- `running`：selected/fallback_reason/validated_output/finished 全空；`valid`：`selected_attempt_no` 非空且指向本 call 的 valid attempt、`validated_output_json` 与 `finished_at_ms` 非空、`fallback_reason` 为空；`fallback`：`fallback_reason` 与 `finished_at_ms` 非空、`selected_attempt_no` 为空。valid/fallback 为终态。
- 禁止 DELETE；trigger 只允许一次 `running → valid | fallback` 终结更新，身份、输入与 seq 列永不可改（见 §13）。
- 以下作用域规则必须落实为数据库 CHECK，而不只由调用方保证：
  - T1：`scope=intake`，subject 为规范化 forge Issue 摄入键；project 必填，run/attempt 为空。
  - T2：`scope=run`，run 必填、attempt 为空。
  - T3–T6：`scope=run`，run 必填；调用发生在具体 attempt 时 attempt 可填，否则为空。
  - T7：`scope=aggregate`，subject 严格使用 [`brain.md` §3](brain.md) 的 `aggregate:v1:...` grammar；project subject 的 `project_id` 必填且 base64url component 解码后逐字节相等，global subject 的 `project_id` 为空；run/attempt 均为空。
- `attempt_no` 非空时 `run_id` 必填且组合外键指向 attempts。Brain call 先于 Gate snapshot 终结且不可回写；输出实际参与 Gate 输入时通过 §10.2 的关联表连接。

#### `brain_attempts`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `logical_call_id` | TEXT | NOT NULL FK brain_calls |
| `provider_attempt` | INTEGER | NOT NULL |
| `outcome` | TEXT | `valid \| invalid_output \| provider_error \| fallback` |
| `provider_error_code` | TEXT | NULL 或 `timeout \| nonzero_exit \| output_too_large \| invalid_envelope \| usage_missing \| usage_invalid \| spawn_failed` |
| `request_digest` | TEXT | NOT NULL；写端口断言等于所属 call 的 `input_digest` |
| `raw_output_text` | TEXT | NULL；受 config `max_raw_output_bytes` 限制 |
| `raw_output_digest` | TEXT | NULL |
| `raw_output_bytes` | INTEGER | NULL |
| `raw_output_truncated` | INTEGER | NOT NULL boolean，默认 0 |
| `stderr_summary` | TEXT | NULL；去凭据后有界摘要 |
| `stderr_truncated` | INTEGER | NOT NULL boolean，默认 0 |
| `exit_code` | INTEGER | NULL |
| `input_tokens` | INTEGER | NULL；非空时非负 |
| `output_tokens` | INTEGER | NULL；非空时非负 |
| `started_at_ms` | INTEGER | NOT NULL |
| `finished_at_ms` | INTEGER | NOT NULL |

唯一约束 `(logical_call_id, provider_attempt)`，同时作为 `brain_calls(selected_attempt_no)` 组合外键目标。约束：

- `provider_attempt=0` 只能是 `outcome=fallback`，且 provider_error_code/raw/stderr/exit/token 全空；`1|2` 才表示已启动 provider。
- `outcome=provider_error` 必须有 `provider_error_code`；其他 outcome 不得有。
- `outcome=valid` 必须有非空 token、`raw_output_digest`；`output_too_large` 时 `raw_output_truncated=1` 且 digest/bytes 覆盖已读部分。
- attempt 行不存 `fallback_used`：单 attempt 失败与整 call 终结兜底是两层事实，后者在 `brain_calls.status/fallback_reason`。

#### T7 aggregate 调度证据与 cursor

`t7_replay_evidence` 是 offline replay 聚合的 append-only 输入，按 `(scope, project_id, task_kind, window_start_ms, window_end_ms)` 唯一冻结 dataset/gate version 与四项计数；project/global 身份互斥。调度器只把该表与当前不可变 certification revision、窗口内 `semantic_material` Ledger entry 组装为 [`brain.md` §13.1](brain.md) 输入，不读取当前 Gate candidate、open Interrupt 或可写 policy/context。

`t7_aggregate_call_bindings` 以 aggregate key 唯一绑定 scheduler-owned T7 logical call；它与 `brain_calls` 同事务创建，使崩溃后复用冻结 call/input 而不重调不确定 provider。terminal trace 后，valid 才经唯一 `SaveProposalDraft` 写 inert draft，fallback 不写；最后 append `t7_aggregate_completions`。三表均不可更新/删除，故 terminal→draft→completion 任一崩溃窗都可 insert-or-return 收敛。

### 10.2 `gate_input_snapshots`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `gate_input_hash` | TEXT | NOT NULL UNIQUE |
| `schema_version` | INTEGER | NOT NULL |
| `canonical_json` | TEXT | NOT NULL |
| `head_sha` | TEXT | NOT NULL |
| `effective_policy_hash` | TEXT | NOT NULL |
| `certification_rules_version` | TEXT | NOT NULL；算法、窗口与阈值的 config hash |
| `certification_version` | TEXT | NOT NULL；类别证据 revision，不能等同 rules version |
| `risk_source_version` | TEXT | NOT NULL |
| `created_at_ms` | INTEGER | NOT NULL |

hash 必须覆盖整份冻结输入；不得在 Gate 内读取快照外的当前值。

#### `brain_gate_input_links`（不可变）

Brain logical call 与 Gate snapshot 是多对多：同一 T3/T5 结果可在 head 不变而 Checks/review/policy 等事实变化时进入多份 snapshot，一份 failure snapshot 也可同时引用 T3 与 T5。不得把 snapshot FK 回写到已终结的 `brain_calls`。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `logical_call_id` | TEXT | NOT NULL FK brain_calls |
| `gate_input_snapshot_id` | TEXT | NOT NULL FK gate_input_snapshots |
| `touchpoint` | TEXT | `T3 \| T5`；必须等于 call.touchpoint |
| `created_at_ms` | INTEGER | NOT NULL |

主键 `(logical_call_id, gate_input_snapshot_id)`。`RecordGateEvaluation` 在创建/复用 snapshot 的同一事务按其 canonical source 插入或返回既有 link；被引用 call 必须已 terminal，且 logical ID、touchpoint、prompt/schema version、validated output 或 fallback source 与 snapshot 逐字段一致。没有实际进入 snapshot 的 call 不建 link，T1/T2/T4/T6/T7 不伪造关联。

### 10.3 `gate_evaluations`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL FK runs |
| `snapshot_id` | TEXT | NOT NULL FK gate_input_snapshots |
| `gate_version` | TEXT | NOT NULL |
| `verdict_json` | TEXT | NOT NULL |
| `verdict_digest` | TEXT | NOT NULL |
| `cache_hit` | INTEGER | NOT NULL boolean |
| `created_at_ms` | INTEGER | NOT NULL |

每次 Gate 调用都新增一行，包括 cache hit。

### 10.4 `gate_cache`

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `gate_input_hash` | TEXT | NOT NULL，PK part |
| `gate_version` | TEXT | NOT NULL，PK part |
| `snapshot_id` | TEXT | NOT NULL FK gate_input_snapshots |
| `verdict_json` | TEXT | NOT NULL |
| `verdict_digest` | TEXT | NOT NULL |
| `created_at_ms` | INTEGER | NOT NULL |

缓存条目、评估与回放必须引用同一 snapshot。相同 `(gate_input_hash, gate_version)` 只能 insert-or-return existing；既有 digest 与本次 verdict 不同是 contract violation，不得覆盖缓存。

### 10.5 `calibration_entries`（single-settle）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL FK runs |
| `gate_evaluation_id` | TEXT | NOT NULL UNIQUE FK gate_evaluations |
| `predicted_decision` | TEXT | NOT NULL；`allow \| block \| inconclusive` |
| `gate_sample_entry_id` | TEXT | NOT NULL UNIQUE FK ledger_entries；唯一校准特征事实 |
| `human_decision` | TEXT | NULL 或 `allow \| block`；仅二元 shadow 可补全 |
| `decision_source` | TEXT | NULL 或 `command \| manual_merge \| manual_close` |
| `gate_bypassed` | INTEGER | NOT NULL boolean，默认 0 |
| `predicted_at_ms` | INTEGER | NOT NULL |
| `decided_at_ms` | INTEGER | NULL |

`gate_sample_entry_id` 与 calibration 在 Gate 事务内预分配 ID 后互相引用；不得有 `features_json` 副本。人的结果只能由受限端口一次性补全；`inconclusive` 永不补全。二元补全必须有 `interrupts.calibration_id` 或 `external_decision_bindings` 的唯一不可变 binding，且与 Ledger entry、认证投影同事务。

### 10.6 `external_decision_bindings`（不可变）

手工 Forge merge/close 事实的因果绑定，禁止由消费者按时间猜测。

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `forge_fact_event_id` | TEXT | PK、FK events |
| `calibration_id` | TEXT | NOT NULL UNIQUE FK calibration_entries |
| `created_at_ms` | INTEGER | NOT NULL |

只有在观测事实时已有明确 Gate 因果身份才插入；不存在或歧义时不建行，人类动作仍可审计但不得结算。

### 10.7 `certifications`

主键 `(task_kind, certification_version)`；是不可变证据快照，不原地更新。当前读取面由下表唯一指针提供。

| 列 | 类型 | 说明 |
|----|------|------|
| `task_kind` | TEXT | `feature \| bug \| chore \| docs \| refactor` |
| `certification_rules_version` | TEXT | NOT NULL；算法、窗口与阈值版本 |
| `certification_version` | TEXT | NOT NULL；类别证据 revision |
| `window_start_ms` / `window_end_ms` | INTEGER | NOT NULL；半开 `[start,end)` 的冻结边界 |
| `total_samples` | INTEGER | NOT NULL |
| `negative_samples` | INTEGER | NOT NULL |
| `leak_count` | INTEGER | NOT NULL |
| `false_block_count` | INTEGER | NOT NULL |
| `certified` | INTEGER | NOT NULL boolean |
| `evidence_digest` | TEXT | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

投影只按类别聚合，不存或输出单条 Run 放行建议。

### 10.8 `certification_current`

每个类别一行可变 current projection，消除按时间选「最新」的歧义。

| 列 | 类型 | 说明 |
|----|------|------|
| `task_kind` | TEXT | PK，任务类别枚举 |
| `certification_version` | TEXT | NOT NULL；与 `certifications` 组成 FK |
| `version` | INTEGER | NOT NULL CAS |
| `updated_at_ms` | INTEGER | NOT NULL |

写入 settled decision 或 Gate 组装发现窗口证据改变时，同事务插入不可变 `certifications` snapshot，并以 CAS 更新此指针；组装器只读取该指针指向的快照。

### 10.9 `ledger_entries`（不可变）

| 列 | 类型 | 约束/说明 |
|----|------|-----------|
| `id` | TEXT | PK |
| `run_id` | TEXT | NOT NULL FK runs |
| `interrupt_id` | TEXT | NULL FK interrupts |
| `entry_kind` | TEXT | `human_decision \| attention_delivery \| semantic_material \| gate_sample` |
| `features_schema_version` | INTEGER | NOT NULL |
| `features_json` | TEXT | NOT NULL；按 `ledger.md` closed schema 校验 |
| `features_digest` | TEXT | NOT NULL；canonical features JSON 的 SHA-256 |
| `natural_language` | TEXT | NULL；仅 reject 原因或 ask 文本，且须与 `semantic_material` 原文相同 |
| `created_at_ms` | INTEGER | NOT NULL |

Ledger 消费者只能是 T7 提案、指标与类别级 certification；不得建立影响单条 Gate 或抑制单条 HITL 的查询端口。

### 10.10 回放集 JSONL v1

导出是只读 `SELECT → JSONL`，UTF-8，每行一个 canonical JSON object，不重新拼当前数据。导出查询把 Gate 的 `created_at_ms` 与 Brain 的 `started_at_ms` 统一别名为 `recorded_at_ms`，按 `recorded_at_ms, record_type, record_id` 稳定排序。两种 record：

```json
{"record_type":"gate","schema_version":1,"record_id":"...","snapshot_id":"...","input":{},"gate_version":"...","expected_verdict":{}}
{"record_type":"brain_call","schema_version":2,"record_id":"...","scope":"run","subject_key":"run:...","touchpoint":"T3","call_seq":2,"prompt_version":"...","output_schema_version":1,"status":"valid","selected_attempt_no":1,"fallback_reason":null,"input":{},"input_digest":"...","validated_output":{},"attempts":[{"provider_attempt":1,"outcome":"valid","provider_error_code":null,"raw_output":"...","raw_output_digest":"...","raw_output_bytes":123,"raw_output_truncated":false,"input_tokens":10,"output_tokens":5,"started_at_ms":0,"finished_at_ms":1}],"gate_input_snapshot_ids":["..."]}
```

Gate record 来自同一 snapshot/evaluation；Brain record 一条 logical record 内携**按 `provider_attempt` 有序**的 attempts，不得把一次 retry 导出成两个彼此无归属的 record。`output_schema_version` 与表字段同名；`gate_input_snapshot_ids` 按 ID UTF-8 bytes 排序，可为空数组并由关联表重建。导出不得包含 capability hash、operator token 或控制文件内容。格式字段新增需 bump 顶层 `schema_version`；旧导出必须继续可读或由显式迁移工具转换。

## 11. 受限写端口

存储模块只暴露以下写入族；调用方不取得 `*sql.Tx`：

| 端口 | 允许写入 |
|------|----------|
| `ApplyMigration` | schema，仅启动期 |
| `ActivateConfig` | config snapshot、daemon boot、projects 当前投影 |
| `FinishDaemonBoot` | daemon boot 的一次性停止补全 |
| `ApplyStartupRecoveryAction(expectedGeneration, observationDigest, action)` | 恢复矩阵单行的确定性收敛：attempt/claim、启动 operation、隔离、Run transition、Interrupt/预算/outbox 与事件；不执行进程/文件 IO |
| `CompleteStartupRecovery(bootID)` | 仅在本 boot 全部恢复候选已处理后一次性写恢复屏障；不得与 launch claim 合并或倒序 |
| `RecordTerminationObservation(expectedGeneration, command)` | 受控终止的身份/信号/复核事件；确认消失后按来源原子终结/换代/解除隔离，未确认时原子冻结并发唯一 `startup_stall` |
| `UpdateProjectRuntime` / `UpdateProjectAutoMergeCapability` | project health/isolation/capabilities + event + 可选唯一告警 outbox；供启动期和持续能力探测 |
| `RecordHookBaseline(expectedDigest)` | hook baseline CAS + 漂移安全事件 + 可选 Run transition/Interrupt/预算/outbox |
| `TransitionRun(expectedVersion, command)` | runs + events + 可选 outbox/幂等记录 |
| `SetInitialTaskSpec(expectedRunVersion, callIdentity)` | 幂等插入初始 Task Spec snapshot + Run 当前指针 + event |
| `CommitT2Assignment` | T2 valid 的单事务提交：初始 Task Spec snapshot + Run kind/agent/status + 可选 `design_approval` Interrupt/预算/event/outbox；不得先 queued 再补 Interrupt |
| `PersistIntakeBatch`（M2 Intake） | forge receipts、`pending_evaluation` intake 投影、Run/事实投影、events，最后推进 cursor |
| `PersistIntakeDecision`（M2 Intake） | intake assessment + intake state CAS + event + 必要 outbox operation；ready/fallback 同事务幂等创建 Run 并写 `linked_run_id` |
| `ChargeForgeAPICall(callAttemptKey)` | forge_api budget counter + entry；适配器每次外部调用尝试前预留且不退款，崩溃可能保守计费但不得超支 |
| `ClaimOutboxOperation` | operation + immutable attempt start；通用 reclaim 时先为旧 attempt 写 `retry/transient:lease_expired` result；达 `max_attempts` 时同 CAS 终结且不建新 lease/attempt，未达限才接管；`rerun_checks` 按 §8.5 的 request-start 分支收敛 |
| `PrepareLaunchDispatch` | lease/generation CAS + dispatch id + bootstrap/run token hash；提交后才可写 bootstrap/spawn wrapper |
| `MarkOutboxAttemptRequestStarted(operationID, expectedLease, attemptID)` | 仅 `rerun_checks`：在实际 `RerunCheck` 前以 lease CAS 插入不可变 request-start 事实；提交前不得调用远端 |
| `CompleteOutboxAttempt(expectedLease, outcomeCommand)` | operation + immutable result + kind-specific 投影/event；可选 project isolation、Run transition、Interrupt/预算、delivery、后继 outbox，必须同事务；`rerun_checks` 受 §8.5 outcome 限制 |
| `AcquireLaunchClaim` | wrapper/session CAS + pending→starting + launch outbox attempt/result/succeeded + event |
| `AdvanceAttempt` | attempt/claim + events；不得旁路 Run transition |
| `StartOrAdvanceProbe` | attempt probe + 受控终止观测事件 |
| `EmitInterrupt` | Run transition + Interrupt + initial attention admission/budget/critical fuse + event + publish outbox |
| `AdvanceInterrupt` | Supervisor 的 hold/escalate/auto_reject，及升级首次 critical admission/fuse：Interrupt CAS + 可选 Run transition/batch/outbox/event |
| `SetApprovalLabelCutoff(interruptID, expectedVersion, nonce, position)` | 唯一 label-cutoff 后继写端口：只以 §6.1 的 NULL/version/nonce CAS 写 `approval_label_cutoff_position`；返回 `cut_over|unavailable|stale|already_cut_over`，不写其他列 |
| `PrepareAttentionBatch` | due batch 的成员 open-CAS、payload sealing、唯一 Channel operation 与事件；不做外部 IO |
| `ApplyCommandEvent(envelope, parsedAction?)` | Command 唯一 public command port：receipt/canonical identity、initial/final event、private Ledger decision/effect、Run/Interrupt CAS、probe request and outbox/ack in one transaction; it may call only private `resolveAttemptRaceTx` and `recordHumanDecisionTx` |
| `ApplyRetryProbeResult` | startup_stall probe finalizer唯一 public result port；内部调用 private `resolveAttemptRaceTx`，不得先关闭 Interrupt |
| `RecordReport` | 先做 schema/phase/两层去重，再 CAS report rate bucket；普通 report 写 receipt+event，完整 blocker 原子写 `report_receipts` + Report charge + `agent_blocked` Interrupt/admission/outbox，子配额已满则原子写 `report_quota_exhaustions` + security event 并至多发专用 `failure_review`；不改变 `runs.status` |
| `ReserveBrainCall` | brain_call_counters CAS 递增 + 插入 `status=running` 的 brain_calls（冻结身份/输入），同一事务 |
| `RecordBrainAttempt` | immutable brain_attempts + token post-charge（budget entry/counter，唯一 operation key 幂等，允许单次越界） |
| `FinalizeBrainCall` | brain_calls 一次性 `running → valid | fallback` 终结（valid 指向本 call 的 valid attempt，fallback 带原因）；恢复时按已有 attempts 收敛遗留 running call |
| `RecordGateEvaluation` | snapshot/cache/evaluation/calibration/gate_sample + T3/T5 Brain links + 必要 Interrupt；Gate HITL 同时写不可变 `interrupt.calibration_id` |
| `RecordHumanDecision` | private `recordHumanDecisionTx` primitive only; no public port. It is callable only inside `ApplyCommandEvent` (or the separately specified operator transaction owner) and writes exactly one decision for one immutable binding |
| `RefreshCertification` | Gate 组装前以冻结 `as_of_ms` 重算窗口；必要时插入 certification snapshot 并 CAS 更新 current 指针，不做外部 IO |

`resolveAttemptRaceTx`、`recordHumanDecisionTx`、Ledger append/settlement、normal transition、receipt/event/probe/effect/ack primitives are private and have no callable public port. Any new mutable table must have one explicit owner here, and every port change must include migration and crash-injection coverage. Recovery ports receive validated observations/digests and never hold a transaction during OS probing; `ApplyStartupRecoveryAction`, `RecordTerminationObservation`, private `resolveAttemptRaceTx` and `ApplyRetryProbeResult` share the same internal transition/isolation/arbitration primitives.

## 12. 关键事务配方

### 12.1 普通 Run 转移

```text
BEGIN IMMEDIATE
  UPDATE runs
    SET status=?, version=version+1, ...
    WHERE id=? AND version=? AND status IN (expected statuses)
  assert rows_affected == 1
  INSERT events(...)
  INSERT required outbox/budget/idempotency rows
COMMIT
```

CAS 失败整笔回滚并返回 `RejectedStale`；非法状态组合在开事务前由领域层拒绝并记录独立审计事件。

### 12.2 Interrupt 五件事

同一事务（普通 reason 模式）：Run 转 `waiting_human`（或确认已处于合法人工态）→ 按 generation key 查重 → 首次按最终 severity 写 initial attention admission（非 critical 做 quota CAS/re-read/retry；初发 critical 做 fuse admission）并仅在实际 charge 时取得 entry id → 插入引用该 entry 或 NULL 的 Interrupt → 追加事件 → 从 Run 的已验证 Issue、已验证 Change 或冻结 `discussion_target_*` 创建 publish operation，并在需要时创建/加入 batch。Report-only no-transition 模式跳过 Run transition，但仍执行同一 generation/admission/Interrupt/outbox 配方；它必须断言 `runs.status` 与 version 不变。generation key 已存在时直接返回既有 Interrupt，不得重复扣费、admission 或创建第二 operation。任一存储/事务错误回滚；manual Run 的冻结 target 不存在时同样以 `interrupt_publish_target_missing` 回滚/拒绝，不得留下无发布目标的 Interrupt。

### 12.2.1 `RecordReport` 原子配方

除子配额耗尽的有意两段边界外，`RecordReport` 在单一 `BEGIN IMMEDIATE` 中按以下顺序执行，所有领域写入全成全败：

1. 校验 run token binding、closed payload、attempt phase 与 Run 的冻结 `config_snapshot_id`；重复 report key/语义窗口命中时直接返回既有 receipt，不消费任何预算。
2. 对非 duplicate report CAS 该 attempt 的 rate bucket；桶参数与 snapshot 不一致则回滚领域写入并记录安全事件，不重置 bucket。
3. 对已消耗 rate token 的 blocker，读取冻结日桶 counter 的 `(bucket_start_ms,bucket_end_ms)`。容量未满时在同一事务插入唯一 Report charge entry；并调用 `EmitInterrupt` 的 Report-only no-transition 模式。
4. `EmitInterrupt` 的 Report-only 模式创建 event/Interrupt/attention admission/outbox；Run 状态必须保持原值。实际 attention charge 才写 `budget_entries` 与两层 charge FK；`quota_batched`/`critical_fused` admission 的 attention FK 均为 NULL。成功后回填 receipt 的 `direct_interrupt_id` 与 `report_interrupt_charge_entry_id`。
5. 子配额已满时，第一笔 `BEGIN IMMEDIATE` 不写 receipt/event/Report charge；它 CAS rate bucket，并以 `(run_id,daily_bucket_start_ms)` INSERT `report_quota_exhaustions` 与安全 event 后提交。已存在 exhaustion 时在 rate CAS 前直接复用；若并发在 CAS 后令 INSERT 冲突，必须回滚该 tentative CAS、重读既有 exhaustion 后返回。因此只有线性化该日 exhaustion 的请求消费一个 rate token；这个 exhaustion 事务是安全事实的线性化点，先于、且不包含专用发射。
6. 提交后，第二笔 `BEGIN IMMEDIATE` 按 `interrupt.md §5.1` 固定 facts 从该 exhaustion 尝试 no-transition `failure_review`。其 binding 必须是 `report_quota_failure_review(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id)`，options 固定 `reject|hold`；generation-key 查重使崩溃/重放至多产生一个 Interrupt。attention admission、Interrupt、binding 与 outbox 仍在这第二笔事务中全成全败。
7. 第二笔的 publish target 缺失、binding/schema 或其他 `EmitInterrupt` 结构拒绝只回滚第二笔，并以 generation-key 幂等的诊断另行提交；不得回滚已提交 rate CAS 或 exhaustion。attention 存储错误同样只回滚第二笔，RPC 返回 retryable `internal`，后续调用重试这笔发射。完整 blocker 分支或第一笔提交前的任意错误仍按原子事务回滚 rate CAS、receipt、charge、Interrupt、admission与 outbox；事务提交后才返回相应 RPC 结局。

### 12.2.2 Approval-label cutover

`EmitInterrupt`、`AdvanceInterrupt` 或 `ApplyCommandEvent` 先在各自 nonce CAS 中置 cutoff 为 NULL 并提交；不得持事务扫描 Forge。Forge 扫描完成后，`SetApprovalLabelCutoff` 在单一事务中读取 Interrupt，验证 open、`version=expectedVersion`、nonce 相等、cutoff=NULL 与 position（`0` sentinel 或正 canonical decimal），再只更新 cutoff。CAS=0 时按 §6.1 返回既有状态；调用者只能重读/重放，绝不以旧扫描写新 nonce。故创建、轮换、全量扫描和迟到旧扫描的调用图唯一为 `EmitInterrupt|AdvanceInterrupt|ApplyCommandEvent → Forge scan → SetApprovalLabelCutoff`，且旧扫描只能得到 stale。

### 12.3 Intake batch

同一事务写完本批 forge receipts、`pending_evaluation` intake 投影、Run/外部事实投影与事件后，最后更新 `forge_cursors`。任一对象失败则整批游标不前进；下次重读靠 receipt 唯一键去重，不重复创建 intake 投影、Run 或事件。

### 12.4 Outbox claim

CAS 认领到期的 pending/retryable，或接管 lease 已过期的 executing operation；通用 reclaim 先关闭旧 attempt result：若已达 `max_attempts`，同一 CAS 终结 operation 并不写新 lease/attempt；仅未达限才写新 lease owner/expiry、attempt_count+1并新增 outbox_attempt。`rerun_checks` 必须改按 §8.5 查询旧 attempt 的 durable request-start：已开始则 conflict + failure_review，无该事实才可重试。`gate_re_evaluation` 以 §8.1 的 succeeded/failed/conflict closed successor matrix 完成；其 worker 在事务外验证 replacement head，`CompleteOutboxAttempt` 再在同一 lease CAS 事务中写 Gate result、failure-review 或 successor operation，不能由 worker 另调 `EmitInterrupt` 或 `RecordGateEvaluation` 拼接。外部动作在提交后执行。执行结果再以 operation id + lease owner CAS 收敛；过期 worker 不得覆盖新 owner 结果。

### 12.5 `startup_stall` retry 成功

单一 `BEGIN IMMEDIATE`，前置同时校验 Run version、attempt generation、open Interrupt version、probe state：

1. probe → succeeded，写消失证据摘要；
2. 旧 attempt → finished/orphaned，写 `attempt_resolution=retry_after_absence`；
3. isolation → none；
4. Interrupt → closed，`close_reason=responded`；
5. Run `waiting_human → queued` 且 version+1；
6. 创建且仅创建一个新 pending attempt + claim；
7. 创建 launch operation 与 command ack operation；
8. 追加事件。

任一步失败整笔回滚。worker 只能在事务提交后派发新 attempt。

### 12.6 Gate 与人的决定

Gate 首次判定：snapshot、evaluation、calibration 与其唯一 gate_sample entry 同事务；需要 HITL 时同事务执行 §12.2 并冻结 `interrupt.calibration_id`。人的决定到达时只能解析该 binding 或 external binding；calibration 一次性补全、ledger entry、认证 revision 与 Run/outbox 结果同事务。`/sift ask` 还须在该事务插入新 Task Spec snapshot 并更新 `runs.current_task_spec_id`；不得原地覆盖旧 snapshot。

### 12.7 启动事实与 resolution 仲裁

四个入口先把输入规范化为 `AttemptRaceCommand`，再调用同一事务函数。`fact_key` 对 wrapper 请求取稳定 request identity，对恢复/result 取身份与文件 digest，对 Interrupt 指令取 interrupt id/version/nonce；events 的 idempotency key 与回执 operation key 均由它派生。

```text
BEGIN IMMEDIATE
  load Run version + attempt generation/phase/resolution/isolation
       + claim session/permit + current startup_stall Interrupt/probe
  assert command identity, expected versions and generation
  if fact_key receipt/event already exists:
      return its persisted disposition

  if legal started/result fact AND attempt_resolution IS NULL:
      persist complete Agent identity and/or result
      advance attempt (result may continue through finished)
      transition Run waiting_human -> running when applicable
      close open startup_stall as superseded_by_fact
      supersede any in-flight probe and create command ack when required
      retain attention charge; append one event
      return superseded_by_fact (or normal running/finished disposition)

  if legal started/result fact AND attempt_resolution IS NOT NULL:
      persist the late identity/result and security event
      do not advance the old Run path or release its isolation
      schedule/continue controlled termination of that old identity
      create ack when required
      return superseded_by_decision

  if terminal reject command AND attempt_resolution IS NULL:
      set resolution=reject once; transition Run -> failed
      close Interrupt as responded; keep isolation frozen
      schedule controlled termination; append Ledger/event/ack
      return decision_applied

  otherwise apply only a legal non-terminal retry/hold/escalate action;
      never write attempt_resolution
COMMIT
```

CAS 失败整笔回滚并重读后重算，不得把“事实已持久化但 Interrupt 未关”当部分成功。已落定分支必须**吸收**迟到身份而非简单拒绝 RPC；这份身份是后续安全终止的证据。`retry_after_absence` 只能由 §12.5 写，不能由该事实分支猜测。

### 12.8 恢复屏障与恢复动作

创建 `daemon_boots` 行即建立关闭的恢复屏障。恢复协调器在不持事务时读取候选、观测控制文件与进程，再逐项调用 `ApplyStartupRecoveryAction`；每次动作以 generation/operation version CAS，记录 observation digest 和事件。观察变化导致 CAS 失败时重观测，不凭旧快照收敛。

仅当“全部非终态 attempt + 全部未完成 `launch_agent` operation”的新一轮候选查询为空，或每项已被确定性收敛为正常监督、终态、可安全重派、或 frozen + 可见 Interrupt，才调用：

```text
BEGIN IMMEDIATE
  assert daemon_boots.id = current boot
  assert recovery_completed_at_ms IS NULL
  assert no unclassified startup recovery candidate remains
  UPDATE daemon_boots SET recovery_completed_at_ms=? WHERE id=? AND ...
COMMIT
```

`ClaimOutboxOperation` 对 `launch_agent` 在其 claim CAS 内再次验证同一 boot 的非空屏障，因此“完成检查”与 worker 启动间即使并发或崩溃也不能绕过恢复顺序。不得先回收过期 launch lease、再补做 attempt 扫描。

## 13. Append-only 执行保障

对以下表创建 `BEFORE UPDATE` 与 `BEFORE DELETE` trigger，业务写入触发 `RAISE(ABORT, 'append-only table')`：

- `config_snapshots`
- `task_spec_snapshots`
- `events`
- `forge_event_receipts`
- `report_receipts`
- `outbox_attempts`
- `outbox_attempt_results`
- `outbox_attempt_request_starts`
- `budget_entries`
- `brain_attempts`
- `intake_assessments`
- `gate_input_snapshots`
- `brain_gate_input_links`
- `interrupt_command_targets`
- `interrupt_command_effect_bindings`
- `command_effects`
- `gate_evaluations`
- `gate_cache`
- `ledger_entries`
- `external_decision_bindings`

`calibration_entries` 只允许专用语句把二元 human decision 的 NULL 字段一次性补全；其余 UPDATE 与全部 DELETE 被 trigger 拒绝。`interrupts.calibration_id`、`calibration_entries.gate_sample_entry_id` 与 external binding 创建后不可修改。`daemon_boots` 同理只允许分别一次性补全 recovery completion 与 stop 字段；两类补全不得修改身份、配置或启动时间。

`brain_calls` 禁止 DELETE；UPDATE trigger 只允许一次 `running → valid | fallback` 终结（补全终结字段），身份、输入与 `call_seq` 列任何修改都 abort。`brain_call_counters` 与 `intake_items` 是可变投影，以 `version` CAS 并发控制，不加 append-only trigger。

对可变投影另设列级 trigger：`outbox_operations.payload_schema_version/payload_json/payload_digest` 创建后不可改；`attempts.attempt_resolution` 只能从 NULL 与同事务 NULL resolution 时间写为一组非空值；隔离不得由 `frozen` 直接覆盖为另一原因，且解除必须同时写 `isolation_released_at_ms/isolation_release_event_id`；同 generation 的 claim permit 不可替换；Interrupt 的 `charged_budget_entry_id`（包括 NULL）/`generation_key` 与关闭字段创建后不可改；`command_event_outcomes` 只允许 §6.4 所述一次 pending→final CAS，且 final event 的 `final_for_event_id`、event key and type 必须匹配 initial event。违反时 abort，而不是只靠存储接口纪律。

迁移连接可在迁移事务中替换 trigger；运行时存储接口无关闭 trigger 的能力。

## 14. 文件与数据库事实边界

- SQLite 是 Run/attempt 当前投影、claim、预算和幂等权威。
- wrapper 的 `control.json`、`heartbeat`、`result.json` 是进程与完成事实证据；恢复先观测文件/进程，再经存储端口收敛，wrapper 不写 DB。
- forge 是 Issue/Change/Checks 权威；本地保存的是最后观测、游标、审计与运行时状态，不把本地缓存冒充 forge 事实。
- `agent.log` 是文件原始字节流，不存 SQLite；事件只记录日志位置与摘要。

## 15. M1 索引最低集

除唯一约束自动索引外必须包含：

- `task_spec_snapshots(run_id, version DESC)`
- `runs(status, updated_at_ms)`
- `runs(project_id, status)`
- `project_hook_baselines(updated_at_ms)`
- `attempts(phase, updated_at_ms)`
- `attempts(run_id, attempt_no DESC)`
- `interrupts(status, expires_at_ms)`
- `interrupts(run_id, status)`
- `events(run_id, seq)`
- `events(project_id, seq)`
- `outbox_operations(state, next_attempt_at_ms)`
- `outbox_operations(lease_expires_at_ms)`
- `attention_admissions(kind, created_at_ms, run_id, interrupt_id)`（critical fuse 滑动窗口与 admission 去重）
- `interrupts(status, dispatch_state, expires_at_ms)`（expiry，含 timed manual hold）
- `interrupts(status, dispatch_state, next_dispatch_at_ms)`（到期 dispatch 扫描）
- `attention_batches(state, due_at_ms)`（摘要 sealing 扫描）
- `forge_cursors(next_poll_at_ms)`
- `brain_calls(scope, subject_key, touchpoint, call_seq)`（唯一）
- `brain_calls(run_id, attempt_no, touchpoint, call_seq)`（`run_id IS NOT NULL` 的查询索引）
- `brain_attempts(logical_call_id, provider_attempt)`（唯一）
- `intake_items(state, updated_at_ms)`
- `brain_gate_input_links(gate_input_snapshot_id, logical_call_id)`
- `gate_evaluations(run_id, created_at_ms)`
- `ledger_entries(run_id, created_at_ms)`

## 16. M1 验收

1. 新库迁移成功并验证 WAL/foreign keys/busy timeout/single writer；较新 schema 拒启。
2. V1 穷举状态转移；任意非 `transition()` 路径不能改 `runs.status`。
3. V2 在投影、事件、outbox、预算、项目健康、Task Spec、Interrupt 推进与 delivery 各写入点注入崩溃，结果只能全有或全无。
4. append-only 表的 UPDATE/DELETE 被数据库级 trigger 拒绝。
5. Intake 批次崩溃不推进游标；重放不重复创建 intake 投影、Run 或事件。
6. outbox operation key、forge event id、Brain 调用身份（call 唯一键与 attempt 组合唯一键）、Interrupt generation key、Report key 的唯一约束可重复验证。
7. Brain call 可以不关联 Gate snapshot；T3/T5 实际进入 snapshot 时写不可变多对多 link，同一 call 可关联多份真实 snapshot且 terminal call 不被回写。
8. `attempt_resolution` 只接受两个 V0 枚举值，写入后不可逆；隔离与 Run 终态相互独立。
9. 数据库文件与目录权限符合配置规格；CLI/wrapper 无直接写库路径。
10. T1/T2/T7 call 可在无 attempt 或无 Run 时合法落库，作用域错误被 CHECK/FK 拒绝。
11. Report burst=4 的令牌桶跨固定分钟边界仍不允许瞬时超发；critical fuse 按 admission evidence 滑动窗口计数，charge 不冒充 admission。
12. §11 每张可变表均有完整、显式的写入族归属；outbox payload 与一次性 resolution 的非法修改被 trigger 拒绝。
13. Gate JSONL schema v1 与 Brain JSONL schema v2 导出可由冻结数据重建并 round-trip；Brain record 单条内携有序 attempts 和排序 snapshot ID 数组，旧 Brain schema v1 继续可读或经显式迁移转换。
14. `brain_calls` 只能一次性 `running → valid | fallback` 终结；身份/输入列修改与 DELETE 被 trigger 拒绝；`provider_attempt=0` 行 token/exit/raw 必空且 `outcome=fallback`。
15. 遗留 `running` call 只按已有 attempts 收敛为 valid/fallback，不重放无法证明未执行的 provider attempt。
16. intake 状态约束生效：`consumed` 必有 `linked_run_id`，awaiting 必有 assessment 与 generation；token post-charge 允许单次越界而 attention/forge 仍被通用 CAS 拒绝。

### 16.1 M3 增补验收

1. 新 daemon boot 的 recovery completion 为空；完成扫描前 `launch_agent` 的首次 claim 与过期 lease reclaim 都被存储 CAS 拒绝，完成后才可认领；重启创建的新 boot 重新关闭屏障。
2. 恢复候选查询不按 Run 状态漏掉非终态 attempt，也不漏掉未完成启动 operation；每项恢复动作带 expected generation/operation version 与 observation digest。
3. `none → frozen → none` 与 Run/phase 正交；Run `failed` 后 frozen 仍可查询，worktree 仍不可回收/复用。无消失证据解除失败；未验证 Agent 的进程组消失不能自动解除。
4. `claim:started`、恢复补 started、迟到 result 与 Interrupt 指令走同一仲裁事务；事实先到只产生一次 `superseded_by_fact` 关闭，决定先到吸收身份并稳定返回 `superseded_by_decision`，两种交错均不出现第二 owner。
5. retry/hold/escalate/封顶 hold 均不写 resolution；reject 只写一次 `reject`，retry 探测成功只写一次 `retry_after_absence`，重放返回既有结果。
6. Interrupt close 字段同空同非空且写后不可变；每个 close reason 的映射有约束测试，关闭从不回退 attention charge。
7. 受控终止的确认消失与未确认分支分别原子收敛；后者保持冻结并只有一条可见 `startup_stall` Interrupt，前者按 recovery/retry/kill 来源决定是否创建新 attempt。
8. manual Run 在无 `issue_*`、`change_*` 时，以创建时已验证并冻结的 `discussion_target_*` 成功写入 interrupt `forge_comment`；三列缺失、非成对或创建后修改均被约束拒绝。

## 17. 自查结果

- [x] S1：T1 无 Run、T2 无 attempt、T7 聚合三种调用均有合法且唯一的 trace 身份。
- [x] S2：§4–§10 每张可变表均在 §11 声明显式写入族；WBS V2 已同步扩展崩溃注入范围。
- [x] S3：hooks 基线覆盖 git config、原始/effective hooksPath 与目录 digest，跨重启可复核。
- [x] S4：Report 使用持久整数令牌桶；critical fuse 使用 append-only admission evidence 的滑动窗口；摘要有 batch/member/operation 的持久身份。
- [x] S5：schema version、FK、隔离释放、原因枚举、probe、manual Run、close reason、回放格式、索引和 payload trigger 均已处置。
- [x] `attempt_resolution` 旧名未进入 spec，V0 枚举保持 `reject | retry_after_absence`。
- [x] M3 §3.4–§3.5：恢复屏障、恢复/终止写端口、独立隔离投影、四入口唯一仲裁与关闭原因映射已落 §1/§4.2/§5.3/§6.1/§8.3/§11/§12/§13/§16.1。
- [x] Brain 评审交叉补丁：call/attempt 拆表、intake 投影、token post-charge 例外与新写端口已同步落 §7/§9/§10/§11/§12/§13/§15/§16。
- [x] 所有 markdown 相对链接存在、代码围栏闭合、无尾随空白。

**自查结论：** 两项 P1 与同轮 P2/P3 已关闭；定向复评通过，允许转 `active`。
