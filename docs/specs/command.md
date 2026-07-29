---
status: draft
created: 2026-07-29
summary: Forge 指令的鉴权、解析、回执与状态契约
---

# Command 规格

本文定义 M5 Command：将 forge 评论和审批标签归一为经鉴权的确定性 `DomainCommand`，并规定其幂等回执、Ledger 写入及 `startup_stall` 的 retry 两段式。它不定义 Interrupt 字段、Gate 判定、进程终止或 Forge API 细节，而是消费这些模块已经冻结的契约。

来源：[PRD §7.1、§9.2](../PRD.md)、[DESIGN §6.2、§6.4、§10.1](../DESIGN.md)、[WBS M5 §5.4](../WBS.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)。Interrupt 的当前对象、options、nonce 与隔离语义见 [`interrupt.md`](interrupt.md)；存储事务、收据、写端口和 race 仲裁见 [`storage.md` §6–§7、§11–§12](storage.md)；人类决定/语义原料/认证见 [`ledger.md`](ledger.md)；Forge actor 与评论/标签端口见 [`forge.md`](forge.md)；回执 outbox 见 [`outbox.md` §2、§5](outbox.md)。

## 1. 边界与不变量

1. Command 只消费 forge 的**驱动性事件**：`/sift` 指令评论和 `sift:approved` 标签的新增事件。Issue/Change 的关闭、合并等事实观测不进入本模块，仍按 Intake 的 facts-first 路径收敛。
2. 处理顺序固定为：事件身份去重 → actor 鉴权 → 严格语法/标签形态 → 目标与当前 Interrupt → nonce/version（评论）或 issuance cutoff（标签）→ `options[]` → 编译 `DomainCommand` → 单事务提交。不得以 LLM、评论语义或当前标签集合猜测动作。
3. 只有编译后的 `DomainCommand` 能请求 Run 转移；评论正文、标签名、T4 文案、Ledger 历史和 Brain recommendation 均不能直接写 `runs.status`。实际转移只经 [`storage.md` §11](storage.md) 的受限写端口及其私有 `transition()`。
4. 任一可执行命令必须绑定**唯一的当前 open Interrupt**，且其 forge target 等于该 Interrupt 冻结的发布 target。只按 Run、Issue、Change、最新评论或同一 target 上“看起来相关”的 Interrupt 匹配一律拒绝。
5. 所有受理结果以 forge event ID 幂等；同一 event 的重放只返回已持久化 outcome，绝不第二次转移、写 Ledger、启动 probe、轮换 nonce 或创建回执 operation。
6. `options[]` 是动作白名单，不是 UI 提示。动词即使在 PRD 全集内，只要不在当前 Interrupt 的 options 中就不得执行；特别是 `startup_stall` 的 `approve` 必须拒绝。
7. Command 不持有 SQL transaction，也不在事务内调用 Forge、发信号、检查进程或运行 Brain。它只把已验证输入交给存储写端口；外部评论和 retry probe 都在提交后推进。
8. 认证只确认“谁可发指令”，不把 allowlist 解释为可以绕过 nonce、options、CAS、Gate 或隔离。actor、token、nonce、session、permit、原始评论内容和数据库错误不得写入回执或安全事件的公开文本。

## 2. 事件入口、鉴权与目标绑定

### 2.1 归一输入

Command 接收 Forge adapter 已归一的下列值：

```text
ForgeCommandEvent {
  forge_event_id, project_id, target: TargetRef,
  source: comment | approval_label,
  actor, observed_at_ms,
  comment_id?, body?, label_event_id?
}
```

`forge_event_id` 是项目内稳定的远端事件身份；评论使用远端 comment/note ID，标签使用资源 label-event ID。`target` 是 `issue | change` 加项目内 ID，不能是裸数字。`actor`、时间、项目和 target 缺任一项都不是可执行输入。

Forge adapter 已对评论/标签事件的 actor 缺失 fail closed；Command 仍必须将缺 actor 视为 `ignored_missing_actor`，不得用 Issue 作者、评论显示名、目标权限、当前登录用户或上一次可信 actor 补全。项目启动期冻结的 allowlist 是唯一授权依据；运行期配置文件变化不热生效，见 [`config.md` §3–§4](config.md)。

### 2.2 鉴权、收据与静默边界

`forge_event_receipts(project_id, forge_event_id)` 是第一层不可变幂等收据。其处理规则如下：

