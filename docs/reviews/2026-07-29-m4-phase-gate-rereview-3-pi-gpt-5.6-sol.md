FAIL

# M4 阶段门禁三次定向复审

评审基线：`91eabbb`，分支 `docs/issue-271-m4-phase-gate-rereview-3-after-269-p1-closure`。本次按 Issue #271 定向复核上一份 [`2026-07-29-m4-phase-gate-rereview-2-pi-gpt-5.6-sol.md`](2026-07-29-m4-phase-gate-rereview-2-pi-gpt-5.6-sol.md) 的 P1-1…P1-4 是否由 #269 / PR #270 关闭，并按 [`WBS.md` M4](../WBS.md) 六项门禁及 active 的 [`policy.md`](../specs/policy.md)、[`ledger.md`](../specs/ledger.md)、[`outbox.md`](../specs/outbox.md) 裁决。已核对 #269、#271 的编排评论、PR #270 的正文、合入提交及完整 diff；PR #270 无评论或 review 补充证据。

## 裁决

**M4 仍不通过，不得进入 M5。**

PR #270 有三个有效实现增量：policy 读取/解析错误会隔离项目并让健康项目继续；merge 成功现在要求 expected head 与非空 merge SHA，merged-B 也会把旧 A operation 收敛为 stale；external merge fact 与候选 binding 改为同事务写入，直接 storage 测试可结算认证并幂等重放。本轮全仓、定向重复和 race 门禁也全部通过。

但关闭证据仍未达到上一轮标尺：P1-1 只测了坏 repo，未覆盖坏 base、既存 policy 的 `git show` 失败和非法 policy；P1-2 的 A/B operation 仍全部由测试手工入队，未证明 B 重新经过生产 Gate，merge SHA 也没有任何自动化断言；P1-3 按 Run/head 搜索“唯一” calibration，正是 `ledger.md` 明令禁止的推测绑定，且生产 intake 测试仍没有 Gate calibration/settlement/certification 断言；P1-4 的阶段测试没有改变，仍从 seed 的 Change 与认证开始。因此 P1-1…P1-4 均不能整体核销。

## P1 对账

### P1-1 未完全关闭：已接隔离，但关闭标尺的错误矩阵只覆盖一支

`internal/gate/reconciler.go:47-55,220-244` 现在把 base resolve、policy parse、`ls-tree` 和既存对象 `git show` 错误统一包装为 `policyReadError`，再调用 `SetProjectHealth(..., "policy_invalid", ...)`；该写口会幂等写隔离事实和一次 alert。`TestReconcilerIsolatesBadPolicyProjectWithoutStoppingHealthyProject` 也证明坏项目不写 Gate/merge、健康项目仍可产生 Gate。这关闭了隔离接线的主体缺口。

但该测试的坏输入只有不存在的 repo（`internal/gate/reconciler_test.go:128-139`）。上一轮关闭标尺明确要求 missing policy、坏 base、坏 repo、既存路径 `git show` 失败的生产 reconciler 分支；其中 missing 的正常路径已有阶段 fixture，PR #270 没有为坏 base、不可读既存对象或非法 policy 增加生产测试，也没有断言一次 alert。故不能把一条坏 repo 用例扩张为完整 fail-closed 错误矩阵。

### P1-2 未关闭：merged-B 实现已修复，B 仍未重过生产 Gate，merge SHA 未取证

`internal/forgeworker/merge.go:59,78,99-102` 已要求已合并快路径和 merge 后重读同时满足 expected head 与非空 merge SHA，并把 merge SHA 放入 evidence；`TestMergeWorkerStalesOldOperationWhenNewHeadIsAlreadyMerged` 关闭了上一轮指出的 merged-B 误成功代码错误。Forge `Change` 与 CLI/Fake 也已补 `MergeSHA`。

然而 `internal/forgeworker/merge_test.go:14-21` 的 helper 仍直接 `EnqueueOperation` 并伪造 `gate-<head>`；A 与 B 分别在 `:54`、`:76`、`:92` 手工入队。测试没有从生产 Gate(A) 生成 operation(A)，也没有在 stale 后由生产 Gate(B) 重新冻结 facts/T3/T5/Shadow 并生成 operation(B)。成功分支只断言 operation succeeded 和 Change merged（`:96-104`），仓库中没有测试断言 CLI/Fake 的 merge SHA 解析或持久化 evidence 的 `merge_sha`。所以关闭标尺 2 的全链和终态证据仍不成立。

### P1-3 未关闭：新增 binding 违反不可推测契约，生产纵向仍未证明结算

`AppendExternalMergeFact` 把 fact 与 binding 放进一个事务，`TestExternalMergeFactBindsExactGateAndSettlesIdempotently` 证明直接 storage 调用在单一候选时能产生 settlement、certification version，并在重复调用时返回同一收据。这是原子性和组件幂等性的有效增量。

