FAIL

# M4 阶段门禁裁决

评审基线：`4319c26`，分支 `docs/issue-256-m4-phase-gate-review`，Issue #256 指定 worktree。判定基准为 [`WBS.md` M4 与验收权威表](../WBS.md)、[`DESIGN.md` §8.5/§8.6/§12](../DESIGN.md) 以及 active 的 [`policy.md`](../specs/policy.md)、[`gate.md`](../specs/gate.md)、[`ledger.md`](../specs/ledger.md) 和 [`brain.md`](../specs/brain.md)。

## 裁决

**M4 不通过，不得进入 M5。**

本轮提交已经形成 Policy、T3/T5、Gate、Ledger、回放和 Change 创建的组件 API、迁移与单元测试，但 M4 要求的是同一纵向切片“同时可用”。生产 `siftd` 尚未装配 Gate 调用链，merge operation 没有生产者或消费者，外部合并也没有接入 Ledger。因而当前证据只能证明若干部件可独立调用，不能证明 WBS 的六项退出谓词。

## P1 阻断项

### P1-1：M4 Gate 生产纵向链未接线

`internal/daemon/daemon.go:23-31,83-91,107-143` 只装配 Intake T1、reverse-sync、回复、comment、create-change 与 launch worker；没有 M4 Gate reconciler/worker。全仓非测试调用点核对显示：

- `policy.Assemble` 仅由 `doctor` 使用；
- `brain.T3Contract`、`brain.T5Contract`、`T3ResultFromCall`、`T5TriageFromCall` 没有生产调用者；
- `gate.EvaluateAndRecord`、`gate.EnqueueCreateChange` 没有生产调用者；
- `storage.ExportReplayJSONL` 与 replay runner 没有命令或 daemon 调用者。

因此成功 attempt 不能经生产路径创建 Change，Change facts/Checks 也不会触发 T3/T5、有效策略组装、Gate、Shadow calibration、HITL 或 merge。该缺口直接阻断 T3/T5、V5b、V6 及“五项同时可用”。现有 `EvaluateAndRecord` 和 `RecordGateEvaluationAndEmitInterrupt` 单元边界不能替代生产调用点。

### P1-2：V7 的 merge stale/no-op 全链不存在

`storage.OperationMergeChange` 与 Forge `MergeChange(expectedHeadSHA)` 端口已存在，但 `internal/forgeworker` 只有 comment/create-change worker；全仓没有 merge worker，也没有根据 `ready/merge` verdict 入队 `merge_change` 的生产代码。故无法形成 Gate(A) → durable merge(A) → head 变 B → 旧 operation stale/no-op → B 重过 Gate 的链路。

现有 Forge adapter 契约测试只证明远端 expected-head CAS 端口，不证明 WBS V7 的 Gate/outbox/worker 全链。M4 的 A6“**Sift 不合并**硬护栏违规”也因 Sift 尚无合并执行链而不能以 vacuous pass 勾选。

### P1-3：V11 外部合并没有写人类决定与校准分类

`internal/intake/reconciler.go:78-92` 观察到 merged Change 后只调用 `TransitionRun(... GateBypassed:true)`；它不追加可绑定的 Forge fact event，不调用 `BindExternalDecision`，也不调用 `RecordHumanDecision(DecisionManualMerge)`。所以生产路径只能得到 `done + gate_bypassed`，不能满足 M4 要求的“并写入人类决定/校准分类”。

`internal/storage/ledger_test.go` 证明合成的 interrupt 决定可以结算，`internal/skeleton/v11_test.go` 证明 M2 的事实收敛首段；二者没有覆盖实际 reconciler → external decision binding → Ledger/认证投影链。

### P1-4：阶段级自动化证据不完整

即使只按组件层检查，现有测试也没有达到门禁明列的覆盖：

- Gate 行为测试只直接命中 `.sift/policy.yaml`；没有 `.github/workflows/**`、`.gitlab-ci.yml` 与 head 漂移的 Gate/operation 断言；
- replay 测试只执行 Gate JSONL；没有 `ReplayBrainJSONL` 的正常输出与 fallback 重放测试；
- T3/T5 正常/fallback 合约测试、手工构造的 linked trace 导出测试彼此分离，没有证明两类来源均经调用壳 trace 后进入同一 Gate 快照。

