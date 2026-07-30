# M5 #807 gate_re_evaluation Complete worker · Sol 复审

> 日期：2026-07-30
> 评审人：pi × DeepSeek V4 Pro（Sol role）
> 检测到的 Forge：GitHub（`gh`）
> 评审对象：#807 / PR #808，实现提交 `cd3eabd`，合入提交 `ab76e57`
> 评审基线：`chore/issue-807-review` @ `cd3eabd`
> 判定基准：[`storage.md` §8.1](../specs/storage.md) Gate re-evaluation terminal protocol、[`gate.md`](../specs/gate.md) Command-triggered re-eval、WBS §5.4 gate_re_evaluation Complete 叙事

## 1. 结论

**PASS。** P0/P1 全部关闭。`gate_re_evaluation` Complete + worker 可核销为本薄切片（部分 §8.1 矩阵）。

- **单写口**：`CompleteGateReEvaluation` 独占 lease CAS + Run/Interrupt/close-event/binding 断言 + succeeded/failed/conflict 三臂闭合。
- **Worker**：`GateReEvaluationWorker` claim → 事务外 Produce（Forge reads + `gate.Evaluate`）→ Complete；不另调 `EmitInterrupt` / `RecordGateEvaluation`。
- **daemon 接线**：`GateReEvaluations` 入 `OutboxTick`（对齐 CommandAck/Comment）。
- **证据**：failed 臂 exact digest（`0b7d2e6f…` / `d5a8c170…`）复算；succeeded no-successor；conflict 替换头；`-race` 相关包绿。
- **诚实 WBS**：once-charge 框保持 `[ ]`；HITL/`rerun_checks`/`ready/merge` 后继与 failed 臂 `failure_review` Interrupt 诚实 deferred。

**不读作** once-charge 全生命周期、probe 进程检查 worker、T7 生产调用壳、完整 §8.1 后继矩阵、或 M5 门禁闭合；不启动 #748+。

## 2. Findings（Scope gate：仅记 P2，不实施）

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | — |
| P1 | 0 | — |
| P2 | 3 | 否（记录） |
| DEFER | 0 | — |

### [P2] succeeded 臂 snapshot 重用不做逐字节校验

`recordGateEvaluationTxWithIDs` 仅靠 `gate_input_hash` ON CONFLICT 重用；conflict 臂的 `insertOrReturnGateSnapshotTx` 有 canonical 比对。规格一致性轻微偏差。`fixer=same`

### [P2] `gateShadowDecision` 与 gate 包判定重复

storage 包内重复映射；循环导入限制导致无法直接复用。`fixer=switch:pi::deepseek-v4-pro`

### [P2] failed 臂未发射 `failure_review` Interrupt

已诚实 deferred：`ErrGateReEvaluationSuccessorNotWired` / 注释。`fixer=switch:pi::deepseek-v4-pro`

## 3. Scope summary

P0=0 / P1=0 / P2=3（不实施）/ DEFER=0。Verdict：**PASS**。
