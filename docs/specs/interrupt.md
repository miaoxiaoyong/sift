---
status: draft
created: 2026-07-29
summary: Interrupt 的发射、调度、投递与升级契约
---

# Interrupt 规格

本文冻结 M3 Attention 泛型发射核心，并在 M5 草案中扩展 T4/T6、Channel、调度、超时升级与 critical 熔断。每个 PRD reason 即使没有 T4/T6 也能生成可见、可发布的 fallback Interrupt；`startup_stall` 仍是在无法证明执行体消失时的唯一安全出口。本文保持 `draft`，直至 M5 字段评审完成。

来源：[PRD §4.1–§4.4、§5.3、§5.5、§7.1](../PRD.md)、[DESIGN §8.7、§10.1](../DESIGN.md)、[WBS M3 §3.6–§3.7、M5 §5.1–§5.3](../WBS.md)、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)。持久化和事务边界见 [`storage.md` §6、§12.2](storage.md)，默认超时见 [`config.md` §3.9](config.md)，发布 operation 见 [`outbox.md`](outbox.md)。

## 1. 范围与不变量

1. `EmitInterrupt` 是**唯一**创建 Interrupt 的领域入口。恢复、Runtime、Gate、Report 与后续 Attention/Command 都只能调用它；不得直接插入 `interrupts`、扣注意力预算或创建发布 operation。
2. 发射器只接受确定性事实、确定性 fallback 模板和确定性 severity 输入；M3 不接受 LLM/T4 severity 输入。M5 的 T6 建议可作为唯一算法的 `suggested_downgrade` 输入，且至多降低一级；它不能改变 `reason`、`min_modality`、options 的领域效果、链接最低事实、expires/on-expire 或使对象更紧急。T4 只能提出渲染候选，T6 只能提出投递建议；两者均不是状态、预算或副作用的写入口。
3. 发射器在同一事务内完成 Run 转移（或确认已在合法人工态）、生成键去重、首次注意力记账、Interrupt、事件和首发 forge comment operation。已有生成键时返回既有记录，不重复扣费或创建 operation；M5 的调度器只能消费该既有记录，不能以 reason、T4、T6、Channel 或熔断为名另建 Interrupt。
4. 任一 Interrupt 必须有 1–4 个互斥 options、独立可朗读且不超过 40 Unicode code points 的 headline、非空 brief、存在的 `links` 数组、`expires_at` 和 `on_expire`。`links` 可在 §3.3 所限情形为空；表达不出所需字段时拒绝发射并记录确定性诊断，而不是发出含糊问题。
5. forge comment 是每条 Interrupt 的确定性首发决策面。M5 的 T4/T6、Channel、智能简报、超时 tick、升级投递和 critical 熔断均在本文定义为**既有 Interrupt 的渲染或推进**；它们不预支为已实现能力，也不改变 M3 的首发、生成键和五件事事务。Command 的执行语义仍以 M5 `command.md` 为准。

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

`k1…kn` 是 §3.1 的必需事实键顺序；`recommended_action` 已在表内时仍只渲染一次，位于事实段的对应位置并再次作为建议值。每个值先执行下列 `Escape`：值含 CRLF、裸 CR 或裸 LF 中任一字节序列时，分别以 `interrupt_brief_crlf_rejected`、`interrupt_brief_cr_rejected` 或 `interrupt_brief_lf_rejected` 拒绝发射，**不得**规范化或保留换行；再拒绝其余 Unicode `Cc` 控制码点；最后依次把 `\\`、`` ` ``、`*`、`_`、`[`、`]`、`(`、`)`、`#`、`+`、`-`、`!`、`>` 替换为前置反斜杠的字面量。不得 trim、插入换行、Markdown 链接化或由 LLM 改写。缺失必需事实即拒绝发射，绝不以“未知”代填。

换行拒绝 vectors（输入均为 UTF-8；未生成 Interrupt、预算或 operation）：

