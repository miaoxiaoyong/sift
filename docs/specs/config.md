---
status: active
created: 2026-07-28
summary: 全局配置、默认值、路径与启动探测契约
---

# 配置规格

本文是 `~/.sift/config.yaml`、`SIFT_HOME`、默认值及启动期能力探测的字段级契约。项目内策略由后续的 `specs/policy.md` 定义；控制面凭据由后续的 `specs/control-plane.md` 定义；持久化快照见 [`storage.md`](storage.md)。

需求与结构来源：[PRD §5.2、§5.3、§5.5、§5.7、§9、§10.2](../PRD.md)，[DESIGN §7、§9.1、§9.2、§9.4、§11、§14.2](../DESIGN.md)，[WBS M1 §1.4、M5 §5.2–§5.7](../WBS.md)。

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

M5 配额字段的后续契约落点见 [2026-07-29-m5-config-field-review-pi-gpt-5.6-sol.md](../reviews/2026-07-29-m5-config-field-review-pi-gpt-5.6-sol.md)：CAS/回滚与 critical admission 在 [`storage.md` §6.3、§9](storage.md)，batch/member/operation 在 [`storage.md` §6.3–§6.4](storage.md) 与 [`outbox.md` §10](outbox.md)，逐成员 Ledger 身份在 [`ledger.md` §2.4](ledger.md)。此处只记录当前字段契约，不改变该存档评审的历史结论。

## 1. 适用范围与不变量

1. 全局配置是 **closed contract**：未知字段、字段缺失导致的歧义、错误类型和未知枚举一律拒绝。
2. YAML 先转为 JSON，再经全系统唯一 decode gateway 的 `closed` 策略校验；业务代码不得另开 YAML/JSON 解码入口。
3. daemon 启动时读取一次、规范化、计算指纹并持久化快照；磁盘变化只告警，不改变有效配置。`sift init`、`sift project add|remove`、`sift agent add|remove` 写入后，运行中的受管服务自动 restart；前台 daemon 明确提示重启。运行中的 Run/attempt 仍只使用冻结 config snapshot。
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
| `backend` | enum | `runtime.backend` | `process \| tmux`；非空时覆盖根默认 |
| `max_concurrent` | integer | `runtime.default_agent_max_concurrent` | `1..32`；Agent 省略时继承根配置 |
| `version_args` | `string[]` | `["--version"]` | 启动探测 argv；空数组表示只探测 executable 可执行 |

`task_transport=stdin` 时禁止 `{task_file}`；`file` 时必须恰有一个完整 argv token 等于 `{task_file}`，wrapper 只做单 token 替换，不做字符串插值或 shell 展开。Agent 定义不接受任意环境变量映射。Runtime 只注入协议明确允许的非机密变量（V0 为 `SIFT_RUN_DIR`）；凭据不得经配置、argv 或环境变量传递。规范化按 `agent.backend ?? runtime.backend` 为每个 Agent 产生 concrete backend；Run/attempt 从其冻结 config snapshot 写入 `attempts.backend`，retry 与 daemon 重启不得重新读取当前磁盘配置。执行、PTY、tmux session 与资格契约见 [`runtime.md`](runtime.md)。

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
| `retry_multiplier` | `2.0` | JSON number in `1..10`, with at most 6 fractional decimal digits; exact millionths representation required (no exponent notation) |
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

### 3.7.1 `attention.channels[]`

Channel 是附加投递面；forge comment 首发不属于此 registry。有效 config snapshot 中每个 Channel 由下列 closed object 冻结：

```yaml
attention:
  channels:
    - id: ops-slack
      type: webhook
      enabled: true
      target: {secret_ref: SIFT_CHANNEL_OPS_SLACK}
      capabilities: [text]
      renderer: plain-v1
      default: true
```

| 字段 | 类型/约束 |
|------|-----------|
| `id` | string；`^[a-z][a-z0-9-]{0,62}$`，全局唯一 |
| `type` | `webhook`；新增类型必须版本化并注册适配器 |
| `enabled` | boolean；false 的 Channel 不进入候选 |
| `target` | closed object；V0 仅 `{secret_ref: string}`，值为环境/密钥存储引用；不得写 URL、token 或凭据明文 |
| `capabilities` | closed set；V0 允许 `voice`、`text`、`visual`，至少一项，排序去重 |
| `renderer` | `plain-v1`；只接受由服务端确定性 renderer 生成的 Interrupt/batch 文本 |
| `default` | boolean；启用 Channel 中最多一个为 true；未声明时按 `id` UTF-8 bytes 最小者作为默认 |