| 输入 | receipt disposition | Command 后果 | 是否回执评论 |
|---|---|---|---|
| 不是 `/sift` 评论、不是审批标签新增 | 不创建 Command receipt | 忽略 | 否 |
| actor 缺失 | `ignored_missing_actor` | 忽略并记低敏安全事件 | 否 |
| actor 不在冻结 allowlist | `ignored_untrusted_actor` | 忽略并记低敏安全事件 | 否 |
| 可信 actor 的候选命令 | `accepted`，`domain_event_id` 指向 `command.accepted` 或 `command.rejected` | 按本文继续 | 是，除非事务本身回滚 |

`accepted` 只表示事件已通过身份闸并被 Command 消费，**不表示该动作被执行**。例如过期 nonce、错误 target、option 不允许和 probe 在途都可有 `accepted` receipt，但其 `command.rejected` event 的 closed `disposition` 才是最终语义。重复事件直接读取这两个既有事实，不创建新 event 或回执。

只有可信 actor 的候选命令可得到回执。对非命令、缺 actor 或不可信 actor 保持静默，避免把 Command 变成给攻击者探测 Run/Interrupt 的 oracle。

### 2.3 当前 Interrupt 的解析

在单个写事务内，按下列全部条件读取唯一对象：

- `status=open`；
- `run_id` 与命令携带的 Run（评论）或 target 所归属的 Run（标签）一致；
- 冻结的 forge comment target 精确等于事件 `target`；
- `dispatch_state` 不是 `probe_in_progress`，除非输入是合法的启动/结果事实而非本模块的人工命令；
- 对评论，`nonce` 与 `version` 快照对应的当前 nonce 相同；
- 对标签，满足 §2.5 的 issuance cutoff；
- 请求的 action ID 出现在 `options_json` 中。

出现零个或多个候选均为 `interrupt_not_current`；不得挑选最新一条。`status=closed`、nonce 不同、已升级后的旧 version、外部事实关闭、或 Run 版本 CAS 失败均是拒绝而非重新解释历史命令。

### 2.4 评论语法

除 `ask` 与可选 reject 原因外，评论 body 必须逐字节匹配一行 ASCII 命令并在最后一个参数后 EOF；不接受前导/尾随空白、Markdown quote、代码围栏、大小写变体、别名、额外参数或同一评论中的第二条命令。`\r\n`、`\r`、`\n` 都不得作为非 `ask` 命令的一部分。

```text
/sift approve <run_id> <nonce>
/sift reject  <run_id> <nonce> [<reason>]
/sift retry   <run_id> <nonce>
/sift hold    <run_id> <nonce> <duration>
/sift ask     <run_id> <nonce> <text>
```

`run_id` 与 `nonce` 均为 32 个小写十六进制字符；二者必须分别精确匹配存储的 Run ID 与当前 Interrupt nonce。`duration` 是无空白的 Go duration 字符串，必须为正且不超过创建时冻结的 `hold_max_duration`；裸数字、复合空白和负数无效。

`reason` 若存在，必须以紧跟 nonce 的单个 ASCII space 开始，去掉这一个分隔 space 后为 1–16384 UTF-8 bytes、不得含 NUL。`text` 同样为 1–16384 UTF-8 bytes、不得含 NUL，且在去掉 `nonce` 后的单个分隔 space 之外**原样**保存；Command 不 trim、折行、Markdown 解析、摘要、翻译或交给 LLM。`ask` 可含换行；它是唯一允许多行 body 的动词。空 reason 不产生 `SemanticMaterialV1`；空 ask text 拒绝。

渲染 Interrupt 时必须把当前可执行评论命令以本节的完整字面量列出；不得只显示 `/sift approve` 而隐藏 Run/nonce。nonce 是公开的防重放关联值，不是 capability secret。

### 2.5 审批标签

唯一标签命令是可信 actor 发出的新增事件：

```text
label = "sift:approved"; action = added
```

它只可编译为 `approve`，且只有目标上唯一当前 open Interrupt 的 `options[]` 含 `approve` 时才有效。移除事件、任意其他 `sift:*` 标签、当前标签集合的读取结果，以及没有对应新增 event 的“标签仍在”都不能生成命令。

标签自身没有承载 nonce 的位置，因此不能伪造一个空 nonce 交给评论解析器。它的 anti-replay 绑定是：同一 forge label-event ID 的 receipt 去重、目标精确匹配、唯一当前 Interrupt、以及

```text
label_event.observed_at_ms >= interrupt.nonce_issued_at_ms
```

其中 `nonce_issued_at_ms` 是该 Interrupt 当前 version/nonce 成为有效值的持久化时间。标签 event 早于该 nonce 的签发、在 target 上有多个候选 Interrupt、或 `approve` 不在 options 内时一律拒绝。这样旧审批标签不会在后续 Interrupt 开启或 nonce 轮换后自动生效；若人要批准新检查点，必须在新 nonce 签发后重新添加标签。标签路径在完成这些绑定后生成与评论同形的 `DomainCommand`，并走同一 Ledger、transition、receipt 和 ack 路径。

