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
2. 发射器只接受确定性事实、确定性 fallback 模板和确定性 severity 输入；M3 不接受 LLM/T4 severity 输入。M5 可将通过 schema 校验的建议作为唯一算法的 `suggested_downgrade` 输入，且至多降低一级；它不能改变 `reason`、`min_modality`、options、链接最低事实、expires/on-expire 或使对象更紧急。
3. 发射器在同一事务内完成 Run 转移（或确认已在合法人工态）、生成键去重、首次注意力记账、Interrupt、事件和发布 operation。已有生成键时返回既有记录，不重复扣费或创建 operation。
4. 任一 Interrupt 必须有 1–4 个互斥 options、独立可朗读且不超过 40 Unicode code points 的 headline、非空 brief、存在的 `links` 数组、`expires_at` 和 `on_expire`。`links` 可在 §3.3 所限情形为空；表达不出所需字段时拒绝发射并记录确定性诊断，而不是发出含糊问题。
5. M3 的可见发布面是已有的 forge comment 加本节 fallback renderer。T4/T6、Channel、智能简报、超时 tick、升级投递、critical 熔断和 Command 的执行语义属于 M5；本规格不把它们预支为已实现能力。

## 2. 最小对象与输入

存储列、nonce、version、delivery 及关闭原因以 [`storage.md` §6](storage.md) 为准。本规格冻结其领域内容：

```text
Interrupt {
  run_id, attempt_no?, reason, severity,
  headline, brief, options[], min_modality, links[],
  expires_at, on_expire, generation_key
}
Option { id, label, effect, risk }
Link { label, target }
```

`id` 是服务端校验的稳定动作 ID；`label` 可朗读；`effect` 和 `risk` 是给 renderer 的确定性说明。options 是互斥的终端选择或互斥的下一步请求，不能把同一动作以不同措辞重复列出。指令必须匹配当前 nonce **且**对应一个 option；指令鉴权、语法和执行归 M5 Command。

`EmitInterrupt` 至少接收：Run/attempt 身份、reason、稳定 cause/source facts、gate 阶段、护栏等级、已升级次数、当前时间和冻结后的 attention/config 快照。它从本规格的表生成其余字段；调用方不能传入任意 severity、options 或生成键覆盖结果。

## 3. 全部 reason 的 fallback 契约

### 3.1 固定对象

下表的 headline、options 四字段与事实键均为规范字面量。`approve`、`reject`、`retry`、`hold`、`ask` 分别对应 PRD §7.1 的动词；`approve` 在软护栏场景表示批准该次豁免。

| reason | min_modality / headline | options（完整 `Option`） | 必需事实键（依次） |
|---|---|---|---|
| `design_approval` | `text` / `需要批准后再开始` | `approve` / 批准并开始 / Run 进入可启动状态 / 开始后会消耗执行资源；`reject` / 拒绝并停止 / Run 停止 / 需人工重新发起；`hold` / 暂缓决定 / 保持等待 / Run 继续占用待处理项 | `risk_summary`, `recommended_action`, `task_spec_ref` |
| `guardrail_violation` | `text` / `软护栏需要人工裁决` | `approve` / 豁免本次规则 / 继续当前 Run / 豁免会接受规则所述风险；`reject` / 拒绝并停止 / Run 停止 / 需人工重新发起；`hold` / 暂缓决定 / 保持等待 / Run 继续占用待处理项 | `rule_id`, `impact_scope`, `recommended_action`, `policy_evidence_ref` |
| `code_review` | `visual` / `变更等待代码审阅` | `approve` / 批准审阅 / 继续后续流程 / 批准变更的当前内容；`reject` / 拒绝变更 / Run 停止 / 需重新修改后发起；`hold` / 暂缓审阅 / 保持等待 / Change 继续待审 | `change_ref`, `head_sha`, `review_requirement`, `recommended_action`, `diff_ref` |
| `agent_blocked` | `voice` / `Agent 需要你澄清` | `ask` / 提供澄清 / 写入澄清内容 / 澄清会改变后续执行方向；`retry` / 重试当前工作 / 再次尝试执行 / 未澄清时可能再次阻塞；`reject` / 停止 Run / Run 停止 / 已完成工作不会继续；`hold` / 暂缓决定 / 保持等待 / Run 继续占用待处理项 | `blocker_summary`, `attempted_summary`, `recommended_action`, `agent_log_ref` |
| `merge_conflict` | `voice` / `合并冲突需要处理` | `retry` / 重新执行合并 / 再次尝试合并 / 冲突未变时会再次失败；`reject` / 停止 Run / Run 停止 / Change 不会合并；`hold` / 暂缓决定 / 保持等待 / Change 继续待处理 | `change_ref`, `head_sha`, `conflict_summary`, `recommended_action`, `conflict_evidence_ref` |
| `failure_review` | `voice` / `失败需要人工决定` | `retry` / 重试失败步骤 / 再次执行 / 相同故障可能再次发生；`reject` / 停止 Run / Run 停止 / 需人工重新发起；`hold` / 暂缓决定 / 保持等待 / Run 继续占用待处理项 | `failure_class`, `failure_evidence_ref`, `recommended_action` |
| `startup_stall` | `text` / `无法确认旧执行体已停止` | `retry` / 重新探测旧执行体 / 请求受控终止再探测 / 未确认消失时仍保持隔离；`reject` / 放弃此 Run / 停止处理并保持隔离 / 不代表旧执行体已停止；`hold` / 继续等待 / 保持等待和隔离 / 旧执行体可能仍在运行 | `attempt_no`, `generation`, `diagnostic_cause`, `isolation_consequence`, `recommended_action`, `attempt_diagnostic_ref`, `worktree_ref` |

