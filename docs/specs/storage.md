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
13. 合法启动/结果事实与 `attempt_resolution` 只在 `ResolveAttemptRace` 内仲裁；四个调用入口不得各自实现近似事务。
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
| `version` | INTEGER | NOT NULL，初值 1；nonce/超时更新时 +1 |
| `status` | TEXT | `open \| closed` |
| `dispatch_state` | TEXT | `ready \| batched \| held \| probe_in_progress` |
| `expires_at_ms` | INTEGER | NOT NULL |
| `on_expire` | TEXT | `hold \| escalate \| auto_reject` |
| `escalation_count` | INTEGER | NOT NULL，默认 0 |
| `max_escalations` | INTEGER | NOT NULL；创建时冻结配置 |
| `close_reason` | TEXT | NULL 或 `responded \| expired_auto_reject \| superseded_by_fact \| superseded_by_decision \| external_fact` |
| `closed_at_ms` | INTEGER | NULL |
| `charged_budget_entry_id` | TEXT | NOT NULL UNIQUE FK budget_entries |
| `calibration_id` | TEXT | NULL UNIQUE FK calibration_entries；仅 Gate HITL 创建时不可变绑定 |
| `created_at_ms` | INTEGER | NOT NULL |
| `updated_at_ms` | INTEGER | NOT NULL |

约束：

- `status=open` 时 close 字段为空；closed 时同为非空。
- `startup_stall` 禁止 `on_expire=auto_reject`。
- escalation 重推不新增 budget charge；关闭不退款。
- 初始 nonce 的 `nonce_issued_at_ms` 等于 `created_at_ms`；每次 nonce 轮换必须同一 CAS 更新 `nonce_issued_at_ms` 并递增 version。非 nonce 更新不得改写该时间。
- `probe_in_progress` 时拒绝新指令，但合法迟到事实仍可经仲裁入口提交。

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
| `interrupt_id` | TEXT | NOT NULL FK interrupts |
| `surface` | TEXT | `forge_comment \| channel` |
| `priority` | TEXT | `normal \| strong` |
| `operation_key` | TEXT | NOT NULL UNIQUE |
| `state` | TEXT | `pending \| delivered \| failed` |
| `attempt_count` | INTEGER | NOT NULL，默认 0 |
| `remote_ref` | TEXT | NULL |
| `last_error` | TEXT | NULL |
| `created_at_ms` | INTEGER | NOT NULL |
| `delivered_at_ms` | INTEGER | NULL |

投递执行仍由 outbox 驱动；本表只给 `ps/doctor` 一致查询面，不替代 outbox 历史。

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
| `event_kind` | TEXT | NOT NULL |
| `target_kind` | TEXT | `issue \| change` |
| `target_id` | TEXT | NOT NULL |
| `actor` | TEXT | NULL |
| `raw_digest` | TEXT | NOT NULL |
| `disposition` | TEXT | `accepted \| fact_observed \| ignored_untrusted_actor \| ignored_missing_actor` |
| `domain_event_id` | TEXT | NULL FK events |
| `observed_at_ms` | INTEGER | NOT NULL |

唯一约束 `(project_id, forge_event_id)`。重复投递返回既有 receipt，不新增“duplicate”行。事实观测不因 actor 缺失被忽略；驱动事件必须有可信 actor 才能 `accepted`。

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
| `received_at_ms` | INTEGER | NOT NULL |

唯一约束 `(run_id, attempt_no, report_key)`。本表只记录已接受报告；重复请求先按该键返回既有 receipt，不消费令牌、不重复写事件；限流拒绝只记安全事件而不占用 report key。Agent 的 completed 只产生事件，不修改 Run 状态。

### 7.5 `intake_items`（可变）

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
| `kind` | TEXT | `forge_comment \| forge_labels \| create_change \| merge_change \| rerun_checks \| channel_publish \| launch_agent \| command_ack \| forge_alert` |
| `run_id` | TEXT | NULL FK runs |
| `attempt_no` | INTEGER | NULL；非空时与 run_id 组成 attempts 组合 FK |
| `interrupt_id` | TEXT | NULL FK interrupts |
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

