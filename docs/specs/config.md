---
status: active
created: 2026-07-28
summary: 全局配置、默认值、路径与启动探测契约
---

# 配置规格

本文是 `~/.sift/config.yaml`、`SIFT_HOME`、默认值及启动期能力探测的字段级契约。项目内策略由后续的 `specs/policy.md` 定义；控制面凭据由后续的 `specs/control-plane.md` 定义；持久化快照见 [`storage.md`](storage.md)。

需求与结构来源：[PRD §5.2、§5.3、§5.5、§5.7、§9](../PRD.md)，[DESIGN §7、§9.1、§9.2、§9.4、§11、§14.2](../DESIGN.md)，[WBS M1 §1.4](../WBS.md)。

## 评审处置

评审原文：[2026-07-28-specs-review-pi-k3.md](../reviews/2026-07-28-specs-review-pi-k3.md)。

| 发现 | 处置 |
|------|------|
| C1 Agent 并发默认遮蔽 | Agent 省略 `max_concurrent` 时继承根配置，不再各自硬编码默认 |
| C2 认证阈值可被 policy 绕过 | 认证移为全局专有 section；定义 version/hash 与 Gate cache 失效联动 |
| C3 缺全局 Context 路径 | 补 `context.md` 权限、缺省与 Task Spec 冻结语义 |
| C4 Report 子配额边界不明 | 移入 Report，限定来源、critical 口径及触顶 `failure_review_once` |
| C5 路径、漂移、探测等细节 | 补控制文件生命周期、漂移周期、权限拒启、Brain 探测、单实例锁及 canonical JSON 单一来源 |

C1–C5 已全部处置，本规格转 `active`；后续字段变更须与代码和派生测试同步。

Brain 字段级评审 [2026-07-28-brain-review-pi-gpt-5.6-sol.md](../reviews/2026-07-28-brain-review-pi-gpt-5.6-sol.md) 的交叉补丁：B5 新增 `brain.protocol`（V0 限定 `claude-json-v1`）；B3 新增 `brain.max_input_bytes` 输入总上限；B4 明确 `daily_token_limit` 的发起前阈值 + 事后越界语义。

## 1. 适用范围与不变量

1. 全局配置是 **closed contract**：未知字段、字段缺失导致的歧义、错误类型和未知枚举一律拒绝。
2. YAML 先转为 JSON，再经全系统唯一 decode gateway 的 `closed` 策略校验；业务代码不得另开 YAML/JSON 解码入口。
3. V0 不热加载全局配置。daemon 启动时读取一次、规范化、计算指纹并持久化快照；磁盘变化只告警，不改变有效配置。修改后必须重启 `siftd`。
4. `config.yaml` 不存 forge token、operator token、run token、bootstrap nonce、wrapper session 或 spawn permit。
5. 全局配置只给项目策略提供缺省值，不覆盖 base 分支 `.sift/policy.yaml` 的显式声明。
6. 所有 duration 使用 Go duration 字符串，例如 `500ms`、`15s`、`2h`；禁止裸数字。所有比例是闭区间 `[0,1]` 的 JSON number。
7. 所有命令均为 executable + argv 数组，禁止 shell 字符串、重定向与管道拼接。

## 2. 路径契约

### 2.1 `SIFT_HOME`

解析顺序：

1. `SIFT_HOME` 非空时使用其值；它必须是绝对路径。
2. 否则使用 `os.UserHomeDir()` 下的 `.sift`。
3. 路径执行 `filepath.Clean`；目录存在时记录其解析后真实路径用于诊断，但稳定对外路径仍使用清理后的配置路径。
4. 无法取得用户 home、路径不是目录、目录不归当前用户或权限宽于属主访问时，daemon 拒绝启动。

V0 不采用 XDG 三目录拆分。macOS 与 Linux 使用同一布局：