## 3. 编译结果与通用执行语义

### 3.1 `DomainCommand`

解析器的输出不是状态转移，而是 closed union：

```text
DomainCommand {
  source: forge_comment | approval_label,
  forge_event_id, command_event_id, actor,
  run_id, interrupt_id, expected_run_version,
  expected_interrupt_version, expected_nonce,
  action: approve | reject | retry | hold | ask,
  hold_duration_ms?, reject_reason?, ask_text?,
  occurred_at_ms
}
```

`expected_*` 是已验证的当前投影快照，不从评论猜测；标签的 `expected_nonce` 是按 §2.5 在事务内解析的当前 nonce。`command_event_id` 是 append-only `command.accepted` 或 `command.rejected` event 的 ID，并是 Ledger `provenance.source.id` 与回执 payload 的唯一来源。该 union 不允许调用方附带目标 Run status、任意 SQL、任意 option、calibration ID、Task Spec ID、severity 或 outbox key。

编译后的执行器只能调用：

- 普通动作：`ApplyInterruptCommand`，并在需要时由它调用 `TransitionRun`/私有 `transition()`、`RecordHumanDecision`、Task Spec snapshot 和 outbox；
- `startup_stall` 的终局 reject 或与事实并发的分支：`ResolveAttemptRace`；
- `startup_stall` retry 的成功结果：`ApplyRetryProbeResult`。

这三个端口必须共享 [`storage.md` §12.7](storage.md) 的仲裁原语；不得让评论 consumer、标签 consumer、Supervisor 或 wrapper 各实现一份相似的 Run/Interrupt 更新。

### 3.2 动作与 Interrupt 生命周期

| action | 前提 | 通用效果 |
|---|---|---|
| `approve` | option 含 `approve` | 关闭为 `responded`；以 reason 的确定性后继恢复编排。它不是泛用的 `waiting_human → running` 快捷写入。 |
| `reject` | option 含 `reject` | 终局拒绝：Run 经唯一 transition 进入 `failed`；关闭为 `responded`。`startup_stall` 另受 §5.4 约束。 |
| `retry` | option 含 `retry` | 请求对应 reason 的确定性重试/重算；非 `startup_stall` 的完成语义由该 reason owner 在同一事务明确给出，不能猜成新 attempt。`startup_stall` 必须走 §5。 |
| `hold` | option 含 `hold` 且 duration 合法 | 保持 open 与 `waiting_human`，将 `expires_at_ms` 设为 `occurred_at_ms + hold_duration_ms`；不写 attempt resolution。 |
| `ask` | option 含 `ask` 且 text 合法 | 写当前 Run 的新 Task Spec snapshot、保留原 snapshots，恢复该 reason 的确定性后继；不得升格项目/全局 Context。 |

除 `startup_stall` 的 retry 请求外，成功的非终局 `hold` 或仍保持 open 的 action 都必须 `version+1` 并轮换 nonce；回执携带新 nonce。这样旧评论不能在一次人工延后或澄清后继续作用。关闭 Interrupt 的动作不轮换 nonce。每个更新均以 `expected_interrupt_version` CAS；CAS 失败重读后只可返回已持久化同 event outcome 或 `rejected_stale`，不得在新对象上重放旧命令。

reason owner 必须把 `approve`/普通 `retry`/`ask` 的最终后继写成确定性 `DomainCommand` 或 operation，而非从自然语言解释动作：`design_approval` 的 approve 使 Run 可重新进入启动队列；Gate 类 approve 只恢复冻结 Gate/merge 后继；`agent_blocked` 的 ask 将澄清带入 Task Spec 后恢复执行；`merge_conflict`/`failure_review` 的 retry 只创建其明确的重试或重新观测后继。任何后继仍须满足 Run 状态图、attempt 所有权和 Gate 契约。

## 4. Ledger、Task Spec 与 transition 契约

### 4.1 人类决定

每个成功消费的命令都以 `command_event_id` 调用 [`ledger.md` §3](ledger.md) 的唯一 `recordHumanDecision` 入口；调用方不得传入或猜测 calibration ID。动作映射为：

| Command action | `HumanDecisionV1.action` | `calibration_decision` | 语义原料 |
|---|---|---|---|
| `approve` | `approve` | 仅 immutable Gate binding 为二元时为 `allow`，否则 null | 无 |
| `reject` | `reject` | 仅 immutable Gate binding 为二元时为 `block`，否则 null | 有 reason 时为原文 `reject_reason` |
| `retry` | `retry` | null | 无 |
| `hold` | `hold` | null | 无 |
| `ask` | `ask` | null | 原文 `ask_text` |

