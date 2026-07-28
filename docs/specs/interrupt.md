---
status: draft
created: 2026-07-29
summary: Interrupt 的确定性字段、发射、去重与 startup_stall 契约
---

# Interrupt 规格

本文冻结 M3 Attention 泛型发射核心的字段和确定性契约。它使每个 PRD reason 即使没有 T4/T6 也能生成可见、可发布的 fallback Interrupt；并规定 `startup_stall` 在无法证明执行体消失时的唯一安全出口。

来源：[PRD §4.1–§4.4、§5.5、§7.1](../PRD.md)、[DESIGN §8.7、§10.1](../DESIGN.md)、[WBS M3 §3.6–§3.7](../WBS.md)、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)。持久化和事务边界见 [`storage.md` §6、§12.2](storage.md)，默认超时见 [`config.md` §3.9](config.md)，发布 operation 见 [`outbox.md`](outbox.md)。

## 1. 范围与不变量

1. `EmitInterrupt` 是**唯一**创建 Interrupt 的领域入口。恢复、Runtime、Gate、Report 与后续 Attention/Command 都只能调用它；不得直接插入 `interrupts`、扣注意力预算或创建发布 operation。
2. 发射器只接受确定性事实、确定性 fallback 模板和确定性 severity 输入；LLM/T4 的建议只能替换或缩短 `brief`，且不得改变 `reason`、`min_modality`、options、links 必填项、expires/on-expire 或 severity，不能使对象更紧急。
3. 发射器在同一事务内完成 Run 转移（或确认已在合法人工态）、生成键去重、首次注意力记账、Interrupt、事件和发布 operation。已有生成键时返回既有记录，不重复扣费或创建 operation。
4. 任一 Interrupt 必须有 1–4 个互斥 options、独立可朗读且不超过 40 Unicode code points 的 headline、非空 brief、`links`、`expires_at` 和 `on_expire`。表达不出这些内容时拒绝发射并记录确定性诊断，而不是发出含糊问题。
5. M3 的可见发布面是已有的 forge comment 加本节 fallback renderer。T4/T6、Channel、智能简报、超时 tick、升级投递、critical 熔断和 Command 的执行语义属于 M5；本规格不把它们预支为已实现能力。

## 2. 最小对象与输入

存储列、nonce、version、delivery 及关闭原因以 [`storage.md` §6](storage.md) 为准。本规格冻结其领域内容：

```text
Interrupt {
  run_id, attempt_no?, reason, severity,
  headline, brief, options[], min_modality, links[],
  expires_at, on_expire,
  generation_key
}
Option { id, label, effect, risk }
```

`id` 是服务端校验的稳定动作 ID；`label` 可朗读；`effect` 和 `risk` 是给 renderer 的确定性说明。options 是互斥的终端选择或互斥的下一步请求，不能把同一动作以不同措辞重复列出。指令必须匹配当前 nonce **且**对应一个 option；指令鉴权、语法和执行归 M5 Command。

`EmitInterrupt` 至少接收：Run/attempt 身份、reason、稳定 cause/source facts、gate 阶段、护栏等级、已升级次数、当前时间和冻结后的 attention/config 快照。它从本规格的表生成其余字段；调用方不能传入任意 severity、options 或生成键覆盖结果。

## 3. 全部 reason 的 fallback 契约

下表是无 T4/T6 时的合法最小对象。headline 不插入不受控文本；brief 使用对应事实的确定性摘要，links 只保留已知且可访问的链接/本地路径。`approve`、`reject`、`retry`、`hold`、`ask` 分别对应 PRD §7.1 的动词；`approve` 在软护栏场景表示批准该次豁免。

| reason | min_modality | fallback headline | options（id：effect） | fallback brief 必含事实 | links 至少包含 |
|---|---|---|---|---|---|
| `design_approval` | `text` | `需要批准后再开始` | `approve`：开始；`reject`：停止；`hold`：继续等待 | 风险摘要、推荐动作 | Issue、Task Spec |
| `guardrail_violation` | `text` | `软护栏需要人工裁决` | `approve`：豁免；`reject`：停止；`hold`：继续等待 | 命中规则、影响范围、推荐动作 | Issue、策略/规则证据 |
| `code_review` | `visual` | `变更等待代码审阅` | `approve`：继续；`reject`：停止；`hold`：继续等待 | Change、head、审阅要求 | Change、diff |
| `agent_blocked` | `voice` | `Agent 需要你澄清` | `ask`：注入澄清；`retry`：重试；`reject`：停止；`hold`：继续等待 | 阻塞摘要、已尝试事项、推荐动作 | Issue、Agent 日志 |
| `merge_conflict` | `voice` | `合并冲突需要处理` | `retry`：重新执行；`reject`：停止；`hold`：继续等待 | 冲突事实、当前 head、推荐动作 | Change、冲突/CI 证据 |
| `failure_review` | `voice` | `失败需要人工决定` | `retry`：重试；`reject`：停止；`hold`：继续等待 | 失败类别、失败证据、推荐动作 | Issue、日志或 CI 证据 |
| `startup_stall` | `text` | `无法确认旧执行体已停止` | `retry`：重新探测；`reject`：放弃并保持隔离；`hold`：继续等待 | attempt/generation、无法证明消失的原因、隔离后果 | Issue、attempt/进程诊断、worktree 路径 |

`visual` 是不可降级的审阅红线；`voice` 只说明最低媒介，不授权绕过 forge 上的服务端校验。links 缺少某项时只省略该项，不得伪造 URL；若一个 reason 的最低必要事实均不可取得，发射失败而非用 LLM 补猜。

