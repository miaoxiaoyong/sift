---
status: draft
created: 2026-07-29
summary: Gate 纯判定、快照、豁免和 Change 创建契约
---

# Gate 规格

本文冻结 M4 Gate 的纯函数边界、输入快照/缓存、判定顺序、护栏豁免、Shadow Gate 事务，以及由 M3 成功事实驱动的 Change 创建。Policy 字段级 schema、T3/T5 字段级输出和 Ledger 特征字段分别由 [`policy.md`](policy.md)、[`brain.md`](brain.md)、[`ledger.md`](ledger.md) 规定；本文不复制它们。

来源：[PRD §5.4、§5.6](../PRD.md)、[DESIGN §6.4–§6.5、§8.5](../DESIGN.md)、[WBS M4 §4.3](../WBS.md)。持久化/写端口与事务配方见 [`storage.md` §10.2–§10.5、§12.2、§12.6](storage.md)；Interrupt 发射器见 [`interrupt.md`](interrupt.md)，Change marker 与 outbox 对账见 [`outbox.md` §2–§3、§7](outbox.md) 和 [`forge.md` §4.7–§4.8](forge.md)。

## 评审状态

字段级评审的 G1–G5 已由本文 §2.2、§2.3、§3.1、§3.2 和 active 的 [`policy.md` §3.3](policy.md) 关闭：Gate 只消费其 `EffectivePolicyV1`，不自定义 policy 字段。#217 尚未合入，G6（每类 verdict 的 calibration 二元/不可比较映射，以及 guardrail Interrupt 的不可变身份）仍未关闭，故本文**保持 `draft`**。处置依据及余项见[评审报告](../reviews/2026-07-29-gate-review-pi-gpt-5.6-sol.md)。实现方不得在 G6 完成前自行改变 calibration/Interrupt 身份接缝。

## 1. 边界与不变量

```text
gate(changeFacts, effectivePolicy, riskScore) -> verdict
```

1. `gate` 是纯函数：不做 IO、不读时钟、网络、文件、数据库或当前配置，不调用 Forge、Brain、T5 或 T3，也不写入状态。调用方先取得并冻结全部事实；时间有关的 Checks 结论（例如 pending 是否已经超时）必须已成为输入事实。
2. 函数只裁定传入的冻结世界。它不得查询历史 Gate、单条 Ledger、认证明细或人的过往选择；认证和 Forge expected-head-CAS 能力已在 Gate 外折入 `effectivePolicy`。
3. 风险评分是输入而不是副作用。T3 正常结果或“高风险”确定性兜底的来源、提示词/schema 版本都必须进入快照；Gate 不得因 Brain 恢复而暗中改变同一输入的 verdict。
4. 每一次调用（包括 cache hit）均须留下同一份输入快照、`gate_evaluations` 和 Shadow Gate 预判；没有“仅运行、不记录”的配置开关。
5. 硬护栏失败不能产出豁免、review 或 merge 动作。无法确定的 Checks、审查、可合并性或输入契约一律 fail closed，转明确的人工路径而不猜成绿灯。

`verdict` 是确定性 tagged union，至少表达终止失败、等待/重试 Checks、`guardrail_violation` HITL、`code_review` HITL、失败复核 HITL、可自动合并和停止于“全绿但不自动合并”的结果。每个分支必须携带其冻结 `head_sha` 和足以构造相应后续 operation 或 Interrupt 的确定性理由；它不能携带自由文本、任意 severity 或调用方提供的 action。

## 2. 冻结输入、hash 与缓存

### 2.1 输入快照

调用方将 `changeFacts`、`effectivePolicy` 和 `riskScore` 组装为 schema-versioned Gate 输入快照。它至少包含以下事实域；具体字段/枚举以所链接规格为准：

