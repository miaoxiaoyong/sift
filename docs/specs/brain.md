---
status: active
created: 2026-07-28
summary: Brain T1–T7 调用壳、schema、版本与确定性兜底契约
---

# Brain 规格

本文冻结 Brain 统一调用壳、调用身份、提示词资产、T1–T7 输入输出、Task Spec 组装、token 记账与确定性兜底。§11–§13 是 M5 的字段级 **draft**：它们复用已生效的调用壳，但不声明 T4/T6/T7 已实现；T1/T2/T3/T5 的既有契约仍为 `active`。

来源：[PRD §5.3、§5.4、§5.5、§5.7、§5.9](../PRD.md)、[DESIGN §8.3、§8.5–§8.7](../DESIGN.md)、[`storage.md` §9–§10](storage.md)、[`config.md` §3.4](config.md)、[`interrupt.md` §1–§5](interrupt.md)、[`ledger.md`](ledger.md)、[`outbox.md` §2、§5](outbox.md)、[WBS M1 §1.7、M4 §4.2、M5 §5.1](../WBS.md)。

## 评审处置

评审原文：[2026-07-28-brain-review-pi-gpt-5.6-sol.md](../reviews/2026-07-28-brain-review-pi-gpt-5.6-sol.md)。

| 发现 | 处置 |
|------|------|
| B1（P1）单行 trace 无法表达 logical call / 双 attempt | 拆 `brain_call_counters`/`brain_calls`/`brain_attempts`，见 §3/§5；表、约束与 trigger 落 [`storage.md` §10.1/§13](storage.md) |
| B2（P1）T1 pre-Run 悬空状态 | 新增 intake 投影、状态机与回复 generation 协议，见 §7.3；表落 storage §7.5–§7.6，outbox 目的/key 落 [`outbox.md` §5.1](outbox.md) |
| B3（P1）T1/T2 schema 未到字段级可生成 | §7.1/§7.2/§8.1/§8.2 逐字段冻结；输入总上限 `max_input_bytes` 落 [`config.md` §3.4](config.md) |
| B4（P2）token 越界语义 | §6 按物理 attempt 定义发起前阈值 + 事后越界 post-charge；storage §9.1 显式排除 token |
| B5（P2）外层 envelope 边界 | §4 顶层/usage open-envelope、内层触点输出 closed；`protocol` 字段落 config §3.4 |
| B6（P2）超限输出与 provider 证据 | §4/§5 冻结截断语义、digest/bytes、stderr 上限与 `provider_error_code` 枚举 |
| B7（P2）T2 审批消费缺状态事务 | §8.3：有效 hitl 取 OR、单事务提交；落 storage `CommitT2Assignment` |

B1–B3 全部 P1 与 B4–B7 均已处置；P3 编辑项（版本独立 bump、SIFT_HOME 单一引用、补充 fixture）同步采纳，见 §2/§11/§12。

### T3/T5 字段级评审

2026-07-29 独立字段级评审结论为 **PASS WITH NOTES**；报告见 [2026-07-29-brain-t3-t5-review-pi-gpt-5.6-sol.md](../reviews/2026-07-29-brain-t3-t5-review-pi-gpt-5.6-sol.md)。评审发现的 head/diff 竞态、T5 rerun 目标缺失、fallback 绕过 Gate 事务、嵌套字段未闭合和 Brain↔Gate 错误一对一关联均已在 §9–§10 及 storage/gate/DESIGN 接缝关闭；无遗留 P1，本文保持 `active`。非阻断注记仅涉及 T5 现有 Forge 元数据的分诊信息量，留待实现证据评估，不扩张 V0 Forge 日志读取面。

## 1. 不变量

1. LLM 只输出建议；状态转移、硬护栏、Agent 存在性/并发、去重、预算和权限均由确定性代码裁定。
2. 每个触点只有一个版本化 prompt + output schema 来源；运行时 schema、生成 JSON Schema 与回放 schema 同源。
3. 调用流程固定为：发起前门禁（每个物理 attempt 独立检查）→ 原 prompt 调用 → closed decode → 失败时**同一输入/同一 prompt**重试一次 → 再失败走触点兜底。
4. 禁止提取 markdown fence、修 JSON、忽略内层未知字段、类型强转或让第二个 LLM“修复”输出。
5. 每次 logical call 与其全部物理 provider attempt（含未调用 provider 的调用前兜底）都必须持久化；外部调用不发生在数据库事务内。
6. 合法 LLM 输出仍须经过领域后校验；未知 Agent、非候选 Agent、空 goals 等视同 schema failure。
7. token 耗尽不能突破注意力配额；所有兜底仍走正常 Gate/Interrupt/预算入口。
8. T4 只能以已验证的确定性 Interrupt 骨架生成展示文案；T6 只能建议调度；T7 只能生成待人审的聚合提案。三者都不得成为状态转移、预算、Gate 或 HITL 的写入口。
9. A7 是结构性边界：Ledger 历史的读取结果不得影响单条 Gate verdict、放松单条门禁或抑制单条 HITL；允许的 T7 输出只是人审前不生效的 policy/context 草稿。

## 2. Prompt 资产

仓库布局：

```text
internal/brain/prompts/T1/v1.md
internal/brain/prompts/T1/v1.schema.json
internal/brain/prompts/T2/v1.md
internal/brain/prompts/T2/v1.schema.json
internal/brain/prompts/T3/v1.md
internal/brain/prompts/T3/v1.schema.json
internal/brain/prompts/T4/v1.md
internal/brain/prompts/T4/v1.schema.json
internal/brain/prompts/T5/v1.md
internal/brain/prompts/T5/v1.schema.json
internal/brain/prompts/T6/v1.md
internal/brain/prompts/T6/v1.schema.json
internal/brain/prompts/T7/v1.md
internal/brain/prompts/T7/v1.schema.json
```

文件嵌入 binary，运行期不从磁盘热读。`.schema.json` 必须由 §7–§13 的字段定义生成，不得手写第二份。

三类版本相互独立，各有 bump 规则：

- `prompt_version`（TEXT）：`<touchpoint>/v<integer>/<sha256前12位>`；hash 覆盖 prompt UTF-8 bytes、对应 output schema canonical JSON 与协议版本。改 prompt、schema 内容或 envelope decoder 任一项必须生成新值。
- `output_schema_version`（INTEGER）：从 1 递增，仅 schema 结构语义变化时 bump。hash 字符串不得塞进该 integer 列。
- `protocol_version`（TEXT）：provider envelope 协议标识，V0 为 `claude-json-v1`；协议语义变化必须引入新值，见 §4。