## 4. 超时与 severity

### 4.1 超时

创建时从 [`config.md` §3.9](config.md) 冻结 `expires_after`、`on_expire` 和 `max_escalations`，并令 `expires_at = now + expires_after`。合法的 `on_expire` 仅为 `hold`、`escalate`、`auto_reject`；不得有 `auto_approve`。配置表是各 reason 的唯一默认值来源，本规格不复制数值。

`startup_stall` 在 schema 和发射器中双重拒绝 `auto_reject`；它只能 `escalate`，到上限后 `hold`，保持 open、隔离和事实优先窗口。其余 reason 到上限的去向按配置冻结值。M3 创建字段但不实现 M5 的 tick/升级或 Command 处置。

### 4.2 severity 纯函数

severity 的唯一计算为：

```text
Severity(reason, gate_phase, guardrail_level, escalation_count) -> low|normal|high|critical
```

`gate_phase` 只能是 `none | pre_start | review | merge`；`guardrail_level` 只能是 `none | soft | hard`。不存在 Gate 的调用传 `none`；`hard` 护栏不产生 Interrupt，调用即为契约错误（硬护栏直接失败）。先取下表 base，再按规则提升，LLM 只能建议向下一级，不能低于 `low`：

| reason | base severity |
|---|---|
| `design_approval` | `normal` |
| `guardrail_violation` | `high` |
| `code_review` | `normal` |
| `agent_blocked` | `normal` |
| `merge_conflict` | `high` |
| `failure_review` | `high` |
| `startup_stall` | `high` |

提升规则按固定顺序执行：`soft` 至少为 `high`；`gate_phase=merge` 的 `code_review` 至少为 `high`；每个未封顶的 escalation 提升一级（`low → normal → high → critical`）。`escalation_count >= max_escalations` 时不再提升。发射器把所得 severity 交给注意力收费口；critical 的 M5 熔断不得由调用者绕过。

## 5. 生成键与发布

生成键是 canonical UTF-8 tuple 的 SHA-256（字段以 NUL 分隔，枚举值为本规格字面量）；它不是 nonce，也不是 outbox key。每个 reason 的 cause 必须稳定地标识同一事实：

| reason | generation tuple（除 `run_id` 外） |
|---|---|
| `design_approval` | `task_spec_snapshot_id` |
| `guardrail_violation` | `policy_snapshot_id, violation_code, subject_digest` |
| `code_review` | `change_id, head_sha` |
| `agent_blocked` | `attempt_no, generation, report_id` |
| `merge_conflict` | `change_id, head_sha, conflict_digest` |
| `failure_review` | `attempt_no, generation, failure_digest` |
| `startup_stall` | `attempt_no, generation, cause` |

因此 `startup_stall` 的生成键语义严格为 **`(run_id, attempt_no, generation, cause)`**。`cause` 必须是受控终止的确定性失败分类（例如 `process_identity_unknown`、`termination_unconfirmed` 或 `process_group_unverified`），不是日志文本。四个发现者（超时扫描、恢复扫描、`kill`、`retry`）对同一 tuple 必须命中同一 open Interrupt。

首次发射创建的发布 operation 使用 [`outbox.md` §2](outbox.md) 的 `interrupt:<interrupt_id>:publish:0`；它只负责发布已选定的 Interrupt，不能替代生成键去重。M3 以 forge comment renderer 发布；发布失败、重试和“已生成/已送达”分离按 storage/outbox 契约处理。

## 6. `startup_stall` 的隔离与事实仲裁

受控终止在身份无法确认或有界终止后仍无法证明旧 wrapper/进程组消失时，必须调用 `EmitInterrupt(startup_stall)`。同一事务将 Run 转 `waiting_human`、attempt 标为 `frozen` 隔离、写一条 Interrupt/一次预算/一个发布 operation。worktree 在执行体消失证据出现前不得回收或被新 attempt 复用；Run 终态也不解除隔离。

这不是“已停止”的结论：冻结的 claim/session/permit 仍可能承接合法迟到启动事实。`claim:started`、恢复补 started、迟到 `result.json` 与后续 Interrupt 指令必须经 [`storage.md` 的 `ResolveAttemptRace`](storage.md) 这一 CAS 仲裁入口：

- resolution 尚未落定时，合法事实优先：接管监督、Run 回 `running`，并以 `superseded_by_fact` 关闭该 Interrupt；
- `reject` 或 `retry_after_absence` 已落定时，迟到事实不推进旧 Run，登记可终止身份并返回 `superseded_by_decision`；
- `hold`、`escalate` 和升级封顶不写 resolution，事实优先窗口继续开放。

M5 才接通 `startup_stall` 的 retry 探测请求/结果两段式和指令执行；其唯一合法动作、probe 及原子成功事务由 [ADR-013](../decisions/013-startup-stall-retry-convergence.md) 与 [`storage.md` §12.5](storage.md) 定义。M3 只负责让“无法证明消失”可见、唯一且保持隔离，绝不把它静默留在 `queued`。

## 7. M3 验收派生

- 七种 reason 都能在无 T4/T6 时按 §3 创建结构合法、可发布的 fallback。
- 重放相同输入产生同一字段值和同一生成键；同一 key 的并发发射只产生一条 Interrupt、一次预算和一条发布 operation。
- 对 `startup_stall` 并发调用四个发现者，断言键为 `(run_id, attempt_no, generation, cause)`、Run 可见为 `waiting_human` 且 attempt/worktree 保持隔离。
- 尝试 `startup_stall + auto_reject`、超过四个 options、非互斥 options、缺失最低事实或调用方指定 severity 必须被拒绝。