| 域 | 必须冻结的内容 |
|---|---|
| Change | 已验证远端 Change ID/URL、state、base/head ref、`head_sha`、draft、mergeability、review/approval 状态、规范化变更路径、路径集合是否完整、文件数与增删行规模；未知值必须显式编码，不能用空数组冒充“已完整读取且无路径” |
| Checks | 对该 `head_sha` 的归一结论、失败项、CI 证据引用、pending 起点/本次观测时刻/是否已超时、已消费的 flaky 重试次数，以及仅在 failure 时存在的 T5 分类与来源；`pending_timed_out` 不能在 Gate 内读时钟重算 |
| Policy | [`policy.md` §3.3](policy.md) 的完整 `EffectivePolicyV1` canonical JSON、其 hash、已被人明确批准的一次性豁免；`certification_version`。软规则例外只来自 `EffectivePolicyV1.protected_paths.soft_exceptions`，不另设 remembered-exceptions 字段。 |
| Risk | [`brain.md` §9](brain.md) 的完整 T3 风险分、风险点、rationale 及来源/提示词/schema，或确定性兜底对象；不得只保存分数而丢失来源 |
| Identity | Run、项目、任务类别和 Change/策略快照身份，仅用于审计、后继动作和回放，不能让 Gate 读取可变 Run 历史 |

`effectivePolicy` 是 Gate 外的产物：base policy 与全局缺省合并后，认证或 Forge CAS capability 不足的提权项已经剔除。特别是 `auto_merge=true` 只有在项目配置、任务类别认证和远端 expected-head CAS 三者均已满足时才可进入有效策略；Gate 不重新计算资格。

使用 [`config.md` §4](config.md) 的 canonical JSON 序列化**整份**输入快照，并计算：

```text
gate_input_hash = SHA-256(canonical_json(full_gate_input_snapshot))
cache key       = (gate_input_hash, gate_version)
```

不得以 run ID、head SHA 或若干手选维度替代该摘要。新增任何实际影响 verdict 的输入都必须写入快照，因而自动改变 hash。相同 head 下 Checks、review、mergeability、risk 结果/来源版本、有效策略 hash 或 certification version 任一改变，都必须 cache miss。

### 2.2 `GateInputV1` 闭合形态（G1）

`GateInputV1` 是本节唯一输入类型，`additionalProperties=false`。M4 实现必须以单一 `internal/gate/contract/gate_input_v1.go` 生成 JSON Schema 和 `internal/gate/contract/testdata/gate-input-v1-{valid,invalid}.json` 正/反 fixtures；不得另建宽松 decoder。实现提交前，下面的字段表是该生成源必须逐字实现的契约。

| 字段 | 类型及约束 |
|---|---|
| `schema_version` | 常量 `1` |
| `identity` | closed object：非空 `run_id`、`project_id`、`task_kind`、`change_id`；所有 ID 为 UTF-8、1–256 bytes |
| `change` | closed object：`state=open|closed|merged`、40（SHA-1 repository）或 64（SHA-256 repository）个小写十六进制字符的 `head_sha`、非空 `base_ref`/`head_ref`、boolean `is_draft`、`mergeability=mergeable|conflicting|unknown`、`review_state=approved|not_approved|unknown`、`paths_complete`、`changed_paths`、`files_changed>=0`、`additions>=0`、`deletions>=0`。路径必须为 repo-relative slash path，非空、不以 `/` 开头、不含 `.`/`..` segment、排序去重；`paths_complete=false` 时 `changed_paths` 必须为空，`true` 时空数组才表示确无变更路径。 |
| `checks` | closed object：`conclusion=success|failure|pending|unknown`、排序去重 `failed_jobs`、非空 `external_url`、`flaky_retries_used>=0`；job 为 closed `{id,name,web_url,allow_failure}`，`id` 非空且排序 key 为 `(id,name)`。`pending` 时必有 `pending_started_at_ms>=0`、`observed_at_ms>=pending_started_at_ms`、`pending_timed_out`，其他结论三字段均为 null；`failure` 时必有 `triage`，否则为 null。 |
| `checks.triage` | closed object：`classification=flaky|real_failure|infrastructure|unknown`、`retry_check_id`、closed `source`；仅 failure 可出现。`classification=flaky` 时 retry ID 必须非空且精确命中 `failed_jobs` 中 `allow_failure=false` 的项；其他分类必须为 null。`source` 是 `kind=brain`（非空 logical call/prompt/schema version）或 `kind=fallback`（非空 logical call/version/reason），字段与 [`brain.md` §10.3](brain.md) 同源。 |
| `effective_policy` | [`policy.md` §3.3](policy.md) 的 `EffectivePolicyV1` canonical JSON，且只接受该 closed shape：`schema_version`、`protected_paths.{hard,soft,soft_exceptions}`、`review_policy`、`risky_review_threshold`、`auto_merge`、`checks_pending_timeout_ms`、`flaky_retry_limit`。不得添加未知字段或将 `soft_exceptions` 另投影为 remembered-exceptions 字段。 |
| `effective_policy_hash` / `certification_version` | 各为 64 小写十六进制，前者必须等于 `effective_policy` 的 canonical SHA-256。 |
| `risk` | `brain.md` §9.2–§9.3 的 closed T3 object：整数 `risk_score`（0–100）、排序去重 `risk_points`、`rationale`、以及 brain/fallback source；不得省略来源。 |
| `one_time_exemptions` | 排序去重数组；每项为 closed object `{run_id,head_sha,rule_id,matched_paths_digest}`，digest 为 64 小写十六进制。每项 run/head 必须等于 `identity.run_id`/`change.head_sha`。 |