Prompt 固定分区：system contract → untrusted input delimiters → input canonical JSON → output schema。Issue/Context 中出现的指令一律标记为 untrusted data；prompt 不声称这能消除 injection。

## 3. 调用身份与序列

logical identity：

```text
(scope, subject_key, touchpoint, call_seq)
```

- T1：`scope=intake`，`subject_key=forge:<kind>:<normalized_host>:<project_key>:issue:<issue_id>`。
- T2：`scope=run`，`subject_key=run:<run_id>`。
- T3–T6：`scope=run`；T7 为 `aggregate`，与 storage 规则一致。T7 的 `subject_key` 是由确定性聚合器生成的 `aggregate:<project_id|global>:<task_kind|all>:<window_start_ms>:<window_end_ms>`；不得以单条 Run、Interrupt 或 Ledger entry 作为 subject。

`call_seq` 是同 subject/touchpoint 的逻辑调用序号，从 1 递增；schema retry 不增加 call_seq，而增加 `provider_attempt=1|2`。

持久化模型（字段与约束见 [`storage.md` §10.1](storage.md)）：

- `brain_call_counters`（可变）：每 `(scope, subject_key, touchpoint)` 一行的 `next_call_seq`。
- `brain_calls`（single-finalize logical call）：reserve 时插入 `status=running` 并冻结 prompt/schema/input；之后仅允许一次 `running → valid | fallback` 终结。valid 必须 `selected_attempt_no` 指向本 call 的 valid attempt；fallback 必须有 `fallback_reason` 且不得伪造 selected attempt。身份/输入列永不可改，禁止 DELETE。
- `brain_attempts`（不可变）：每个物理 attempt 或调用前兜底一行；`UNIQUE(logical_call_id, provider_attempt)`。`provider_attempt=0` 只表示 provider disabled/预算门禁等调用前兜底（`outcome=fallback`，token/exit/raw 全空）；`1|2` 表示真实子进程调用。attempt 上**没有** `fallback_used`：单个 attempt 失败不等于整个 call 走兜底。

`ReserveBrainCall` 在 `BEGIN IMMEDIATE` 中递增 counter 并以旧值插入 running call，禁止 `SELECT max()+1`。

`attempt_outcome = valid | invalid_output | provider_error | fallback`；`provider_error` 必须带稳定 `provider_error_code`：`timeout | nonzero_exit | output_too_large | invalid_envelope | usage_missing | usage_invalid | spawn_failed`。

prompt/input/schema 只存 call 一次，attempt 通过 FK 继承“同 prompt”事实；attempt 另存 `request_digest`，写端口断言其等于 call 冻结的 `input_digest`，以证明实际发送 bytes 未漂移。

## 4. Provider 子进程协议

V0 protocol 为 `claude-json-v1`，config 字段见 [`config.md` §3.4](config.md)（`protocol` V0 只能为该值）。

调用使用配置 executable + args，prompt/input 从 stdin 传入，不使用 shell，不把输入放 argv。子进程工作目录为空临时目录，环境只保留运行 CLI 所需的最小 allowlist；不得注入 operator/run/wrapper credential。timeout 使用 config `call_timeout`。

### 4.1 `claude-json-v1` 外层 envelope：open-envelope

adapter 将 CLI 外层结果规范化为 `result_text` + `usage`：

- 顶层 object **接受未知诊断字段**（如 CLI 新增的 `session_id/cost/diagnostics`），但 `result_text` 与 `usage` 仍 required 且类型精确。
- `usage` object 接受未知计数项，但 `input_tokens/output_tokens` required、非负整数，不做数值字符串强转。
- JSON parser 拒绝重复键、非 UTF-8、非有限数字、尾随文本及非 object 顶层。未知字段只忽略，不进入 prompt、领域输出或 token 计算。
- `result_text` 必须是仅含一个 JSON object 的 UTF-8 字符串，内层按触点 schema `additionalProperties:false` closed decode——open 只到外层为止。
- 协议重大语义改变以新的 `protocol` 值适配，不靠 open-envelope 猜兼容。

usage 缺失/非法使本 attempt 无法计费：记 `provider_error`（`usage_missing | usage_invalid`），不猜测、不收费、不当 0，触发重试/兜底。

### 4.2 输出上限与 stderr

- stdout 读到 `max_raw_output_bytes + 1` 即终止进程；该 attempt 记 `provider_error/output_too_large`，保存前 `max_raw_output_bytes` bytes、已读部分完整 digest 与 byte count、`raw_output_truncated=true`。“完整保存原始 stdout”只对未截断输出成立。
- stderr 另设固定上限（V0 为 4096 bytes）：先做凭据模式去除，保存 `stderr_summary` 与 `stderr_truncated`；不拼入重试 prompt，不入事件或 outbox。

## 5. 重试与 trace

每次 logical call：

1. `ReserveBrainCall`：counter 递增 + 插入 `status=running` call，同一事务。
2. 每个物理 attempt 发起前串行检查 provider 可用性与当日 token counter；disabled 或 `consumed >= limit`：`RecordBrainAttempt` 写 `provider_attempt=0, outcome=fallback` 行，`FinalizeBrainCall` 终结为 fallback，返回触点兜底。
3. 调 provider attempt 1；子进程结束后 `RecordBrainAttempt` 落 immutable attempt 行，同事务按实际 usage post-charge（见 §6）。
4. outcome=valid：`FinalizeBrainCall` → valid（`selected_attempt_no=1`），返回。
5. 否则在 attempt 2 发起前**重新**执行第 2 步门禁；通过则以**完全相同 prompt bytes/input digest** 调 attempt 2 并落库。
6. attempt 2 valid → finalize valid（`selected_attempt_no=2`）；否则 finalize fallback，返回确定性兜底。

重试不得加入“上次哪里错了”的修复提示。attempt 1/2 分别落库后再决定后续动作；最终 call 收敛只能由 `FinalizeBrainCall` 一次性完成，不得更新 immutable attempt。

崩溃恢复：daemon 重启遇到遗留 `status=running` call，只按已持久 attempts 收敛——已有 valid attempt 则终结 valid，否则终结 fallback（`fallback_reason` 含 recovery）；不得重放无法证明未执行的 provider attempt。

## 6. Token 预算

`daily_token_limit` 是**发起新物理 attempt 的实际消费阈值**，语义如下：

