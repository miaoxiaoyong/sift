---
status: draft
created: 2026-07-29
summary: 校准账本、人类决定与类别认证投影契约
---

# Ledger 规格

本文定义 M4 校准账本（Ledger）的不可变记录、人类决定的唯一写入口，以及由校准样本增量维护的类别级认证投影。它不定义 Gate 判定、Command 语法、策略阈值或指标查询；分别见后续 `gate.md`、M5 Command/Report 规格、[`PRD §5.6`](../PRD.md) 与 [`PRD §10.2`](../PRD.md)。

来源：[PRD §5.6、§5.9、§10.2–§10.3](../PRD.md)、[DESIGN §8.2、§8.5–§8.6](../DESIGN.md)、[ADR-004](../decisions/004-gate-as-pure-function.md)、[WBS M4 §4.4](../WBS.md)。持久化表、不可变约束和事务配方见 [`storage.md` §10.5–§10.7、§12.6](storage.md)。

## 1. 范围与不变量

1. Ledger 是影子门禁的超集，保存 Gate 预判、人类实际决定、富特征、打扰特征及人写的语义原料；记录只追加，不以当前 Run、策略或 forge 状态回填历史。
2. 每次 Gate 调用先落一条 `calibration_entries` 预判，无开关。需要 HITL 时，该预判必须和 Interrupt 的五件事同事务，不能等评论回复后再计算。
3. `recordHumanDecision` 是**唯一**写入人类实际决定、补全校准样本并更新认证投影的应用入口。M5 Command、forge 外部事实收敛和未来界面均只能调用它，不能直接更新 `calibration_entries`、`ledger_entries` 或 `certifications`。
4. Gate、Ledger、认证和 T7 都不得从历史单条记录推导或改变当前单条 Gate verdict，也不得抑制当前单条 HITL。允许的读取面仅为 T7 人工待批提案、观测指标和类别级认证投影。
5. 人的响应间隔只是调度特征，绝不能作为注意力成本或「Human 分钟数」的代理口径。
6. 外部 forge/LLM/文件 IO 在事务外完成；进入写入口的均是已经验证并冻结的事实。所有 JSON 使用 canonical JSON，且按其 schema version 校验。

## 2. 记录模型

`ledger_entries` 的行形态、关联和 append-only 约束以 [`storage.md` §10.7](storage.md) 为准。`features_json` 是下列有版本对象；字段级 DDL、索引和迁移评审不在本文重复。

### 2.1 公共 envelope

每条 Ledger entry 的 `features_json` 均包含：

```json
{
  "schema_version": 1,
  "run": {"id": "…", "kind": "docs"},
  "provenance": {"source_event_id": "…", "recorded_at_ms": 0}
}
```

- `run.id` 必须等于行的 `run_id`；`kind` 是作出决定时冻结的任务类别，未知类别不得猜测或归入相邻类别。
- `source_event_id` 是触发该记录的已接受 command/forge/系统事件；没有事件的 Gate 首次预判使用 Gate evaluation 身份。时间由写入口传入，不在领域层取时钟。
- 任何随后的 policy、Issue 作者、路径或 Interrupt 变化均不改变这份对象。

### 2.2 `gate_sample`：预判与校准特征

每个 Gate evaluation 对应一条 `calibration_entries` 预判；`gate_sample` 将该预判及其冻结特征写入 Ledger。它至少含：

```text
calibration_id, gate_evaluation_id, gate_input_snapshot_id,
gate_version, predicted_decision,
task_kind, risk_score { value, source_version, fallback },
change { head_sha, changed_paths[], file_types[], change_size },
guardrails[], issue_author
```

约束：

- `predicted_decision` 是 Gate 输出映射后的 `allow | block`；无法得到可比较的二元预判时不得伪造样本。Gate 的原始 verdict 仍由 `gate_evaluations` 权威保存。
- `changed_paths[]` 为冻结 Gate 输入中的、仓库根目录相对且规范化的路径；不得写绝对路径或以 `..` 逃逸的路径。`file_types[]` 是由同一份路径确定性分类得到的去重、排序集合；二者不可从当前 worktree 重算。
- `change_size` 是冻结输入中的确定性规模分桶/统计，不以事后 diff 覆盖。
- 每个 `guardrails[]` 元素含稳定 `rule_id`、`level`（`hard | soft`）和本次命中/豁免的确定性结局；硬护栏不可能携带豁免结局。
- `issue_author` 是 Run 创建时冻结的 forge Issue 作者；manual Run 无此事实时为显式 `null`，不得用当前评论者代替。