| 路径 | 模式 | 说明 |
|------|------|------|
| `$SIFT_HOME/` | `0700` | 配置与状态根目录 |
| `config.yaml` | `0600`（存在时） | 全局配置；文件可缺省，权限更宽时拒启且不自动修正 |
| `context.md` | `0600`（存在时） | 全局 Context；缺省为空；组装新 Task Spec 时读取并冻结 hash |
| `sift.db` | `0600` | SQLite 数据库 |
| `siftd.lock` | `0600` | daemon 生命周期 advisory lock；进程退出后由内核释放 |
| `operator.token` | `0600` | 运维 capability；不是配置字段 |
| `siftd.sock` | `0600` | 运维 socket |
| `run.sock` | `0600` | Report 与 wrapper socket |
| `logs/` | `0700` | 系统日志 |
| `worktrees/` | `0700` | Run worktree |
| `runs/<run_id>/` | `0700` | Run 控制根目录 |
| `runs/<run_id>/attempts/<attempt_no>/` | `0700` | `$SIFT_RUN_DIR`；每 attempt 独立，不覆盖重试历史 |
| `runs/<run_id>/attempts/<attempt_no>/bootstrap.json` | `0600` | 原子创建；wrapper 读取后立即 unlink |
| `runs/<run_id>/attempts/<attempt_no>/task.json` | `0600` | 冻结 Task Spec；存活到 attempt 清理 |
| `runs/<run_id>/attempts/<attempt_no>/control.json` | `0600` | 含 run token；存活到 attempt 清理 |
| `runs/<run_id>/attempts/<attempt_no>/heartbeat` | `0600` | wrapper 原子更新 |
| `runs/<run_id>/attempts/<attempt_no>/result.json` | `0600` | wrapper 原子写入的完成证据 |
| `runs/<run_id>/attempts/<attempt_no>/agent.log` | `0600` | Agent 原始字节流，按 logging 配置轮转 |

控制文件的字段与原子写协议归 `specs/control-plane.md`；本表只定义路径、权限和生命周期。Sift 创建目录或文件后必须复核实际模式；不得只依赖进程 `umask`。

## 3. 顶层 schema

```yaml
version: 1

operators: {}
agents: []
projects: []
brain: {}
scheduler: {}
runtime: {}
outbox: {}
forge: {}
attention: {}
report: {}
gate_defaults: {}
certification: {}
metrics: {}
labels: {}
logging: {}
```

除 `version` 外均可省略并使用本节默认值。`version` 在文件存在时必填且当前只能为 `1`；文件不存在等价于完整默认配置。

### 3.1 `operators`

```yaml
operators:
  github: ["hex"]
  gitlab: ["hex"]
```

| 字段 | 类型 | 默认 | 约束 |
|------|------|------|------|
| `github` | `string[]` | `[]` | GitHub login 精确 allowlist，去重后非空字符串 |
| `gitlab` | `string[]` | `[]` | GitLab username 精确 allowlist，去重后非空字符串 |

V0 只支持静态用户名 allowlist，不把“组织成员”或“具备仓库权限”当作隐式批准者。适配器按平台规范化 actor 后比较；取不到 actor 时 fail closed。空 allowlist 意味着所有 forge 驱动性事件均被忽略，但外部事实观测仍照常收敛。

### 3.2 `agents[]`

```yaml
agents:
  - id: claude-code
    executable: claude
    args: ["-p"]
    task_transport: stdin
    backend: process
    max_concurrent: 1
    version_args: ["--version"]
```

| 字段 | 类型 | 默认 | 约束 |
|------|------|------|------|
| `id` | string | — | 必填；匹配 `^[a-z][a-z0-9-]{0,62}$`，全局唯一 |
| `executable` | string | — | 必填；非空；直接传给 `exec`，不得包含 shell 片段 |
| `args` | `string[]` | `[]` | argv；每项不得含 NUL；仅允许完整 token `{task_file}` 占位符 |
| `task_transport` | enum | `stdin` | `stdin \| file`；`file` 要求 args 中恰有一个 `{task_file}`，具体文件契约在 `specs/control-plane.md` 定义 |
| `backend` | enum | `process` | `process \| tmux` |
| `max_concurrent` | integer | `runtime.default_agent_max_concurrent` | `1..32`；Agent 省略时继承根配置 |
| `version_args` | `string[]` | `["--version"]` | 启动探测 argv；空数组表示只探测 executable 可执行 |

