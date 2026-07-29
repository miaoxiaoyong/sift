---
status: draft
created: 2026-07-29
summary: Agent Layer 1 上报的 run.sock、事件、去重与配额契约
---

# Report 规格

本文冻结 M5 `sift report` 的 Layer 1 上报契约：它只经 `run.sock` 使用 attempt 作用域 token，上报只留下事件和可选的 Attention 输入，不能裁定或推进 Run。控制面 envelope、socket 权限和控制文件格式以 [`control-plane.md`](control-plane.md) 为准；本文件定义其尚未展开的 CLI、payload、去重与配额语义。

来源：[PRD §5.8、§9.1 TM5](../PRD.md)、[DESIGN §8.9](../DESIGN.md)、[WBS M5 §5.5](../WBS.md)、[ADR-006](../decisions/006-report-via-cli-not-mcp.md)、[ADR-008](../decisions/008-control-plane-endpoints-and-capabilities.md)。配置数值见 [`config.md` §3.10](config.md)，receipt、令牌桶和写端口见 [`storage.md` §7.4、§9、§11](storage.md)，直接致扰的对象和收费口见 [`interrupt.md`](interrupt.md)。

> 本文是 M5 实现前的 draft；不得把本文件或既有 schema 误述为 Report 已接通。

## 1. 范围与不变量

1. Layer 1 的四种 report kind 是 `progress`、`goal`、`blocker`、`completed`。它们的来源一律为 `agent`，是 Agent 的自述，不是执行、提交、Change 或 Gate 的证据。
2. `sift report` **只**连接 `$SIFT_HOME/run.sock` 并只调用 `report.submit`；它不得连接 `siftd.sock`、读取 `operator.token`、直写 SQLite，或在 daemon 不可用时落文件/离线写入。
3. CLI 只从 `SIFT_RUN_DIR/control.json` 安全读取 run token 及绑定的 `run_id`、`attempt_no`、`generation`。`SIFT_RUN_DIR` 是唯一注入 Agent 环境的 Sift 变量；token 不进入环境、argv、stdout/stderr、事件、日志或错误文本。
4. run token 只授权绑定 attempt 的 `report.submit`，不能调用 `claim.*` 或任何 `ops.*`。这限定的是 Report 凭据的能力；同 UID Agent 在 V0 读取 operator token 的未闭合边界仍按 ADR-008 如实报告。
5. 接受的 report 至少写一条 append-only `events` 行和一条 `report_receipts` 行。`progress`、`goal`、`completed` 永远只写事件；`blocker` 也不得直接写 `runs.status`，但可按 §5 在同一事务中生成 `agent_blocked` Interrupt。
6. `completed` 只是完成声明，绝不表示 Run done，也不触发 Change 创建；最终裁定只来自 Layer 2 的进程结果和 Gate。
7. 鉴权、payload 校验、canonicalization、去重、限流和配额均为确定性代码；LLM 不参与任何一个决定。

## 2. CLI 与 RPC

CLI 的稳定形状为：

```text
sift report <progress|goal|blocker|completed> --key <report-key> --payload <json>
```

`--key` 必填；调用方在同一逻辑上报的所有重试中复用它，而不是每次生成新 key。`--payload` 必须恰为 §3 对应 kind 的 closed JSON object；CLI 不接受 stdin、文件名、任意额外 flag 或未知子命令作为绕过 payload schema 的入口。CLI 在本地先拒绝缺失/不安全的 `SIFT_RUN_DIR`、`control.json`、token 或绑定字段，不尝试任何其他凭据或 socket。

CLI 将 control 文件中的 binding 和调用方字段组成 [`control-plane.md` §5.2](control-plane.md) 的唯一请求：

```json
{
  "method": "report.submit",
  "auth": {"kind": "run_token", "token": "<control.json run_token>"},
  "params": {
    "run_id": "<control.json>",
    "attempt_no": 1,
    "generation": 1,
    "report_key": "<--key>",
    "kind": "progress",
    "payload": {"message": "已完成检索"}
  }
}
```

`report_key` 是 32 个小写十六进制字符。成功 result 为：

```json
{"disposition":"accepted|duplicate","receipt_id":"...","event_id":"..."}
```

