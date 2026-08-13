---
status: active
created: 2026-07-29
summary: Agent Layer 1 上报的 run.sock、事件、去重与配额契约
---

# Report 规格

本文冻结 M5 `sift report` 的 Layer 1 上报契约：它只经 `run.sock` 使用 attempt 作用域 token，上报只留下事件和可选的 Attention 输入，不能裁定或推进 Run。控制面 envelope、socket 权限和控制文件格式以 [`control-plane.md`](control-plane.md) 为准；本文件定义其尚未展开的 CLI、payload、去重与配额语义。

来源：[PRD §5.8、§9.1 TM5](../PRD.md)、[DESIGN §8.9](../DESIGN.md)、[WBS M5 §5.5](../WBS.md)、[ADR-006](../decisions/006-report-via-cli-not-mcp.md)、[ADR-008](../decisions/008-control-plane-endpoints-and-capabilities.md)。配置数值见 [`config.md` §3.10](config.md)，receipt、令牌桶和写端口见 [`storage.md` §7.4、§9、§11](storage.md)，直接致扰的对象和收费口见 [`interrupt.md`](interrupt.md)。

> 字段评审已由[第六次定向复审 PASS](../reviews/2026-07-29-m5-report-field-rereview-6-pi-gpt-5.6-sol.md)确认；本规格 `active` 不表示 Report 或 M5 已接通，也不表示阶段门禁已通过。

## 1. 范围与不变量

1. Layer 1 的四种 report kind 是 `progress`、`goal`、`blocker`、`completed`。它们的来源一律为 `agent`，是 Agent 的自述，不是执行、提交、Change 或 Gate 的证据。
2. `sift report` **只**连接 `$SIFT_HOME/run.sock` 并只调用 `report.submit`；它不得连接 `siftd.sock`、读取 `operator.token`、直写 SQLite，或在 daemon 不可用时落文件/离线写入。
3. CLI 只从 `SIFT_RUN_DIR/control.json` 安全读取 run token 及绑定的 `run_id`、`attempt_no`、`generation`。`SIFT_RUN_DIR` 是唯一注入 Agent 环境的 Sift 变量；token 不进入环境、argv、stdout/stderr、事件、日志或错误文本。
4. run token 只授权绑定 attempt 的 `report.submit`，不能调用 `claim.*` 或任何 `ops.*`。这限定的是 Report 凭据的能力；同 UID Agent 在 V0 读取 operator token 的未闭合边界仍按 ADR-008 如实报告。
5. 接受的 report 至少写一条 append-only `events` 行和一条 `report_receipts` 行。`progress`、`goal`、`completed` 永远只写事件；`blocker` 也不得直接写 `runs.status`，但可按 §5 在同一事务中生成 `agent_blocked` Interrupt。
6. `completed` 只是完成声明，绝不表示 Run done，也不触发 Change 创建；最终裁定只来自 Layer 2 的进程结果和 Gate。
7. 鉴权、payload 校验、canonicalization、去重、限流和配额均为确定性代码；LLM 不参与任何一个决定。
8. 每个 Report 参数均从绑定 Run 创建时冻结的 `runs.config_snapshot_id` 读取：`report.*`、`runtime.retry_multiplier`、`attention.day_timezone` 及由 `EmitInterrupt` 使用的 attention 参数。daemon 当前激活配置只影响新 Run；重启或激活新配置不得改写既有 Run 的 Report 结果或其已存在令牌桶。

## 2. CLI 与 RPC

CLI 的稳定形状为：

```text
sift report <progress|goal|blocker|completed> [--key <report-key>] --payload <json>
```

`--key` 可选；省略时 CLI 使用 `crypto/rand` 自动生成随机 key。调用方在同一逻辑上报的所有重试中复用同一个 key，而不是每次生成新 key。`--payload` 必须恰为 §3 对应 kind 的 closed JSON object；CLI 不接受 stdin、文件名、任意额外 flag 或未知子命令作为绕过 payload schema 的入口。CLI 在本地先拒绝缺失/不安全的 `SIFT_RUN_DIR`、`control.json`、token 或绑定字段，不尝试任何其他凭据或 socket。