`task_transport=stdin` 时禁止 `{task_file}`；`file` 时必须恰有一个完整 argv token 等于 `{task_file}`，wrapper 只做单 token 替换，不做字符串插值或 shell 展开。Agent 定义不接受任意环境变量映射。Runtime 只注入协议明确允许的非机密变量（V0 为 `SIFT_RUN_DIR`）；凭据不得经配置、argv 或环境变量传递。

### 3.3 `projects[]`

```yaml
projects:
  - id: sift-github
    repo: /absolute/path/to/sift
    forge:
      kind: github
      project: miaoxiaoyong/sift
      host: github.com
    enabled: true
    agents: [claude-code, codex]
```

| 字段 | 类型 | 默认 | 约束 |
|------|------|------|------|
| `id` | string | — | 必填；同 Agent id 规则，全局唯一 |
| `repo` | string | — | 必填绝对路径；须为本地 git 工作仓库 |
| `forge.kind` | enum | — | 必填；`github \| gitlab` |
| `forge.project` | string | — | 必填；平台项目键，如 `owner/repo` 或 `group/project` |
| `forge.host` | string | 平台公共 host | GitHub 默认 `github.com`，GitLab 默认 `gitlab.com` |
| `forge.cli` | string/null | `null` | 覆盖 CLI executable；null 时 GitHub=`gh`、GitLab=`glab` |
| `enabled` | boolean | `true` | false 时不摄入、不调度，但保留历史状态 |
| `agents` | `string[]` | 全部已定义 Agent id | 候选 Agent；每项必须引用存在且可用的 Agent |

`repo` 经清理后不得与另一启用项目重复。项目策略只从该仓库 base 分支 `.sift/policy.yaml` 读取；此处不得提供 worktree policy 路径。

### 3.4 `brain`

```yaml
brain:
  executable: claude
  args: ["-p", "--output-format", "json"]
  protocol: claude-json-v1
  daily_token_limit: 1000000
  call_timeout: 2m
  schema_retries: 1
  max_input_bytes: 262144
  max_raw_output_bytes: 1048576
```

| 字段 | 类型 | 默认 | 约束 |
|------|------|------|------|
| `executable` | string/null | `null` | null 表示确定性模式，全部触点走各自兜底 |
| `args` | `string[]` | `[]` | 直接 argv |
| `protocol` | enum | `claude-json-v1` | V0 只能为 `claude-json-v1`；协议语义变化必须引入新值，见 [`brain.md` §4](brain.md) |
| `daily_token_limit` | integer | `1000000` | `0` 表示禁止 LLM 调用；否则 `>=1000`。它是发起新物理 attempt 的消费阈值；已发起 attempt 按实际 usage 事后全额记账，允许单次越界，见 [`brain.md` §6](brain.md) |
| `call_timeout` | duration | `2m` | `1s..30m` |
| `schema_retries` | integer | `1` | V0 只能为 `1`：同 prompt 重试一次 |
| `max_input_bytes` | integer | `262144` | `4096..16777216`；单次 Brain 调用 input canonical JSON 总上限，超限不调用 provider、走触点确定性兜底 |
| `max_raw_output_bytes` | integer | `1048576` | `4096..16777216` |
| `version_args` | `string[]` | `["--version"]` | 启动探测 argv；空数组只探测 executable 可执行 |

`executable=null` 不属于启动错误。配置了 executable 时，进程级启动探测必须按 `version_args` 确认可执行；调用失败仍按触点兜底，不让 Run 静默丢失。

### 3.5 `scheduler`