1. Brain 调用壳全局串行。每个物理 attempt 发起前检查当日 counter：`consumed >= limit` 即不再发起，logical call 走兜底。attempt 1 已失败且使 counter 越界时直接 fallback，**不发 attempt 2**。
2. 阈值检查只决定能否发起；attempt 返回后按实际 `input_tokens + output_tokens` post-charge：attempt trace、`budget_entries` 与 counter 在同一事务写入，即使新值大于 limit。收费 operation key 为 `brain:<logical_call_id>:provider:<provider_attempt>`；重复 key 返回原 charge，不重复计费。重试是真实第二次成本，单独收费。
3. [`storage.md` §9.1](storage.md) 的通用 `consumed + amount <= limit` CAS **不适用于** token post-charge；token 使用专用“发起前阈值检查 + 完成后允许一次越界”语句。单 daemon/单 writer 不等于协议，由事务与唯一 operation key 保证。
4. usage 已知且总和为 0：保存 trace，但不创建要求 `amount>0` 的 budget entry；usage 缺失/非法不猜测、不收费，按 §4.1 的 provider error 处理。
5. 日桶为 UTC 自然日；跨午夜以 attempt **开始时**冻结的 bucket 收费，不以结束时间换桶。
6. 越界告警走 `forge_alert`，稳定 key（见 [`outbox.md` §5.1](outbox.md)），每 UTC 日桶只发一次；它仍消耗正常 attention budget，不得因 token 告警突破注意力配额。

token counter 是固定预算允许单次越界的唯一例外：实际 usage 必须全额 append entry，不得丢弃；attention/Forge/Report 仍禁止借支。

## 7. T1 Intake 体检

canonical schema 的唯一来源是与 prompt 同目录的 `.schema.json`（由本节字段表生成）；领域后校验只承接依赖运行时事实的规则（候选必须存在、确定性身份确认），不替 schema 补类型和长度。

### 7.1 Input v1

顶层 closed object（`additionalProperties:false`），三个 required 字段：

`forge`（closed object，required）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `kind` | string | 必填；`github \| gitlab` |
| `host` | string | 必填；规范化 host，1..253 bytes |
| `project_key` | string | 必填；1..255 bytes |

`issue`（closed object，required）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `id` | string | 必填；1..64 bytes |
| `title` | string | 必填；≤512 bytes |
| `body` | string | 必填；≤65536 bytes，可空串 |
| `author` | string | 必填；≤128 bytes |
| `url` | string | 必填；≤1024 bytes |
| `labels` | string[] | 必填；≤32 项，每项 ≤128 bytes，由 intake 排序去重 |

`known_candidates`（array，required，≤20 项，按 `run_id` 排序），每项为 closed object：

| 字段 | 类型 | 约束 |
|------|------|------|
| `run_id` | string | 必填 |
| `issue_id` | string | 必填；1..64 bytes |
| `title` | string | 必填；≤512 bytes |
| `status` | string | 必填；`queued \| running \| waiting_human \| done \| failed` |

整份 input canonical JSON ≤ config `brain.max_input_bytes`；超限不调用 provider，直接按 T1 兜底 ready 入队（确定性结果，不伪造 LLM 输出）。该上限是输入契约，不复用 `max_raw_output_bytes`。

候选由确定性检索产生；不得让 LLM 查询数据库/Forge。

### 7.2 Output v1

closed object（`additionalProperties:false`）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `disposition` | string | 必填；`ready \| needs_clarification \| possible_duplicate` |
| `questions` | string[] | 必填；0..5 项，每项 trim 后 1..1000 bytes，trim 后去重 |
| `possible_duplicate_run_id` | string/null | 必填；非空时长度 ≤64 bytes |
| `rationale` | string | 必填；≤2000 bytes，可空串 |

disposition 互斥矩阵（schema 层表达，领域后校验复核）：

- `ready`：questions 为空且 duplicate 为 null；
- `needs_clarification`：questions 1..5 且 duplicate 为 null；
- `possible_duplicate`：duplicate 非空且 questions 为空。

领域后校验：duplicate id 必须精确命中 input candidate 的 `run_id`，否则视同 schema failure。T1 只是建议：duplicate 必须由确定性 Issue identity/既有 Run 事实确认，LLM 不能单独吞掉任务。

失败兜底固定为：`disposition=ready, questions=[], possible_duplicate_run_id=null, rationale="fallback"`。

### 7.3 pre-Run intake 投影与消费协议

本节契约在 M1 冻结；投影/CAS、真实 Forge comment worker、回复 receipt 消费以及 crash/generation 验收在 [WBS M2 §2.3/§2.5](../WBS.md) 实现。它们必须随真实 Forge 适配层交付，不能用 M1 的 schema 或通用 outbox 框架代替实现证据。

`needs_clarification`/`possible_duplicate` 不创建 Run，也不能只靠 event/trace 推导当前待办。权威投影为 [`storage.md` §7.5–§7.6](storage.md) 的 `intake_items`（可变）与 `intake_assessments`（不可变），唯一键 `(forge_kind, normalized_host, forge_project_key, issue_id)`。

状态机：

```text
pending_evaluation → evaluating → ready
                                → awaiting_clarification
                                → awaiting_duplicate_confirmation
awaiting_* --(可信回复)--> pending_evaluation | ready | consumed
ready --(Run 创建)--> consumed
```

协议：

1. `PersistIntakeBatch` 一笔事务写 receipt、`pending_evaluation` intake 投影与事件，最后推进 forge cursor；崩溃不推进 cursor，重放靠 receipt 唯一键去重。
2. T1 worker 先 `ReserveBrainCall` 并把 intake CAS 到 `evaluating`；外部 provider 调用不占数据库事务。遗留 `evaluating` 按 §5 的 running call 收敛规则恢复，不靠内存超时猜测。
3. `PersistIntakeDecision` 一笔事务：写 assessment、CAS intake state、追加事件、创建必要 outbox operation；ready（含兜底）同事务幂等创建 Run、写 `linked_run_id` 并转 `consumed`。
4. 澄清/确认评论使用 `forge_comment`，purpose 与稳定 key 见 [`outbox.md` §5.1](outbox.md)（如 `comment:intake-clarification:<intake_id>:<generation>`）；payload 带 `intake_id/generation`，outbox 行 `run_id` 保持 NULL，不伪造 run 关联。每一轮新澄清 `clarification_generation` +1。
5. 回复由可信 actor 的 `issue_comments` receipt 驱动，按 `intake_id + 当前 generation` 关联：接受则 CAS 回 `pending_evaluation` 重新评估；**旧 generation 回复只记审计事件，不推进当前状态**。重启靠 intake state + pending outbox 恢复，不扫描自然语言猜状态。
6. duplicate 只能经确定性事实确认：candidate Run 的 Issue identity 与本 Issue 相同 → 直接 `consumed` 并链接既有 Run；否则发确认评论，可信回复确认 → `consumed` + `linked_run_id`=candidate；否决 → `ready`，按正常路径创建 Run。任何路径不得静默丢弃 Issue。