CLI 将 control 文件中的 binding 和调用方字段组成 [`control-plane.md` §3.2、§5.2](control-plane.md) 的唯一 Request v1：

```json
{
  "protocol_major": 1,
  "protocol_minor": 0,
  "client_version": "0.1.0",
  "request_id": "0123456789abcdef0123456789abcdef",
  "method": "report.submit",
  "auth": {"kind": "run_token", "token": "<control.json run_token>"},
  "params": {
    "run_id": "<control.json>",
    "attempt_no": 1,
    "generation": 1,
    "report_key": "<generated-or-explicit-key>",
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
| 为 `spawning`，且 permit 已签发、`claim.started` 尚未提交 | `not_ready`，details 为 closed `retry_policy` | 只重试这一种结果 |
| `pending`、`starting`、`finished`、`orphaned`，或 phase 已越过 | 永久拒绝 | 不重试 |
| token/binding 跨 Run、attempt 或 generation | `unauthorized` 或 `stale` | 不重试 |

对 `not_ready`，服务端在每个错误 envelope 的 closed `details` 返回本 Run snapshot 导出的完整策略：

```json
{"retry_policy":{"initial_delay_ms":100,"multiplier_micros":2000000,"max_delay_ms":1000,"total_timeout_ms":10000}}
```

四个值均为正整数；`multiplier_micros` 是配置中非 exponent、至多六位小数的 `runtime.retry_multiplier × 1,000,000` 精确整数，范围 `1000000..10000000`；三个 delay 是配置中精确的整数毫秒。CLI 只接受这一个 schema；缺字段、额外字段、范围错误、整数溢出或 `initial_delay_ms <= max_delay_ms <= total_timeout_ms` 不成立时均本地 fail closed，不猜默认值、不读 `config.yaml`。首次收到 `not_ready` 时记录单调时钟起点；第 `n` 次等待（从 0 开始）为 `min(max_delay_ms, floor(initial_delay_ms × multiplier_micros^n / 1000000^n))`，且不得使下一次等待后的累计时间超过 `total_timeout_ms`。达到该上限即失败。边界 vector：示例 policy 的等待依次为 `100,200,400,800,1000×8` ms（累计 `9500ms`），下一次 `1000ms` 必须因超过 `10000ms` 拒绝；`multiplier_micros=1000000` 长序列保持 `100ms`；`initial_delay_ms=1001,max_delay_ms=1000`、缺 `multiplier_micros`、或第二次响应把 `max_delay_ms` 改为 `999` 均为本地失败。每次重试复用相同 report key 和 payload（未显式提供时，该 key 在调用开始时生成一次）。任何其他错误（包括限流、配额冲突及 payload/schema 错误）都不进入该退避循环。

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

`report` 是 §3 的原 payload；Run 和 attempt 身份由 event 的列承载，不从 Agent payload 复制。receipt 的 `report_kind`、`payload_digest`、`event_id`、`received_at_ms` 以及可空的 `direct_interrupt_id`、`report_interrupt_charge_entry_id` 是事件的审计锚点。事件顺序仅是本地提交顺序，不把 Agent 声称的时间当作事实发生时间。

`blocker` 在 payload 和 attempt 日志引用均可形成 [`interrupt.md` §3](interrupt.md) 的 `agent_blocked` 最小事实时，预先生成新 receipt ID，并以它作为 `report_id` 调用唯一 `EmitInterrupt` 入口。它不得自行指定 severity、options 或 generation key。Report 子配额 charge 与 Interrupt attention admission 是两个独立账本：receipt 的 `report_interrupt_charge_entry_id` 指向一笔实际 `kind=report` entry；`direct_interrupt_id` 指向该 Interrupt；Interrupt 的 `charged_budget_entry_id` 仅在 attention 实际收费时指向 `budget_entries`。若 attention admission 为 `quota_batched` 或 `critical_fused`，attention entry 与两个 attention charge FK 均为 NULL；不得写零额或虚构 entry。Report charge、receipt、event、Interrupt、admission 和首发 outbox 仍按 storage 事务配方绑定，且不改变 Run 状态。

### 5.2 两层去重

去重以绑定 attempt 为域，发生在任何令牌或子配额消费之前：

1. **idempotency key：** 已存在 `(run_id, attempt_no, report_key)` 时，若 `payload_digest` 相同，返回原 receipt 的 `duplicate`；不同则返回不可重试 `conflict`。这条规则不随时间过期。
2. **语义窗口：** 当 `report.dedupe_window > 0`，若同一 attempt 已在 `[now - dedupe_window, now]` 接受相同 `(kind, payload_digest)`，新 key 也返回那个原 receipt 的 `duplicate`。窗口为 `0s` 时只保留第 1 层。

两种 duplicate 都不消费 report token、不增加 report 子配额、不创建 event/receipt/Interrupt，也不占用新 key。限流或配额拒绝只记录安全事件，绝不占用 report key；调用方修正或等待后可用相同 key 再试。

## 6. 令牌桶与 Interrupt 子配额

### 6.1 上报速率

对非 duplicate、阶段合法的 report，`RecordReport` 从 Run snapshot 读取参数，并使用 [`storage.md` §9.2](storage.md) 的持久化整数令牌桶：

```text
kind     = report
scope_id = run:<run_id>:attempt:<attempt_no>
capacity = report.burst
refill   = report.events_per_minute / 60s
```

补充、余数和 CAS 严格按 storage 规格；不得以固定一分钟窗口替代。既有桶的 `capacity_units`、`refill_numerator`、`refill_period_ms` 必须仍等于该 Run snapshot 的导出值；不等时以存储损坏拒绝并记安全事件，绝不按 daemon 当前配置重置。桶不足时请求以不可重试 `conflict` 拒绝，只写安全事件，不写 receipt/event，也不占用 key。重启不重置桶。

### 6.2 Report 直接致扰的每日子配额

只有实际由 `blocker` report 直接调用 `EmitInterrupt` 的 `agent_blocked` 计入该子配额；`progress`、`goal`、`completed`，以及 Gate、恢复和其他系统事实创建的 Interrupt 一律不计。Report charge 的完整不可变 row 为：

```text
kind=report
scope=run
scope_id=<run_id>
bucket_start_ms=<snapshot timezone 的本地日 00:00 对应 instant>
amount=1
reason=report_agent_blocked
run_id=<run_id>
operation_key=report-interrupt-quota:<receipt_id>
created_at_ms=<server received_at_ms>
```

`bucket_start_ms <= created_at_ms < bucket_end_ms`；`bucket_end_ms` 只从权威 `budget_counters` 由 `(run_id,bucket_start_ms)` 取得，不复制到 `budget_entries`；`attention.day_timezone=local` 在 config snapshot 创建时规范化为具体 IANA 名称。日桶按该冻结时区的日历日计算，使用下一本地日的 00:00 作为 exclusive end，故 DST 日可为 23 或 25 小时。新 receipt 的 `report_interrupt_charge_entry_id` 与 `direct_interrupt_id` 均为 nullable、各自 UNIQUE FK，且仅在相应写入成功时设置；它们把 Report receipt、Report charge 与直接 Interrupt 固定为一对一。attention charge 不由 Report receipt 伪造：只有 `attention_admissions.kind=quota_charged`（或实际 critical admission）时，`interrupts.charged_budget_entry_id` 与 admission 的 `attention_charge_entry_id` 才非空；`quota_batched` / `critical_fused` 时两者均为 NULL。重放和语义 duplicate 不重复收费。critical 若由未来扩展的 Report 直接产生，仍计入本子配额，并同时遵守 attention critical fuse。

当子配额已满，后续会直接致扰的 blocker 不创建 receipt/event/普通 Interrupt，但 `RecordReport` 先以独立、可重放事务写入唯一 `(run_id, daily_bucket_start_ms)` 的 quota-exhaustion 安全事件记录，同时提交已消费的 rate token。该记录的唯一约束而非先查后写保证并发至多一次；提交后才以 [`interrupt.md` §5.1](interrupt.md) 的 Report 专用 domain/version **best-effort** 调用 `failure_review`。该异常的 facts 固定为 `failure_class=report_interrupt_quota_exhausted`、受控 `failure_evidence_ref=sift://event/<security_event_id>` 与 `recommended_action=hold`，不接受 Agent 自由文本；`attempt_no` 为 NULL。它是 Report quota v1 的独立 option/binding variant，只提供 canonical `reject|hold`，没有 `retry`：`reject` 失败 Run，`hold` 只 hold Interrupt 而保持 running Run。它仍经过 `EmitInterrupt` 的全局 attention 配额和 critical fuse：合批/熔断保留该异常 Interrupt；结构拒发、publish target 缺失或 binding 拒绝都保留已提交 exhaustion/rate token，并以 generation-key 幂等的确定性诊断记录，不借支或另开告警旁路。Report quota 的 canonical options 选择必须先按 `report_quota_failure_review` variant 分派，再执行 Interrupt §3.6 的逐字段/同序接纳；不得套用 attempt `failure_review` 的 `retry,reject,hold` 集合。