| 字段 | 默认 | 约束 |
|------|------|------|
| `intake_idle_interval` | `60s` | `5s..1h` |
| `intake_active_interval` | `15s` | `2s..10m` |
| `intake_interrupt_interval` | `10s` | `2s..10m` |
| `intake_interrupt_burst_duration` | `5m` | `10s..1h` |
| `supervisor_interval` | `1s` | `100ms..30s` |
| `config_drift_check_interval` | `30s` | `1s..1h` |
| `per_class_tick_limit` | `100` | `1..10000` |

每项目独立保存当前轮询态；这些值只决定下一次调度，不得回写为领域事实。

### 3.6 `runtime`

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `backend` | `process` | `process \| tmux`；Agent 定义可覆盖 |
| `max_concurrent_total` | `3` | `1..32` |
| `default_agent_max_concurrent` | `1` | `1..32` |
| `max_attempts` | `3` | `1..20`；含首次 attempt |
| `attempt_timeout` | `2h` | `1m..24h` |
| `agent_silence_timeout` | `30m` | `1m..24h` |
| `retry_initial_delay` | `30s` | `0s..1h` |
| `retry_max_delay` | `5m` | 不小于 initial |
| `retry_multiplier` | `2.0` | `1..10` |
| `spawn_operation_lease_ttl` | `30s` | `5s..10m` |
| `starting_permit_timeout` | `30s` | `1s..10m` |
| `spawning_started_timeout` | `30s` | `1s..10m` |
| `heartbeat_interval` | `5s` | `500ms..1m` |
| `heartbeat_stale_after` | `15s` | 不小于 heartbeat interval，最大 `10m` |
| `termination_term_grace` | `10s` | `0s..10m` |
| `termination_kill_grace` | `5s` | `0s..10m` |
| `absence_recheck_count` | `3` | `1..20` |
| `absence_recheck_interval` | `1s` | `100ms..1m` |

这些字段是 M3 启动交接与停滞收敛的唯一配置入口，命名与控制面/存储契约保持一致：`spawn_operation_lease_ttl` 对应启动 operation lease；`starting_permit_timeout` 是 `starting` 等待 `claim.permit_spawn` 的上限；`spawning_started_timeout` 是 `spawning` 等待 `claim.started` 的上限；复核使用 `absence_recheck_count` 与 `absence_recheck_interval`。控制面方法与阶段以 [`control-plane.md` §5](control-plane.md) 为准，attempt 的持久化阶段、generation 与 `attempt_probes` 字段以 [`storage.md` §5.3–§5.5](storage.md) 为准；本节不另造字段名或持久化状态。

受控终止固定为：确认进程身份 → 按进程组发送 `SIGTERM` → 等 `termination_term_grace` → `SIGKILL` → 等 `termination_kill_grace` → 每隔 `absence_recheck_interval` 复核，最多 `absence_recheck_count` 次。身份不确定时不得发信号，直接进入 `startup_stall`；确认不了消失时必须冻结 attempt 并经统一 Interrupt 路径收敛。`spawning` 超时永远不能单独授权换 owner，必须先取得消失证据。

### 3.7 `outbox`

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `lease_ttl` | `30s` | `5s..10m` |
| `retry_initial_delay` | `1s` | `100ms..1h` |
| `retry_max_delay` | `5m` | 不小于 initial |
| `retry_multiplier` | `2.0` | `1..10` |
| `max_attempts` | `0` | `0`=持续重试；正数 `1..1000` |
| `max_advance_delay` | `2s` | `100ms..30s`；V8 的推进目标 |
| `worker_batch_size` | `50` | `1..1000` |

语义错误不受 `max_attempts` 影响，直接收敛为 terminal/stale/conflict；瞬时错误才退避。

### 3.8 `forge`

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `hourly_api_limit` | `1000` | `1..100000` |
| `warning_ratio` | `0.8` | `(0,1)` |
| `slow_poll_interval` | `5m` | 不小于 active interval |
| `command_timeout` | `30s` | `1s..10m` |