T1 的待澄清问题不是 PRD §4.3 的 Run Interrupt：V0 保持独立 intake 投影与 forge comment 协议，不把 Interrupt 扩展为可空 Run。

## 8. T2 分派

### 8.1 Input v1

顶层 closed object，required 字段：

| 字段 | 类型 | 约束 |
|------|------|------|
| `run_id` | string | 必填 |
| `issue` | closed object | 必填；`title` ≤512 bytes、`body` ≤65536 bytes、`url` ≤1024 bytes，三者均必填 |
| `candidate_agents` | array | 必填；1..32 项，按 `id` 排序 |
| `base_context` | closed object | 必填 |

`candidate_agents[]`（closed object）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `id` | string | 必填；≤64 bytes |
| `capabilities` | string[] | 必填；≤16 项，每项 ≤64 bytes |

`base_context`（closed object）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `project_context` | string | 必填；≤65536 bytes，可空串 |
| `global_context` | string | 必填；≤65536 bytes，可空串 |
| `task_annotations` | array | 必填；≤50 项，每项 closed object：`event_id` string 必填、`text` string ≤2000 bytes 必填 |

整份 input canonical JSON ≤ config `brain.max_input_bytes`；超限不调用 provider，直接走 T2 确定性兜底（人工分派）。

candidate agents 已经过配置、项目引用和启动探测过滤。Context 内容是 untrusted data。

### 8.2 Output v1

closed object（`additionalProperties:false`）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `kind` | string | 必填；`feature \| bug \| chore \| docs \| refactor` |
| `agent` | string | 必填；≤64 bytes；领域后校验必须精确命中 candidate id |
| `hitl_before_start` | boolean | 必填 |
| `goals` | string[] | 必填；1..10 项，每项 trim 后 1..1000 bytes，trim 后去重，合计 ≤8000 bytes |
| `risk_notes` | string | 必填；≤2000 bytes，可空串 |
| `rationale` | string | 必填；≤2000 bytes，可空串 |

LLM 不输出 guardrails、max attempts、并发或 policy；出现额外字段即被 closed decode 拒绝。

失败兜底：Run 保持/进入 `waiting_human`，agent/kind 为空，生成 `design_approval` 人工分派 Interrupt；不自动挑“第一个 Agent”。

### 8.3 审批消费事务

有效 `hitl_before_start = LLM 建议 OR 确定性强制`：来自非 allowlist Issue 作者的 Run 由确定性规则强制 true，LLM 输出的 `false` 不得降级该强制。

- 有效值为 false：`SetInitialTaskSpec` 一笔事务写 Run kind/agent、初始 Task Spec snapshot、Run → `queued` 与事件。
- 有效值为 true：一笔事务（storage `CommitT2Assignment`）写 Run kind/agent、初始 Task Spec snapshot、Run → `waiting_human`、`design_approval` Interrupt、事件与 outbox；批准指令到达后才 queued/launch。**不得先把 Run 暴露为可 launch 的 queued 再补 Interrupt。**

## 9. T3 风险评分

T3 在 Gate 外、Gate 输入快照组装前调用；Gate 只接收冻结的风险结果，不调用 Brain。它读取 Forge 返回的原始 unified diff（[`forge.md` §4.10](forge.md)），不得自行读取 worktree、配置或 Forge。任一调用的作用域为 `run`，`subject_key=run:<run_id>`；与具体 attempt 关联时才填写 attempt。

### 9.1 Input v1

顶层 closed object，required 字段：

| 字段 | 类型 | 约束 |
|------|------|------|
| `run_id` | string | 必填；1..256 bytes |
| `task_kind` | string | 必填；`feature \| bug \| chore \| docs \| refactor`，必须等于 T2 冻结值 |
| `change` | closed object | 必填；仅含下表四个 required 字段 |

`change`（closed object）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `id` | string | 1..256 bytes |
| `url` | string | 1..2048 bytes；已验证的 HTTPS Change URL |
| `head_sha` | string | 40 或 64 个小写十六进制字符 |
| `diff` | string | Forge 返回的未改写 unified diff；可空，受整份 input 上限约束 |

`change.id`、`change.url`、`change.head_sha` 必须与该 Run 当前 Forge Change 投影精确一致。为防止 head 在两次 Forge 读取之间漂移，组装器固定执行 `GetChange → GetChangeDiff → GetChange`：前后两次 Change 的 ID/URL/head 必须逐字段相同，才可 reserve T3 call；否则丢弃本次组装并重新观测，不得把 diff 标成旧 head。T3 结果只可进入 `identity.run_id`、`identity.change_id`、`change.head_sha` 分别等于本 input 的 Gate snapshot；head 已变化时必须重新调用 T3。

整份 canonical JSON 不得超过 `brain.max_input_bytes`；超过上限不调用 provider，按 §9.3 兜底。diff 和其中的文本均为 untrusted data。

### 9.2 Output v1

closed object（`additionalProperties:false`）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `risk_score` | integer | 必填；0..100，数值越大风险越高 |
| `risk_points` | string[] | 必填；0..10 项，每项 trim 后 1..1000 bytes；trim 后按 UTF-8 bytes 排序去重 |
| `rationale` | string | 必填；≤2000 bytes，可空串 |

`risk_score` 与 `risk_points` 只是建议；确定性 Gate 依照有效策略裁定 review、合并与状态转移，LLM 不得输出或覆盖 policy、review requirement、verdict 或 auto-merge 决定。

### 9.3 高风险兜底与 Gate 来源

调用前 provider 禁用、token 阈值/输入上限阻止调用，以及两次 provider attempt 均未产生合法输出时，都按 §5 记录 call/attempt、沿用 §6 token 语义，并返回固定结果：

```json
{"risk_score":100,"risk_points":["T3 unavailable; deterministic high-risk fallback"],"rationale":"fallback"}
```

因此失败和超预算一律是高风险，不能因缺少评分而降级 `risky-only` 的人审要求。

Gate 输入快照必须将风险结果连同**由确定性消费者追加、而非 LLM 输出**的 closed 来源对象 canonical 化：

- 正常结果：`{kind:"brain",logical_call_id,prompt_version,output_schema_version}`；三个值分别为非空 string、非空 string、正整数，且必须逐字段等于被引用的 terminal valid T3 call；
- 兜底结果：`{kind:"fallback",logical_call_id,version:"T3/fallback/v1",reason}`；logical ID 必须引用 terminal fallback T3 call，`reason` 只能为 `provider_disabled | token_threshold | input_too_large | invalid_output | provider_error | recovery`。