| 原始值 | 结局 |
|---|---|
| `a\r\nb` | 拒绝：`interrupt_brief_crlf_rejected` |
| `a\rb` | 拒绝：`interrupt_brief_cr_rejected` |
| `a\nb` | 拒绝：`interrupt_brief_lf_rejected` |

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

首次发布是 `forge_comment`，其 `purpose=interrupt`，`subject_id=interrupt_id`，`generation=1`，operation key 为 **`comment:interrupt:<interrupt_id>:1`**。payload 使用 [`outbox.md` §5.1](outbox.md) 的 `purpose=interrupt`，其 markdown 由本节确定性 renderer 产生；目标依次为 Run 的已验证 Issue、已验证 Change、或 `runs.discussion_target_*` 中创建 manual Run 时冻结的已验证 discussion target。三者皆无时拒绝发射并记录 `interrupt_publish_target_missing`，不得创建一个无目标的 operation。

manual Run 的 discussion target 以 [`storage.md` §5.2](storage.md) 的三列为唯一权威：创建端口在插入 Run 前，按其绑定 project 用 Forge `GetIssue` 或 `GetChange` 验证调用方给出的 `TargetRef` 和 URL；验证成功后同 Run 一起持久化 `discussion_target_kind`、`discussion_target_id`、`discussion_target_url`，随后不可更新。manual Run 必须冻结该目标；未提供、验证失败或与 project 不符即拒绝创建 Run。它是预先选定的讨论面，不会把 `issue_id` 或 `change_id` 伪造成 Run 的来源/产物；因此 manual Run 可没有 Issue、也尚无 Change，同时仍有可恢复的 comment 目标。outbox payload 只从这三个冻结值产生，不从当前 Task Spec、links 或可漂移的 forge 搜索重建。

`interrupt:<interrupt_id>:publish:<escalation_no>` 专属于 M5 `channel_publish`；M3 不创建该 key 或 Channel delivery。发布失败、重试和“已生成/已送达”分离按 storage/outbox 契约处理。

### 3.5 Golden vectors

以下 vectors 仅为 fallback **renderer 子对象**的逐字节基准：覆盖 `reason`、`headline`、`brief`、`min_modality`、`links` 和 `options`，不声明或省略对象级 `severity`。`links=[]` 仅在 §3.3 允许时使用；`options` 的每项均是 `{id,label,effect,risk}`。`expires_at`、`on_expire` 和 `generation_key` 分别由 §4/§5 冻结。

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

M3 的唯一调用为 `BaseSeverity(reason, gate_phase, guardrail_level, escalation_count, max_escalations)`；调用者不能传最终 severity。M5 如有 schema 校验的 T6 建议，只能调用 `Severity(..., suggested_downgrade)`，其中 `suggested_downgrade` 为 `false|true`，并在 base 结果后最多降一级：

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

生成键是 `SHA-256` 的 canonical typed UTF-8 preimage。每一段严格是实际单个 NUL 字节分隔的 `<type:name>\x00<value>\x00`，不是字面六字节 `\\x00`。完整固定前缀为：

```text
string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00<run_id>\x00enum:reason\x00<reason>\x00
```

后续字段按下表顺序追加。所有 `string` 值是非空 UTF-8 且不含 NUL；`enum` 值必须是其表列出的精确 ASCII 字面量；`uint` 是无正负号、无前导零的十进制正整数；`sha256` 是 64 个小写十六进制字符，表示原始 SHA-256 digest 的**小写 hex 文本**；`git_oid` 是 40（SHA-1 repository）或 64（SHA-256 repository）个小写十六进制字符的 Git object ID，绝不是 `sha256` digest。字段名、类型标签、domain 和 version 都参与散列。

