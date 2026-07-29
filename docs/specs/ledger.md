---
status: active
created: 2026-07-29
summary: 校准账本、人类决定与类别认证投影契约
---

# Ledger 规格

本文定义 M4 校准账本的不可变记录、人类决定的唯一写入口及类别级认证投影。Gate 判定见 [`gate.md`](gate.md)，持久化关系、写端口和事务见 [`storage.md` §10.2–§10.9、§12.6](storage.md)，认证配置见 [`config.md` §3.12](config.md)。

来源：[PRD §5.6、§5.9、§10.2–§10.3](../PRD.md)、[DESIGN §8.2、§8.5–§8.6](../DESIGN.md)、[ADR-004](../decisions/004-gate-as-pure-function.md)、[WBS M4 §4.4](../WBS.md)。

## 1. 边界与不变量

1. Ledger 只追加，保存 Gate shadow 预判、人类实际决定、特征、送达及人写语义原料；不会以当前 Run、策略或 Forge 状态回填历史。
2. 每次 Gate evaluation 同事务创建一条 calibration 和一个 `gate_sample` Ledger entry；`gate_sample` 是校准特征的**唯一物理事实源**。`calibration_entries` 只以真实 FK 指向该 entry，不复制特征 JSON。
3. `recordHumanDecision` 是 Command、手工 merge/close 事实消费者和未来界面记录人类动作的唯一应用入口。只有因果绑定的、`shadow_decision` 为二元值的动作才可结算 calibration。
4. Gate、Ledger、认证和 T7 不得从单条历史记录改变单条 Gate verdict 或抑制 HITL。允许读取面只有 T7 提案、指标和类别级认证投影。
5. 人的响应间隔只是调度特征，不是注意力成本或「Human 分钟数」。所有 JSON 都按本节 closed schema 校验并 canonical 化；领域层不读时钟。

## 2. JSON contract v1

### 2.1 单一生成源、canonical 与通用 envelope

M4 实现的唯一生成源为 `internal/ledger/contract/ledger_v1.go`；它生成四份 JSON Schema 和 `internal/ledger/contract/testdata/{gate-sample,human-decision,semantic-material,attention-delivery}-v1-{valid,invalid}.json`。不得另建宽松 decoder。每份 fixture 至少含一个最大边界有效对象及每条 required、enum、排序、互斥和 unknown-field 反例。

四种对象均为 UTF-8 closed object（`additionalProperties=false`），且有如下 closed envelope：

```json
{"schema_version":1,"run":{"id":"…","kind":"docs"},"provenance":{"source":{"kind":"event","id":"…"},"recorded_at_ms":0}}
```

- `schema_version` 恒为整数 `1`；`run.id` 为 1–256 bytes 且等于行 `run_id`；`run.kind` 只能是 `feature | bug | chore | docs | refactor`。
- `source.kind` 只能是 `event | gate_evaluation | interrupt_delivery`，`id` 为 1–256 bytes 的真实对应身份；`recorded_at_ms` 是非负 JSON integer。Gate sample 用 `gate_evaluation`，送达用 `interrupt_delivery`，其余用 `event`。
- ID、SHA-256 digest、路径和 JSON integer 的格式分别沿用 [`storage.md` §2.1](storage.md) 与 [`gate.md` §2.2](gate.md)；字符串不得含 NUL。所有对象最大 64 KiB，任一自然语言字段最大 16 KiB，数组最大 256 项。
- canonical JSON 采用 [`config.md` §4](config.md)：对象 key 词典序、无空白、UTF-8、整数不写浮点；数组必须按下列规定排序且去重。canonical bytes 的 SHA-256 是该 entry 的 `features_digest`。未知字段、重复 key、非 canonical bytes 或交叉约束失败一律拒绝。

### 2.2 `GateSampleV1`

除 envelope 外必有以下字段：

| 字段 | 类型及约束 |
|---|---|
| `calibration_id`、`gate_evaluation_id`、`gate_input_snapshot_id` | 1–256 bytes ID；前二者分别等于行 FK 和 provenance 的 evaluation 身份 |
| `gate_version` | 1–128 bytes 非空字符串 |
| `shadow_decision` | `allow | block | inconclusive`；等于 `calibration_entries.predicted_decision` |
| `risk_score` | closed `{value: 0..100 integer, source_version: 1..128 bytes, fallback: boolean}` |
| `change` | closed `{head_sha: 40 or 64 lowercase hex, changed_paths: string[], file_types: string[], change_size: {files:0..100000, additions:0..10000000, deletions:0..10000000}}` |
| `guardrails` | 按 `(rule_id,level)` 排序去重的 closed `{rule_id,level,outcome}`；`level=hard|soft`，`outcome=hit|exempted`，hard 只能 `hit` |
| `issue_author` | `string | null`；字符串 1–256 bytes |

