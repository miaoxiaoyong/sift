# ledger.md L1–L6 定向复审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/ledger.md`](../specs/ledger.md)
> 基线：[首次字段级评审](2026-07-29-ledger-review-pi-gpt-5.6-sol.md) 的 L1–L6
> 复审范围：PR #229 合入后的 ledger/config/gate/policy/storage 接缝；不重复开启首次评审范围外的产品设计

## 1. 结论

**PASS WITH NOTES（0×P1）。** L1–L6 均已关闭：四类 Ledger 对象已有 closed v1 contract 与生成/fixture 约束；shadow decision 已从运行期 verdict 分离并穷尽映射；人的决定只能解析不可变因果 binding；gate sample 已成为唯一物理特征事实；认证规则版本与证据 revision 已拆分并共同进入 Gate 快照；窗口、分母、minimum、零分母及迟到样本口径均已冻结。

因此 [`ledger.md`](../specs/ledger.md) 可由 `draft` 转为 `active`，允许作为 M4 Ledger/认证实现与测试的规格基线。下列两项是实现期可闭合的 P2 注记，不阻断激活。

## 2. L1–L6 关闭对账

| 项 | 结论 | 关闭证据 |
|---|---|---|
| L1 | **CLOSED** | ledger §2.1 指定 `internal/ledger/contract/ledger_v1.go` 为四份 JSON Schema 的唯一生成源，要求 valid/invalid fixtures；公共 envelope 与四种对象均为 closed object，并冻结 required/null、枚举、长度/数值/数组上限、排序去重、互斥、unknown-field/重复 key/非 canonical 拒绝及 64 KiB 总上限。§2.2 还把 `change_size` 固定为 files/additions/deletions 统计对象。 |
| L2 | **CLOSED** | ledger §3 将 `shadow_decision=allow|block|inconclusive` 与运行期 verdict 分离，逐项覆盖 gate §2.3 的全部 verdict；storage §10.5 同步允许三值预判并禁止结算 `inconclusive`。code review、未认证或 `auto_merge=false` 不再被伪造成 block。 |
| L3 | **CLOSED** | Gate HITL 由 `interrupts.calibration_id` 在创建事务中不可变绑定；外部 merge/close 只能经 `external_decision_bindings(forge_fact_event_id, calibration_id)`；`recordHumanDecision` 的 union 不接受 calibration/evaluation ID，命令与事实消费者只能解析 binding，明确禁止按 Run/head/时间或“最新 evaluation”猜测。storage §10.5–§10.6、§11–§12.6 同步该关系和唯一写端口。 |
| L4 | **CLOSED** | ledger §1/§2.2 选择 `gate_sample` Ledger entry 为校准特征唯一物理事实；storage §10.5 删除 calibration 的特征 JSON 副本，增加唯一真实 FK `gate_sample_entry_id`；gate §5、storage `RecordGateEvaluation` 与 §12.6 均把 snapshot/evaluation/calibration/gate_sample 纳入同一事务。 |
| L5 | **CLOSED** | config §3.12、ledger §4.2、policy §4.2、gate §2.2 与 storage §10.2/§10.7–§10.8 统一拆分 `certification_rules_version` 和类别证据 `certification_version`。revision 确定性承诺 task kind、rules version 与 evidence digest；不可变 certification snapshot + current CAS 指针取代原地改计数，两版本共同进入完整 Gate input，证据变化不再命中旧 cache。 |
| L6 | **CLOSED** | ledger §4.1 固定按 task kind、`decided_at_ms` 和半开窗口 `[as_of-window, as_of)` 重算；明确 `leak/negative`、`false_block/total` 两个分母、两个 minimum、零分母、未来/迟到/过期/无 binding/inconclusive 处理及 V0 固定阈值边界。§4.3 要求左右边界、minimum 前/等于/后、分类方向、过期与 digest 身份 fixtures。 |

## 3. 非阻断注记

| 项 | 级别 | 注记 |
|---|---|---|
| N1 | P2 | storage §10.5 采用 `calibration_entries.gate_sample_entry_id ↔ ledger_entries` 的同事务互引。SQLite migration 实现必须把环上的 FK 声明为可延迟约束（或采用同等、且不产生可见半成品的可验证方案）；仅“预分配 ID”不能通过默认 immediate FK 的首条 INSERT。应在 M4 DDL/迁移测试中加入提交前缺边、提交时完整及回滚反例。 |
| N2 | P2 | ledger §4.2 把窗口边界写入 `evidence_digest`，若 `as_of_ms` 每次取实时毫秒，则即使参与样本不变也会产生新 revision；storage §10.8 又描述“窗口证据改变时”才更新 current。实现前宜在 contract 中固定 refresh 的 `as_of_ms` 粒度/调度，或明确边界本身即 evidence change，避免每次 Gate 组装都制造 certification snapshot 与 cache miss。该问题使缓存退化但不会复用过期资格，故不构成安全 P1。 |

## 4. 交叉一致性判断

- **Gate 接缝：通过。** gate verdict 集合与 ledger shadow 矩阵逐项对应；每次 evaluation 有 calibration/gate sample，HITL 创建路径原子绑定。
- **Storage 接缝：通过（带 N1）。** 唯一事实源、single-settle、不可变 binding、认证 snapshot/current 与受限端口均已落到字段/事务层。
- **Policy/config/cache 接缝：通过（带 N2）。** rules/evidence 两版本不再混名，均进入 Gate input；资格变化不会沿用旧 verdict。
- **A7 与指标边界：通过。** Ledger 只供 T7 提案、指标和类别级认证；单条历史不得改变单条 Gate 或抑制 HITL；响应间隔仍不作为注意力成本。

## 5. 验收判断

- L1–L6：**6/6 CLOSED**
- P1 数量：**0**
- `ledger.md` 转 `active`：**YES**
- 允许开始 Ledger/认证实现：**YES，N1/N2 随 M4 contract/DDL 实现闭合**