| reason | 后续 typed fields |
|---|---|
| `design_approval` | `string:task_spec_snapshot_id` |
| `guardrail_violation` | `sha256:effective_policy_hash`, `string:rule_id`, `sha256:matched_paths_digest` |
| `code_review` | `string:change_id`, `git_oid:head_sha` |
| `agent_blocked` | `uint:attempt_no`, `uint:generation`, `string:report_id` |
| `merge_conflict` | `string:change_id`, `git_oid:head_sha`, `sha256:conflict_digest` |
| `failure_review` | `uint:attempt_no`, `uint:generation`, `sha256:failure_digest` |
| `startup_stall` | `uint:attempt_no`, `uint:generation`, `enum:cause`（值为 `startup_stall`） |

下列完整 vectors 中 `\x00` 表示一个 NUL 字节；最后一列是整个 preimage 的 SHA-256 小写 hex。它们同时冻结字段名和值的编码。

| reason | 完整 preimage | generation key |
|---|---|---|
| `design_approval` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00design_approval\x00string:task_spec_snapshot_id\x00task-01\x00` | `2eff88491a846f04025bc5a7019be780e96b00172adfa1b35154e71a77a27a83` |
| `guardrail_violation` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00guardrail_violation\x00sha256:effective_policy_hash\x00aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00string:rule_id\x00rule-01\x00sha256:matched_paths_digest\x00aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00` | `ecc93ad7d910f74c2a042ad8f6052dfd9a265e02aabf8de4522fdfbad86111fb` |
| `code_review` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00code_review\x00string:change_id\x00change-01\x00git_oid:head_sha\x000123456789abcdef0123456789abcdef01234567\x00` | `7389e85b479a5c919062677e5a9a9e9f3465db0473b2d41171479be736a83e59` |
| `agent_blocked` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00agent_blocked\x00uint:attempt_no\x001\x00uint:generation\x002\x00string:report_id\x00report-01\x00` | `ebc17dc66d66fb86c9d48d7e79c86a632e44f0fd0248b5c5713b6a9e95825643` |
| `merge_conflict` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00merge_conflict\x00string:change_id\x00change-01\x00git_oid:head_sha\x00aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00sha256:conflict_digest\x00bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\x00` | `56378c8559b5f6bdcebb3e097ff7385c78c0eabdcb1a56ae5effac50f0cdf1a3` |
| `failure_review` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00failure_review\x00uint:attempt_no\x001\x00uint:generation\x002\x00sha256:failure_digest\x00cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\x00` | `98da21cd0a751c6f54f043302d88fa93b08f15c98a406ecac4b09d51ad573cca` |
| `startup_stall` | `string:domain\x00sift.interrupt.generation\x00uint:version\x001\x00string:run_id\x00run-01\x00enum:reason\x00startup_stall\x00uint:attempt_no\x001\x00uint:generation\x002\x00enum:cause\x00startup_stall\x00` | `18630f7c14d7526246fab89c1c99c6a47e80e38cca3efe9e54c1e54d149badae` |

`startup_stall` 的 generation key 始终语义为 **`(run_id, attempt_no, generation, cause=startup_stall)`**。`process_identity_unknown`、`termination_unconfirmed`、`process_group_unverified` 是 `diagnostic_cause` 事实/隔离诊断，不参与键。超时扫描、恢复扫描、`kill`、`retry` 的同一 attempt/generation 必须命中一条 open Interrupt，即使其诊断分类不同。

## 6. `startup_stall` 的隔离与事实仲裁

受控终止在身份无法确认或有界终止后仍无法证明旧 wrapper/进程组消失时，必须调用 `EmitInterrupt(startup_stall)`。同一事务将 Run 转 `waiting_human`、attempt 标为 `frozen` 隔离、写一条 Interrupt/一次预算/一个 §3.4 forge comment operation。worktree 在执行体消失证据出现前不得回收或被新 attempt 复用；Run 终态也不解除隔离。

这不是“已停止”的结论：冻结的 claim/session/permit 仍可能承接合法迟到启动事实。`claim:started`、恢复补 started、迟到 `result.json` 与后续 Interrupt 指令必须经 [`storage.md` 的 `ResolveAttemptRace`](storage.md) 这一 CAS 仲裁入口：