## 7. 原子边界与验收

`RecordReport` 是唯一写入口。下表的领域列都在一笔事务中提交或回滚；`安全审计` 是拒绝的独立、必须在响应前提交的安全事件，绝不伪造成 Agent 已上报的领域 event。安全审计存储不可用时不返回成功。

| 有序结局 | rate token | Report charge | receipt/key、领域 event | 安全审计 | Run | Interrupt / attention / outbox |
|---|---|---|---|---|---|---|
| duplicate（两层任一） | 不消费 | 不写 | 复用既有；不占新 key | 不写 | 不变 | 不写 |
| 普通 `progress`/`goal`/`completed` | 消费 | 不写 | 同事务新 receipt + event | 不写 | 不变 | 不写 |
| blocker 不能形成最小 facts | 消费 | 不写 | 同事务新 receipt + `report.blocker` event | 同事务诊断 | 不变 | 不写 |
| blocker facts 完整、子配额可用、attention 实际收费 | 消费 | 写一行 Report charge | 同事务新 receipt + event | 不写 | 不变 | 新 `agent_blocked`、一笔 attention charge/admission 和 forge-comment outbox 全部提交 |
| blocker facts 完整、attention `quota_batched` 或 critical fuse | 消费 | 写一行 Report charge | 同事务新 receipt + event | 不写 | 不变 | 同一 Interrupt/admission/forge-comment 提交；`charged_budget_entry_id=NULL`，delivery 标为 batched/held，不另扣或借支 |
| 子配额已满，exhaustion 线性化事务 | 消费并提交 | 不写 | 不写，key 不占位 | quota-exhaustion 行及其安全 event 提交 | 不变 | 尚未尝试；崩溃后重放该唯一事实 |
| 子配额已满，专用 `failure_review` 尝试 | 已提交，不再消费 | 不写 | 不写，key 不占位 | 已提交；结构拒发诊断以 generation key 幂等提交 | 不变 | 至多一个；attention 合批/熔断保留 admission 且无 attention charge；结构拒发、缺 publish target 或 binding 拒绝不回滚 exhaustion |
| `EmitInterrupt` 结构拒绝（完整 blocker 分支） | 回滚 | 回滚 | 回滚，key 不占位 | 领域回滚后提交拒绝诊断 | 不变 | 回滚 |
| 事务内部错误 | 回滚 | 回滚 | 回滚，key 不占位 | 存储可用时提交 `report_transaction_failed`；否则 RPC 返回 retryable `internal` | 不变 | 回滚 |