这些缺口不是把“生产未接线”重复计数，而是说明当前测试集本身也不足以作为 V5b、T3/T5 和完整 V6 的正式证据。

## M4 门禁逐项对账

- [ ] **V5b / A5 / A6**：内建 hard rules 与纯 Gate 拒绝逻辑存在；CI 两入口、head 漂移和 merge 全链证据缺失，生产 Gate 未接线。
- [ ] **T3/T5 trace/Gate snapshot**：schema、版本化 prompt、正常与 fallback 单测存在；生产调用壳到 Gate 快照未接，组合证据缺失。
- [ ] **V6**：纯函数、整输入摘要、cache、append calibration、Gate 导出重放的组件测试存在；生产每次调用强制记录及 Brain replay 闭环未证。
- [ ] **V7**：create-change marker worker 与 Forge expected-head CAS 各自存在；merge operation 生产/消费及 stale/no-op 全链缺失。
- [ ] **V11 审计段**：`done + gate_bypassed` 首段存在；外部 merge 到 human decision/calibration/certification 的生产接线缺失。
- [ ] **五项同时可用**：组件已落地，但没有一条生产纵向链同时串起 Gate、Shadow、认证、回放和 Change 创建。

故 [`WBS.md` M4 门禁](../WBS.md) 六项保持未勾选，`M4 门禁通过` 与 M5 前置也保持未勾选。这是按现有证据裁决，不是否认已完成的组件工作。

## 执行证据

从 Issue #256 指定 worktree、基线 `4319c26` 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**失败**；`internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 出现共享 deadline 下 fixture `signal: killed`，`internal/launchworker/TestLaunchWorkerWrapperCrashSuite` 一次未观察到 `agent-started`。
- `CGO_ENABLED=0 go test -count=10 ./internal/gate ./internal/policy ./internal/brain ./internal/replay ./internal/storage ./internal/forgeworker ./internal/intake`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/gate ./internal/policy ./internal/brain ./internal/replay ./internal/storage ./internal/forgeworker ./internal/intake`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/controlplane -run '^TestDoctorBaselineChecksConfiguredDependencies$'`：**失败 2/10**，均为 fixture `signal: killed`。
- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker -run '^TestLaunchWorkerWrapperCrashSuite$'`：**失败 1/10**，未观察到 `agent-started`。

两项时序失败延续/扩大了既有测试稳定性 NOTE，但本次 FAIL 的决定性依据是上述 M4 生产链与验收证据 P1；即使两项重跑全绿，M4 仍不能通过。

## 关闭标尺

复审前至少需要：

1. 在生产 daemon/reconciler 接通 verified success → create Change → 冻结 Change/Checks → T3/T5 → effective policy → Gate+Shadow → HITL 或 merge operation；所有 Gate 路径都经过强制记录边界。
2. 实现并测试 `merge_change` durable producer/worker，覆盖 marker、expected-head CAS、head A→B stale/no-op 与 B 重过 Gate。
3. 将外部 merge 事实、calibration binding、`RecordHumanDecision(manual_merge)`、认证投影与 `done + gate_bypassed` 按契约接成可崩溃/幂等的生产路径。
4. 补齐 V5b 两类 CI hard path、head 漂移、T3/T5 正常/fallback trace→snapshot、Brain replay，以及 Gate/Shadow/认证/回放/Change 创建的自动化纵向证据。
5. 全仓标准门禁稳定通过，或对两项时序失败给出独立修复与可重复证据。

## Issue #256 对账

- [x] 读取 Issue #256 全文与 Conductor 编排信息。
- [x] 在指定 worktree/分支、指定基线完成 M4 正式 phase gate。
- [x] 产出 `docs/reviews` 报告并逐项按证据核对 WBS 门禁。
- [x] 未将组件存在误写为生产纵向切片通过；未提前勾选 M5 前置。

## 结论

**FAIL。** 当前没有充分证据勾选任一 M4 门禁项，也没有资格勾选 `M4 门禁通过`。修复 P1-1 至 P1-4 并形成生产纵向与自动化闭环后再复审。