整数均为 JSON integer，不接受浮点、NaN 或 Infinity；时间均为 Unix ms。对象 key 词典序、数组按本表指定的稳定 key 排序后按 [`config.md` §4](config.md) canonical 化。任何交叉约束失败、未知字段、未知枚举或无法完整取得路径，均拒绝建立 snapshot，转 fail-closed 输入错误而非把缺失降格为绿灯。

### 2.3 `VerdictV1` 闭合并集（G2）

每个 verdict 都是 closed object，公共字段为 `schema_version:1`、`head_sha`（必须等于 input）、`kind`、`code`；除下表 payload 外禁止字段。`verdict_digest=SHA-256(canonical_json(verdict))`。下表穷尽分支，按 §3 顺序第一个命中者唯一返回：

| `kind` / `code` | 必需 payload | 后继 |
|---|---|---|
| `failed` / `hard_guardrail` | `rule_id`、排序 `matched_paths` | Run failed；无 Interrupt |
| `wait_checks` / `checks_pending` | `external_url`、`pending_started_at_ms` | 等待重新观测 |
| `retry_checks` / `flaky_retry` | `check_run_id`（等于 triage 的 `retry_check_id`）、`retry_no`（1-based） | 创建 §3.2 `rerun_checks` operation |
| `hitl` / `guardrail_violation` | `rule_id`、`matched_paths_digest` | `guardrail_violation` Interrupt |
| `hitl` / `failure_review` | `external_url`、`classification` | `failure_review` Interrupt |
| `hitl` / `code_review` | `review_policy`、`risk_score` | `code_review` Interrupt |
| `hitl` / `merge_conflict` | `mergeability=conflicting` | `merge_conflict` Interrupt |
| `hitl` / `mergeability_unknown` | `mergeability=unknown` | 明确人工路径 |
| `ready` / `merge` | `change_id`、`expected_head_sha` | `merge_change` operation |
| `ready` / `no_auto_merge` | `reason=policy_disabled|draft` | 无 merge operation |
| `hitl` / `input_unknown` | `field`、`reason` | fail-closed 人工路径 |

`retry_checks` 仅当 failure/`flaky`、合法 `retry_check_id` 且 `flaky_retries_used < flaky_retry_limit` 命中；`pending_timed_out=true`、unknown Checks 和未定义/耗尽的 triage 均不得返回 wait/retry。canonical fixtures 至少覆盖上表每一分支、同 SHA Checks 漂移 cache miss、路径不完整和每条交叉约束反例。

### 2.4 缓存与回放

`gate_version` 版本化纯函数及其判定语义；输入 schema 变更以快照 schema version 表示。缓存仅可按上述二元键 insert-or-return existing：同键却得到不同 verdict digest 是 contract violation，不得覆盖或选择其中之一。

缓存条目、每次 evaluation、Shadow Gate/校准记录和 JSONL 回放记录必须引用同一个 `gate_input_snapshot_id`。cache hit 复用 verdict 可以避免重新计算，但仍必须新建 evaluation 和 calibration 预判；回放只把冻结输入重新喂给相同 `gate_version`，绝不拼接当前 Forge、策略或认证数据。

## 3. 判定顺序

下列是逻辑顺序；为保持纯函数，T3/T5 和 Forge 读取由调用方先完成并冻结。前一阶段产生终局结果时不得继续执行后续阶段或绕过它。