API 只在 Forge 适配层收费。达到 `warning_ratio` 后降低轮询频率；达到上限后不执行非必要轮询，并发一次告警，不得影响注意力配额的硬约束。

### 3.9 `attention`

Severity 枚举为 `low | normal | high | critical`。`critical` 不设日配额，但受熔断约束。

```yaml
attention:
  day_timezone: local
  daily_quota:
    low: 3
    normal: 5
    high: 5
  max_escalations: 2
  critical_fuse:
    window: 15m
    total_limit: 5
    per_run_limit: 2
  daily_summary_at: "09:00"
```

| 字段 | 默认 | 约束 |
|------|------|------|
| `day_timezone` | `local` | `local` 或 IANA timezone |
| `daily_quota.low` | `3` | `0..1000` |
| `daily_quota.normal` | `5` | `0..1000` |
| `daily_quota.high` | `5` | `0..1000` |
| `max_escalations` | `2` | `0..10` |
| `critical_fuse.window` | `15m` | `1m..24h` |
| `critical_fuse.total_limit` | `5` | `1..1000` |
| `critical_fuse.per_run_limit` | `2` | `1..total_limit` |
| `daily_summary_at` | `09:00` | 本地 `HH:MM` |
| `hold_max_duration` | `720h`（30 天） | `1m..8760h` |
| `channel_failure_alert_after` | `3` | `1..100`；连续失败后改走 forge 告警评论 |

每个 reason 的默认超时与到期动作：

| reason | `expires_after` | `on_expire` | 达升级上限后 |
|--------|-----------------|-------------|----------------|
| `design_approval` | `24h` | `hold` | `hold` |
| `guardrail_violation` | `24h` | `hold` | `hold` |
| `code_review` | `72h` | `hold` | `hold` |
| `agent_blocked` | `8h` | `escalate` | `auto_reject` |
| `merge_conflict` | `8h` | `escalate` | `auto_reject` |
| `failure_review` | `24h` | `auto_reject` | `auto_reject` |
| `startup_stall` | `1h` | `escalate` | `hold` |

`hold_max_duration` 是每条 `/sift hold <duration>` 指令的单次上限，不限制人多次显式 hold。`auto_approve` 不是合法值。`startup_stall` 在 schema 与运行时双重禁止 `auto_reject`。

### 3.10 `report`

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `events_per_minute` | `12` | `1..10000` |
| `burst` | `4` | `1..1000` |
| `dedupe_window` | `30s` | `0s..1h` |
| `max_payload_bytes` | `65536` | `1024..1048576` |
| `not_ready_initial_delay` | `100ms` | `10ms..5s` |
| `not_ready_max_delay` | `1s` | 不小于 initial |
| `not_ready_total_timeout` | `10s` | 不小于 max delay，最大 `1m` |
| `interrupts_per_run_daily_quota` | `4` | `1..100`；只约束 Layer 1 Report 直接触发的 Interrupt |
| `on_interrupt_quota_exceeded` | `failure_review_once` | V0 只能为此值 |

Report 子配额统计所有 Report 直接触发的 Interrupt（含 critical）；不统计 Gate、恢复或系统事实产生的 Interrupt。触顶后拒绝后续致扰报告，并以稳定生成键最多发一条 `failure_review`；该异常打扰仍受全局注意力配额，critical 还同时受 critical fuse。Report 超限与重复判断只由确定性代码执行。

`spawning` 可返回 `not_ready`。Report 客户端以 `not_ready_initial_delay` 为首个等待间隔，按 `runtime.retry_multiplier` 指数增长并封顶于 `not_ready_max_delay`，累计等待不得超过 `not_ready_total_timeout`；因此必须满足 `not_ready_max_delay >= not_ready_initial_delay` 且 `not_ready_total_timeout >= not_ready_max_delay`。仅 `not_ready` 可重试，过期 attempt、跨 Run token、`pending/starting/finished/orphaned` 与其他鉴权/冲突错误永久拒绝，不进入退避。

### 3.11 `gate_defaults`