`gate_sample` 是校准特征的权威副本；`calibration_entries.features_json` 保存同一 canonical 特征或其不可变引用，不得出现两份可漂移的特征事实。

### 2.3 `human_decision` 与 `semantic_material`

`human_decision` 记录一个已验证人的实际裁定，含：

```text
calibration_id?, decision { allow | block }, decision_source,
interrupt_id?, command_event_id?, gate_bypassed
```

- `decision_source` 只能是存储契约中的 `command | manual_merge | manual_close`。
- `allow` 表示人允许继续/合并；`block` 表示人拒绝/关闭。Command 动词到这两个值的映射由 Command 规格定义，写入口只接受已规范化的结果。
- 有匹配 Gate 预判时 `calibration_id` 必填，且该预判只能被补全一次。没有预判的决定仍可作为人类决策记录，但不是校准样本、不得参与认证计数。
- `gate_bypassed=true` 只允许 `manual_merge`；它是 forge 已合并事实的审计属性，不表示 Gate 曾经放行。

`semantic_material` 保存人实际输入的自然语言原料：`/sift reject <原因>` 的原因及 `/sift ask <文本>` 的文本。它含来源 command event、可空 Interrupt、`material_kind=reject_reason|ask_text` 和原始 UTF-8 文本；不得以摘要、LLM 改写或后续 context 内容替换原料。`ask` 同时仍按 Task Spec 的不可变快照契约写入，Ledger 不取代该快照。

### 2.4 `attention_delivery`

打扰特征以独立追加记录保存，至少包括 Interrupt 的 `reason`、`severity`、是否合批、送达时段、当日配额占用和 delivery 身份。人类决定关联某个 Interrupt 时，可在该决定特征中写入从**实际送达**到决定的响应间隔；未送达或无关联 Interrupt 则为 `null`。

升级重推不重复计注意力配额，也不把「已生成」误作「已送达」。这些字段供调度与聚合观察，不构成注意力成本。

## 3. `recordHumanDecision` 唯一入口

输入是已认证的 actor/外部事实、稳定幂等身份、Run 与可空 Interrupt/Gate evaluation 身份、规范化的 `allow|block` 结果、来源、可空语义原料和 `gate_bypassed`。入口在开事务前验证：

1. Run、Interrupt 和 Gate evaluation 的归属一致；Interrupt 若存在，当前命令已通过 nonce、option 与 actor 鉴权。
2. `manual_merge` 只接受已由 Forge 观测确认的合并事实；`gate_bypassed` 只可随该来源写入。
3. 结果、来源和可选自然语言的组合合法；自然语言只来自 `reject`/`ask` 的已接受原文，不从 forge 页面或 LLM 推断。
4. 同一 command/forge fact 的幂等键重放返回既有结局；同一 calibration 已有不同人类结果时拒绝冲突，绝不覆盖。

当决定关联 Gate 预判时，单个事务按以下顺序（顺序不产生可见半成品）提交：

```text
BEGIN IMMEDIATE
  CAS 补全 calibration 的 human_decision/decision_source/gate_bypassed/decided_at
  INSERT human_decision Ledger entry（及存在的 semantic_material）
  由该条已补全 calibration 增量重算其 task_kind 的 certification
  INSERT/UPDATE 该类别 certification 投影及证据摘要
  写 Run/Interrupt/Task Spec/outbox 的该决定结果和事件
COMMIT
```

任一步失败则全部回滚。无 Gate 预判时不写认证投影，但人类决定、语义原料及其 Run/Interrupt 结果仍同事务提交。Gate 首次预判的事务和本入口是两阶段记录：前者绝不预写人的结果，后者绝不重新计算当时 Gate。

M5 Command 只能组装上述输入并调用本入口；它不能拥有第二条 Ledger 写入路径。手工 close 和手工 merge 的 forge 事实消费者同样调用本入口，而不是模仿 Command 的 SQL。

## 4. 校准分类与认证投影

### 4.1 样本分类

只对同时具有 `predicted_decision` 和 `human_decision` 的已结算 calibration 计数，按冻结 `task_kind` 聚合：

| 条件 | 分类 |
|---|---|
| Gate `allow`，人 `block` | 漏放（`leak_count`） |
| Gate `block`，人 `allow` | 误拦（`false_block_count`） |
| 人 `block`，无论 Gate 预判 | 负样本（`negative_samples`） |
| 任意已结算预判/人类对 | 总样本（`total_samples`） |