- resolution 尚未落定时，合法事实优先：接管监督、Run 回 `running`，并以 `superseded_by_fact` 关闭该 Interrupt；
- `reject` 或 `retry_after_absence` 已落定时，迟到事实不推进旧 Run，登记可终止身份并返回 `superseded_by_decision`；
- `hold`、`escalate` 和升级封顶不写 resolution，事实优先窗口继续开放。

M5 才接通 `startup_stall` 的 retry 探测请求/结果两段式和指令执行；其唯一合法动作、probe 及原子成功事务由 [ADR-013](../decisions/013-startup-stall-retry-convergence.md) 与 [`storage.md` §12.5](storage.md) 定义。M3 只负责让“无法证明消失”可见、唯一且保持隔离，绝不把它静默留在 `queued`。

## 7. M5：T4/T6 与渲染边界（草案）

T4/T6 的调用、输入/输出 schema、版本和 trace 由 [`brain.md`](brain.md) 定义；本节只定义其接入 Interrupt 时不可越过的边界。两次调用都在发射器事务**外**，只读取本次已冻结的 facts、fallback 对象、配置快照和（对 T6）调度快照；调用结果必须先通过 closed schema，再由确定性代码重新校验。它们不可读取可漂移的 Run/Forge 当前态来替代冻结输入。

### 7.1 T4 决策简报

T4 可以输出 `headline`、`brief` 和面向人的 option 文案候选；它不是 Interrupt 的领域构造器。确定性接纳器必须同时满足：

1. headline 仍符合 §1 的可朗读/40 code points 限制；brief 为非空、受限 Markdown 文本，不能携带 HTML、链接、`<!-- sift-op:` marker、nonce、动作语法或未冻结的事实。链接仅由 §3.2–§3.3 的 renderer 追加。
2. 候选 options 的 ID 集合、顺序、数量必须与 §3.1 对应 reason 的 canonical options 完全相同；服务端始终采用 canonical `effect` 和 `risk`，并以 canonical ID 校验 Command。T4 不能新增、删除、重排或赋予 option 新效果。
3. 输入不完整、调用失败、超预算、schema/领域校验失败，或输出无法安全渲染时，直接使用 §3 的确定性 fallback；失败本身不得再生成 `failure_review` 或任何其他 reason。

因此 T4 改善的是简报表达，不是可执行动作、severity、过期、计费或状态转移。每次调用和 fallback 都按 `brain.md` 写 trace；发射仍只由 `EmitInterrupt` 完成。

### 7.2 T6 调度建议

T6 可建议 `{suggested_downgrade, dispatch, channel_id}`：`dispatch` 只能为 `immediate | batch | defer`，`channel_id` 只能是启动期冻结且与 `min_modality` 兼容的已配置 Channel。它不能给出任意时间戳、任意 Channel URL、追加 reason、修改首发 forge comment，或要求跳过配额/熔断。

接纳器按下列确定性规则裁决：

- `critical` 必为 `immediate`；`high` 至少为 `immediate`。T6 只能建议降级一档，且降级后的 severity 重新经过 §4.2、配额和熔断检查。
- T6 不可用或建议无效时，`high | critical` 立即投递，`low | normal` 合批至配置的下一次 `daily_summary_at`。这正是 token 耗尽时的确定性阈值兜底，不是无差别立即打扰。
- `batch` 在当前摘要批次聚合；`defer` 只可顺延至下一摘要批次，绝不无限期压住。`batched`/`held` 是可在 `ps`/`doctor` 查询的既有 Interrupt 投递状态，不是新的 Run 状态或 reason。
- 无兼容 Channel、Channel 被隔离或尚无可用 Channel 时，Interrupt 及其 forge comment 仍保持有效；仅 Channel delivery 保持 `held` 并报告原因。不得因为 T6/Channel 失败关闭 Interrupt、回滚首次注意力记账，或假装已送达。