`changed_paths` 是 0–256 个冻结的 repo-relative slash path，按 UTF-8 bytes 排序去重，不以 `/` 开头且不含空、`.` 或 `..` segment；`file_types` 是从同一冻结路径确定性得出的 0–256 个小写扩展名（无 `.`，1–32 bytes），排序去重。`change_size.files` 必须等于 `changed_paths` 长度，且 `additions + deletions >= files`；这些统计绝不从事后 worktree/diff 重算。`guardrails` 只能记录本 evaluation 已命中或有效豁免的规则。

### 2.3 `HumanDecisionV1`

除 envelope 外必有 `action`、`calibration_decision`、`decision_source`、`calibration_id`、`interrupt_id`、`command_event_id`、`gate_bypassed` 和 `response_interval_ms`：

- `action=approve|reject|retry|hold|ask|manual_merge|manual_close`；`decision_source=command|manual_merge|manual_close`。后两者只能分别配同名 action；command 不得配 `manual_*` action。
- `calibration_decision` 为 `allow|block|null`。`approve→allow`、`reject→block` 只在绑定的 Gate Interrupt 上成立；`manual_merge→allow`、`manual_close→block` 只在其不可变 external-decision binding 上成立；`retry|hold|ask` 为 null。`gate_bypassed=true` 当且仅当 `action=manual_merge`。
- 二元 `calibration_decision` 要求非空 `calibration_id`、该 calibration 的 `shadow_decision` 为 `allow|block`，以及唯一、不可变的 binding；否则三者均拒绝。`calibration_id`、`interrupt_id`、`command_event_id` 是 `string|null`，各非空时为 1–256 bytes ID；command 动作 command event 必填，外部动作为空。
- `response_interval_ms` 为非负 integer 或 null；只有绑定 Interrupt 已成功送达且首次送达不晚于动作时，等于动作时间减首次送达时间，否则为 null。

### 2.4 `SemanticMaterialV1` 与 `AttentionDeliveryV1`

`SemanticMaterialV1` 除 envelope 外必有 closed `material_kind=reject_reason|ask_text`、非空 `command_event_id`、`interrupt_id: string|null` 和 `text`（1–16384 UTF-8 bytes）。它只能随 command 的 `reject` 或 `ask` 同事务写入；原文不摘要、不经 LLM 改写。

`AttentionDeliveryV1` 除 envelope 外必有 `interrupt_id`、`delivery_id`、`reason`、`severity`、`delivered_at_ms`、`batched`、`batch_id`、`attention_charge_entry_id`、`quota_day`。reason 为 PRD 七种枚举，severity 为 `low|normal|high|critical`，时间非负整数，ID 非空；`quota_day` 是 `YYYY-MM-DD`。`batched=true` 时 `batch_id` 必填，反之为 null。只在 delivery 首次成功转为 `delivered` 后按 `delivery_id` 幂等追加；重推可另记 delivery，但复用同一 charge ID，不重复收费。

## 3. Shadow decision、因果关系与唯一写入口

`shadow_decision` 独立于运行期 verdict/action，表示同一冻结事实下可供人类二元决定校准的影子判断。Gate 必须按下表写它，不能从运行期 action 倒推：

| Gate verdict | shadow decision |
|---|---|
| `failed/change_not_open`、`failed/hard_guardrail`、`hitl/guardrail_violation`、`hitl/failure_review` | `block` |
| `ready/merge`、`ready/no_auto_merge` | `allow` |
| `wait_checks`、`retry_checks`、`hitl/checks_timeout`、`hitl/code_review`、`hitl/merge_conflict`、`hitl/mergeability_unknown`、`hitl/input_unknown` | `inconclusive` |

因此「策略要求 code review」、认证尚未获得或 `auto_merge=false` 不会被伪造成 `block`；`inconclusive` 可审计但永不结算、永不参与认证。

创建 Gate HITL Interrupt 时，`interrupts.calibration_id` 必须在同一事务不可变地绑定本 evaluation 的 calibration；非 Gate Interrupt 为 null。外部手工 merge/close 只可消费当前 `waiting_human` Gate Interrupt 已落库的精确 `(gate_evaluation_id, calibration_id)`，并在观察该 Forge fact 的同一事务创建不可变 `external_decision_bindings(forge_fact_event_id, calibration_id)`；缺失、歧义或非二元 binding 一律拒绝该事实结算，绝不写空 binding。禁止按 Run、head、时间或「最新 evaluation」猜测。

`recordHumanDecision` 接受已鉴权 actor、稳定 command/Forge-fact 幂等身份和 tagged union：

```text
command_action { action, command_event_id, interrupt_id, semantic_material? }
external_action { action: manual_merge|manual_close, forge_fact_event_id }
```