`executing` 必须同时有 lease owner/expiry；其他 state 不得保留有效 lease。terminal state 为 succeeded/failed/stale/conflict。payload 一经创建不可改；重试只更新执行字段。`rerun_checks` 的 claim/reclaim、request-start 与 complete 另按 §8.5 执行，不得套用通用 lease-expiry 重试。

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

每个 attempt 最多一个结果。operation claim 用 CAS：pending/retryable 且到期，或 executing 且 lease 已过期的行可被认领；后者先为旧 attempt 插入 `outcome=retry,error_class=transient,error_summary=lease_expired` result，再替换 lease owner并创建新 attempt，旧 owner 的 complete 随即 CAS 失败。

`launch_agent` 是特殊 claim：`ClaimOutboxOperation(current_boot_id, ...)` 必须在同一 SQL 语句/事务中证明 `daemon_boots.id=current_boot_id AND recovery_completed_at_ms IS NOT NULL`；先在应用内检查再 claim 不成立。恢复扫描可把旧启动 operation 收敛为 pending/retryable/succeeded/stale/conflict，但在屏障落定前同样不得取得执行 lease。daemon 在 `CompleteStartupRecovery` 提交后才启动 launch worker；崩溃产生的新 boot 因新行屏障为空而重新关闭该入口。外部动作结束后，同一事务插入 result 并 CAS 更新 operation；旧 lease owner 的结果整笔拒绝。

### 8.5 `outbox_attempt_request_starts`（不可变，`rerun_checks` 专用）

这是远端 `RerunCheck` 已可能发生的唯一 durable boundary；它不以 worker 内存、日志或 lease 推断。

| 列 | 类型 | 约束/说明 |
|---|---|---|
| `attempt_id` | TEXT | PK、FK `outbox_attempts` |
| `started_at_ms` | INTEGER | NOT NULL；写入前注入的时间 |

只允许 `outbox_operations.kind=rerun_checks` 的 attempt 插入一行；其他 kind 不得伪造该事实；该跨表约束与本表的 UPDATE/DELETE 禁止均由数据库 trigger 执行。`MarkOutboxAttemptRequestStarted(operationID, expectedLease, attemptID)` 在一个 `BEGIN IMMEDIATE` 中验证 operation 仍为 `executing` 且 owner/未过期 lease 和 attempt 均匹配，再插入本行；重复调用返回既有事实。该事务成功提交后 worker 才可调用 `RerunCheck`，提交失败则不得调用。

`ClaimOutboxOperation` 对 `rerun_checks` 的规则替代通用 reclaim：pending/retryable 到期可正常 claim；executing lease 到期时，若旧 attempt **没有**本表记录，事务为旧 attempt 插入 `retry/transient:lease_expired` result 后创建新 attempt。若旧 attempt已有本表记录，事务必须为旧 attempt 插入（或确认既有）`conflict` result，将 operation CAS 为 `conflict`、清除 lease，并同事务创建/去重 `failure_review` Interrupt；不得创建新 attempt。`CompleteOutboxAttempt` 对已有本表记录的 `rerun_checks` 只接受 `success` 或 `conflict`，不接受 `retry`；调用结果不明、调用后错误或 complete CAS 丢失都走 `conflict`。因此调用前崩溃可重试，request-start 后的崩溃或 lease expiry 永远不会导致第二次远端调用。

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

计费入口在同一事务执行 `consumed + amount <= limit` CAS。本表只承载日/小时固定桶（token、forge API、非 critical 注意力）；注意力不可借支。

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