但实现通过 `run_id + head_sha + binary predicted_decision` 查询 calibration，并在结果恰为一条时绑定（`internal/storage/ledger.go:37-40,63-80`）。active `ledger.md` §3 明确规定 external binding 不得按 Run、head、时间或“最新 evaluation”猜测；“结果数量为一”不能把推测变成因果身份。该查询也未证明候选 evaluation 就是外部合并发生时所等待/展示给人的 Gate。

生产测试仍停在旧粒度：`TestReconcilerOnceExternalMergeCompletesWaitingHuman` 从 seed waiting Run 开始，没有创建 Gate calibration，只断言一条 fact 与一条 human-decision audit（`internal/intake/reconciler_test.go:51-84`）；没有断言 binding、二元 calibration、certification revision 或重复恢复。Sift merge 测试仍手工入队并手工完成一个不存在真实 Gate evaluation 的 operation（`:87-107`）。因此既未满足因果契约，也未提供关闭标尺 3 要求的生产收敛证据。

### P1-4 未关闭：五项同链阶段证据没有新增

PR #270 没有改写 `TestReconcilerPhaseEvidence` 的链路。该测试仍以 `SeedGateCandidateForTest` 和 `SeedCertificationForTest` 起步（`internal/gate/reconciler_test.go:50-56`），跳过 verified success → SuccessReconciler → create-change worker → Change marker/ID，以及 Gate shadow → 人类决定 → calibration settlement → certification projection。

它对 Brain link 的断言仍只是字符串包含 `"gate_input_snapshot_ids":[`（`:94-95`），不能证明每个真实参与的 T3/T5 trace 都各自带非空 snapshot ID；V7、reverse-sync 和认证仍在分离 fixture。故关闭标尺 4 要求的 Change 创建、Gate/Shadow、HITL/merge、Ledger/认证及两类 replay 同链证据不存在。

## WBS M4 六项

- [ ] **V5b / A5 / A6**：硬路径/head drift 及坏 repo 隔离有证据，但 policy 错误矩阵仍不完整，不能勾选。
- [ ] **T3/T5 trace/Gate snapshot**：正常/fallback、版本和组件 link 证据存在；阶段测试仍未逐条证明真实参与的 T3/T5 trace 有非空 snapshot association。
- [ ] **V6**：纯函数、每次 Gate 记录、导出和 replay 的组件/生产边界证据存在；完整 cache miss 与认证 revision 变化纵向仍未证明。
- [ ] **V7**：merged-B stale 代码缺口已修复，但 A/B 均手工入队，B 未经生产 Gate，merge SHA 终态 evidence 无测试断言。
- [ ] **V11 审计段**：外部 merge 可收敛 `done + gate_bypassed`，但 binding 由 Run/head 推测，生产链未证明 calibration/certification 幂等结算。
- [ ] **五项同时可用**：阶段测试仍 seed Change/认证，并把 create-change、认证结算、merge/reverse-sync 留在分离 fixture。

本轮不修改六个 checkbox，`M4 门禁通过` 与 M5 前置继续保持 `[ ]`。

## 执行证据

从 Issue #271 指定 worktree、基线 `91eabbb` 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。

本轮未复现 doctor 时序 flake；FAIL 仅由上述协议与证据缺口决定。

## 后续关闭标尺

1. 补齐坏 base、坏 repo、非法 policy、既存 policy `git show` 失败的生产 reconciler 测试；逐支断言无 Gate/merge、只隔离坏项目且 alert 幂等，missing policy 仍正常。
2. 以生产 Gate 生成 A/B operation：open/merged B 下 A 均 stale，B 必须重新冻结并通过 Gate；成功后断言 operation evidence 含 merged state、expected head 和非空 merge SHA，并覆盖双平台解析。
3. 让 external merge fact 携可验证的精确 Gate/calibration 因果身份，不按 Run/head/时间/最新或候选数量推测；以生产 intake 测试证明 binding、manual decision、settlement、certification、`done + gate_bypassed` 在重复/恢复下幂等，Sift merge 不写 bypass。
4. 增加不靠 seed 跳过阶段核心的纵向测试，串起 verified success、Change marker/ID、T3/T5、Gate/Shadow、HITL 或 merge、Ledger/认证及 Gate/Brain replay；结构化断言每个真实参与 trace 的非空 snapshot link。

## Issue #271 对账

- [x] 读取 Issue #271、#269 与 PR #270 可获得的正文、评论及合入 diff。
- [x] 在指定 worktree、分支和基线复核 P1-1…P1-4。
- [x] 产出指定路径报告并逐项对照 WBS M4 六项。
- [x] 无充分证据的 WBS 项未勾选。

## 结论

**FAIL。** PR #270 修复了坏 repo 隔离、merged-B stale 和 merge SHA 实现主体，并增加了 external settlement 的 storage 级原子测试；但 P1-1 的错误矩阵、P1-2 的 B 重过生产 Gate、P1-3 的精确因果 binding 与生产结算、P1-4 的五项同链证据仍未关闭。M4 与 M5 前置不得勾选。