### 3.2 Canonical brief 与 links renderer

fallback（无 T4/T6）不得采用自然语言自由模板。其 `brief_markdown` 的 UTF-8 字节严格为：

```text
事实：<k1>=<v1>；<k2>=<v2>；…；<kn>=<vn>。建议：<recommended_action>
```

`k1…kn` 是 §3.1 的必需事实键顺序；`recommended_action` 已在表内时仍只渲染一次，位于事实段的对应位置并再次作为建议值。每个值先执行下列 `Escape`：将 CRLF/CR 规范成 LF，拒绝其余控制字符；依次把 `\\`、`` ` ``、`*`、`_`、`[`、`]`、`(`、`)`、`#`、`+`、`-`、`!`、`>` 替换为前置反斜杠的字面量；不得 trim、换行、Markdown 链接化或由 LLM 改写。缺失必需事实即拒绝发射，绝不以“未知”代填。

`links` 总是存在。每项是 `Link {label,target}`；`target` 必须是已验证可访问的 HTTPS forge URL 或绝对本地路径，`label` 是表中对应事实键。先按 `(target UTF-8 bytes, label UTF-8 bytes)` 升序排序，再对完全相同的 `(label,target)` 去重；renderer 依此顺序输出。链接不是从 `brief` 或 LLM 文本抽取的。

### 3.3 链接与 manual Run

下列是链接最低要求，不在表内的链接可附加但仍按 §3.2 排序。`Issue` 只在 Run 有已验证 forge Issue 时加入，**不是**所有 reason 的前置条件。

| reason | 最低链接 | 可选链接 |
|---|---|---|
| `design_approval` | `task_spec_ref` | `Issue` |
| `guardrail_violation` | `policy_evidence_ref` | `Issue` |
| `code_review` | `change_ref`, `diff_ref` | `Issue` |
| `agent_blocked` | `agent_log_ref` | `Issue`, `task_spec_ref` |
| `merge_conflict` | `change_ref`, `conflict_evidence_ref` | `Issue`, `diff_ref` |
| `failure_review` | `failure_evidence_ref` | `Issue`, `agent_log_ref` |
| `startup_stall` | `attempt_diagnostic_ref`, `worktree_ref` | `Issue` |

没有 Issue 的 manual Run 以其 Task Spec、attempt、日志、策略或 worktree 的绝对本地路径充当适用的最低链接；不得伪造 Issue URL。仅当 manual Run 没有适用的最低链接、但其必需事实均可在本地投影中取得时，`links=[]` 合法（目前只限 `design_approval`、`agent_blocked`、`failure_review`）。其余 reason 缺最低链接即拒绝发射。`links=[]` 从不表示可跳过 §3.4 的发布目标。

### 3.4 M3 forge comment 发布