Channel 的 `target` 只在启动探测时解析并验证；凭据引用本身进入 canonical snapshot，解析值不进入 config JSON、日志、Brain 输入或 outbox payload。payload 的唯一 `target_ref` 是非秘密 handle `secret_ref:<name>`，`<name>` 逐字节等于该 `target.secret_ref`；它不是 URL 或已解析 endpoint。候选算法为：从**创建 Run 冻结的 config snapshot** 取 `enabled=true`、capability 包含 `min_modality` 且项目未隔离的 Channel，按 `id` UTF-8 bytes 排序；default 必须属于该集合，否则选择排序后的第一项。零候选不调用 T6，Interrupt 仍完成 forge 首发，并以 `held_reason=no_compatible_channel|channel_isolated` 保存。

每条单 delivery 和每个 attention batch 都冻结 `channel_id/type/target_ref/capabilities/renderer` snapshot；outbox 只携带该 handle 和非秘密 snapshot，不从当前配置重建，不携带凭据或 endpoint。Channel adapter 是唯一 resolver owner：每个 executing attempt 从 sealed handle 解析当前 endpoint，故 secret rotation 在下一 attempt 生效；缺失/拒绝 handle 为 `auth_or_capability`，不合契约的解析值为 `contract_violation`。运行期 Channel isolation 只影响新的 delivery，既有 Interrupt 的冻结选择不漂移；失败按 outbox 的唯一告警和重试规则处理。

### 3.8 `forge`

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `hourly_api_limit` | `1000` | `1..100000` |
| `warning_ratio` | `0.8` | `(0,1)` |
| `slow_poll_interval` | `5m` | 不小于 active interval |
| `command_timeout` | `30s` | `1s..10m` |

API 只在 Forge 适配层收费。达到 `warning_ratio` 后降低轮询频率；达到上限后不执行非必要轮询，并发一次告警，不得影响注意力配额的硬约束。

### 3.9 `attention`

Severity 枚举为 `low | normal | high | critical`。本节是 M5 Attention 的唯一全局配置入口；发射、批处理和递送的对象/事务契约见 [`interrupt.md`](interrupt.md)、[`storage.md` §9](storage.md) 与 [`outbox.md` §10](outbox.md)。`critical` 不占日配额，但绝不是无限制通道：它仍受 `critical_fuse` 的硬熔断约束。

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
  reason_defaults:
    design_approval:
      expires_after: 24h
      on_expire: hold
      on_max_escalations: hold
    guardrail_violation:
      expires_after: 24h
      on_expire: hold
      on_max_escalations: hold
    code_review:
      expires_after: 72h
      on_expire: hold
      on_max_escalations: hold
    agent_blocked:
      expires_after: 8h
      on_expire: escalate
      on_max_escalations: auto_reject
    merge_conflict:
      expires_after: 8h
      on_expire: escalate
      on_max_escalations: auto_reject
    failure_review:
      expires_after: 24h
      on_expire: auto_reject
      on_max_escalations: auto_reject
    startup_stall:
      expires_after: 1h
      on_expire: escalate
      on_max_escalations: hold
  daily_summary_at: "09:00"
  channels: []