Interrupt 升级重推复用原 charge，不新增 entry；Interrupt 关闭不退款。非 critical 日配额 entry 使用 `kind=attention, scope=severity, scope_id=<severity>`；Report 致扰子配额使用 `kind=report, scope=run, scope_id=<run_id>`。critical 不写日配额 counter，但每次首次发射仍写 `kind=attention, scope=severity, scope_id=critical` entry，并令 `bucket_start_ms=created_at_ms`。熔断在 `EmitInterrupt` 事务内按 `created_at_ms >= now-window` 对该 append-only 流做全局与 per-Run 计数，形成真实滑动窗口；不以固定桶近似。`budget_entries` 不反向保存 Interrupt FK；Interrupt 通过不可变 `charged_budget_entry_id` 指向 charge，避免循环外键。

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
  - T7：`scope=aggregate`，subject 为 `global` 或 project/category/window 聚合键；run/attempt 为空。
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
| `ClaimOutboxOperation` | operation + immutable attempt start；通用 reclaim 时先为旧 attempt 写 `retry/transient:lease_expired` result；`rerun_checks` 按 §8.5 的 request-start 分支收敛 |
| `PrepareLaunchDispatch` | lease/generation CAS + dispatch id + bootstrap/run token hash；提交后才可写 bootstrap/spawn wrapper |
| `MarkOutboxAttemptRequestStarted(operationID, expectedLease, attemptID)` | 仅 `rerun_checks`：在实际 `RerunCheck` 前以 lease CAS 插入不可变 request-start 事实；提交前不得调用远端 |
| `CompleteOutboxAttempt(expectedLease, outcomeCommand)` | operation + immutable result + kind-specific 投影/event；可选 project isolation、Run transition、Interrupt/预算、delivery、后继 outbox，必须同事务；`rerun_checks` 受 §8.5 outcome 限制 |
| `AcquireLaunchClaim` | wrapper/session CAS + pending→starting + launch outbox attempt/result/succeeded + event |
| `AdvanceAttempt` | attempt/claim + events；不得旁路 Run transition |
| `StartOrAdvanceProbe` | attempt probe + 受控终止观测事件 |
| `ResolveAttemptRace(expectedRunVersion, expectedGeneration, factKey, command)` | claim:started、恢复补 started、迟到 result、Interrupt 指令共用的唯一 CAS 仲裁；resolution/身份/结果、隔离、Run transition、Interrupt close、回执 outbox 与事件同事务 |
| `EmitInterrupt` | Run transition + Interrupt + attention budget/fuse + event + publish outbox |
| `AdvanceInterrupt` | Supervisor 的 hold/escalate/auto_reject：Interrupt CAS + 可选 Run transition/outbox/event |
| `ApplyInterruptCommand` | 通用指令：Interrupt CAS + 可选 Task Spec/Ledger/calibration/certification + Run transition/outbox/event |
| `ApplyRetryProbeResult` | ADR-013 全部结果字段的一笔 CAS 事务；内部必须调用与 `ResolveAttemptRace` 相同的仲裁原语，不得先关闭 Interrupt |
| `RecordReport` | token bucket + report receipt + event；必要时原子发唯一异常 Interrupt |
| `ReserveBrainCall` | brain_call_counters CAS 递增 + 插入 `status=running` 的 brain_calls（冻结身份/输入），同一事务 |
| `RecordBrainAttempt` | immutable brain_attempts + token post-charge（budget entry/counter，唯一 operation key 幂等，允许单次越界） |
| `FinalizeBrainCall` | brain_calls 一次性 `running → valid | fallback` 终结（valid 指向本 call 的 valid attempt，fallback 带原因）；恢复时按已有 attempts 收敛遗留 running call |
| `RecordGateEvaluation` | snapshot/cache/evaluation/calibration/gate_sample + T3/T5 Brain links + 必要 Interrupt；Gate HITL 同时写不可变 `interrupt.calibration_id` |
| `RecordHumanDecision` | Command、手工 merge/close 唯一人类动作入口；只能解析 immutable Interrupt/external binding，写 ledger + certification snapshot/current CAS + Run/Interrupt/outbox |
| `RefreshCertification` | Gate 组装前以冻结 `as_of_ms` 重算窗口；必要时插入 certification snapshot 并 CAS 更新 current 指针，不做外部 IO |

任何新增可变表必须在本表有完整、显式的写入族归属；新增端口必须证明不能由上述端口表达，并同步本规格与崩溃注入测试。恢复端口接收的是已验证观测及其 digest，不持有数据库事务做 OS 探测；`ApplyStartupRecoveryAction`、`RecordTerminationObservation`、`ResolveAttemptRace` 与 `ApplyRetryProbeResult` 共享内部 transition/isolation/仲裁原语，不形成四套可漂移规则。

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

