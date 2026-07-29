FAIL

# M4 阶段门禁四次定向复审

评审基线：`6afc5dc`，分支 `docs/issue-275-m4-phase-gate-rereview-4-after-273-p1-closure`。本次按 Issue #275 定向复核上一份 [`2026-07-29-m4-phase-gate-rereview-3-pi-gpt-5.6-sol.md`](2026-07-29-m4-phase-gate-rereview-3-pi-gpt-5.6-sol.md) 的 P1-1…P1-4 是否由 #273 / PR #274 关闭，并按 [`WBS.md` M4](../WBS.md) 六项门禁及 active 的 [`policy.md`](../specs/policy.md)、[`gate.md`](../specs/gate.md)、[`ledger.md`](../specs/ledger.md) 裁决。已核对 #273、#275 的编排与进度评论、PR #274 正文、四个合入提交及完整 diff；PR #274 无 review 或评论补充证据。

## 裁决

**M4 仍不通过，不得进入 M5。**

PR #274 带来了实质增量：policy 生产 reconciler 已覆盖 missing/非法 policy、坏 base、坏 repo及既存 blob 读取失败；open B 已证明必须重新经过生产 Gate 才能在 A stale 后合并；external merge 不再按 Run/head 猜 calibration；新增纵向测试也首次串起 verified success、Change worker、T3/T5、Gate/HITL、Ledger/认证和双 replay。

但上一轮关闭标尺仍未全部满足，而且发现一个 V11 阻断回归：当前 external merge 只接受 `allow|block` calibration。`code_review`、`merge_conflict`、`mergeability_unknown` 等合法 waiting-human Gate 的 shadow 是 `inconclusive`；它们发生外部合并时，事实写入先报错，后面的 `done + gate_bypassed` 转移不会执行。这违反 WBS V11 与 Gate/Ledger 的“不可结算不等于不可收敛”边界。另有 policy alert 幂等未按矩阵取证、merged-B 仍未经过生产 Gate、真实双平台 CLI merge SHA 未取证，以及纵向未断言 Change marker。故不能据本轮证据勾选六项门禁。

## P1 对账

### P1-1 未完全关闭：错误输入齐全，逐支 alert 幂等仍无证据

`TestReconcilerPolicyReadErrorMatrixIsolatesOnlyBadProject`（`internal/gate/reconciler_test.go:280-366`）已覆盖 missing policy、非法 policy、坏 base、坏 repo和既存 policy blob 的 `git show` 失败；逐支断言坏项目无 Gate/merge、健康项目有 Gate，missing policy 使用 defaults。这关闭了上一轮输入矩阵主体。

但测试每支只调用一次坏项目 reconciler，查询项只有 isolation、Gate/merge 与健康 Gate（`:340-364`），没有统计 `forge_alert` / `project.isolated`，也没有重复调用后断言仍各一条。`SetProjectHealth` 的通用实现虽以 project-scoped key 幂等，但 #273 的关闭标尺明确要求生产 reconciler 各错误分支取证 alert 幂等；该证据仍缺。因此 P1-1 只能部分核销。

### P1-2 未关闭：open-B 生产链成立，merged-B 与真实 CLI merge SHA 仍缺

`TestMergeWorkerRequiresProductionGateForReplacementHead`（`internal/forgeworker/merge_test.go:46-102`）从生产 reconciler 生成 Gate(A)，A 在 open B 前 stale，再由第二次生产 Gate 生成并成功执行 B operation；成功 evidence 也断言 `state=merged`、expected head B 和非空 `merge_sha`。这是有效的 V7 open-B 增量。

然而 merged-B 场景仍在 `TestMergeWorkerStalesOldOperationWhenNewHeadIsAlreadyMerged`（`:129` 起）使用测试 helper 手工入队，未证明 B 重新冻结/过生产 Gate。更重要的是新增 evidence 断言只走 `forge.Fake`；双平台 CLI matrix 的 merge 仍只断言 `State == merged`（`internal/forge/verb_matrix_test.go:58-60`），checked-in GitHub/GitLab merged fixtures 均不含 merge SHA，也没有断言 adapter 的 `MergeSHA`。生产 worker 已要求非空 merge SHA，因此关闭标尺中的 Fake/CLI 解析证据不成立。

### P1-3 未关闭：精确身份接上，但 inconclusive HITL 会阻断外部事实收敛

`WaitingHumanGateBinding` 只从当前 `waiting_human` 的 open Gate Interrupt 取得不可变 evaluation/calibration 身份（`internal/storage/ledger.go:79-108`），`AppendExternalMergeFact` 在同事务校验并写 binding；生产 intake 测试证明 binary calibration 的 binding、manual decision、settlement、certification 及恢复 tick 幂等，Sift merge 也仍跳过 bypass。这关闭了上一轮 Run/head“唯一候选”推测问题。

但 `AppendExternalMergeFact` 将合法 binding 进一步限定为 `predicted_decision IN ('allow','block')`（`internal/storage/ledger.go:64-69`）。当当前 Gate Interrupt 的 shadow 为 `inconclusive` 时，`recordExternalMerge` 在 `internal/intake/reconciler.go:132-144` 返回错误；调用点 `:93-101` 因而到不了 Forge 权威事实要求的 `TransitionRun(... done, GateBypassed=true)`。现有 intake 与纵向测试都人为选了 shadow=`block`，未覆盖最常见的 `code_review` 等 inconclusive waiting-human 分支。不可结算的 calibration 应保持审计但不能阻止 Run 按外部 merge 收敛；当前实现使 V11 失败。