`duplicate` 总是指向最初已接受的 receipt/event；它不是第二次写入。失败沿用控制面错误 envelope，且不得返回 token 或控制文件内容。

## 3. Payload v1

所有 payload 是 UTF-8、closed JSON object；未知字段、缺字段、错误类型、空字符串、NUL 和 Unicode `Cc` 控制码点均拒绝。单个 string 不得含 CR、LF；这让 event 可安全投影到单行时间线，也不会把 Agent 文本变成 renderer 控制序列。编码后的 canonical payload 不得超过 `report.max_payload_bytes`；RPC frame 的 1 MiB 上限仍独立适用。

| kind | payload v1 | 语义 |
|---|---|---|
| `progress` | `{"message":"..."}` | 当前工作的简短进度事实 |
| `goal` | `{"goal":"..."}` | Agent 计划、开始或完成的一个目标声明 |
| `blocker` | `{"blocker_summary":"...","attempted_summary":"...","recommended_action":"..."}` | 无法自行继续的事实、已尝试事项和建议动作 |
| `completed` | `{"summary":"..."}` | Agent 声称已完成的摘要，不是完成证据 |

`blocker` 的 `agent_log_ref` 不是 Agent 可任意指定的 payload 字段；服务端从绑定 attempt 的受控日志位置产生它。若该位置不能形成 [`interrupt.md` §3.3](interrupt.md) 合法链接，blocker 仍可作为普通 report 事件接受，但不得生成 Interrupt，并记确定性诊断。

对每个请求，服务端计算：

```text
payload_digest = SHA-256(canonical JSON of
  {"kind": <kind>, "payload": <payload>})
```

因此相同 `report_key` 改 kind 也必为冲突，而不是误认成重放。digest 和 canonical JSON 均不接受调用方传入的预计算值。

## 4. 尝试阶段与重试

服务端先按 run token 验证 `(run_id, attempt_no, generation)`，再判定 attempt 阶段：

| 条件 | 结果 | CLI 行为 |
|---|---|---|
| 绑定 attempt 为 `running` | 按 §5 处理 | 正常返回 |
| 为 `spawning`，且 permit 已签发、`claim.started` 尚未提交 | `not_ready`，仅 details `retry_after_ms` | 只重试这一种结果 |
| `pending`、`starting`、`finished`、`orphaned`，或 phase 已越过 | 永久拒绝 | 不重试 |
| token/binding 跨 Run、attempt 或 generation | `unauthorized` 或 `stale` | 不重试 |

对 `not_ready`，首个等待为 `report.not_ready_initial_delay`，随后按 `runtime.retry_multiplier` 指数增长并封顶 `report.not_ready_max_delay`；累计等待不超过 `report.not_ready_total_timeout`。每次重试复用相同 report key 和 payload。任何其他错误（包括限流、配额冲突及 payload/schema 错误）都不进入该退避循环。

## 5. 接受、事件与去重

### 5.1 事件投影

接受的请求在同一事务插入 source=`agent`、稳定 type=`report.progress`、`report.goal`、`report.blocker` 或 `report.completed` 的 event，以及对应不可变 `report_receipts` 行。event 的 `payload_schema_version=1`，其 `payload_json` 严格为：

```json
{
  "report_key": "...",
  "payload_digest": "...",
  "generation": 1,
  "report": {}
}
```

`report` 是 §3 的原 payload；Run 和 attempt 身份由 event 的列承载，不从 Agent payload 复制。receipt 的 `report_kind`、`payload_digest`、`event_id` 和 `received_at_ms` 是事件的审计锚点。事件顺序仅是本地提交顺序，不把 Agent 声称的时间当作事实发生时间。

`blocker` 在 payload 和 attempt 日志引用均可形成 [`interrupt.md` §3](interrupt.md) 的 `agent_blocked` 最小事实时，使用新 receipt 的 ID 作为 `report_id` 调用唯一 `EmitInterrupt` 入口。它不得自行指定 severity、options 或 generation key。无论该 Interrupt 因注意力规则被合批/拒发，report receipt 与事件仍保留；它们不改变 Run 状态。

### 5.2 两层去重

去重以绑定 attempt 为域，发生在任何令牌或子配额消费之前：