`gate_input_snapshots.risk_source_version` 对正常结果保存 `prompt_version`，对兜底保存固定 `version`；完整对象留在 `canonical_json`，两者均参与 `gate_input_hash`。同一 diff 在 T3 正常结果与兜底结果间不得命中同一个 Gate 缓存键。`RecordGateEvaluation` 同事务写 [`storage.md` §10.2](storage.md) 的多对多关联；同一 T3 call 可被 Checks/review 等事实不同的多份 snapshot 合法复用，不回写 terminal call。

## 10. T5 Checks 失败分诊

T5 只在 Forge Checks 已归一为 `failure` 时调用；它不重查 Forge、不修改 Check 结论，也不决定 Run 状态。任一调用的作用域为 `run`，`subject_key=run:<run_id>`；与具体 attempt 关联时才填写 attempt。

### 10.1 Input v1

顶层 closed object，required 字段：

| 字段 | 类型 | 约束 |
|------|------|------|
| `run_id` | string | 必填；1..256 bytes |
| `change` | closed object | 必填；仅含 `id`、`url`、`head_sha` 三个 required 字段，约束与 §9.1 同源且必须等于当前 Change 投影 |
| `checks` | closed object | 必填；仅含 `external_url`、`failed_jobs` 两个 required 字段 |

`checks`（closed object）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `external_url` | string | 1..2048 bytes；已验证的 HTTPS CI 详情 URL |
| `failed_jobs` | array | 1..100 项；按 `(id, name)` UTF-8 bytes 排序，`id` 去重 |

`failed_jobs[]`（closed object）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `id` | string | 必填；1..256 bytes；[`forge.md` §4.12](forge.md) 的稳定 check run/job ID |
| `name` | string | 必填；1..512 bytes |
| `web_url` | string | 必填；1..2048 bytes；已验证的 HTTPS job/run URL |
| `allow_failure` | boolean | 必填 |

T5 只在 `GetChecks(head_sha)` 返回 `conclusion=failure` 且至少一个失败项时调用；`change.head_sha` 必须等于该次查询使用的 SHA。failure 却没有可归一失败项时不调用 provider，按 §10.3 以 `input_incomplete` 兜底。整份 canonical JSON 不得超过 `brain.max_input_bytes`；超限不调用 provider，按 §10.3 兜底。所有 Check 名称与 URL 均为 untrusted data；T5 不跟随 URL，也不自行读取日志。

### 10.2 Output v1

closed object（`additionalProperties:false`）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `classification` | string | 必填；`flaky \| real_failure \| infrastructure` |
| `retry_check_id` | string/null | 必填；`flaky` 时为 1..256 bytes string，其他分类必须为 null |
| `rationale` | string | 必填；trim 后 1..2000 bytes |

分类和重试目标都是建议，不是动作命令。`flaky` 的 `retry_check_id` 必须精确命中 input 中 `allow_failure=false` 的一项，否则视同 schema/domain failure并按同 prompt 重试；`real_failure`/`infrastructure` 不得夹带目标。确定性消费矩阵固定为：合法 `flaky` 建议仅可把该 ID 交给 Gate 的既有、有限且仍受预算/幂等约束的 Check 重试路径；Gate 仍独立核对 head、额度与目标身份。`real_failure` 与 `infrastructure` 都生成 `failure_review` HITL。T5 不得要求、发起或无限重复 CI/Agent 重试。

### 10.3 失败兜底与 Gate 来源

provider 禁用、token 阈值/输入上限、`input_incomplete`、schema/domain failure、provider failure 或 recovery 收敛为 fallback 时，T5 不猜测 flaky，也不直接改变 Run 或发 Interrupt。确定性消费者组装 Gate triage `{classification:"unknown",retry_check_id:null,source:{kind:"fallback",logical_call_id,version:"T5/fallback/v1",reason}}`；logical ID 必须引用本次 terminal fallback T5 call，`reason` 只能为 `provider_disabled | token_threshold | input_too_large | input_incomplete | invalid_output | provider_error | recovery`。Gate 随后以 `failure_class=triage_unavailable` 和现有 Check/Brain trace 证据在 `RecordGateEvaluation` 事务内调用 `EmitInterrupt(reason=failure_review)`，其 options、severity、预算与发布遵循 [`interrupt.md` §3–§5](interrupt.md)。不得在 T5 fallback 和 Gate 各发一次 Interrupt，也不得绕过 Gate evaluation/calibration。

Check 失败参与 Gate 输入时，快照必须 canonical 化 T5 的分类结果、`retry_check_id` 及其确定性来源。正常来源 closed object 与 §9.3 的 brain 形态同源，并必须引用 terminal valid T5 call；兜底来源使用上段固定对象。来源版本规则与 §9.3 相同。完整分类/目标/来源进入 Gate 快照及其 hash，正常分类与兜底不得共用缓存输入；关联通过 [`storage.md` §10.2](storage.md) 的多对多表写入，不回写 terminal call。本节不把 T5 建议扩展为 Gate 决定。

## 11. T4 决策简报（M5 draft）

T4 在已有的、确定性生成的 Interrupt 候选上调用；它不判断是否应当打扰，也不创建或更新 Interrupt。调用作用域为 `run`，`subject_key=run:<run_id>`；仅候选绑定具体 attempt 时填写 attempt。T4 的输入在调用前冻结，provider 调用结束后由唯一 `EmitInterrupt` 入口消费正常结果或兜底，见 [`interrupt.md` §1–§3](interrupt.md)。T4 call 不进入 Gate 输入，也不创建 `brain_gate_input_links`。

### 11.1 Input v1

顶层为 closed object，以下字段均 required：

| 字段 | 类型 | 约束 |
|------|------|------|
| `run_id` | string | 1..256 bytes；等于 trace Run |
| `attempt_no` | integer/null | 非空时为正整数，且等于 trace attempt |
| `interrupt` | closed object | 仅含下表字段 |

`interrupt` 是发射器已验证、尚未生成的候选骨架；它不是 LLM 可改写的领域对象：

| 字段 | 类型 | 约束 |
|------|------|------|
| `reason` | string | PRD 七种 Interrupt reason 之一 |
| `base_severity` | string | `low | normal | high | critical`；由确定性 `BaseSeverity` 计算 |
| `min_modality` | string | `voice | text | visual`；由 fallback 契约给出 |
| `fallback_brief` | string | 1..8192 bytes；[`interrupt.md` §3.2](interrupt.md) 已生成的原始状态文本 |
| `links` | array | 0..32 个 closed `{label,target}`，按 `(target,label)` UTF-8 bytes 排序去重；只含发射器已验证链接 |
| `candidate_options` | array | 1..4 个 closed `{id,label,effect,risk}`；顺序和内容逐字段等于 [`interrupt.md` §3.1](interrupt.md) 对该 reason 的确定性候选集 |