此外，#274 把 active `ledger.md` 从“无 binary binding 时记录动作但不结算”改成“拒绝事实”，但这不能覆盖 WBS/PRD 的外部事实权威与 `done + gate_bypassed` 门禁。P1-3 因生产语义缺口不能核销。

### P1-4 大部关闭，但 Change marker 的同链断言仍缺

`TestM4VerticalVerifiedSuccessToExternalMerge`（`internal/gate/reconciler_test.go:119-252`）不再使用 `SeedGateCandidateForTest` 或 `SeedCertificationForTest`：它经 `ResolveAttemptRace`、生产 `SuccessReconciler` 和 `ChangeWorker` 建立 Change，再经生产 Gate 取得 T3/T5、Gate/Shadow/HITL，外部 merge 后取得 Ledger/认证，并运行 Gate/Brain replay。它还结构化查询 `brain_gate_input_links`，证明实际 T3/T5 各有非空 snapshot link（`:200-216`）。这是本轮最重要的阶段证据，关闭了上一轮 seed 跳过核心与弱字符串 link 断言。

剩余缺口是同链只断言 `run.ChangeID != ""`（`:179-182`），没有观察 create body 的 operation marker、marker identity/digest，亦未在本链重放 create worker 证明按 marker 收敛。worker 生产代码确会渲染 marker，Forge 组件测试也有 marker 契约，但 P1-4 的“Change marker/ID 同链”关闭标尺尚未被该纵向测试完整取证。因此可视为主体关闭、证据仍有一项缺口；且 P1-2/P1-3 已使“五项同时可用”不能成立。

## WBS M4 六项

- [ ] **V5b / A5 / A6**：硬路径、head drift 与完整 policy 读取错误输入已有测试；但关闭标尺要求的各错误分支 alert 幂等仍未取证。
- [ ] **T3/T5 trace/Gate snapshot**：新纵向已证明正常 T3/T5 的非空 snapshot link；fallback 仍只在分离 assembly fixture 中取证，未有同等结构化 association 断言。
- [ ] **V6**：纯函数、记录、导出与双 replay 有证据；cache miss 自动化仍只显式变化 Checks/mergeability，未逐项覆盖 DESIGN V6 要求的 review、risk 与 certification revision 变化。
- [ ] **V7**：open-B 的生产 Gate 链已成立；merged-B 仍手工入队，双平台 CLI 的非空 merge SHA 解析/证据未覆盖。
- [ ] **V11 审计段**：binary failure-review 路径可结算，但 inconclusive waiting-human Gate 的外部 merge 会在事实写入阶段失败，无法收敛 `done + gate_bypassed`。
- [ ] **五项同时可用**：纵向主体已存在，但 marker 未同链断言，且上述 V7/V11 阻断仍在。

本轮不修改六个 checkbox，`M4 门禁通过` 与 M5 前置继续保持 `[ ]`。

## 执行证据

从 Issue #275 指定 worktree、基线 `6afc5dc` 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：并行首轮出现既有 doctor fixture 被 killed 与 launchworker marker 时序失败；独立重跑：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。

首轮时序失败未在独立重跑复现，且不改变上述协议/证据裁决。

## 后续关闭标尺

1. 在 policy 错误矩阵每个隔离分支重复生产 reconciler，断言一次 `project.isolated` 与一次 `forge_alert`；missing policy 保持零隔离告警。
2. 让 open/merged B 的 stale 场景都从生产 Gate 身份出发；双平台 checked-in CLI fixture 补真实 merge SHA，并断言 adapter、worker evidence 均保留该值。
3. external merge 始终先按 Forge 权威事实幂等收敛 `done + gate_bypassed`；只有 exact binary binding 才结算 calibration/certification，exact inconclusive binding 则审计但不结算。补 `code_review` 等 inconclusive 生产 intake 重放测试。
4. 在现有无-seed 纵向中捕获并验证 create body 的 marker/key/digest，重放 worker 后仍为同一 Change ID。
5. 补 DESIGN V6 的同 head review、risk source/value及 certification revision 变化 cache-miss vectors；为 T3/T5 fallback 增加结构化非空 snapshot association 断言。

## Issue #275 对账

- [x] 读取 Issue #275、#273 与 PR #274 可获得的正文、评论、提交及完整 diff。
- [x] 在指定 issue worktree、分支和基线复核 P1-1…P1-4。
- [x] 产出指定路径报告并逐项对照 WBS M4 六项。
- [x] 无充分证据的 WBS 项未勾选。

## 结论

**FAIL。** #274 显著推进了错误矩阵、open-B 生产 Gate、精确 binding 和无-seed 阶段纵向，但 P1-1 的 alert 幂等、P1-2 的 merged-B/双 CLI merge SHA、P1-4 的 marker 同链证据仍不完整；更关键的是 P1-3 当前会拒绝 inconclusive Gate 下的外部 merge并阻断 `done + gate_bypassed`。M4 与 M5 前置不得勾选。