1. **idempotency key：** 已存在 `(run_id, attempt_no, report_key)` 时，若 `payload_digest` 相同，返回原 receipt 的 `duplicate`；不同则返回不可重试 `conflict`。这条规则不随时间过期。
2. **语义窗口：** 当 `report.dedupe_window > 0`，若同一 attempt 已在 `[now - dedupe_window, now]` 接受相同 `(kind, payload_digest)`，新 key 也返回那个原 receipt 的 `duplicate`。窗口为 `0s` 时只保留第 1 层。

两种 duplicate 都不消费 report token、不增加 report 子配额、不创建 event/receipt/Interrupt，也不占用新 key。限流或配额拒绝只记录安全事件，绝不占用 report key；调用方修正或等待后可用相同 key 再试。

## 6. 令牌桶与 Interrupt 子配额

### 6.1 上报速率

对非 duplicate、阶段合法的 report，`RecordReport` 使用 [`storage.md` §9.2](storage.md) 的持久化整数令牌桶：

```text
kind     = report
scope_id = run:<run_id>:attempt:<attempt_no>
capacity = report.burst
refill   = report.events_per_minute / 60s
```

补充、余数和 CAS 严格按 storage 规格；不得以固定一分钟窗口替代。桶不足时请求以不可重试 `conflict` 拒绝，只写安全事件，不写 receipt/event，也不占用 key。重启不重置桶。

### 6.2 Report 直接致扰的每日子配额

只有实际由 `blocker` report 直接调用 `EmitInterrupt` 的 `agent_blocked` 计入该子配额；`progress`、`goal`、`completed`，以及 Gate、恢复和其他系统事实创建的 Interrupt 一律不计。计数使用：

```text
kind=report, scope=run, scope_id=<run_id>, amount=1
```

日桶按 `attention.day_timezone` 切分，limit 为 `report.interrupts_per_run_daily_quota`。该 report entry、attention 入口、receipt/event 和 Interrupt 必须在一笔事务中成功或一笔回滚；重放和语义 duplicate 不重复收费。critical 若由未来扩展的 Report 直接产生，仍计入本子配额，并同时遵守 attention critical fuse。

当子配额已满，后续会直接致扰的 blocker 不创建 receipt/event/普通 Interrupt，并只追加安全事件。按 `on_interrupt_quota_exceeded=failure_review_once`，系统以 `(run_id, daily_bucket_start_ms)` 为稳定异常键至多发一个 `failure_review`；它说明“Report Interrupt 子配额耗尽”，不接受 Agent 自由文本作为失败事实。该异常仍经过 `EmitInterrupt` 的全局 attention 配额和 critical fuse；若它们拒发，系统只保留安全事件，绝不借支或另开告警旁路。

这条异常键是 Report 专用生成域，不得误用某个 attempt 的 `failure_review` generation key；实现时须将该 domain/version key 同步纳入 [`interrupt.md`](interrupt.md) 的生成键契约。

## 7. 原子边界与验收

`RecordReport` 是唯一写入口。对一个新、可接受的 report，它以单事务完成 token bucket CAS、receipt、event，以及适用时的 Report 子配额和 `EmitInterrupt`；任一步失败则没有部分可见状态。安全审计与领域 event 分开：被拒绝的 report 绝不伪造成 Agent 已上报的领域事件。

M5 至少覆盖：

1. CLI 只读取 run dir 控制文件、只连 `run.sock`；operator token、`siftd.sock` 和 SQLite 均不可作为 fallback。
2. run token 的跨 Run/attempt/generation、调用 `claim.*`/`ops.*`、以及 `pending`/`starting`/终态上报全部拒绝；仅合法 `spawning` 返回有界 `not_ready`。
3. 四类 payload 的 closed schema、canonical digest、大小上限和 token 脱敏；`completed` 与所有其他 report 均不改变 `runs.status`。
4. 同 key 同 digest、同 key 异 digest、窗口内新 key 同语义、窗口到期和 `dedupe_window=0` 分别有可断言结果；所有 duplicate 零收费、零新 event。
5. `burst=4` 跨固定分钟边界不瞬时超发，重启后桶连续；超限与配额拒绝没有 receipt/event/key 占位。
6. blocker 的 receipt/event、直接 `agent_blocked`、report 子配额和异常 `failure_review_once` 的并发调用均验证“一次或零次”的事务结果；四个并发触顶者最多产生一条当日异常 Interrupt。