1. **`protected_paths`（G3）。** matcher 和 pattern/path normalization 只按 [`policy.md` §2.1](policy.md) 执行；规则唯一来自 `EffectivePolicyV1.protected_paths`，其中 hard 已包含 [`policy.md` §3.2](policy.md) 的内建集合。Gate 不接受 `{rule_id,pattern,level}`、独立默认 CI 清单或其他 policy 投影。为 verdict、豁免和 Ledger 特征提供稳定身份时，Gate 从已规范化 pattern 确定性派生 `rule_id`：hard 为 `hard:<pattern>`，soft 为 `soft:<pattern>`；该 ID 不属于 `EffectivePolicyV1`，也不改变其 hash。命中按 `(rule_id, matched_path)` 词典序排序；任一 hard 命中优先于全部 soft，多个 soft 则选择首个未被同一路径集合的一次性豁免覆盖、且未被任一 `soft_exceptions` pattern 匹配的 rule。路径不完整或 matcher/policy 无效返回 `input_unknown`，绝不据空数组放行。hard 命中立即返回 failed；soft 命中且没有本次有效豁免，返回 `guardrail_violation` HITL。
2. **Checks。** success 才进入下一阶段；pending 未超时返回等待结果，pending 已超时转 HITL。failure 使用冻结的 T5 分类：仅 `flaky` 且尚有有效重试额度可请求确定性重试；真实失败、基础设施失败、T5 不可用/超预算或任何未知分类均转 `failure_review` HITL。Gate 不自行重试或调用 T5。
3. **review policy。** `always` 在冻结的有效审查未满足时转 `code_review` HITL；`risky-only` 只在 `riskScore.risk_score >= effectivePolicy.risky_review_threshold`（确定性高风险兜底固定为 100）、且有效审查尚未满足时转 HITL；`never` 不要求 review。需要审查时，审查状态或平台能力未知不得视为已经满足审查。
4. **auto merge。** 只有有效策略允许、前述所有阶段全绿、Change 非 draft 且 mergeability 明确可合并时，才返回可创建 `merge_change` 的 verdict。该 operation 必须携带本 verdict 的 `head_sha` 作为 `expected_head_sha`。`auto_merge=false` 或 draft 可返回“门禁全绿但不自动合并”；`auto_merge=true` 时，`mergeability=conflicting` 必须转 `merge_conflict` HITL，`unknown` 必须转显式人工/等待分支，二者都不得冒充全绿结果。合并时远端 CAS 拒绝或 head 已变化，旧 operation 必须 stale/no-op，新 head 必须重新组装快照并过 Gate。

硬护栏、未知事实和所有 HITL 分支都不能被后续 review、auto merge 或缓存命中放宽。

### 3.2 Flaky Checks rerun 副作用（G4）

V0 的 `flaky_retry` 是一次远端 CI rerun，不是重新读取 Checks。它只能由 `retry_checks` verdict 创建 `rerun_checks` outbox operation；Gate/reconciler 不得直接调用 Forge。operation payload 固定含 `run_id`、`change_id`、`head_sha`、`check_run_id`、`retry_no` 和 triage 的 source digest；key 为 `run:<run_id>:checks-rerun:<head_sha>:<check_run_id>:<retry_no>`。写入该 operation 与递增该 head/check 的已消费 retry 数必须同一事务，额度上限以冻结的 `flaky_retry_limit` 判定。

Forge 端口 `RerunCheck(ctx, project, checkRunID, expectedHeadSHA)` 必须在请求中将目标绑定同一 head；适配器无法提供该绑定或 rerun 目标不唯一时返回 `AuthOrCapability`/`SemanticConflict`，不降级调用。该副作用没有可靠 marker 或查询证据，故 `rerun_checks` 是**最多一次调用、非 effectively-once**：首次 worker attempt 的 lease 过期、调用返回丢失或完成事务失败时，operation 直接 `conflict` 并发 `failure_review`，不得 reclaim 后再次调用。仅明确的调用前 transient/rate-limit 可在尚未发出请求时 retry；实际请求已发出即永久记录 attempt result。远端成功只表示已请求 rerun，随后必须重新 `GetChecks` 并为新观测建立新 Gate snapshot；不得把成功响应当作 CI success。

## 4. 软护栏豁免

软护栏命中默认只可请求一次性豁免。豁免至少绑定 `run_id`、`head_sha`、`rule_id` 与本次命中路径集合的 canonical digest；不得绑定包含该豁免自身的整份 `gate_input_hash`，否则批准进入下一快照后永远无法匹配。换 head、规则或命中路径集合、其他 Run 均不能复用。批准后该受限事实进入下一次冻结输入；原来的 `guardrail_violation` verdict 不被原地改写。