所有 facts、链接 label 和 fallback brief 都是不可信展示数据；T4 不跟随链接。整份 canonical JSON 超过 `brain.max_input_bytes` 时不调用 provider，按 §11.3 兜底。

### 11.2 Output v1

closed object（`additionalProperties:false`）：

| 字段 | 类型 | 约束 |
|------|------|------|
| `headline` | string | trim 后 1..40 Unicode code points；不得含 Cc 控制码或换行；独立可朗读 |
| `conclusion` | string | trim 后 1..1000 bytes；不得含 Cc 控制码或换行 |
| `key_points` | string[] | 1..3 项；每项 trim 后 1..1000 bytes、无 Cc/换行、去重；按输出顺序即简报中的阅读顺序 |
| `recommended_option_id` | string | 必须精确命中 input `candidate_options[].id` |
| `options` | string[] | 1..4 项，必须是 input `candidate_options[].id` 的**完整排列**，无重复；只允许重排，不得添加、删除或改写动作 |

确定性 renderer 以 `conclusion`、有序 `key_points` 和由 `recommended_option_id` 查得的候选 label 组装 Interrupt `brief`；以 `options` 的顺序组装完整 `Option{id,label,effect,risk}`。LLM 不能输出 `severity`、`reason`、`min_modality`、links、effect 或 risk，不能新增/删除人类动作，不能把 `visual` 改成可语音渲染。领域后校验失败（包括 option ID 不精确、集合不完整或 headline 不可朗读）与 closed decode failure 相同。

T4 的 prompt/schema 初始版本分别为 `T4/v1/<sha256前12位>`、`output_schema_version=1`，按 §2 的独立 bump 规则演进。正常结果和其版本只作为本次 Interrupt 的可审计展示来源；不得参与其 generation key、severity、Gate snapshot 或注意力 charge。

### 11.3 兜底与消费

provider 禁用、token 阈值、输入超限、两次输出均无效、provider failure 或 recovery 收敛时，T4 不产生半份文案。确定性消费者直接以 `interrupt.md` §3 的 `fallback_brief`、原始状态 facts、已验证 links 与完整候选 options 调用 `EmitInterrupt`；这就是 PRD 所说的「裸链接 + 原始状态文本」。不提取有效 attempt 的片段，不以第二个 LLM 修补。

该次 terminal fallback call 的消费者来源为 `{kind:"fallback",logical_call_id,version:"T4/fallback/v1",reason}`；正常来源为 `{kind:"brain",logical_call_id,prompt_version,output_schema_version}`。来源写入 Interrupt 展示审计/事件，不能替代 `EmitInterrupt` 的确定性 facts，也不建立 Gate link。无论正常或兜底，发射器仍独占结构校验、generation 去重、severity、critical 熔断和注意力配额；发射被拒绝时不得因 T4 重试另开旁路。

## 12. T6 打扰调度（M5 draft）

T6 只对一条尚未发射的候选 Interrupt 建议时机和 Channel；它不创建、关闭、合并或抑制 Interrupt。调用作用域为 `run`，`subject_key=run:<run_id>`；与具体 attempt 相关时才填写 attempt。调度结果只是交给确定性 scheduler 的候选，最终发射始终经过 `EmitInterrupt` 和 Channel delivery；T6 call 不进入 Gate 输入。

### 12.1 Input v1

顶层 closed object，以下字段均 required：

| 字段 | 类型 | 约束 |
|------|------|------|
| `run_id` | string | 1..256 bytes；等于 trace Run |
| `attempt_no` | integer/null | 非空时为正整数，且等于 trace attempt |
| `candidate` | closed object | 仅含下表字段 |
| `availability` | closed object | 仅含 `state`、`next_window_at_ms`；state 为 `available | unavailable | unknown`，时间为非负 integer/null；available 时 null，其余非空 |
| `attention` | closed object | 仅含 `fallback_immediate_min_severity`、`remaining`；前者为 `low | normal | high | critical`，后者是按 severity 排序的 closed `{severity,remaining}` 数组，remaining 为非负整数 |

`candidate`：

| 字段 | 类型 | 约束 |
|------|------|------|
| `reason` | string | PRD 七种 Interrupt reason 之一 |
| `severity` | string | `low | normal | high | critical`；确定性 `BaseSeverity` 结果 |
| `min_modality` | string | `voice | text | visual` |
| `expires_at_ms` | integer | 非负且晚于本次 scheduler 冻结时间 |
| `channel_candidates` | string[] | 1..8 项，1..128 bytes，UTF-8 bytes 排序去重；仅含配置的、可用的 Channel ID |
| `default_channel_id` | string | 必须精确命中 `channel_candidates`；由确定性配置选择 |

T6 只观察上述冻结快照，不能读取 Ledger、重新查 Forge、改变候选 facts 或自行计费。`remaining` 是建议排序的输入，不能作为发射额度的授权。

### 12.2 Output v1

closed object：

| 字段 | 类型 | 约束 |
|------|------|------|
| `delivery` | string | `immediate | batch | next_window` |
| `channel_id` | string | 必须精确命中 input `channel_candidates` |
| `suggested_downgrade` | boolean | 只作为 [`interrupt.md` §4.2](interrupt.md) 的至多一级降级输入 |
| `rationale` | string | trim 后 1..2000 bytes，无 Cc 控制码或换行 |

领域后校验还要求：`severity=critical` 只能为 `immediate`；`delivery=next_window` 时 `availability.next_window_at_ms < candidate.expires_at_ms`；`delivery=batch` 与 `next_window` 都不等于取消或关闭。T6 不能输出 severity、quota、reason、options、expires、on-expire 或任何「不发出」指令。`channel_id` 是选择，不是 Channel 凭据或发布请求。

T6 初始版本为 `T6/v1/<sha256前12位>`，`output_schema_version=1`；版本规则同 §2。正常来源只进入调度审计，不能改变 Interrupt generation key、Gate snapshot 或 Ledger 记录。

### 12.3 确定性兜底与配额

provider 禁用、token 阈值、输入超限、schema/domain failure、provider failure 或 recovery 时，不产出 LLM 调度建议。确定性 scheduler 以冻结的 `fallback_immediate_min_severity` 比较候选 severity：达到或超过阈值则 `immediate + default_channel_id`，否则 `batch + default_channel_id`，且 `suggested_downgrade=false`。它不把 token 耗尽解释为所有候选立即打扰。