因此漏放是负样本的子集；误拦不是负样本。分类只从预判和实际决定得出，不把后续 revert、当前策略或响应时长混入影子门禁计数。

认证按 [`PRD §5.6`](../PRD.md) 的双向不对称门槛判断：漏放率仅以负样本为分母，且必须同时满足负样本绝对数量下限；误拦上限由注意力配额与吞吐共同约束。具体窗口、阈值和其版本属于 policy/config 的单一事实来源，本文不复制数值。

### 4.2 类别级投影

认证投影键为 `(task_kind, certification_version)`，其计数、`certified` 和 `evidence_digest` 的存储契约见 [`storage.md` §10.6](storage.md)。增量更新必须是纯确定性统计：从同类别的已结算校准样本得到上述四个计数和门槛结论，不调用 LLM、Forge 或文件系统。

对外的认证读取结果**只能**是：

```text
{ task_kind, certification_version, certified, evidence_summary }
```

`evidence_summary` 可说明聚合计数、窗口和阈值版本，但不得带 Run ID、路径、作者、自然语言或任何单条特征，更不得输出某条 Run 的放行建议。投影版本必须同时标识统计规则/窗口/阈值版本及当前可重算证据版本；任一会改变资格的增量更新都会改变读取的 `certification_version`。有效策略组装将该版本冻结进 Gate 输入，故旧缓存自然失效。

认证只决定类别是否具备 `auto_merge` 的策略资格；它不改变单条 Gate 判定，也不能单独开启自动合并。有效策略仍同时要求配置声明和 forge expected-head CAS capability。

## 5. 手工合并与指标分母

Forge 是 Change 合并事实的权威。外部手工合并时，Run 仍收敛为 `done` 并标记 `runs.gate_bypassed=true`；Sift 不 revert 或重开 Change。

- 若已有 Gate 预判，事实消费者必须调用 `recordHumanDecision(decision=allow, source=manual_merge, gate_bypassed=true)`。这保留人的实际决定、`gate_sample` 与认证样本；手工合并不是“无数据”。
- 若尚无 Gate 预判，仍保留 `gate_bypassed` 审计属性和门禁绕过观察记录，但不得补算/伪造影子预判或认证样本。
- `gate_bypassed` 样本参与类别校准（若有预判）和门禁绕过率；它**不进入**「Sift 发起的合并」的误放行率分母。后者仅统计由 Sift merge operation 成功发起的合并，不能借手工合并稀释或放大产品质量红线。

门禁绕过率的分母是全部 `done`，分子是带 `gate_bypassed` 的 `done`；该指标衡量绕过频率，不把外部权限事实冒充为 Gate 失败。

## 6. 派生验收

1. 每次 Gate 调用均写冻结预判；需要 HITL 时，预判和 Interrupt 五件事在一次事务中，崩溃后不会只有其一。
2. `recordHumanDecision` 是 Command、manual merge、manual close 唯一的人类结果入口；重放幂等，冲突结果和直接写表均被拒绝。
3. 人类结果、Ledger、语义原料、校准补全、认证投影及决定的 Run/Interrupt/outbox 后果要么全有要么全无。
4. 特征覆盖 kind、风险来源、变更规模、冻结路径/文件类型、护栏、Issue 作者、reason/severity/送达/合批/配额及 reject/ask 原文；响应间隔不进入注意力成本指标。
5. `allow→block` 只计漏放且属于负样本；`block→allow` 只计误拦；未结算或无预判的记录不污染认证计数。
6. 认证查询只能返回类别级布尔和证据摘要；断言不存在以单条 Ledger 特征影响单条 Gate 或抑制 HITL 的接口。
7. 外部手工合并已有预判时写 `manual_merge` 人类决定、保留校准并标 `gate_bypassed`；无预判时不伪造样本；两种情况都不进入 Sift 自发合并的误放行率分母。

## 7. 自查

- [x] 覆盖 WBS M4 §4.4 的 Gate 预判、人类决定、路径/文件类型、护栏、Issue 作者、打扰特征和自然语言原料。
- [x] 定义 `recordHumanDecision` 唯一入口及 M5 Command/forge 事实消费者边界。
- [x] 定义人类结果、校准样本与认证投影的同事务提交。
- [x] 定义按任务类别的漏放、误拦、总样本、负样本和类别级布尔输出。
- [x] 定义 `gate_bypassed` 手工合并、校准保留与指标分母规则。