调度器只经 `AdvanceInterrupt` 等既有写端口推进 open Interrupt 的 delivery/dispatch state 及其必要的 Channel operation；它不直接插入 `interrupts`、预算 entry 或发布 operation。任何需要重新读事实而产生的新 HITL，仍回到 `EmitInterrupt` 并使用 §5 的原 reason generation key。

## 8. M5：Channel 与调度推进（草案）

### 8.1 Channel 是第二渲染面

forge comment 仍是可回复的首发面；Channel 是同一 Interrupt 的附加投递面，不是第二个 reason 或第二个 Command 入口。每次 Channel delivery 都引用既有 `interrupt_id`、当前 immutable 内容版本和 `escalation_no`，并由 [`outbox.md` §2、§10](outbox.md) 的 `channel_publish` operation 与 [`storage.md` §6.2](storage.md) 的 delivery 投影驱动：

- 初次即时投递使用 `escalation_no=0`；一次升级只创建该升级号对应的一项 Channel operation，稳定 key 为 `interrupt:<interrupt_id>:publish:<escalation_no>`。同号重试沿用该 operation，升级或重试均不新增 attention charge。
- Channel 是 at-least-once；renderer 必须保留 outbox 规定的可见 operation 标识，不能把重试伪装为精确一次。renderer 只读 Interrupt 和 delivery priority，不能改写 options、severity、nonce、状态或预算。
- `min_modality=visual` 的对象只能路由至声明 visual capability 的 Channel；语音或纯 voice renderer 必须在入口拒绝该路径。拒绝后保留 forge comment 和可见的 held delivery，不得降格为语音批准。
- 连续失败计数、稳定 `forge_alert(channel_failure)`、继续重试和 `ps`/`doctor` 故障投影完全遵守 [`outbox.md` §10](outbox.md)。alert 不是新的 Interrupt，不得递归告警或消耗第二次 attention charge。

合批是一次 Channel 摘要 delivery，不是将多个 Interrupt 改写为一个新 reason：摘要只列出既有 open Interrupt 的稳定 ID、headline、链接和各自可执行 options；回复仍必须携带目标 Interrupt 的当前 nonce，不能对摘要整体执行动作。批次身份、成员冻结、payload 与 operation key 的字段契约由 `outbox.md` / `storage.md` 承接；M5 实现前必须在这两份字段权威中补齐摘要 delivery，本文不另立一份 Channel payload 协议。

### 8.2 Supervisor 调度与超时

Supervisor 使用注入时间扫描 `status=open` 的 Interrupt：到期扫描和已到调度时点的 delivery 扫描是两个工作项，但都只调用 `AdvanceInterrupt` 等既有推进端口（见 [`storage.md` §12](storage.md)）。每次 CAS 必须校验 Interrupt version/status；旧 tick、旧 Channel worker 或关闭后的对象不得重开、重推或覆盖新 nonce。

对到期对象按创建时冻结的 `on_expire`、`max_escalations` 与 reason 上限去向执行：

1. `hold`：进入 `held`，停止自动升级；保留 open Interrupt 和其事实优先窗口，等待显式 Command 或外部事实。
2. `escalate` 且 `escalation_count < max_escalations`：递增 count，按 §4.2 重算 severity，轮换 nonce、version 加一，并以 `strong` priority 排入当前 Channel。首发 charge 原样复用。
3. `escalate` 已达上限：severity 保持封顶，不再创建升级投递；按 [`config.md` §3.9](config.md) 的 reason 映射进入 `auto_reject` 或 `hold`。不得把全局 `max_escalations` 误读成所有 reason 都可自动拒绝。
4. `auto_reject`：仅对配置允许的非 `startup_stall` reason 关闭 Interrupt（`expired_auto_reject`）并经唯一 Run transition 进入 `failed`。