正常建议和该兜底均须依序经过：到期/availability 的确定性检查、`Severity(..., suggested_downgrade)`、`EmitInterrupt` 的 generation 去重、注意力收费、非 critical 合批及 critical 熔断，最后才创建 `channel_publish`。配额耗尽只能由发射器合批，不能由 T6、其兜底或 Channel 借支。terminal fallback 的调度审计来源固定为 `{kind:"fallback",logical_call_id,version:"T6/fallback/v1",reason}`；不建立 Gate link。

## 13. T7 校准提案与 A7 防火墙（M5 draft）

T7 是 Ledger 的聚合读取面，不是学习后的判定器。它只能从确定性导出的 `AggregateLedgerEvidence` 生成待人审文本提案；调用作用域必须为 `aggregate`，`run_id/attempt_no` 必为空，subject 使用 §3 的 aggregate key。T7 不读取当前单条 Gate candidate、未冻结的 Forge 状态、open Interrupt 或可写 policy/context 文件，也不进入 Gate snapshot/link。

### 13.1 Input v1

顶层 closed object，以下字段均 required：

| 字段 | 类型 | 约束 |
|------|------|------|
| `aggregate_key` | string | 必须精确等于 trace `subject_key`，符合 §3 格式 |
| `window` | closed object | `{start_ms,end_ms}`；均为非负 integer，`start_ms < end_ms` |
| `categories` | array | 1..16 项、按 `task_kind` 排序；每项是 closed `{evidence_id,task_kind,certification_version,certified,evidence_summary}`，evidence_id 为 1..256 bytes 的确定性聚合 ID，task_kind 为五种 Run kind，版本和摘要均为确定性聚合值 |
| `replay_summary` | closed object | `{evidence_id,dataset_version,gate_version,total_samples,negative_samples,leak_count,false_block_count}`；evidence_id 为 1..256 bytes 的确定性聚合 ID，版本非空，计数为非负 integer |
| `semantic_material` | array | 0..64 项，按 `entry_id` UTF-8 bytes 排序；每项是 closed `{entry_id,material_kind,text}`，kind 为 `reject_reason | ask_text`，text 1..16384 bytes |

输入只能由 [`ledger.md` §2–§4](ledger.md) 的 immutable records、类别认证投影和离线 replay 集确定性组装。`categories`/`replay_summary` 不含 Run ID、路径、作者或单条 verdict；semantic material 只保留其 immutable entry ID 和原文，禁止附带可用于消费当前 Run 的身份。文本均为 untrusted data。整份 canonical JSON 超过 `brain.max_input_bytes` 时不调用 provider，按 §13.3 兜底。

### 13.2 Output v1

closed object：

| 字段 | 类型 | 约束 |
|------|------|------|
| `proposal_kind` | string | `policy | context` |
| `target_scope` | string | `project | global`；仅为人审时的建议，不是写入目标 |
| `title` | string | trim 后 1..160 bytes，无 Cc 控制码或换行 |
| `body` | string | trim 后 1..8192 bytes；可含 Markdown，但不得含 HTML、链接目标或可执行指令 |
| `evidence_entry_ids` | string[] | 1..64 项，按 UTF-8 bytes 排序去重；每项必须精确命中 input `semantic_material[].entry_id`，或为确定性 categories/replay evidence ID |
| `requires_human_approval` | boolean | 必须为 `true` |

该 schema 故意没有 policy patch、配置写入、context path、Gate verdict、auto-merge、severity、Interrupt、Run 或 action 字段。正常输出只能持久化为不可变 `proposal_draft`（带 logical call、prompt/schema version、aggregate key、evidence IDs 和 `pending_human_approval`）；它不是有效 policy、不是 context.md 内容，也不产生 outbox、预算或状态转移。人类通过独立、审计化流程选择采纳后，policy 仍须经启动/加载 schema、影子门禁认证和下一次 Gate 的冻结输入；context 仍是人写 Agent 读，不能回写当前 Run。

T7 初始版本为 `T7/v1/<sha256前12位>`，`output_schema_version=1`。T7 的正常来源仅关联 proposal draft；它不与任何 Gate snapshot 建 link，不能以 prompt/schema bump 改写历史认证。

### 13.3 不提案兜底与 A7 的可执行边界

provider 禁用、token 阈值、输入超限、schema/domain failure、provider failure 或 recovery 时，T7 的确定性兜底是**不创建 proposal draft**。terminal fallback call 与原因照 §3–§6 持久化；若需要审计引用，来源固定为 `{kind:"fallback",logical_call_id,version:"T7/fallback/v1",reason}`。不得用上一次有效提案、模板或单条历史决定替代，更不得因没有 T7 而改变 Gate 或 Interrupt 行为。

A7 通过以下边界落实，而不是靠 prompt 自律：

1. Ledger 的查询端口对 T7 只返回 §13.1 的聚合证据；T7 输入/输出类型不携带当前 Run、Gate candidate、open Interrupt、单条 verdict 或可执行 policy patch。
2. T7 的唯一写端口是 `proposal_draft`；它没有 `EmitInterrupt`、Gate evaluation、certification projection、policy loader 或 Context writer 的调用能力。任何 proposal 先由人显式批准，才可进入相应的独立写入口。
3. Gate 只读取冻结的有效 policy、类别认证投影和当前快照事实；不得读取 proposal draft、T7 trace、Ledger semantic material 或 T7 输出。Interrupt 发射器同样不得查询它们来压制或合批某一条 HITL。
4. 测试必须证明：任意 T7 输出、fallback、重放或历史数据变化都不能改变同一 `GateInput` 的 verdict、不能使既有/应发的单条 HITL 消失，也不能绕过 §5.6 认证；只有人审后形成的新有效配置才可能在**后续**冻结 Gate 输入中起作用。

## 14. Task Spec v1

T2 valid 后由确定性 assembler 生成：

```json
{
  "schema_version":1,
  "description":{"title":"...","body":"...","source_url":"..."},
  "goals":["..."],
  "guardrails":{"policy_hash":"...","rules":[]},
  "context":{
    "project":{"blob_hash":"...","text":"..."},
    "global":{"content_hash":"...","text":"..."},
    "task_annotations":[{"event_id":"...","text":"..."}]
  },
  "assignment":{"kind":"feature","agent":"claude-code","hitl_before_start":false},
  "brain":{"logical_call_id":"...","prompt_version":"..."}
}
```

组装顺序固定为 Description → Goals → Guardrails → project/global/task Context。project/global context 的来源路径、权限与缺省语义以 [`config.md`](config.md) 为唯一事实来源，本文不复制路径事实；缺失时为空内容，hash 为对规定空内容的 SHA-256（canonical 与 hash 规则见 [`config.md` §4](config.md)）。Guardrails 只来自有效 policy/硬编码默认，不接受 T2 改写。

