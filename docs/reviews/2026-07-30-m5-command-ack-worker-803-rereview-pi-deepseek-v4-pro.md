# M5 #803 command_ack Forge publish worker · Sol 复审

> 日期：2026-07-30
> 评审人：pi × DeepSeek V4 Pro（Sol role）
> 检测到的 Forge：GitHub（`gh`）
> 评审对象：#803 / PR #804，实现提交 `a4a4d5a`，合入提交 `0657f29`
> 评审基线：`chore/issue-803-review` @ `0657f29`
> 判定基准：[`outbox.md` §5](../specs/outbox.md)、[`command.md` §1.4 / §6.1](../specs/command.md)、WBS §5.4 Command ack 发布 worker 叙事

## 1. 结论

**PASS。** P0/P1 全部关闭。`command_ack` outbox worker 可核销为本薄切片。

- **marker-then-send**：镜像 CommentWorker（outbox.md §5）；崩溃/响应丢失经 marker 收敛不双发。
- **不可变路由**：`ResolveCommandAckRouting` 自 append-only `command_receipts`+`projects` 解析 target/forge（command.md §6.1）；缺 receipt fail-closed。
- **daemon 接线**：`CommandAckWorker` 共享 forge_alert 的 per-project adapter map，入 `OutboxTick`（`siftd:command_ack`）。
- **诚实 WBS**：once-charge 框保持 `[ ]`；§5.4 叙事已注明 ack worker 落地，并声明不读作 once-charge/M5 完成。

**不读作** once-charge 全生命周期、`gate_re_evaluation` Complete、probe 进程检查 worker、或 M5 门禁闭合；不启动 #748+。

## 2. Findings（Scope gate：仅记 P2，不实施）

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | — |
| P1 | 0 | — |
| P2 | 3 | 否（记录） |
| DEFER | 0 | — |

### [P2] `storage.CommandAckOperationKey` 是死代码

`internal/storage/outbox.go` 定义与 `command.AckOperationKey` 同格式，生产路径未用。`fixer=same`

### [P2] `ackEventKey` 解析失败 summary 未携带原始 key

诊断信息可含 `c.Key`。`fixer=same`

### [P2] 缺少 worker 级分类错误路径测试

rate_limited / auth_or_capability / contract / semantic_conflict 的 worker 级回归未覆盖（adapter 层有分类）。`fixer=same`

## 3. Scope summary

P0=0 / P1=0 / P2=3（不实施）/ DEFER=0。Verdict：**PASS**。