`startup_stall` 无论在首发、升级还是上限处都禁止 `auto_reject`：上限只能 `hold`，不得写 `attempt_resolution`。其 retry/hold/reject 仍遵循 §6、ADR-013 与 Command 的两段式仲裁；超时推进不得绕过 `ResolveAttemptRace`。

### 8.3 critical 熔断、配额与合批

非 critical 首发先在 `EmitInterrupt` 的同一事务按 severity 日配额 CAS 收费；额度不足时对象进入合批，不借支、不以 T4/T6 fallback 或升级另行收费。首次 critical 仍写其唯一 attention entry，但不占日配额；它必须在同一发射事务检查 append-only entry 的真实滑动窗口，分别比较全局和 per-Run `critical_fuse` 上限（配置唯一见 [`config.md` §3.9](config.md)，计数口径见 [`storage.md` §9.2](storage.md)）。

任一 critical 窗口达到上限时，发射器不允许把后续对象作为额外的即时 critical delivery。它必须将这些**既有 reason**的 Interrupt 原子纳入一个可见的 critical 汇总批次：每个成员仍保留自己的 generation key、Run、facts、options、nonce 与审计事件；汇总只改变 delivery 调度，绝不伪造 `critical_summary` 等新 reason，也不允许一个汇总指令影响多个 Interrupt。每个 scope 的熔断期至多安排一次汇总 delivery；窗口恢复后再按当时对象的当前 version/状态重新裁决。

熔断、配额耗尽和 T6 合批都只是同一发射器/调度器对既有对象的确定性投递降级：

- 不得由 Report、Runtime、Gate、Channel worker 或某个 reason 专门直接写预算、`interrupts` 或 summary operation；
- escalation 重推和汇总 delivery 复用成员的首次 charge，不能作为借支或退款通道；
- 任何合批成员若在摘要前被 Command 或外部事实关闭，必须从该批次排除；摘要内剩余成员仍分别可审计、可回复。

## 9. 验收派生

### 9.1 M3 回归

- 七种 reason 的 §3.5 renderer 子对象在无 T4/T6 时逐字节相同；links 排序/去重、缺失必需事实、CRLF/CR/LF 拒绝和 Markdown 转义均有测试。
- 同形字段但不同 reason 的 generation preimage 不相同；同 key 的并发发射只产生一条 Interrupt、一次预算和一条 `forge_comment` operation。
- 对 `startup_stall` 并发调用四个发现者，断言键为 `(run_id, attempt_no, generation, cause=startup_stall)`，诊断分类不拆条，Run 可见为 `waiting_human` 且 attempt/worktree 保持隔离。
- manual Run 无 Issue、尚无 Change 时，用创建时冻结的已验证 discussion target 成功创建 `forge_comment`；另覆盖本地链接、合法空数组、缺最低链接和缺 forge discussion target 的拒发边界。
- 尝试 `startup_stall + auto_reject`、超过四个 options、非互斥 options、调用方指定 severity 必须被拒绝。

### 9.2 M5 新增

- T4 合法候选可替换简报；任何 schema/领域失败精确回退 §3，不改变 generation key、options 效果、状态或一次收费。
- T6 无效/超预算/不可用时运行确定性阈值；其建议不能升级 severity、绕过 `min_modality`、选择未冻结 Channel 或令对象无限 defer。
- Channel 注入成功响应丢失时可能重复但带可见标识；连续失败到阈值只建一个 forge alert，仍继续重试且 `ps`/`doctor` 可见。
- 到期、首次升级、`max_escalations=0`、恰达/超过上限、`auto_reject` 与 `hold` 分别覆盖；每次合法升级只重推一次并轮换 nonce/version，`startup_stall` 上限仍为 open + hold + 无 resolution。
- 配额耗尽的非 critical 合批、critical 全局/per-Run 滑动窗口、并发撞熔断和窗口恢复均可确定性重放；没有借支、重复 charge、第二 reason、可批量执行的 summary 指令或 reason 专用发射入口。