因此非 Gate Interrupt 的 approve/reject 仍保留人类动作审计，但不得伪造 calibration 或认证样本；`retry`、`hold`、`ask` 永不结算 calibration。对 Gate-linked 二元决定，calibration 一次性补全、Ledger entry、认证 snapshot/current CAS、Interrupt/Run 结果、event 和回执 operation 必须同一事务提交。`inconclusive`、无 binding 或重复不同决定一律不猜测结算。

### 4.2 Task Spec 与 Run 转移

`ask` 的 text 只写新 `task_spec_snapshots` 版本和 `runs.current_task_spec_id`，不 UPDATE 旧 snapshot，也不写项目/全局 Context。新 snapshot 必须记录 command event 的不可变来源，使回放可知澄清来自哪条已鉴权评论。

所有 Run 变更仍由 [`storage.md` §12.1](storage.md) 的 CAS 纪律执行：预期 version 与合法状态均须命中，转移事件与必要 outbox 同事务；非法转移是可审计拒绝而不是 silent success。Command 不可把 `approve` 解释成自动合并、把 `retry` 解释成无条件 spawn，或以 Ledger 写入成功掩盖 transition 失败。

## 5. `startup_stall`：请求与结果分离

`startup_stall` 的 options 永远只有 `retry | reject | hold`。它没有 `approve`；`auto_reject` 也在配置、发射器与 Command 三层拒绝。attempt 的 `frozen` 隔离独立于 Run/Interrupt 终态，只有持久化消失证据或明确 operator 强制清理才可解除，见 [`storage.md` §1、§5.5](storage.md)。

### 5.1 retry 请求段

合法 `/sift retry <run_id> <nonce>` 或经 §2.5 验证的标签（仅当该 Interrupt 理论上有 approve，故对 startup_stall 标签永不适用）在单事务中：

1. CAS 当前 open `startup_stall` 的 Run version、attempt generation、Interrupt version/nonce 和无在途 probe；
2. 写 `command.accepted`、`forge_event_receipt`、`HumanDecisionV1(action=retry)` 与必要 event；
3. 插入唯一 `attempt_probes(state=pending)`，冻结 `expected_run_version`、`expected_generation`、`interrupt_id` 和请求 event；
4. 将 Interrupt `dispatch_state` 置为 `probe_in_progress`。

请求段**不**关闭 Interrupt、不改变 `waiting_human`、不创建新 attempt、不解除隔离、不写 `attempt_resolution`，也不宣称执行体已停止。Supervisor 在事务外运行既有受控终止/身份/消失探测；其信号和观测按 [`storage.md` §5.5](storage.md) 记入 probe/event，不进 outbox。

`probe_in_progress` 时任何新的人工命令（含第二次 retry、hold、reject）都不得改变状态或创建第二 probe；对可信 actor 的候选命令创建一次 `probe_in_progress` 回执。合法迟到 `claim:started`/result 事实不受此限制，仍进入 `ResolveAttemptRace`。

### 5.2 probe 未确认消失或事实先到

探测未证明消失时：probe 终结为 `failed`；同一 Interrupt 保持 open/冻结，按既有升级规则增加 escalation、轮换 nonce、version+1，达到上限则 `hold`。它不新增 Interrupt、不退注意力费用、不写 resolution，并为最初 retry command 创建 outcome 为 `absence_unconfirmed` 的回执。

若 probe 在途时合法 started/result 事实先提交，`ResolveAttemptRace` 必须：以事实接管监督、Run 在适用时 `waiting_human → running`、关闭同一 Interrupt 为 `superseded_by_fact`、probe 置 `superseded`，并为等待结果的 retry 命令创建 `superseded_by_fact` 回执。retry 请求的前提已被事实推翻，不能继续终止该正常执行体或开新 attempt。

### 5.3 retry 成功结果段

只有 probe 已取得可持久化的旧执行体消失证据时，`ApplyRetryProbeResult` 执行 [`storage.md` §12.5](storage.md) 的单一 CAS 事务：

1. probe 变为 `succeeded` 并写证据摘要；
2. 旧 attempt 终结，并仅在这里写 `attempt_resolution=retry_after_absence`；
3. isolation 解除；
4. 当前 Interrupt 关闭为 `responded`；
5. Run `waiting_human → queued`；
6. 创建且仅创建一个新的 `pending` attempt 与 claim；
7. 创建 launch 和该 forge command 的 ack operation；
8. 追加全部领域事件。