```

| 字段 | 默认 | 约束 |
|------|------|------|
| `day_timezone` | `local` | `local` 或 IANA timezone；有效快照中一律为具体 IANA zone |
| `daily_quota.low` | `3` | `0..1000` |
| `daily_quota.normal` | `5` | `0..1000` |
| `daily_quota.high` | `5` | `0..1000` |
| `max_escalations` | `2` | `0..10`；一条 Interrupt 初发后最多允许的升级次数 |
| `critical_fuse.window` | `15m` | `1m..24h` |
| `critical_fuse.total_limit` | `5` | `1..1000`；窗口内全局 critical 上限 |
| `critical_fuse.per_run_limit` | `2` | `1..total_limit`；同窗口同一 Run 的 critical 上限 |
| `daily_summary_at` | `09:00` | `HH:MM`；按 `day_timezone` 解释，而非 daemon 所在机器的时区 |
| `channels` | `[]` | closed Channel objects，字段契约见 §3.7.1 |
| `hold_max_duration` | `720h`（30 天） | `1m..8760h` |
| `channel_failure_alert_after` | `3` | `1..100`；连续失败后改走 forge 告警评论 |

`daily_quota` 是 closed map，只允许 `low`、`normal`、`high` 三个键；省略的键采用表中默认值，不得声明 `critical` 或其他 severity。每个非 critical Interrupt 在首次发射时以其确定性 severity 尝试原子扣一格对应日配额；同 generation 重放、Channel 重试、升级重推和关闭均不得再扣或退款。日桶和 `daily_summary_at` 都按发射时冻结的规范化 `day_timezone` 计算。扣费比较-and-set 的零行结果**不是**额度耗尽：必须以同一稳定 generation/admission key 重读权威 counter；若 `consumed + 1 <= limit`，在有界重试内重新 CAS，只有重读证明 `consumed + 1 > limit` 才可把原 Interrupt 入批。不可恢复的 SQLite/事务/存储错误整笔回滚，不得伪装成额度耗尽或合批。成功扣费与配额拒绝入批分别写不可变 admission；后者不产生借支或伪造 charge，见 [`storage.md` §6.3、§9.1](storage.md)。故障与并发的派生验收固定如下：在同一冻结 quota counter、`limit=2` 下两个并发候选均以 `quota_charged` 成功且最终 `consumed=2`；在 `limit=1` 下恰有一条 `quota_charged`/charge、另一条 `quota_batched`/NULL charge。CAS 重试耗尽、无法重读、SQLite/事务故障时，Interrupt、admission、counter、budget entry、batch member 和 outbox operation 全部回滚，结果为错误而不是 `quota_batched`；重试只能从未提交状态重新开始。

`critical_fuse` 使用真实滑动窗口，不得以自然日或固定桶近似。首次发射为 critical 由 `EmitInterrupt`、升级后首次成为 critical 由 `AdvanceInterrupt` 在各自的 Interrupt CAS 事务中执行同一全局和 per-Run 检查；二者都以每个 Interrupt 至多一条的 append-only critical admission evidence 计数，而不是以 attention charge 计数。任一候选会使计数超过对应 limit 时，熔断该候选的单独 critical 递送，归入该窗口的唯一汇总 HITL；同刻同时命中时全局 scope 优先于 per-Run scope，因此一个 Interrupt 只属于一个汇总。重放、重推和旧 tick 不重复写 admission，升级也不新增 attention charge。`quota_batched → critical` 的固定 vectors 为：fuse 有余量时写一行 `critical_admitted` 且 charge 仍为 NULL；fuse 饱和时写一行 `critical_fused` 并加入唯一 critical batch；两条路径重放 `AdvanceInterrupt` 都返回原 admission/batch，不新增 admission、charge、member 或 operation。汇总不是对原 critical 的借支、升级重推或第二条逐项 critical 递送；源事实和熔断决定必须可审计。证据、窗口边界、episode 和 batch 身份以 [`storage.md` §6.3、§9.3](storage.md) 为准。固定验收 vectors 为：窗口 `10m` 时，`now=t+window-1ms` 仍计入，`now=t+window` 不计入，`now=t+window+1ms` 也不计入；同一毫秒的并发 admission 以唯一 admission key 串行化，恰有一条 admission 占用名额。若 `due_at` 重裁决时新 evidence 仍使 scope 饱和，旧 episode 不改 due、不丢成员：事务先 seal/cancel 旧 `attention_batches`，再以当前最早仍计数 admission 打开 successor episode；只有 evidence 少于 limit 才允许后续 candidate 创建新 episode。恢复扫描重复执行该动作只返回已存在的 batch/operation，不创建第三批。

#### 日历与摘要 batch

启动规范化时，`day_timezone=local` 必须解析为当次启动环境可识别的具体 IANA zone；该解析后的 IANA 名称替换 `local` 进入有效 canonical JSON、`config_hash` 和持久化 config snapshot。不能取得稳定 IANA 名称时拒绝启动并要求显式 IANA zone；运行期不得再次读取机器 local zone。显式 IANA zone 同样在启动期加载并校验。

对在 `t` 入批的对象，`due_at` 是其冻结 zone 中**严格晚于** `t` 的下一次 `daily_summary_at`；恰在该时刻入批也取次日，不立即补发。该 local wall-clock 时刻落入 DST gap 时取 gap 后第一个有效 instant；落入 DST fold 时取第一次出现（较早的 UTC instant）。batch 同时冻结成员各自的 quota day、zone、`due_at`、Channel snapshot 和成员快照；同一 zone、scheduled occurrence 与冻结 Channel 至多一个 daily batch，因此不得把 batch 级 quota day 编进 identity。固定 epoch vectors（时间均为 UTC 毫秒；每行明确完整生效 `day_timezone/daily_summary_at`）如下：

| 生效配置 / 入批 instant | 期望结果 |
|---|---|
| `Asia/Shanghai` / `09:00`，`2026-07-28T15:59:59.999Z` (`1785254399999`，本地 `23:59:59.999`) | `quota_day=2026-07-28`, `due_at_ms=1785286800000`（本地 `2026-07-29 09:00`） |
| `Asia/Shanghai` / `09:00`，`2026-07-28T16:00:00.000Z` (`1785254400000`，本地 `00:00:00.000`) | `quota_day=2026-07-29`, `due_at_ms=1785286800000`（本地 `2026-07-29 09:00`） |
| `Asia/Shanghai` / `09:00`，`2026-07-28T08:59:59Z` (`1785229199000`) | `quota_day=2026-07-28`, `due_at_ms=1785286800000` |
| `Asia/Shanghai` / `09:00`，`2026-07-29T00:59:59Z` (`1785286799000`) | `quota_day=2026-07-29`, `due_at_ms=1785286800000` |
| `America/New_York` / `02:30`，`2026-03-08T06:59:00Z` (`1772953140000`，本地 `01:59 EST`) | `quota_day=2026-03-08`, `due_at_ms=1772953200000`（gap 后本地 `03:00 EDT`） |
| `America/New_York` / `01:30`，`2026-11-01T05:29:00Z` (`1793510940000`，本地第一次 `01:29 EDT`) | `quota_day=2026-11-01`, `due_at_ms=1793511000000`（fold 第一次本地 `01:30 EDT`） |

入批恰在 `daily_summary_at` 的 instant（`1785286800000`）与其后一毫秒都取下一次 occurrence，不能立即补发。前一日摘要时刻后至次日摘要时刻前的两个 quota day 成员，只有冻结 Channel、project 与完整已验证 Forge discussion target 都相同才加入同一个 `daily:<project_id>:<zone>:<due_at_ms>:<channel_id>:<forge_kind>:<base64url(forge_host)>:<base64url(forge_project_key)>:<target_kind>:<base64url(target_id)>` batch；不同 Channel、项目或任一 target 字段必为不同 batch。运行期机器时区变化不影响已冻结 snapshot/hash 或历史回放。daily batch 的稳定键和关闭成员、发送 payload 的规则见 [`storage.md` §6.3、§6.6](storage.md) 与 [`outbox.md` §10](outbox.md)；并发、拒绝混批与响应丢失重放统一复用 storage §6.6 的 exact bytes fixture。

#### `attention.reason_defaults`

`reason_defaults` 是 closed map，只允许 PRD 的七个 reason：`design_approval`、`guardrail_violation`、`code_review`、`agent_blocked`、`merge_conflict`、`failure_review`、`startup_stall`。该 map 及其中每个 reason object 都可省略；省略字段逐项采用下表默认值。出现的 reason object 只允许 `expires_after`、`on_expire`、`on_max_escalations`，未知 reason 或字段拒绝。每条 Interrupt 在创建时冻结三个解析后的值，后续重启或配置变更不得改写已存在 Interrupt。

| 字段 | 约束/语义 |
|------|-----------|
| `expires_after` | duration，`1m..8760h`；创建时令 `expires_at = created_at + expires_after` |
| `on_expire` | `hold \| escalate \| auto_reject`；到期 supervisor 的动作 |
| `on_max_escalations` | `hold \| auto_reject`；只有 `on_expire=escalate` 且已用尽 `max_escalations` 时的确定性去向 |

| reason | `expires_after` | `on_expire` | `on_max_escalations` |
|--------|-----------------|-------------|----------------------|
| `design_approval` | `24h` | `hold` | `hold` |
| `guardrail_violation` | `24h` | `hold` | `hold` |
| `code_review` | `72h` | `hold` | `hold` |
| `agent_blocked` | `8h` | `escalate` | `auto_reject` |
| `merge_conflict` | `8h` | `escalate` | `auto_reject` |
| `failure_review` | `24h` | `auto_reject` | `auto_reject` |
| `startup_stall` | `1h` | `escalate` | `hold` |

达到上限不是再提高 severity 或延后检查：按冻结的 `on_max_escalations` 立即收敛。`hold` 保持当前 Interrupt/Run 的人工等待语义；`auto_reject` 走该 reason 已有的拒绝状态机，不能由配置伪造 `approve`。`startup_stall.on_expire=auto_reject` 与 `startup_stall.on_max_escalations=auto_reject` 一律在 schema 和运行时拒绝；它在封顶后保持 `hold`、Interrupt open 与 attempt 隔离，且不写 resolution，详见 [`interrupt.md` §4.1、§6](interrupt.md)。`auto_approve` 在任意字段都不是合法值。

`hold_max_duration` 是每条 `/sift hold <duration>` 指令的单次上限，不限制人多次显式 hold。

### 3.10 `report`

| 字段 | 默认 | 约束/语义 |
|------|------|-----------|
| `events_per_minute` | `12` | `1..10000` |
| `burst` | `4` | `1..1000` |
| `dedupe_window` | `30s` | `0s..1h` |
| `max_payload_bytes` | `65536` | `1024..1048576` |
| `not_ready_initial_delay` | `100ms` | duration exactly representable as integer milliseconds; `10ms..5s` |
| `not_ready_max_delay` | `1s` | duration exactly representable as integer milliseconds; not less than initial |
| `not_ready_total_timeout` | `10s` | duration exactly representable as integer milliseconds; not less than max delay, maximum `1m` |
| `interrupts_per_run_daily_quota` | `4` | `1..100`；只约束 Layer 1 Report 直接触发的 Interrupt |
| `on_interrupt_quota_exceeded` | `failure_review_once` | V0 只能为此值 |

Report 子配额统计所有 Report 直接触发的 Interrupt（含 critical）；不统计 Gate、恢复或系统事实产生的 Interrupt。触顶后拒绝后续致扰报告，并以稳定生成键最多发一条 `failure_review`；该异常打扰仍受全局注意力配额，critical 还同时受 critical fuse。Report 超限与重复判断只由确定性代码执行。

`spawning` 可返回 `not_ready`。配置加载器拒绝 exponent notation、超过六位小数的 `runtime.retry_multiplier`，以及不能精确表示为整数毫秒的三个 `not_ready_*` duration（例如 `10.5ms`）；不得 round/floor/ceil。规范化后唯一 wire 导出为 `multiplier_micros = retry_multiplier × 1,000,000` 与整数 `*_delay_ms`，并拒绝整数计算溢出。CLI 与 daemon 都 fail closed：`initial_delay_ms <= max_delay_ms <= total_timeout_ms`；未知或额外 retry 字段、非法策略和序列中策略漂移均拒绝。仅 `not_ready` 可重试，过期 attempt、跨 Run token、`pending/starting/finished/orphaned` 与其他鉴权/冲突错误永久拒绝，不进入退避。

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

`certification_rules_version = SHA-256(算法版本 + canonical certification 配置)`，只标识规则、窗口与阈值。Ledger 为每个 task kind 独立计算 `certification_version`（又名证据 revision）：`SHA-256({task_kind, certification_rules_version, evidence_digest})`；二者不得混名或互相代替。规则变化或影响资格的样本进入/离开窗口都会改变 revision。两者均进入 Gate 输入快照，使旧 Gate cache 自动失效。认证仍只按任务类别聚合，不能被单条 Run 历史绕过。

### 3.13 `metrics`

北极星权重单位为“典型人工分钟”，仅作 [PRD §10.2](../PRD.md) 的代理口径：

```yaml
metrics:
  attention_weight_minutes:
    design_approval: 10
    guardrail_violation: 5
    code_review: 15
    agent_blocked: 5
    merge_conflict: 3
    failure_review: 5
    startup_stall: 5
