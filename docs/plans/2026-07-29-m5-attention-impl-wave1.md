---
status: active
created: 2026-07-29
summary: M5 Attention/Command 实现波次一：T4/T6/Channel/A7 接线顺序
---

# M5 Attention 实现波次一

> **For agentic workers:** 按 Issue 关闭条件交付；交叉改 `storage`/`interrupt` 时串行 rebase；合入后扫 `<<<<<<<`。

**Goal:** 在字段规格全部 `active`、Brain T4/T6/T7 调用壳 [rereview-6 PASS](../reviews/2026-07-29-m5-brain-t4-t6-t7-impl-406-rereview-6-pi-gpt-5.6-sol.md)、Channel [字段复审 PASS WITH NOTES](../reviews/2026-07-29-m5-channel-field-rereview-3-pi-gpt-5.6-sol.md) 之后，按依赖接通 M5 运行时首批能力。

**Architecture:** T4/T6 在 `EmitInterrupt` **事务外**调用统一 Brain 壳，closed 结果经确定性接纳器进入唯一发射器；Channel 只消费 sealed `channel_publish`；调度/升级只经 `AdvanceInterrupt`。不得新建第二 Interrupt 入口或第二收费口。

**Tech Stack:** Go、SQLite、既有 `internal/brain` shell、`internal/storage` EmitInterrupt、outbox workers。

---

## 基线（已完成，勿重做）

| 项 | 证据 |
|---|---|
| M5 字段规格 `active` | interrupt/command/report/config/brain T4–T7/channel |
| Brain 调用壳 + 验收矩阵 | #406…#451；rereview-6 PASS |
| Channel 规格 `active` | #442 / PR #443 |
| 生产调度器/提交唤醒 | #302 rereview-2 PASS |

## 依赖拓扑

```text
WBS sync (docs)
    │
    ▼
[1] T4 → EmitInterrupt 接纳（事务外 Call + 确定性接纳 + forge 首发不变）
    │
    ├──────────────► [2] T6 severity/delivery + 持久化 dispatch 快照
    │                      │
    │                      ▼
    │                 [3] AdvanceInterrupt 最小 tick（expires / next_dispatch）
    │
    └──────────────► [4] Channel webhook adapter + channel_publish worker
                              │
                              ▼
                         [5] failure episode → forge_alert（阈值）
    │
    ▼
[6] T7 A7 防火墙：提案不自动生效；不能放松单条 Gate/HITL
    │
    ▼
波次二：Command §5.4 → Report §5.5 → 预算/指标 §5.6–5.7
```

## 波次一 Issue 切片

### I0 — WBS/AGENTS 对账（docs）
- 勾选/脚注：调用壳与 channel 规格状态；明确 §5.1–§5.2 实现项仍开
- 不写实现代码

### I1 — T4 接入 EmitInterrupt（feat，阻断后续）
对照 [`interrupt.md` §7.1](../specs/interrupt.md)、[`brain.md` §11](../specs/brain.md)
- 生产路径：事务外 `Shell.Call(T4)` → `T4ResultFromCall` → 确定性接纳 → 既有 `EmitInterrupt` 五件事事务
- T4 失败/拒绝：确定性 fallback 仍可发射；写 trace
- golden：interrupt.md §3.6 attempt + Report quota variant 各至少一组
- 禁止：T4 改 options 效果、改 severity、第二发射入口

### I2 — T6 调度建议接入（feat，依赖 I1）
对照 interrupt.md §7.2 / brain.md §12
- `suggested_downgrade` 最多降一级；high|critical 强制 immediate
- 持久化 channel_id/delivery/next_dispatch/held_reason
- 无兼容 Channel：held，不回滚 admission，forge comment 仍有效

### I3 — Channel webhook 首包（feat，可与 I2 并行但交叉 storage 时串行）
对照 [`channel.md`](../specs/channel.md)、outbox §10、storage §6.5–§6.6
- adapter：closed payload + `secret_ref` resolver；无业务写端口
- worker：`channel_publish` claim/complete；delivery 状态与 episode
- 连续失败阈值 → 唯一 `forge_alert(channel_failure)`（含 diagnostics 行）

### I4 — AdvanceInterrupt 最小 Supervisor 扫描（feat，依赖 I2）
对照 interrupt.md §8 / storage AdvanceInterrupt
- expires / next_dispatch 两谓词；只调 `AdvanceInterrupt`
- nonce/version CAS；升级不重复收费

### I5 — T7 A7 防火墙（feat/test，可与 I1 后并行）
对照 WBS §5.1、brain.md §13
- 提案/草稿持久化但不自动生效
- 测试：T7/历史数据不能放松单条 Gate、不能抑制单条 HITL

## 波次二（本计划不拆 Issue，I1–I5 稳定后再开）

- Command：approve/reject/retry/hold/ask + startup_stall 两段式（§5.4）
- Report：`run.sock` + not_ready（§5.5）
- 三类预算并存（§5.6）与九项指标/CLI（§5.7）
- M5 门禁定向复审

## 派工约束

- Forge=`gh` / `origin`→`xsift/sift`；Pi `run-pi-task.sh --issue … --approve`
- worktree：`setup_worktree.sh`；**勿**在卡住的 `ensure_agent_labels` 上阻塞
- 审：Sol；修：Terra/Luna 交替；禁止自修自审
- 合入以 `mergedAt` 为准；cleanup worktree