它只从上述 immutable binding 解析 calibration；调用者不能传 `calibration_id`、`gate_evaluation_id` 或自行选择结果。重放返回既有结局；同一 calibration 的第二个不同决定拒绝，绝不覆盖。

二元、Gate-linked 动作在一个事务内：CAS 补全 calibration → 写 `human_decision`（及语义原料）→ 以该类别当前 `as_of_ms` 重算认证 → 写 projection/revision → 写 Run、Interrupt、Task Spec、outbox 与事件。无预判、`inconclusive` 或非终局动作只写动作及必要领域后果，不写认证。Gate 首次事务绝不预写人类结果，人的入口绝不重算当时 Gate。

## 4. 认证投影

### 4.1 精确窗口与公式

只计入同时有二元 shadow 预判和二元人类结果的 settled calibration；类别是 `GateSampleV1.run.kind`。`decided_at_ms` 是唯一归窗时间。对一次重算冻结的 `as_of_ms`，窗口为半开区间 `[as_of_ms-window_ms, as_of_ms)`；`decided_at_ms` 为 null、未来或区间外的样本不参与。迟到决定在其实际 `decided_at_ms` 落库时进入当时窗口；绝不回写 predicted 时间。

```text
total_samples    = count(settled in window)
negative_samples = count(human_decision = block)
leak_count       = count(shadow_decision = allow and human_decision = block)
false_block_count= count(shadow_decision = block and human_decision = allow)
leak_rate        = leak_count / negative_samples
false_block_rate = false_block_count / total_samples
```

`negative_samples=0` 时 `leak_rate` 未定义且不认证；`total_samples=0` 时两率均未定义且不认证。`certified=true` 当且仅当：`total_samples >= total_samples_min`、`negative_samples >= negative_samples_min`、两分母非零、`leak_rate <= leak_rate_max` 且 `false_block_rate <= false_block_rate_max`。`inconclusive`、无预判、手工事实无 binding 和窗口外样本均不进入任何分子或分母。

V0 使用配置的固定 `false_block_rate_max`；注意力配额/吞吐只能驱动后续人工 policy/config 提案，不能在运行时重写该阈值或公式。

### 4.2 revision、版本与缓存

`certification_rules_version` 仅是算法+canonical config 的 hash。每次写入 settled 样本及每次 Gate 外组装资格时，以冻结 `as_of_ms` 重算该 task kind 的窗口；按 calibration ID 排序的参与样本 `{id,shadow_decision,human_decision,decided_at_ms}`、四个计数、窗口边界和 rules version canonical 化后 hash 为 `evidence_digest`。

`certification_revision = SHA-256(canonical_json({task_kind, certification_rules_version, evidence_digest}))`；`certification_version` 是同一值，且不是 rules version 的别名。任一影响资格的样本进入/离开窗口、决定补全或规则变化都会改变 revision。不可变 `certifications` 保存该 revision 的证据快照，`certification_current` 以 task kind 的 CAS 指针唯一指定当前快照；policy 组装将该 revision 和 rules version 都冻结进 Gate input，因此旧 cache 无法命中。

对外认证读取仅为 `{task_kind, certification_version, certified, evidence_summary}`。summary 可含聚合计数、窗口和规则版本，绝不含 Run、路径、作者、自然语言或单条放行建议。

### 4.3 必备边界 fixtures

生成源必须覆盖：窗口左边界纳入、右边界排除；迟到决定；零分母；各 minimum 的前一/等于/后一；`allow→block` 只计 leak/negative；`block→allow` 只计 false block；`inconclusive` 和无 binding 排除；样本过期导致 revision/资格变化；及同一计数但不同参与 calibration ID 得到不同 evidence digest。

## 5. 手工合并、用途与验收

Forge 是合并事实权威。手工 merge 收敛 Run 为 `done` 并标 `gate_bypassed=true`；它不进入 Sift 发起合并的误放行率分母。外部事实必须携带当前 waiting-human Interrupt 的 immutable binary binding，随后 `recordHumanDecision` 记录 `manual_merge/allow` 并保留校准；没有 binding、binding 歧义或影子为 inconclusive 均拒绝，绝不伪造样本。manual close 同理映射 `block`。

验收：closed schema/fixtures 与大小限制可生成并拒绝未知字段；每次 Gate 都有一条 calibration 和唯一 gate-sample FK；所有 verdict 均有上表 shadow 值；Interrupt/external binding 不可变且命令不能猜选；同一 Gate 事务原子写 snapshot/evaluation/calibration/gate sample/必要 Interrupt；认证公式、边界 fixtures、revision 和 Gate cache 失效均可重放验证；响应间隔不进入注意力成本。