```

`attention_weight_minutes` 是 closed map，只允许 PRD 的七个 reason；该 field 可省略，出现时其中省略的 reason 采用下表默认值。不得添加未知 reason 或按 severity/Channel 定义另一张权重表。每项是有限 JSON number，范围 `0..1440`，单位为典型人工分钟；允许小数以支持人工抽样校准。默认值如下：

| reason | 默认权重（分钟） |
|--------|------------------|
| `design_approval` | `10` |
| `guardrail_violation` | `5` |
| `code_review` | `15` |
| `agent_blocked` | `5` |
| `merge_conflict` | `3` |
| `failure_review` | `5` |
| `startup_stall` | `5` |

加权打扰分子对每个首次成功送达的 attention metric identity 恰取一次其 reason 的权重；该 identity 固定为 [`storage.md` §6.3](storage.md) 的 `metric_identity=<interrupt_id>`，而非会在初发/critical 升级间变化的 admission ID。同一 lineage 的重试、升级重推和重复送达不得重复加权；但每次 delivery 仍保留其真实 admission ID 以供审计。非 critical 合批保留源 Interrupt/admission 的 reason；配额拒绝的 batch member 没有伪造 budget charge，仍按其唯一 metric identity 与真实成功 batch delivery 计入。未成功送达的 admission 不计入该北极星分子。权重取该 admission 所属 Run 创建时冻结的 `config_snapshot_id` 中的值；指标查询必须使用持久化快照，而不得以当前配置重算历史。配置变更后，新 Run 使用新值，既有 Run 与历史序列保持原值。

人的响应间隔只能作为 T6 调度特征；不得替代、校正或隐式乘入本权重。权重表仅定义北极星分子，不能改变注意力配额、critical 熔断、Gate 阈值或单条 HITL 决定。

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

每个 label 是 1–256 UTF-8 bytes、不得含 NUL，按配置字节精确匹配（不 trim、不 case-fold、不做平台重写）；未知字段和相同标签值均拒绝。labels 是全局配置、但在每个 Run 的启动 `config_snapshot_id` 中冻结，并按该 Run 的 project/Forge platform 使用。驱动动作必须回溯事件 actor，不能根据当前标签集合直接执行；Command 的 approval label 只接受该冻结 `approved` 值，重启前的配置文件漂移不改变既有 Run。

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
6. `tmux`（`runtime.backend=tmux` 或任一 effective Agent backend 为 tmux 时）：解析 absolute path，`tmux -V` 必须为 `>=3.2` 且支持 `new-session ... [shell-command [argument ...]]` 的多 argv 形态；process-only 配置不探测。
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

## 8. 验收映射

| WBS | 规格判据 |
|-----|----------|
| 1.1 / V14 | closed 配置拒绝未知/缺失歧义字段；schema 生成物漂移使 CI 失败 |
| 1.4 / V12 | 文件缺失健康 idle；最小配置所有可选值均有默认 |
| 1.5 / V10 | 双 socket、权限、探测分级和 doctor 退出码可确定验证 |
| H16 | 全局配置只启动期生效；漂移只告警 |
| M5 §5.2–§5.3 / V8、V13 | 非 critical CAS 竞争重读重试、权威超额才合批；critical 用真实滑动全局/每 Run 双阈值 admission 熔断，初发/升级首次进入、重推和 batch 边界均可验证 |
| M5 §5.3 | 七个 reason 的 expiry/封顶去向冻结；`startup_stall` 的 `auto_reject` 在 schema 与运行时都被拒绝 |
| M5 §5.7 | 七项权重 closed/default/range 可校验；同一 attention admission 只贡献一次、历史权重不被新配置改写 |
| DESIGN §14.2 | 开放数值及启动协议时限均有本文件中的确定性默认值 |

## 9. 自查结果

- [x] C1：Agent 级并发省略时唯一继承 `runtime.default_agent_max_concurrent`。
- [x] C2：认证阈值不属于 project policy，阈值变化可确定改变 certification version 与 Gate cache key。
- [x] C3：全局 Context、控制文件与单实例锁均有路径、权限和生命周期。
- [x] C4：Report 子配额只统计 Report 直接致扰，并确定触顶动作及 critical 口径。
- [x] C5：漂移周期/新文件、权限拒启、Agent/Brain 探测、hold 上限、持续项目探测与委派文档均明确。
- [x] M5：非 critical 配额的 CAS/回滚边界、critical admission、摘要日历/批次引用、reason 封顶去向与指标权重均有 closed/default/freeze 契约；本项不改变任何字段评审的历史结论。
- [x] canonical JSON 只在 §4 定义，storage 通过链接引用。
- [x] 所有 markdown 相对链接存在、代码围栏闭合、无尾随空白。
