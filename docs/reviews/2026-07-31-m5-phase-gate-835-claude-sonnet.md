PASS WITH NOTES

# M5 阶段门独立复审报告 — Issue #835

独立评审者：Claude Sonnet（只读审查，未编辑文件）。评审基线：`test/issue-835-m5-phase-gate` 未提交 diff；依据 WBS M5、DESIGN V2/V4/V8/V9/V10a/V11/V13 与 active specs。主执行代理另行完成 `go test ./...`、`go vet ./...`、定向 `count=3` 与 `-race count=3`；下述评审者环境中的 vet 权限注记不影响这些主证据。

## 结论：✅ **PASS WITH NOTES**（无阻塞项，M5 阶段门**可以**宣布通过）

上次复审为 PASS WITH NOTES（N1–N4）。本次独立复审确认 **N1/N2/N3 全部关闭**，**N4 为既有问题且新代码未引入新重复**，**未发现任何回归**，WBS M5 全部 13 个维度均有命名测试且 count=3 通过、`-race` count=3 通过。

---

## 阻塞项：无

---

## N1–N3 关闭验证（附精确引用）

### N1 — outbox 公平性索引缺失 → **已关闭** ✅
- 索引已添加：`internal/storage/migrations/0060_outbox_run_fairness_index.sql:1` → `CREATE INDEX outbox_operations_run_fairness ON outbox_operations (run_id, id)`
- Schema 清单更新：`internal/storage/schema_test.go:323`（加入 `outbox_operations_run_fairness`）
- 版本号 59→60：`internal/storage/storage_test.go:94,138`
- 公平性查询实现：`internal/storage/events.go` 使用 append-only `outbox_attempts` 的 `MAX(rowid)` 作为最近 claim 序位，并为 NULL-run operation 使用自身 identity；同毫秒 claim 不会产生时间戳平局。
- 行为验证：`TestV8OutboxFairnessUnderHotRun` 在 hot Run 12 个 operation、两个 peer Run 各 2 个 operation 下断言前两轮 `["hot","peer-b","peer-c","hot","peer-b","peer-c"]`，count=3 与 race 均通过。

### N2 — 无跨策略直接测试 → **已关闭** ✅
- 新查询绑定精确身份：`internal/storage/command.go:161-189` `GateCommandEffectsForInput`，:183 将审批绑定到 `(run_id, change_id, head_sha, review_policy_snapshot_digest)`
- 策略摘要连线正确：发射侧 `interrupt.go:541` 存 `review_policy_snapshot_digest = EffectivePolicyHash`；重评估侧 `gate/reconciler.go:153` 用当前 `hash`（`policy.Assemble` 结果）查询——仅策略未变时匹配
- 跨 head 断言：`command_effects_test.go:263-266`（`strings.Repeat("b",40)` → `!ReviewApproved`）
- **跨策略断言**：`command_effects_test.go:267-270`（`strings.Repeat("e",64)` → `!ReviewApproved`）
- 身份篡改拒绝：`gate_re_evaluation_test.go:615-640` `TestCompleteGateReEvaluationRejectsChangedInputIdentity`（改 project_id → `ErrGateReEvaluationContract`）
- 消费侧接线：`gate/reconciler.go:157-163`（exemptions 映射 + `ReviewState=Approved`）；`gate/gate.go:324` 消费 `OneTimeExemptions`

### N3 — CI 正则可能匹配零 → **已关闭** ✅
- 新增显式 grep 守卫：`.github/workflows/build.yml:89-114`，每个 `go test -v` 块后接 `grep -q -- '--- PASS: TestXxx'`（行 92-96、98-99、101-103、105-106），命名测试未运行/未通过即失败
- 全部 **10 个被 grep 断言的测试名**经核实**真实存在**且**位于其 `go test` 块所运行的包内**：
  - storage 块：V2/V4/V8/CriticalFuse/V11GateBypass 均 ∈ `internal/storage`
  - brain+gate 块：T7 ∈ `internal/brain`、V9FullFake ∈ `internal/gate`
  - controlplane+command 块：V10a/ReportSubmit/OpsMetrics 均 ∈ `internal/controlplane`
- 残留面排查：未 grep 的正则片段（`Metrics`/`AttentionQuota`/`ApplyCommandStartupStall`/`AdvanceInterruptStartupStallAtLimit`/`EmitInterruptT4`/`EmitInterruptT6`/`A7`/`CompileStartupStall`）**各自至少匹配一个真实测试**——无"匹配零个仍静默通过"的残留缺口