项目 policy 的字段全集由 `policy.md` 定义。本节只规定可被项目 policy 显式声明覆盖的全局缺省：

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `review_policy` | `always` | `always \| risky-only \| never` |
| `risky_review_threshold` | `1` | `0..100`；T3 `risk_score >= threshold` 时 `risky-only` 要求人审 |
| `auto_merge` | `false` | true 仍须通过认证与 forge capability |
| `checks_pending_timeout` | `1h` | `1m..24h` |
| `flaky_retry_limit` | `1` | `0..10` |

### 3.12 `certification`

认证阈值是**全局专有配置**，不属于项目 policy schema，项目不能覆盖：

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `total_samples_min` | `100` | `1..100000` |
| `negative_samples_min` | `30` | `1..total_samples_min` |
| `leak_rate_max` | `0.0` | `[0,1]` |
| `false_block_rate_max` | `0.2` | `[0,1]` |
| `window` | `4320h`（180 天） | `24h..8760h` |

`certification_rules_version = SHA-256(算法版本 + canonical certification 配置)`。它只标识规则、窗口与阈值；Ledger 再把该 version、任务类别和当前可重算证据版本纳入类别级 `certification_version`。任一规则、阈值、窗口或资格证据变化都必须改变最终 version；后者进入 Gate 输入快照，使旧 Gate cache 自动失效。认证仍只按任务类别聚合，不能被单条 Run 历史绕过。

### 3.13 `metrics`

北极星权重单位为“典型人工分钟”，仅作代理口径：

| reason | 默认权重 |
|--------|----------|
| `design_approval` | `10` |
| `guardrail_violation` | `5` |
| `code_review` | `15` |
| `agent_blocked` | `5` |
| `merge_conflict` | `3` |
| `failure_review` | `5` |
| `startup_stall` | `5` |

每项必须为非负 number。人的响应间隔不得替代本权重参与注意力成本计算。

### 3.14 `labels`

| 字段 | 默认 |
|------|------|
| `trigger` | `sift:run` |
| `approved` | `sift:approved` |
| `queued` | `sift:queued` |
| `running` | `sift:running` |
| `waiting_human` | `sift:waiting-human` |
| `done` | `sift:done` |
| `failed` | `sift:failed` |

标签只是投影。驱动动作必须回溯事件 actor，不能根据当前标签集合直接执行。

### 3.15 `logging`

| 字段 | 默认 | 约束 |
|------|------|------|
| `system_max_bytes` | `10485760`（10 MiB） | `1048576..1073741824` |
| `agent_max_bytes` | `52428800`（50 MiB） | `1048576..10737418240` |
| `retained_files` | `5` | `1..100` |

轮转只影响文件日志，不删除领域事件或 Ledger。

## 4. 规范化与指纹

有效配置按以下步骤生成：

1. 读取文件；不存在则使用空对象。
2. YAML 转 JSON，拒绝重复 key、非字符串 map key、alias 循环和多文档输入。
3. closed schema 校验。
4. 填入默认值，解析 duration、timezone 与路径。
5. 排序所有语义为集合的列表（operators）；保持 agents/projects 的输入顺序但以 id 校验唯一。
6. 生成 UTF-8、对象 key 词典序、无多余空白的 canonical JSON；拒绝 NaN/Infinity。
7. 对 canonical JSON 计算 SHA-256 小写十六进制 `config_hash`。
8. 将 canonical JSON 与 hash 写入配置快照；运行期只使用该内存快照。

每隔 `scheduler.config_drift_check_interval` 先比较文件存在性、mtime 与大小；可能变化时重算 hash。启动时文件缺失、运行期新出现文件也属于漂移。监测到磁盘 hash 与启动 hash 不同后：只追加一次 `config_drift_detected` 安全事件并在 `doctor` 报 warning；不得解析并应用新内容。文件恢复原 hash 后可清除当前 warning，但历史事件保留。

## 5. 启动探测

### 5.1 进程级：任一失败即拒启