整份 canonical JSON 与 digest 写 immutable snapshot；初始版本为 1。`/sift ask` 创建下一版本并保留旧 snapshot，已启动 attempt 继续引用旧版本。

## 15. 分阶段验收

### 15.1 M1：调用壳、T1/T2 与 Brain replay

1. fixture 覆盖 valid first、invalid→valid、invalid→fallback、timeout、nonzero exit、oversize、usage missing、usage invalid、spawn failed。
2. 两次 provider attempt 的 prompt bytes/request digest 完全相同；attempt identity 只差 provider_attempt；call 只经一次终结。
3. 内层触点输出的 unknown field/type/enum/fenced JSON/尾随文本均拒绝，不尽力解析；外层 envelope 未知诊断字段接受、重复键拒绝。
4. provider disabled 与 token threshold 写 `provider_attempt=0` attempt 行并走触点兜底。
5. token：attempt 1 用量越界后禁止 attempt 2；越界 post-charge 全额入账；跨 UTC 日界按 attempt 开始冻结的桶收费；零 token usage 不写 budget entry；重复 operation key 不重复收费。
6. T1：fallback 直接入队且不静默丢 Issue；LLM duplicate 建议不能绕过确定性确认。
7. T2 unknown/noncandidate Agent 触发同 prompt retry，最终进入人工分派；硬护栏不从 LLM 输出读取；hitl 强制规则不被 LLM `false` 降级；批准前 Run 不可 launch。
8. Task Spec 的四段来源/hash 可重建；worktree 中 context/policy 修改不进入 snapshot。
9. fake provider 合法 T2 输出可跑 M1 skeleton；真实 CLI 用 fixture 子进程测试，不依赖线上模型。
10. replay JSONL 一条 `brain_call` record 内携有序 attempts，可区分两个 provider attempt 并还原最终 fallback。
11. input 超过 `max_input_bytes` 时不调用 provider，走触点确定性兜底。

### 15.2 M2：T1 Intake crash/generation

以下验收依赖 M2 的真实 Forge comment worker、回复 receipt 消费与 `PersistIntakeDecision` 写端口，因此不属于 M1 退出条件：

1. 澄清/确认评论在“远端成功、本地提交前崩溃”后按 outbox marker 查询收敛，不重复发送。
2. 回复按当前 `clarification_generation` 仲裁；旧 generation 回复只追加审计事件，不推进 intake 状态。

### 15.3 M4：T3/T5、Gate 输入与 replay

1. T3 valid 输出和 provider disabled、token threshold、input over-limit、invalid→fallback 均产生高风险兜底；后者不能绕过 `risky-only` 人审。
2. T5 只消费归一为 failure 且有非空失败项的 CheckSuite；三个合法分类及 `retry_check_id` 互斥矩阵均可 closed decode，unknown field、枚举外值、未知/allow-failure retry ID 和 fenced JSON 触发同 prompt retry 后 HITL 兜底。
3. T5 的合法 `flaky + retry_check_id` 只建议有界确定性重试；`real_failure`、`infrastructure` 与任何 T5 fallback 都以 `failure_review` 进入 HITL，不由 LLM 改写 Interrupt 内容或预算。
4. T3/T5 使用同一 call/attempt trace、retry 与 per-attempt token post-charge；调用只在输出实际进入 Gate 输入时通过关联表连接对应 snapshot，同一 call 可连接多份 snapshot。
5. Gate 快照及 `gate_input_hash` 覆盖 T3 risk result/T5 triage（适用时）及来源与版本；正常输出与 fallback 的快照必然不同，导出的 Brain trace 可按版本重跑。

### 15.4 M5：T4/T6/T7 与 A7

1. T4 的 closed output 覆盖 headline、三点上限、候选 action ID 全排列和不可改写 effect/risk；任一失败只生成 `interrupt.md` §3 的 fallback，且仍由唯一发射器收费、去重与发布。
2. T6 的正常/兜底路径都覆盖 severity 阈值、availability/expiry、channel candidate 和 quota exhausted；critical 不可被延后，任何路径不得绕过配额或 critical 熔断。
3. T7 的两个 proposal kind 均只生成 `pending_human_approval` draft；schema、写端口和集成测试拒绝 Gate/Interrupt/action/policy patch 字段，fallback 不创建 draft。
4. 改变 T7 输出、历史 Ledger、replay 记录或 proposal draft 后，以相同冻结 Gate 输入重放仍得到相同 verdict；单条 Gate 和 HITL 路径没有 T7/Ledger proposal 查询。仅人审后的有效 policy 在新的冻结输入、认证与 Gate 评估下可产生后续影响。

## 16. 自查结果

- [x] B1：call/attempt 拆表、一次性终结与 `provider_error_code` 枚举落 storage §10.1/§13；replay 单 record 携有序 attempts。
- [x] B2：intake 投影、状态机、回复 generation 协议与 outbox 目的/key 的规格完整；实现与 crash/generation 验收明确归属 M2。
- [x] B3：T1/T2 输入输出逐字段冻结，含枚举、长度、互斥矩阵与总输入上限；`.schema.json` 单一来源。
- [x] B4–B7：token per-attempt 阈值与越界 post-charge、open-envelope 边界、截断/stderr 语义、T2 审批单事务均已写死。
- [x] Task Spec 来源、hash、不可变版本与 control-plane transport 对齐；路径事实只引用 config.md。
- [x] T3 风险分/风险点、失败或超预算高风险兜底与 Gate 来源/版本快照契约已冻结。
- [x] T5 flaky/真实失败/基础设施分类、有限确定性重试边界与失败 `failure_review` HITL 兜底已冻结。
- [x] T3/T5 prompt/schema 沿用统一版本规则、调用壳、token 收费与 trace；独立字段级评审已关闭嵌套字段、head/diff 绑定、rerun 目标和多 snapshot 关联。
- [x] M5 draft：T4/T6/T7 各自的 closed schema、独立 prompt/schema/fallback 版本、调用壳/trace 身份、T4/T6 兜底和 T7 不提案兜底已冻结。
- [x] A7：T7 聚合输入、仅 proposal draft 写口、Gate/Interrupt 的禁止读取面及回放测试判据已写成结构契约。
- [ ] M5 实现、对应 prompt assets/schema 生成源、storage proposal draft 和集成测试尚未交付；不得将 §11–§13 描述为已实现。
- [x] 相对链接存在、代码围栏闭合、无尾随空白。

**自查结论：** 既有 T1/T2/T3/T5 契约及其评审 P1（B1–B3）和采纳的 P2（B4–B7）保持 `active`；§11–§13 是待实现、待 M5 评审的字段级 draft。