首次发布是 `forge_comment`，其 `purpose=interrupt`，`subject_id=interrupt_id`，`generation=1`，operation key 为 **`comment:interrupt:<interrupt_id>:1`**。payload 使用 [`outbox.md` §5.1](outbox.md) 的 `purpose=interrupt`，其 markdown 由本节确定性 renderer 产生；目标依次为 Run 的已验证 Issue、已验证 Change、或创建 manual Run 时冻结的已验证 discussion target。三者皆无时拒绝发射并记录 `interrupt_publish_target_missing`，不得创建一个无目标的 operation。

`interrupt:<interrupt_id>:publish:<escalation_no>` 专属于 M5 `channel_publish`；M3 不创建该 key 或 Channel delivery。发布失败、重试和“已生成/已送达”分离按 storage/outbox 契约处理。

### 3.5 Golden vectors

以下 vectors 固定 `severity=normal`、无 soft/merge/escalation、`links=[]` 仅在 §3.3 允许时使用；`options` 的每项均是 `{id,label,effect,risk}`。它们是 fallback renderer 的逐字节基准（省略的 `expires_at`、`on_expire` 和 `generation_key` 分别由 §4/§5 冻结）：

```json
{"reason":"design_approval","headline":"需要批准后再开始","brief":"事实：risk_summary=高；recommended_action=approve；task_spec_ref=/r/task。建议：approve","min_modality":"text","links":[{"label":"task_spec_ref","target":"/r/task"}],"options":[{"id":"approve","label":"批准并开始","effect":"Run 进入可启动状态","risk":"开始后会消耗执行资源"},{"id":"reject","label":"拒绝并停止","effect":"Run 停止","risk":"需人工重新发起"},{"id":"hold","label":"暂缓决定","effect":"保持等待","risk":"Run 继续占用待处理项"}]}
{"reason":"guardrail_violation","headline":"软护栏需要人工裁决","brief":"事实：rule_id=R1；impact_scope=src；recommended_action=reject；policy_evidence_ref=/r/policy。建议：reject","min_modality":"text","links":[{"label":"policy_evidence_ref","target":"/r/policy"}],"options":[{"id":"approve","label":"豁免本次规则","effect":"继续当前 Run","risk":"豁免会接受规则所述风险"},{"id":"reject","label":"拒绝并停止","effect":"Run 停止","risk":"需人工重新发起"},{"id":"hold","label":"暂缓决定","effect":"保持等待","risk":"Run 继续占用待处理项"}]}
{"reason":"code_review","headline":"变更等待代码审阅","brief":"事实：change_ref=https://f/c；head_sha=abc；review_requirement=required；recommended_action=approve；diff_ref=https://f/d。建议：approve","min_modality":"visual","links":[{"label":"change_ref","target":"https://f/c"},{"label":"diff_ref","target":"https://f/d"}],"options":[{"id":"approve","label":"批准审阅","effect":"继续后续流程","risk":"批准变更的当前内容"},{"id":"reject","label":"拒绝变更","effect":"Run 停止","risk":"需重新修改后发起"},{"id":"hold","label":"暂缓审阅","effect":"保持等待","risk":"Change 继续待审"}]}
{"reason":"agent_blocked","headline":"Agent 需要你澄清","brief":"事实：blocker_summary=缺需求；attempted_summary=已搜索；recommended_action=ask；agent_log_ref=/r/log。建议：ask","min_modality":"voice","links":[{"label":"agent_log_ref","target":"/r/log"}],"options":[{"id":"ask","label":"提供澄清","effect":"写入澄清内容","risk":"澄清会改变后续执行方向"},{"id":"retry","label":"重试当前工作","effect":"再次尝试执行","risk":"未澄清时可能再次阻塞"},{"id":"reject","label":"停止 Run","effect":"Run 停止","risk":"已完成工作不会继续"},{"id":"hold","label":"暂缓决定","effect":"保持等待","risk":"Run 继续占用待处理项"}]}
{"reason":"merge_conflict","headline":"合并冲突需要处理","brief":"事实：change_ref=https://f/c；head_sha=abc；conflict_summary=冲突；recommended_action=retry；conflict_evidence_ref=/r/conflict。建议：retry","min_modality":"voice","links":[{"label":"conflict_evidence_ref","target":"/r/conflict"},{"label":"change_ref","target":"https://f/c"}],"options":[{"id":"retry","label":"重新执行合并","effect":"再次尝试合并","risk":"冲突未变时会再次失败"},{"id":"reject","label":"停止 Run","effect":"Run 停止","risk":"Change 不会合并"},{"id":"hold","label":"暂缓决定","effect":"保持等待","risk":"Change 继续待处理"}]}
{"reason":"failure_review","headline":"失败需要人工决定","brief":"事实：failure_class=CI；failure_evidence_ref=/r/ci；recommended_action=retry。建议：retry","min_modality":"voice","links":[{"label":"failure_evidence_ref","target":"/r/ci"}],"options":[{"id":"retry","label":"重试失败步骤","effect":"再次执行","risk":"相同故障可能再次发生"},{"id":"reject","label":"停止 Run","effect":"Run 停止","risk":"需人工重新发起"},{"id":"hold","label":"暂缓决定","effect":"保持等待","risk":"Run 继续占用待处理项"}]}
{"reason":"startup_stall","headline":"无法确认旧执行体已停止","brief":"事实：attempt_no=1；generation=2；diagnostic_cause=termination_unconfirmed；isolation_consequence=worktree 保持隔离；recommended_action=retry；attempt_diagnostic_ref=/r/attempt；worktree_ref=/r/wt。建议：retry","min_modality":"text","links":[{"label":"attempt_diagnostic_ref","target":"/r/attempt"},{"label":"worktree_ref","target":"/r/wt"}],"options":[{"id":"retry","label":"重新探测旧执行体","effect":"请求受控终止再探测","risk":"未确认消失时仍保持隔离"},{"id":"reject","label":"放弃此 Run","effect":"停止处理并保持隔离","risk":"不代表旧执行体已停止"},{"id":"hold","label":"继续等待","effect":"保持等待和隔离","risk":"旧执行体可能仍在运行"}]}
```