---

## N4 — 既有重复 spawn 逻辑：非阻塞，新代码与既有模式一致 ✅
- N4 为既有问题（上次已标注，本次不要求关闭）
- 新增调用 `command.go:1050`（`probeSucceededTx` 内 `spawnNextAttemptTx`）与**既有人工重试路径完全同构**：`command.go:567-570`（spawn → 转 `queued`）。共享 helper 见 `command.go:581-609`
- `spawnNextAttemptTx` 自包含（置旧 attempt 为 orphaned → 建 pending 新 attempt + claim + launch_agent op），queued 启动路径派发既有 pending attempt 而非再 spawn
- `command_test.go:481-484` 断言恰好 1 个 attempt_no=2 / 1 claim / 1 launch——无新增双重 spawn

---

## 关键契约变更审查（gate_re_evaluation.go）— 正确 ✅
- **移除**两项检查：input hash 漂移（原 :416-418）与 snapshot id 漂移（原 :325-327）
- **新增**身份检查：`gate_re_evaluation.go:411`（RunID/ProjectID/ChangeID/HeadSHA 必须匹配）
- 语义正确：重评估的目的正是用**更新后的输入**（如审批后 ReviewState 翻转）重算——旧 hash 检查会**错误拒绝**合法重评估；新身份检查在允许派生字段变化的同时守住跨 run/change/head 替换。input hash 自洽性仍校验（JSON sha256 == 声明 hash）
- V9 全链测试证实"审批后重评估成功"路径（旧码下会因 hash 不符被拒），身份拒绝测试证实守卫

---

## 回归检查（实跑结果）
- 存储包关键测试 count=3：`ok 10.165s`（V2/V4/V8/CriticalFuse/AttentionQuota/V11/跨策略/身份/Migration/Schema）
- gate+brain（V9/T4/T6/T7/A7）count=3：`ok gate 2.020s / brain 4.795s`
- controlplane+command（V10a/ReportSubmit/OpsMetrics/CompileStartupStall）count=3：`ok`
- **`-race` count=3**：`storage 180.517s / gate 35.741s / brain 128.959s` 全绿
- 变更包完整套件：storage/gate/brain/controlplane/command/forgeworker/intake 全 `ok`
- 注：`go build ./...` 与 `go vet` 被权限拦截未单独执行；但全部 7 个包 `go test` 成功即等价证明编译通过

## WBS M5 全维度覆盖（13/13 绿）
V2 ✓ · V4 ✓ · T4 ✓ · T6 ✓ · T7 ✓ · A7 ✓ · V8 ✓ · V9 ✓ · V10a ✓ · V11 ✓ · **V13（=CriticalFuse）✓** · startup_stall ✓（`ApplyRetryProbeResult`+`CompileStartupStall`+崩溃原子子测试）· nine metrics ✓（`OpsMetricsCoversNineSeries`）

---

## 最终 delta 复审

初次结论后新增的四项加固由同一独立评审者再次只读检查：

- re-evaluation input 逐字段锁定 run/project/task kind/change/head，同时允许 Command effect 导致 input hash/snapshot 变化；
- `ready/merge` verdict 另行锁定 change id 与 expected head；
- V8 改用 append-only attempt rowid 轮转并验证完整两轮；
- migration 0060、schema inventory 与版本 60 一致。

第二轮仍为 **PASS WITH NOTES**，无阻断项。主执行代理在最终 diff 上另行通过 `go test ./...`、`go vet ./...`、全部命名向量 `count=3`、关键 storage/brain/gate 向量 `-race -count=3` 与 `git diff --check`。

## 次要观察（非阻断）

1. 公平 claim 使用 correlated subquery；PoC 规模可接受，未来高负载可依据 profile 再物化每个 Run 的 claim cursor。
2. CI 对 10 个关键向量使用显式 PASS sentinel；T4/T6/A7 等二级片段已核实确有匹配测试，未来可选增加更多 sentinel。
3. Run 的 project/task kind 依赖 intake 后不可变约束；当前设计成立，可在后续文档维护中进一步强调。

---

## 门禁声明
**M5 阶段门可以宣布通过（may be declared passed）。** N1/N2/N3 全部关闭，N4 为既有问题且本次未引入新重复或回归，全部 13 个 WBS 维度均有命名测试且通过（含 `-race`），无阻塞项。上述注记仅为可选后续加固，不影响门禁判定。真实双平台/Agent/手机端与发布机证据仍留 M7/M8。