“记住”不是一次性豁免的别名，也不是 Gate 选项的隐式副作用。它必须是人显式选择的独立动作，形成一个由人发起、按**旧**策略审查的仓库 policy 例外变更；只有该变更已进入 base policy 并经有效策略组装，后续 Gate 才可使用该例外。Gate 从不写 policy、从不把人的自然语言回复解释为 remembered exception。

默认硬护栏及任何被有效策略标为 hard 的规则永远不进入一次性或记住豁免路径：命中即 failed，也不得创建 `guardrail_violation` Interrupt 来寻求批准。

## 5. 调用、Shadow Gate 与 HITL 事务

Gate reconciler 在事务外读取 Forge/Brain 事实、组装快照并运行纯函数；不得持有数据库事务执行这些 IO。随后通过 `RecordGateEvaluation` 原子持久化：输入 snapshot（按 hash 去重）、cache insert-or-return、一次 evaluation、Shadow Gate 预判 calibration 和必要的领域后继动作。

若 verdict 需要 HITL，该写端口必须在**同一事务**内完成 Gate snapshot/cache/evaluation/calibration，并调用 M3 `EmitInterrupt` 的五件事：Run 转合法 `waiting_human` 状态、generation-key 去重和首次注意力记账、Interrupt、事件、发布 operation。任一步失败则整体回滚；严禁等人回复后再补 Shadow Gate，或先使 Run 等待再补 Interrupt。人类决定的补全、Ledger entry 和认证投影属于 `recordHumanDecision` 的后续同事务职责，见 [`ledger.md`](ledger.md)。

非 HITL verdict 同样必须创建 calibration 预判，且不得把“没有打扰人”误作人类决定。每次 evaluation 的 `cache_hit` 状态和 verdict digest 都可审计；同一输入的多次调用应有多行 evaluation/calibration，而不是覆盖先前记录。

## 6. Change 创建与 Gate 的衔接

Gate/Change reconciler 只接受 M3 `EvaluateSuccess` 已确认的“可创建 Change”领域事实：成功 `result.json` 身份一致、final head 已冻结且一致、分支至少一个提交。失败 attempt、中间提交、未冻结 head 或 Agent 自报完成都不得创建 Change operation；Gate 不重新从 worktree 或 Agent 输出推断这些事实。

满足该事实时创建 `create_change` operation，key 固定为 `run:<run_id>:create-change:<head_sha>`，payload 冻结 base/head/title/body。worker 必须先经 Forge `FindChangeForCreateOperation` 跨开启、关闭和已合并状态搜索 marker：

1. marker 唯一命中时持久化远端 Change ID，之后只按该 ID 收敛；命中已关闭/已合并对象按外部事实处理，绝不重建；
2. 无命中才调用 `CreateChange`，其 body 由 outbox renderer 追加同一 operation marker；创建成功后同样持久化远端 ID；
3. 同 base/head 的无 marker 对象、marker 歧义或 key/digest 不符均为 `SemanticConflict`，转 HITL，绝不接管他人 Change。

远端 Change ID、head 和归一后的 Change facts 成为后续 Gate 输入的一部分。创建并不授权合并；只有本规格 §3 得到的、对应同一 head 的 auto-merge verdict 才可建立带 `expected_head_sha` 的 merge operation。

## 7. 验收派生

本节是行为验收映射，不替代尚未冻结的 input/verdict closed schema。字段级评审阻断关闭后，schema fixture 与下列行为断言必须同时成立。

- `gate` 在无 Forge/数据库/时钟/文件/Brain 的环境中，对相同冻结输入得到字节等价 verdict；回放集可重跑。
- 整份输入 hash 与唯一二元缓存键生效：同一 head 下 Checks、review、mergeability、risk（含来源版本）或 certification/effective policy 变化均 cache miss。
- `.sift/**`、CI 配置和其他 hard rule 命中失败，不能走两种软豁免；soft rule 分别覆盖一次性批准和显式 remembered-policy 路径。
- Checks、review policy、风险兜底和 auto merge 按 §3 顺序 fail closed；merge 的 head 漂移使旧 operation stale/no-op，远端 CAS 不能以 Gate(A) 合并 B。
- 每次 Gate 调用（包括 cache hit）都有新的 evaluation 与 calibration；HITL 时它们和 Interrupt 五件事全有或全无。
- Change 仅由 M3 可创建事实触发；marker 全状态命中、远端 ID 持久化、已关闭/已合并收敛及无 marker 冲突拒绝均有测试。
