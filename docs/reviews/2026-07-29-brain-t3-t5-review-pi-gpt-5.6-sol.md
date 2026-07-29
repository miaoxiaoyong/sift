# brain.md T3/T5 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/brain.md`](../specs/brain.md) §9–§10（PR #214）
> 依据：PRD §5.3–§5.4、DESIGN §6.5/§8.3/§8.5、WBS M4 §4.2，以及 active/draft 的 forge、gate、storage、interrupt、policy 规格

## 1. 结论

**PASS WITH NOTES（0 个遗留 P1）。**

PR #214 的安全方向正确：T3 不可用固定高风险、T5 不可用固定转 `failure_review`，两者均复用已审定的 call/attempt、closed decode、重试、token 和版本协议。但候选稿尚未达到可直接生成 schema 的字段粒度，并有四处会让错误事实进入 Gate、无法产生确定性后继动作或破坏事务边界的 P1。本文已同步修正全部 P1/P2：

- T3 用双读 Change 夹住 diff，结果严格绑定 Run/Change/head；
- T5 补齐稳定 job ID 与条件 `retry_check_id`，使 Gate 的单目标 rerun 可由合法输出生成；
- T3/T5 nested input、source union、排序、长度、枚举和 fallback reason 全部闭合；
- Brain↔Gate 从错误的 terminal-call 单 FK 改为不可变多对多关联；
- T5 fallback 先进入 Gate snapshot，再由 Gate 原子记录 evaluation/calibration 并发 Interrupt；
- Gate 的 Git OID 接受 SHA-1/SHA-256，并消费 T5 retry target。

`brain.md` 的 T1/T2 既有 active 契约未回退。修正后无 P1，规格保持 `active`。

## 2. 发现与处置

| ID | 级别 | 发现 | 处置 |
|---|---|---|---|
| BT1 | P1 | T3 的 `GetChangeDiff(changeID)` 与 `head_sha` 没有原子绑定。head 在 GetChange/GetChangeDiff 间变化时，新 diff 可被标成旧 head，随后风险分进入旧 head 的 Gate snapshot。 | `brain.md` §9.1 固定 `GetChange → GetChangeDiff → GetChange`，前后 ID/URL/head 全等才 reserve；结果只可进入同 run/change/head 的 Gate snapshot，漂移必须重算。 |
| BT2 | P1 | T5 output 只有 suite 级 `classification`，但 Gate `retry_checks`/Forge `RerunCheck` 必须携带唯一 `check_run_id`。多失败项时确定性消费者无法选择目标，任意挑选会把 LLM 分类暗中扩张为动作。 | T5 output 增 required `retry_check_id`：仅 `flaky` 非空，且必须命中 input 中 `allow_failure=false` 的稳定 ID；其他分类为 null。Gate 独立核对 head、目标和额度。同步修正 gate triage/verdict 接缝。 |
| BT3 | P1 | `brain_calls.gate_input_snapshot_id` 是单值且 call 终结时 snapshot 尚不存在；call 又被 single-finalize trigger 禁止后写。同一 T3/T5 结果还会随 Checks/review 变化进入多份 snapshot，结构上既写不进也表达不了；fallback source 还漏了 logical call ID。 | 删除 call 上单 FK，新增 append-only `brain_gate_input_links` 多对多表，由 `RecordGateEvaluation` 同事务按 canonical source 写入；valid/fallback source 都携 logical ID；Brain replay 升 schema v2，导出排序 snapshot ID 数组。同步修正 DESIGN 关联说明。 |
| BT4 | P1 | 候选稿令 T5 fallback 直接 `EmitInterrupt`，同时又要求该 triage 进入 Gate；这会绕过 Gate snapshot/evaluation/calibration 的原子事务，或由 T5 与 Gate 重复发 Interrupt。 | §10.3 改为确定性 `classification=unknown` triage 先进入 Gate；仅 Gate 在 `RecordGateEvaluation` 事务内发 `failure_review`。 |
| BT5 | P2 | T3/T5 nested objects 只列字段名，缺 string 长度、URL/SHA 格式、数组上限/排序/去重和 T5 job ID；无法从正文生成唯一 closed schema。 | §9.1/§10.1 逐字段冻结；T3 risk points 与 Gate 对齐为 UTF-8 byte 排序去重；T5 failed jobs 与 Forge/Gate 对齐为 `(id,name)` 排序、ID 去重。 |
| BT6 | P2 | Gate source 只有示例对象，未冻结字段类型、与 terminal call 的一致性、fallback logical ID/reason 枚举及 `risk_source_version` 取值。 | §9.3/§10.3 冻结 brain/fallback closed union、stable reason enum、版本列映射和 link 写入约束。 |
| BT7 | P2 | GateInputV1 把 `head_sha` 写死为 64 hex，与 GitHub/GitLab 常见 SHA-1 40 hex 及 active interrupt `git_oid` 契约矛盾。 | `gate.md` 改为 40（SHA-1）或 64（SHA-256）个小写 hex；不把 Git object ID 误当 SHA-256 digest。 |

## 3. 字段级核对

### T3

- **Input：通过。** 顶层与 `change` 均 closed；run/task/change 字段有 required、范围和格式；diff 受总输入上限约束；双读协议阻断 head/diff 错配。
- **Output：通过。** `risk_score` 为 0–100 integer；风险点数量、trim 后长度、排序去重明确；额外字段和类型强转仍由统一 closed decoder 拒绝。
- **Fallback：通过。** provider disabled、token threshold、input over-limit、invalid/provider/recovery 均固定 100 分，`risky-only` 在合法阈值下不能绕过人审。
- **Gate source：通过。** valid/fallback 来源均为 closed/versioned object，进入完整快照/hash，并可由多对多 link 回溯 logical call。

### T5

- **Input：通过。** 只消费指定 head 的 failure CheckSuite；change/check/job 字段闭合，稳定 ID 与 Forge rerun 身份一致；failure 无可归一失败项时 fail closed，不把空数组交给模型猜。
- **Output：通过。** 三分类与 `retry_check_id` 互斥；retry ID 必须命中非 allow-failure 候选，unknown/额外/fenced/错误 ID 均走同 prompt retry 后 fallback。
- **确定性消费：通过。** LLM 只建议分类和目标；Gate 决定是否 retry，仍检查 head、policy quota 和已消费次数；其他分类和全部 fallback 均为 `failure_review`。
- **Trace/cache：通过。** source、目标和分类进入 Gate snapshot/hash；正常与 fallback 不会共用缓存输入。

### 统一调用壳

T3/T5 未建立第二套 provider 协议：prompt/schema 独立版本、两次同输入重试、per-attempt token post-charge、provider error、recovery 和 raw evidence 均继续以 `brain.md` §2–§6 为唯一事实来源。

## 4. 非阻断注记

**N1（P3，信息质量）：** V0 `GetChecks` 提供 job/run 的 ID、名称、URL 和 allow-failure，但不读取 CI log。T5 因而只能用现有归一元数据分诊；这不破坏 fail-closed 安全性——不确定或错误输出最终转 HITL——但可能降低 flaky 自动重试命中率。实现期应以 fixture/回放测量分类效用；若证据不足，后续应单独版本化 Forge 的有界、脱敏诊断输入，而不是让 T5 跟随 URL 或在本次评审中暗增日志权限/API 成本。

## 5. 验收判断

- P1：**0（BT1–BT4 已同步关闭）**
- 结论：**PASS WITH NOTES**
- `brain.md`：**保持 `active`**
- T1/T2 active 契约：**未回退**
- 允许进入 M4 T3/T5 实现：**YES；须以修正后的 §9–§10 和交叉规格为准**