同一事务：Run 转 `waiting_human`（或确认已处于合法人工态）→ 按 generation key 查重 → 首次时扣一次注意力预算并取得 entry id → 插入引用该 entry 的 Interrupt → 追加事件 → 从 Run 的已验证 Issue、已验证 Change 或冻结 `discussion_target_*` 创建 publish operation。generation key 已存在时直接返回既有 Interrupt，不得重复扣费或创建第二 operation。manual Run 的冻结 target 不存在时，该事务以 `interrupt_publish_target_missing` 回滚/拒绝，不得留下无发布目标的 Interrupt。

### 12.3 Intake batch

同一事务写完本批 forge receipts、`pending_evaluation` intake 投影、Run/外部事实投影与事件后，最后更新 `forge_cursors`。任一对象失败则整批游标不前进；下次重读靠 receipt 唯一键去重，不重复创建 intake 投影、Run 或事件。

### 12.4 Outbox claim

CAS 认领到期的 pending/retryable，或接管 lease 已过期的 executing operation；通用 reclaim 先关闭旧 attempt result，再写新 lease owner/expiry、attempt_count+1并新增 outbox_attempt。`rerun_checks` 必须改按 §8.5 查询旧 attempt 的 durable request-start：已开始则 conflict + failure_review，无该事实才可重试。外部动作在提交后执行。执行结果再以 operation id + lease owner CAS 收敛；过期 worker 不得覆盖新 owner 结果。

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
- `gate_evaluations`
- `gate_cache`
- `ledger_entries`
- `external_decision_bindings`

`calibration_entries` 只允许专用语句把二元 human decision 的 NULL 字段一次性补全；其余 UPDATE 与全部 DELETE 被 trigger 拒绝。`interrupts.calibration_id`、`calibration_entries.gate_sample_entry_id` 与 external binding 创建后不可修改。`daemon_boots` 同理只允许分别一次性补全 recovery completion 与 stop 字段；两类补全不得修改身份、配置或启动时间。

`brain_calls` 禁止 DELETE；UPDATE trigger 只允许一次 `running → valid | fallback` 终结（补全终结字段），身份、输入与 `call_seq` 列任何修改都 abort。`brain_call_counters` 与 `intake_items` 是可变投影，以 `version` CAS 并发控制，不加 append-only trigger。

对可变投影另设列级 trigger：`outbox_operations.payload_schema_version/payload_json/payload_digest` 创建后不可改；`attempts.attempt_resolution` 只能从 NULL 与同事务 NULL resolution 时间写为一组非空值；隔离不得由 `frozen` 直接覆盖为另一原因，且解除必须同时写 `isolation_released_at_ms/isolation_release_event_id`；同 generation 的 claim permit 不可替换；Interrupt 的 `charged_budget_entry_id/generation_key` 与关闭字段一旦非空不可改。违反时 abort，而不是只靠存储接口纪律。

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
- `budget_entries(kind, created_at_ms, run_id)`（critical fuse 滑动窗口）
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
11. Report burst=4 的令牌桶跨固定分钟边界仍不允许瞬时超发；critical fuse 按滑动窗口计数。
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
- [x] S4：Report 使用持久整数令牌桶；critical fuse 使用 append-only entry 的滑动窗口。
- [x] S5：schema version、FK、隔离释放、原因枚举、probe、manual Run、close reason、回放格式、索引和 payload trigger 均已处置。
- [x] `attempt_resolution` 旧名未进入 spec，V0 枚举保持 `reject | retry_after_absence`。
- [x] M3 §3.4–§3.5：恢复屏障、恢复/终止写端口、独立隔离投影、四入口唯一仲裁与关闭原因映射已落 §1/§4.2/§5.3/§6.1/§8.3/§11/§12/§13/§16.1。
- [x] Brain 评审交叉补丁：call/attempt 拆表、intake 投影、token post-charge 例外与新写端口已同步落 §7/§9/§10/§11/§12/§13/§15/§16。
- [x] 所有 markdown 相对链接存在、代码围栏闭合、无尾随空白。

**自查结论：** 两项 P1 与同轮 P2/P3 已关闭；定向复评通过，允许转 `active`。