## 4. 超时与 severity

### 4.1 超时

创建时从 [`config.md` §3.9](config.md) 冻结 `expires_after`、`on_expire` 和 `max_escalations`，并令 `expires_at = now + expires_after`。合法的 `on_expire` 仅为 `hold`、`escalate`、`auto_reject`；不得有 `auto_approve`。配置表是各 reason 的唯一默认值来源，本规格不复制数值。

`startup_stall` 在 schema 和发射器中双重拒绝 `auto_reject`；它只能 `escalate`，到上限后 `hold`，保持 open、隔离和事实优先窗口。M3 创建字段但不实现 M5 的 tick/升级或 Command 处置。

### 4.2 severity 唯一算法

M3 的唯一调用为 `BaseSeverity(reason, gate_phase, guardrail_level, escalation_count, max_escalations)`；调用者不能传最终 severity。M5 如有 schema 校验的 T4/T6 建议，只能调用 `Severity(..., suggested_downgrade)`，其中 `suggested_downgrade` 为 `false|true`，并在 base 结果后最多降一级：

```text
base = Promote(Base(reason), gate_phase, guardrail_level,
               min(escalation_count, max_escalations))
Severity(..., false) = base
Severity(..., true)  = max(low, OneLevelDown(base))
```

`gate_phase` 只能是 `none | pre_start | review | merge`；`guardrail_level` 只能是 `none | soft | hard`。不存在 Gate 的调用传 `none`；`hard` 护栏不产生 Interrupt，调用即为契约错误（硬护栏直接失败）。base 如下：

| reason | base severity |
|---|---|
| `design_approval` | `normal` |
| `guardrail_violation` | `high` |
| `code_review` | `normal` |
| `agent_blocked` | `normal` |
| `merge_conflict` | `high` |
| `failure_review` | `high` |
| `startup_stall` | `high` |

`Promote` 先令 `soft` 至少为 `high`，再令 `gate_phase=merge` 的 `code_review` 至少为 `high`，最后提升 `steps=min(escalation_count,max_escalations)` 级（`low → normal → high → critical`，critical 饱和）。因此 `max=0,count=0` 提升 0 次；`max=2,count=1/2/3` 分别提升 1/2/2 次。发射器把 M3 base 结果交给注意力收费口；critical 的 M5 熔断不得由调用者绕过。

## 5. 生成键与发布

生成键是 `SHA-256` 的 canonical typed UTF-8 preimage：