1. `SIFT_HOME`、目录/文件权限与单实例互斥。单实例使用 `$SIFT_HOME/siftd.lock` advisory lock，持有整个 daemon 生命周期；不使用数据库行冒充进程锁。
2. 配置 decode、默认值填充与指纹生成。
3. SQLite 打开、PRAGMA、迁移；数据库 schema 比二进制新时拒启。
4. 每个已定义 Agent executable；仅当存在启用项目时要求至少一个可用候选 Agent。即使 Agent 暂未被项目引用也探测——Agent 定义是启动期敏感配置，V0 选择整份 closed 校验而非保留潜在坏定义。
5. Brain executable（仅配置时）。
6. `tmux`（仅有效 Agent/backend 使用时）。
7. 每个启用项目引用的 forge CLI 登录与版本；未引用的 forge 不探测。
8. 双 socket 路径可安全创建，且进程未创建 TCP/UDP listener。

所有通过 PATH 配置的 Agent、Brain、Forge 与 wrapper executable 均在启动探测时解析为绝对路径；运行期启动和进程身份记录使用该已解析路径，不再次按可能漂移的 PATH 查找。原始配置仍进入 config hash，解析结果进入启动诊断。

### 5.2 项目级：仅隔离项目

1. repo 路径、git 仓库与远端映射。
2. base 分支存在且可读。
3. base 分支 `.sift/policy.yaml` schema。
4. 项目所列 Agent 引用有效。
5. 运行期 forge `AuthOrCapability` 失效；这一项由 Intake/Forge 持续检查，不是只在启动时执行一次。

项目级失败写入项目健康投影、告警一次并由 `doctor` 报 error；其他项目继续调度。

## 6. 零配置与最小配置验收

V12 包含两个场景：

1. **配置文件缺失**：daemon 以空 operators/agents/projects 启动为健康 idle；控制面和迁移可用，不探测 `gh`、`glab`、tmux 或 Brain。没有项目不等于启动失败。
2. **最小可调度配置**：只提供一个 Agent 的 `id/executable` 与一个项目的 `id/repo/forge.kind/forge.project`，不提供任何可选字段；系统必须用本规格全部默认值完成 fake 调度。

任一可选字段缺少默认值，V12 失败。

## 7. `sift doctor` 基线退出码

| 退出码 | 语义 |
|--------|------|
| `0` | 无 warning/error |
| `1` | 至少一个 warning，无 error |
| `2` | 至少一个 error |

Daemon 不可用时，doctor 输出必须标记 `offline: true`；只能读取文件、权限、二进制与 SQLite 只读信息，绝不修改 DB、迁移或状态。

## 8. M1 验收映射

| WBS | 规格判据 |
|-----|----------|
| 1.1 / V14 | closed 配置拒绝未知/缺失歧义字段；schema 生成物漂移使 CI 失败 |
| 1.4 / V12 | 文件缺失健康 idle；最小配置所有可选值均有默认 |
| 1.5 / V10 | 双 socket、权限、探测分级和 doctor 退出码可确定验证 |
| H16 | 全局配置只启动期生效；漂移只告警 |
| DESIGN §14.2 | 开放数值及启动协议时限均有本文件中的确定性默认值 |

## 9. 自查结果

- [x] C1：Agent 级并发省略时唯一继承 `runtime.default_agent_max_concurrent`。
- [x] C2：认证阈值不属于 project policy，阈值变化可确定改变 certification version 与 Gate cache key。
- [x] C3：全局 Context、控制文件与单实例锁均有路径、权限和生命周期。
- [x] C4：Report 子配额只统计 Report 直接致扰，并确定触顶动作及 critical 口径。
- [x] C5：漂移周期/新文件、权限拒启、Agent/Brain 探测、hold 上限、持续项目探测与委派文档均明确。
- [x] canonical JSON 只在 §4 定义，storage 通过链接引用。
- [x] 所有 markdown 相对链接存在、代码围栏闭合、无尾随空白。
