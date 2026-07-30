# M5 #779 critical fuse 定向复审 (#777 / #778)

> 日期：2026-07-30
> 评审人：pi × DeepSeek V4 Pro（Sol role）
> 检测到的 Forge：GitHub（`gh`）
> 评审对象：#777 / PR #778，合入提交 `54f0191`
> 评审基线：`origin/main` @ `54f0191` vs prior `20d8f07`
> 判定基准：[`interrupt.md` §8.3](../specs/interrupt.md)、[`config.md` §3.9](../specs/config.md)、[`storage.md` §6.3/§9.1/§9.3](../specs/storage.md)

## 1. 结论

**PASS。** PR #778 (`54f0191`) 闭合了 Issue #777 WBS §5.3 的 critical 熔断 + 非 critical 超额合批/不可借支项。无 P0/P1 finding。

## 2. 实现范围审查

### 生产代码变更（1 文件，+23 / -6）

`internal/storage/interrupt.go::chargeAttentionTx` 的日配额 CAS 逻辑修改：

- **旧行为**：`UPDATE ... WHERE consumed_value<limit_value` 的 0 行结果直接视为 `ErrAttentionQuotaExceeded` → `quota_batched`。
- **新行为**：CAS 0 行后重读权威 counter `consumed_value`/`limit_value`；若 `consumed+1 ≤ limit` 则在有界重试（8 次）内重新 CAS；仅当重读证明 `consumed+1 > limit` 才返回 `ErrAttentionQuotaExceeded` → `quota_batched`。重读失败或 counter 行缺失 → 错误回滚，不由 `quota_batched` 伪装。

此变更与 `config.md` §3.9 的要求精确一致：

> "扣费比较-and-set 的零行结果**不是**额度耗尽：必须以同一稳定 generation/admission key 重读权威 counter；若 `consumed + 1 <= limit`，在有界重试内重新 CAS，只有重读证明 `consumed + 1 > limit` 才可把原 Interrupt 入批。不可恢复的 SQLite/事务/存储错误整笔回滚，不得伪装成额度耗尽或合批。"

### 已存在的熔断基础设施（本次未修改）

以下功能在 `54f0191` 之前已正确实现，本次 PR 未改动：

- `admitCriticalTx`：在单个 CAS 事务内执行 `SELECT COUNT(*) WHERE kind='critical_admitted' AND created_at_ms>? AND created_at_ms<=?`（global）+ 同条件 per-Run，半开窗口 `(now-window, now]` 正确匹配 `[created_at_ms, created_at_ms+window)` 生命周期。global 优先于 per-Run。`admission_key` 幂等返回已有 admission/batch。
- **EmitInterrupt 初始 critical 路径**：`severity==SeverityCritical` 时调用 `admitCriticalTx(critical_source="initial")`。
- **AdvanceInterrupt 升级 critical 路径**：升级后 `next==SeverityCritical` 时调用 `admitCriticalTx(critical_source="escalation")`，charge 复用无第二笔 `budget_entry`。
- `quota_batched` 处理：`ChargedBudgetEntryID=""` → `nullable("")` → NULL，admission charge 为 NULL。
- 熔断拒绝：`!admitted` → `addCriticalBatchMemberTx` + `UPDATE interrupts SET dispatch_state='batched'`。
- 升级后原 charge 复用：`admitCriticalTx` 读取 `COALESCE(charged_budget_entry_id,'')` → `nullable(charge)` 传递到 admission。

### 测试（1 新文件，550 LOC）

`internal/storage/critical_fuse_test.go` 包含 7 个测试函数，覆盖全部规格固定向量：

| 测试 | 覆盖的规格要求 |
|---|---|
| `TestCriticalFuseEmitInterruptWindowBoundary` | 初发 critical 窗口边界：`t+window-1ms` 计、`t+window` 及 `t+window+1ms` 不计 |
| `TestCriticalFuseAdvanceInterruptWindowBoundary` | 升级 critical 窗口边界：同上半开区间；charge 复用 |
| `TestCriticalFuseConcurrentAdmissionSerializes` | N 并发 vs K 名额 → 精确 K admitted + N-K fused；重放不占名额 |
| `TestCriticalFuseGlobalPreferredOverPerRun` | global 优先于 per-Run（双饱和/仅 per-Run 饱和两子向量） |
| `TestCriticalFuseQuotaBatchedToCritical` | `quota_batched→critical`：有余量→`critical_admitted`(NULL charge)、饱和→`critical_fused`(NULL charge)；重推不写新 admission |
| `TestAttentionQuotaConcurrentNoBorrowing` | limit=1→1 charged+1 batched(NULL charge)；limit=2→2 charged；counter ≤ limit |

全部测试 `-race` 通过（`go test ./internal/storage/ -run "CriticalFuse|AttentionQuota" -count=3 -race` 三连无 flake）。

## 3. 核对矩阵

| #777 要求 | 代码证据 | 判定 |
|---|---|---|
| 熔断在唯一发射器内生效 | `admitCriticalTx` 仅被 `EmitInterrupt` 与 `AdvanceInterrupt` 调用；无旁路 | ✅ |
| 计数权威（仅 `critical_admitted`，半开窗口） | `SELECT COUNT(*) WHERE kind='critical_admitted' AND created_at_ms>now-window AND created_at_ms<=now` | ✅ |
| 饱和写 `critical_fused`，global 优先 per-Run | `admitCriticalTx` L460-463 | ✅ |
| 非 critical 超额重读后才合批 | `chargeAttentionTx` retry loop + `consumed+1>limit` guard | ✅ |
| 存储故障不伪装 `quota_batched` | 重读失败 → error → rollback；CAS 耗尽 → `ErrInterruptRejected`（非 `ErrAttentionQuotaExceeded`）→ rollback | ✅ |
| 幂等 | `admission_key` UNIQUE；`admitCriticalTx` 查已有 admission | ✅ |
| 诚实测试 | 7 测试 × -race 三连 PASS | ✅ |
| 不借支 | `TestAttentionQuotaConcurrentNoBorrowing` counter ≤ limit | ✅ |
| `quota_batched→critical` NULL charge | `TestCriticalFuseQuotaBatchedToCritical` 两向量 | ✅ |

## 4. Findings

无 P0/P1 finding。

### [P2] CAS 重试常量命名偏差

- **描述**：`quotaCASRetries = 8` 但在 `for attempt := 0; ; attempt++` 中 `attempt >= quotaCASRetries` 实际允许 9 次 CAS 尝试（attempt 0-8）。命名暗示 8 次重试，实际为 9 次尝试。
- **关闭标尺**：`quotaCASRetries` 改为 `7` 使实际重试=8 次，或在注释中写明允许 9 次尝试。
- **fixer=same**

### [P2] `addCriticalBatchMemberTx` batch identity 拼接脆弱

- **描述**：`strings.Count(batch, ":") < 8` 判断是否需追加 target 后缀，依赖冒号数为 3→8 的隐式约定。若未来 batch identity 结构变更，可能产生静默重复追加。
- **关闭标尺**：抽取 `criticalBatchIdentityComplete` 布尔函数或使用显式 flag。
- **fixer=same**

## 5. Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | N/A |
| P1 | 0 | 是（无需实施） |
| P2 | 2 | 否（记录） |
| DEFER | 0 | 否（backlog） |

## 6. Verdict

**PASS。** P0/P1 全关。#778 可核销 WBS §5.3 critical 熔断项；WBS checkbox 同步由后续 docs sync Issue 诚实执行。