因此 token 在子配额满分支已消费；它是通过两层去重、payload/phase 验证及 rate CAS 后的一次上报尝试，而不是可借此绕开的免费探测。exhaustion 线性化提交后的 RPC 固定返回不可重试 `conflict`，closed code 为 `report_interrupt_quota_exhausted`；若专用发射因结构拒绝未能创建 Interrupt，仍返回同一 code（诊断只供审计，不能成为调用方分支）。attention 存储错误返回 retryable `internal`，但保留该 exhaustion/rate-token 事实；同一或后续请求重放只重试 generation-key 的发射，不再扣 token。Report-only `agent_blocked` 与 quota-exhaustion `failure_review` 都必须写 immutable `interrupt_command_effect_bindings`：前者绑定 `ask|retry|reject|hold` 的当前 effect；后者固定 `attempt_no=NULL` 并使用 storage 的 `report_quota_failure_review(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id)` arm，canonical options 只有 `reject|hold`。running Run 上该 variant 的 `hold` 保持 `running`（仅 Interrupt 进入人工 hold）；`reject` 才令 Run failed；Report Interrupt 不执行其他 Run transition。安全 event 与领域 event 的身份、时钟和 source 以 [`storage.md` §7](storage.md) 为准。

M5 至少覆盖：

1. CLI 只读取 run dir 控制文件、只连 `run.sock`；完整 Request v1、错误 details schema 与非法 policy 响应均可断言；operator token、`siftd.sock`、SQLite 和 `config.yaml` 均不可作为 fallback。
2. run token 的跨 Run/attempt/generation、调用 `claim.*`/`ops.*`、以及 `pending`/`starting`/终态上报全部拒绝；仅合法 `spawning` 返回从 Run snapshot 导出的有界 `not_ready`。旧 Run 在重启/新配置后仍使用旧 policy，新 Run 使用新 policy。
3. 四类 payload 的 closed schema、canonical digest、大小上限和 token 脱敏；`completed` 与所有其他 report 均不改变 `runs.status`。
4. 同 key 同 digest、同 key 异 digest、窗口内新 key 同语义、窗口到期和 `dedupe_window=0` 分别有可断言结果；所有 duplicate 零收费、零新 event。
5. `burst=4` 跨固定分钟边界不瞬时超发，重启后桶连续且不随新配置重置；超限与配额拒绝没有 receipt/event/key 占位。
6. 覆盖 DST 前后日桶、同 operation key/语义 duplicate、崩溃重放和并发收费；四个并发触顶者最多产生一条当日 quota-exhaustion 记录与 `failure_review`。在 exhaustion 线性化提交前/后、专用发射前/后分别注入崩溃：重放不得二次扣 token或安全事实，且最终至多一个 generation key Interrupt。publish target 缺失或 binding 结构拒绝保留 exhaustion 并返回同一 closed conflict；attention 存储错误也保留 exhaustion/token、返回 retryable internal，重放只再试发射。逐行验证上表，除这条有意拆分的安全事实/专用发射边界外，绝不出现部分领域状态。
7. 逐字节执行 [`interrupt.md` §3.6](interrupt.md#36-t4-接纳与命令-golden-vectors) 的 Report quota v1 fallback、合法 `reject,hold` T4 input/output 和 persisted bytes；重排、添加 `retry`、错误 recommended option 都回退同一 quota fallback，绝不套用 attempt golden。
8. 对共用 binding union 断言 new-attempt terminal pair 逐字段等于 binding `(attempt_no,generation)`、命中同 Run `failed` attempt；错 Run/generation、non-failed、pair 不等、attempt 字段混入 quota、quota 字段混入 attempt，以及两 arm options 交叉错配全部拒绝。
