---
status: active
created: 2026-07-30
summary: PRD §10.2 九项指标、CLI 与事件时间线的派生口径
---

# 指标、CLI 与时间线规格

本文冻结 M5 WBS §5.7 的指标 / CLI / 时间线 bootstrap：从已落盘的事件流 / Ledger / 预算表确定性派生 [PRD §10.2](../PRD.md) 的九项指标，提供 `sift ps` / `logs` / 事件时间线与触发→启动延迟分布的只读查询面。它**不闭合 M5**：完整 Command 仍开，真实 P50<60s 属 M7（critical 熔断已由 [#779 PASS](../reviews/2026-07-30-m5-critical-fuse-779-rereview-pi-deepseek-v4-pro.md) 闭合；Channel `ops.ps`/`ops.doctor` 端点级跨重启验收已由 [#782 Sol PASS](../reviews/2026-07-30-m5-channel-ops-ps-doctor-endpoint-782-rereview-pi-deepseek-v4-pro.md) 闭合——二者均不属本规格验收）。

来源：[PRD §10.2](../PRD.md)、[WBS M5 §5.7](../WBS.md)、[`config.md` §3.13](config.md)、[`ledger.md` §2.4、§4](ledger.md)、[`storage.md` §6.3、§7](storage.md)、[`control-plane.md` §6](control-plane.md)、[`interrupt.md`](interrupt.md)。

## 1. 边界与不变量

1. 指标是**只读派生**：本规格不新增任何写端口。所有数据来自 M1–M5 已有的写端口产物（events、`interrupts`、`interrupt_deliveries`/`batch_deliveries`、`budget_counters`/`budget_entries`、`calibration_entries`、`brain_attempts`、`outbox_operations`、`runs`）。
2. **失败闭合（fail-closed）**：当某指标所需字段尚未被写入时，查询返回其分母与显式 `coverage` 注记，分子为零/空。**绝不发明数字**，也绝不以当前配置重算历史。
3. **北极星权重取冻结快照**：加权打扰分子按每个首次成功送达的 `metric_identity` 恰取一次其 `reason` 的权重，权重来自该 Run 创建时冻结的 `config_snapshot_id`（[`config.md` §3.13](config.md)）。当前配置只能影响新 Run。
4. **响应间隔不得用作人类分钟数**：人的响应间隔仅作 T6 调度特征，绝不替代、校正或隐式乘入北极星权重。
5. **`gate_bypassed` 双口径**：手工合并（`gate_bypassed=1`）**不进入**误放行率分母（它只含 Sift 发起的合并），**进入**门禁绕过率分子。

## 2. 九项指标的派生口径

每项以 `numerator / denominator / rate / coverage` 四元组暴露，`rate` 在分母为零时为 `0`。

| 指标 | 分子 | 分母 | coverage 注记（V0 诚实边界） |
|------|------|------|------------------------------|
| **加权打扰 / 已合并 Change**（北极星） | 每个已成功送达 `metric_identity` 的冻结 `reason` 权重之和（去重一次） | 已合并 Change 数（`status='done'` 的 Run，每个必带 `change_id`） | 权重来自各 Run 冻结快照；响应间隔不计入 |
| **误放行率** | Sift 发起合并后被 revert / 紧急修复的比例 | Sift 发起的合并（`status='done'` 且存在 `state='succeeded'` 的 `merge_change` outbox 操作）；`gate_bypassed` 手工合并**不计入** | 分子所需「合并后 revert/修复」事实 V0 尚未写入，分子失败闭合为 0 |
| **门禁绕过率** | `status='done' AND gate_bypassed=1` | 全部 `status='done'` | 测的是人绕过筛子的频率（PRD §10.2） |
| **Gate 漏放率** | `predicted_decision='allow' AND human_decision='block'`（leak） | 负样本 `human_decision='block'` | 影子门禁产出；权威的按 task-kind 滚动窗口见 [`ledger.md` §4](ledger.md)，此处为聚合 |
| **Gate 误拦率** | `predicted_decision='block' AND human_decision='allow'`（false_block） | 正样本 `human_decision='allow'` | 与注意力配额权衡；权威窗口见 [`ledger.md` §4](ledger.md) |
| **HITL 率** | 产生过 ≥1 条 Interrupt 的 Run 数（去重） | 全部 Run | Interrupt 存在是人类打扰信号；`waiting_human` 投影日后可细化 |
| **注意力配额消耗率** | 最新每日 bucket 的 `consumed_value`（按 severity） | 同一 bucket 的 `limit_value` | 读 `budget_counters` kind='attention' scope='severity' 的最新 bucket，无时区依赖 |
| **分派准确率** | T2 选定 Agent 未被人改写 | 存在 `status='valid'` 的 T2 `brain_calls` 的 Run 数 | V0 无人类 Agent 改写命令/事件；比率为结构性 100%，非经验测量 |
| **LLM 成本 / 已合并 Change** | `brain_attempts` `outcome='valid'` 的 `input_tokens+output_tokens` 之和 | 已合并 Change 数 | 报告 token 计数；token→货币映射是后续定价决策 |

### 2.1 加权打扰的 metric_identity 去重

加权分子对每个首次成功送达的 `metric_identity` 恰取一次权重。该 identity 固定为 [`storage.md` §6.3](storage.md) 的 `metric_identity=<interrupt_id>`，而非会在初发/critical 升级间变化的 admission ID。「成功送达」满足下列之一：

- `interrupt_deliveries.state='delivered'`；或
- 该 Interrupt 是 sealed attention batch 的 member，且对应 `batch_deliveries.state='delivered'`。

同一 lineage 的重试、升级重推与重复送达不得重复加权（按 `interrupt.id` 去重）；每次 delivery 仍保留其真实 admission/delivery 审计。配额拒绝的 `quota_batched` member 按其真实成功 batch delivery 计入；未成功送达的 admission 不计入分子。

### 2.2 Sift 发起合并的因果身份

误放行率分母 = Sift 发起的合并 = `status='done'` 且存在 `state='succeeded'` 的 `merge_change` outbox 操作。这与 [`storage.md` `IsSiftMerge`](storage.md) 的因果身份一致（带不可变 `gate_evaluation_id` 的成功合并）。`gate_bypassed=1` 的手工合并来自 Forge fact，无此 outbox 操作，因此**自然被排除**——这是 V11 指标段的关键不变量（见 §4 测试）。

## 3. CLI 与只读 RPC 面

`sift ps` / `sift logs` / `sift metrics` / `sift timeline` 只连接 `siftd.sock` 并读 operator token，envelope 与授权见 [`control-plane.md` §6](control-plane.md)。新增的 `ops.metrics` 与 `ops.timeline` 是 §5.7 bootstrap 的只读方法，加入 `control-plane.md` §6.1 方法全集与 §6.2 schema。

- `sift ps [--run ID] [--project ID] [--status S] [--limit N] [--after-run-id ID]`：Run/attempt、今日注意力余量（按 severity 的 `limit−consumed`）、开放 Interrupt / pending outbox 计数、隔离状态与 Channel 推送故障（durable delivery/episode 投影）。
- `sift logs <run-id> [--attempt N] [--offset B] [--limit B]`：该 attempt `agent.log` 的有界 base64 读取；轮转导致 offset 不可达时返回 `not_found`，不从当前文件偷偷回零。
- `sift metrics [--project ID]`：§2 九项指标 + 触发→启动延迟分布。
- `sift timeline [--run ID] [--project ID] [--type T] [--after-seq N] [--limit N]`：append-only 事件流的 keyset 分页查询。

## 4. 触发→启动延迟分布

延迟 = 首个 `run.transitioned`（payload `to="running"`）的 `occurred_at_ms` − 首个 `intake.trigger_observed` 的 `occurred_at_ms`，按 Run 计算。缺任一锚点的 Run **被排除而非插补**。分布输出 `count`、`min_ms`、`p50_ms`、`p90_ms`、`max_ms` 与采样列表，百分位用 nearest-rank。**真实 P50<60s 是 M7 验收**，本 bootstrap 只闭合查询/fixture 路径。

## 5. 验收派生

- [ ] 九项指标可经 `ops.metrics` 查询，缺数据处有诚实 coverage 注记
- [ ] `gate_bypassed` 排除误放行率分母且进入门禁绕过率（[`storage.md` §11 测试](storage.md) 的 V11 段）
- [ ] 北极星权重取冻结快照；响应间隔不作人类分钟数
- [ ] `sift ps` 显示 Run/attempt、今日注意力余量、隔离与 Channel 推送故障
- [ ] `sift logs` 提供 Run 原始日志有界读取
- [ ] 事件时间线经 `ops.timeline` 可查（keyset + type 过滤）
- [ ] 触发→启动延迟分布可查（真实 P50 留 M7）

## 6. 不闭合项（诚实声明）

- **完整 Command**仍开；critical 熔断已由 [#779 PASS](../reviews/2026-07-30-m5-critical-fuse-779-rereview-pi-deepseek-v4-pro.md) 闭合；Channel `ops.ps`/`ops.doctor` 端点级跨重启验收已由 [#782 Sol PASS](../reviews/2026-07-30-m5-channel-ops-ps-doctor-endpoint-782-rereview-pi-deepseek-v4-pro.md) 闭合（均不在本规格验收范围内）。
- 误放行率分子（合并后 revert/修复）与经验性分派改写检测依赖尚未写入的事件；本 bootstrap 暴露查询与 fixture 路径，不发明数据。
- 本规格 `active` 不表示 M5 已实现。