```text
sift.interrupt.generation\x00v1\x00string:run_id\x00<run_id>\x00enum:reason\x00<reason>\x00...
```

`...` 按下表的字段名和值顺序以 `<type:name>\\x00<value>\\x00` 继续；每个值必须是非空 UTF-8 且不得含 NUL，整数使用无前导零的十进制。字段名、类型标签、domain 和 version 都是 preimage 的一部分。

| reason | 后续 typed fields |
|---|---|
| `design_approval` | `string:task_spec_snapshot_id` |
| `guardrail_violation` | `string:policy_snapshot_id`, `string:violation_code`, `sha256:subject_digest` |
| `code_review` | `string:change_id`, `sha256:head_sha` |
| `agent_blocked` | `uint:attempt_no`, `uint:generation`, `string:report_id` |
| `merge_conflict` | `string:change_id`, `sha256:head_sha`, `sha256:conflict_digest` |
| `failure_review` | `uint:attempt_no`, `uint:generation`, `sha256:failure_digest` |
| `startup_stall` | `uint:attempt_no`, `uint:generation`, `enum:cause=startup_stall` |

`startup_stall` 的 generation key 始终语义为 **`(run_id, attempt_no, generation, cause=startup_stall)`**。`process_identity_unknown`、`termination_unconfirmed`、`process_group_unverified` 是 `diagnostic_cause` 事实/隔离诊断，不参与键。超时扫描、恢复扫描、`kill`、`retry` 的同一 attempt/generation 必须命中一条 open Interrupt，即使其诊断分类不同。

## 6. `startup_stall` 的隔离与事实仲裁

受控终止在身份无法确认或有界终止后仍无法证明旧 wrapper/进程组消失时，必须调用 `EmitInterrupt(startup_stall)`。同一事务将 Run 转 `waiting_human`、attempt 标为 `frozen` 隔离、写一条 Interrupt/一次预算/一个 §3.4 forge comment operation。worktree 在执行体消失证据出现前不得回收或被新 attempt 复用；Run 终态也不解除隔离。

这不是“已停止”的结论：冻结的 claim/session/permit 仍可能承接合法迟到启动事实。`claim:started`、恢复补 started、迟到 `result.json` 与后续 Interrupt 指令必须经 [`storage.md` 的 `ResolveAttemptRace`](storage.md) 这一 CAS 仲裁入口：

- resolution 尚未落定时，合法事实优先：接管监督、Run 回 `running`，并以 `superseded_by_fact` 关闭该 Interrupt；
- `reject` 或 `retry_after_absence` 已落定时，迟到事实不推进旧 Run，登记可终止身份并返回 `superseded_by_decision`；
- `hold`、`escalate` 和升级封顶不写 resolution，事实优先窗口继续开放。

M5 才接通 `startup_stall` 的 retry 探测请求/结果两段式和指令执行；其唯一合法动作、probe 及原子成功事务由 [ADR-013](../decisions/013-startup-stall-retry-convergence.md) 与 [`storage.md` §12.5](storage.md) 定义。M3 只负责让“无法证明消失”可见、唯一且保持隔离，绝不把它静默留在 `queued`。

## 7. M3 验收派生

- 七种 reason 的 §3.5 golden object 在无 T4/T6 时逐字节相同；links 排序/去重、缺失必需事实和 Markdown 转义均有测试。
- 同形字段但不同 reason 的 generation preimage 不相同；同 key 的并发发射只产生一条 Interrupt、一次预算和一条 `forge_comment` operation。
- 对 `startup_stall` 并发调用四个发现者，断言键为 `(run_id, attempt_no, generation, cause=startup_stall)`，诊断分类不拆条，Run 可见为 `waiting_human` 且 attempt/worktree 保持隔离。
- M3 首发 operation 为 `forge_comment` / `comment:interrupt:<interrupt_id>:1`；Channel key 只在 M5 升级投递测试出现。
- severity 覆盖 `max=0`、首次升级、恰达上限、超过上限和 critical 饱和，以及 M5 至多降一级。
- manual Run 无 Issue 时覆盖本地链接、合法空数组、缺最低链接和缺 forge discussion target 的拒发边界。
- 尝试 `startup_stall + auto_reject`、超过四个 options、非互斥 options、调用方指定 severity 必须被拒绝。