任一 CAS 前置（Run/attempt/Interrupt/probe）变化使整笔回滚并重读，不得留下“已关 Interrupt、未建 attempt”或“已 queued、未入 launch outbox”。worker 仅在提交后派发新 attempt。

### 5.4 reject、hold 与迟到事实

`startup_stall` reject 必须调用 `ResolveAttemptRace` 的终局决定分支，而不是普通 `TransitionRun`：一次写不可逆 `attempt_resolution=reject`、Run → `failed`、Interrupt → `responded`、Ledger/语义原料/event/ack；attempt 仍 frozen，并安排对随后取得身份的执行体继续受控终止。reject 的“放弃”从不表示执行体已消失。

hold 仅顺延 expiry 并轮换 nonce；自动 escalate、封顶 hold、retry 请求与 hold 都不写 resolution。故其后合法 started/result 仍事实优先。若 `reject` 或 `retry_after_absence` 已先落定，迟到事实必须被 `ResolveAttemptRace` 吸收：记录身份/安全事件、返回 `superseded_by_decision`、不复活旧 Run、不解除旧隔离，并继续或安排受控终止。拒绝 RPC 而丢弃迟到身份是违约。

## 6. 回执

每个可信 actor 的候选命令在其受理或最终 probe outcome 事务中创建一个且仅一个 `command_ack` operation：

```text
key = command:<forge_event_id>:ack
kind = command_ack
purpose = command_ack
```

该事务先把下列 closed `CommandAckV1` 写入 `command.ack_requested` event payload；`action`、`run_id`、`interrupt_id` 对语法/target 尚未解析的拒绝可为 null，其他字段不可空：

```json
{
  "schema_version": 1,
  "command_event_id": "…",
  "action": "retry",
  "disposition": "applied",
  "run_id": "…",
  "interrupt_id": "…",
  "next_nonce": null
}
```

`disposition` 只能是 `applied | rejected_stale | rejected_syntax | rejected_option | rejected_target | probe_in_progress | absence_unconfirmed | superseded_by_fact | superseded_by_decision`。`next_nonce` 仅当同一 open Interrupt 已轮换 nonce 时为 32 位小写 hex；其余为 null。event payload 不含评论原文、reject/ask 文本、allowlist、进程身份、消失证据、token 或数据库错误。

outbox operation 使用 [`outbox.md` §5.1](outbox.md) 已冻结的 `forge_comment` outer payload、target 和 marker 协议：target 固定为原 command target，不能在重试时改投另一 Issue/Change；其 `markdown` 仅由 `CommandAckV1` 确定性渲染。renderer 至少输出 action、disposition、Run 和 Interrupt；字段为 null 时明确说明“未识别到可执行目标”。`next_nonce` 非空时必须输出新的可执行命令提示。Forge comment worker 依 operation marker 收敛为 effectively-once；ack 投递失败不回滚已执行的领域决定，按 outbox 重试。原 command 的重放只能复用同一 operation，不能再发一条确认。

## 7. 验收派生

M5 至少覆盖：

1. 评论与标签均要求可信 actor；缺 actor、非 allowlist、旧 label event、错误 target、关闭/歧义 Interrupt 均不改变 Run。
2. 语法对大小写、前后空白、额外参数、非法 ID/nonce/duration、空 ask、过长/NUL 文本和多命令 body fail closed；ask/reject 原文按 Ledger schema 保留。
3. 旧 nonce、升级后 nonce、非当前 option、`startup_stall approve`、`startup_stall auto_reject` 全部拒绝；成功保持 open 的动作轮换 nonce。
4. 同一 forge event 的任意重放只产生一个 receipt/event、一次 Ledger 写入、一次 state effect 和一个 ack operation；评论与标签走同一 `DomainCommand` 执行器。
5. Gate 绑定的 approve/reject 正确结算 calibration/认证；无 binding 或 inconclusive 不伪造样本；retry/hold/ask 不结算；ask 创建新 Task Spec 而不覆盖历史。
6. `startup_stall` retry 请求不关闭 Interrupt、不写 resolution、不创建 attempt；probe 在途拒绝新命令；失败复用同一 Interrupt、轮换 nonce、封顶 hold。
7. retry 成功的崩溃注入逐点断言消失证据、resolution、隔离解除、Interrupt、Run、唯一新 attempt/claim、launch/ack operation 和事件全有或全无。
8. 事实先到、reject 先到、retry 成功先到及 probe 在途事实到达均经同一 race 原语收敛；不出现第二 owner、悬空 Interrupt、错误的 isolation release 或丢失回执。
