FAIL

# M4 阶段门禁定向复审

评审基线：`a9e334b`，分支 `docs/issue-263-m4-phase-gate-rereview-after-p1-closure`。本次按 Issue #263 只复核上一份 [`2026-07-29-m4-phase-gate-pi-gpt-5.6-sol.md`](2026-07-29-m4-phase-gate-pi-gpt-5.6-sol.md) 的 P1-1…P1-4 是否由 #258 / PR #259–#262 关闭，并按 [`WBS.md` M4](../WBS.md)、[`DESIGN.md` V5/V6/V7/V11](../DESIGN.md) 及 active 的 [`policy.md`](../specs/policy.md)、[`gate.md`](../specs/gate.md)、[`ledger.md`](../specs/ledger.md)、[`outbox.md`](../specs/outbox.md) 裁决。Issue #263 只有 Conductor 编排评论；#258 的续派记录与四个 PR body 均已核对。

## 裁决

**M4 仍不通过，不得进入 M5。**

PR #259–#262 已把 daemon、成功事实、Change worker、T3/T5、Gate/Shadow/HITL、merge worker、外部合并审计和两类 replay 接成了明显更完整的生产形纵向；定向与 race 套件也通过。但关闭标尺要求的安全语义仍有可直接定位的缺口：策略读取会把任意 `git show` 失败伪装成 policy 缺失；V7 测试用手工入队 B 代替“B 重新过 Gate”，merge 成功后也未按契约重读；reverse-sync 无法区分 Sift 合并和外部合并，且用 Run/head 猜绑定；阶段测试没有覆盖 Change 创建、认证结算及五项同时可用。因此 P1-1…P1-4 均不能判定全部关闭。

## P1 对账

### P1-1 未关闭：纵向已接线，但有效策略读取仍可 fail open

`internal/daemon/daemon.go:147-165` 已生产装配 success → create-change worker → Gate → merge worker；`internal/gate/reconciler.go:55-200` 已调用 T3/T5、`policy.Assemble`、强制 Gate/Shadow 记录，以及 HITL 原子端口或 merge 入队。这关闭了上一份报告所说的“没有生产调用者”主体缺口。

但 `internal/gate/reconciler.go:208-213` 对 `git show <base>:.sift/policy.yaml` 的**所有**错误都返回 `policy.Missing()`。这与 `specs/policy.md` §3.1 明确规定的“只有文件缺失可规范化；base 不存在、对象不可读或 git show 失败必须是 repo/project error”相反。base/repo 读取失败时，生产 Gate 会改用全局缺省继续组装，可能得到 `auto_merge`，故不是安全的 effective-policy 纵向。

此外，非 HITL 后继动作不在 Gate 记录事务内：`internal/gate/reconciler.go:85-90` 先提交 evaluation/calibration，再单独 `EnqueueMergeChange`；`retry_checks` 等非 HITL verdict 也没有生产后继 operation。稳定 key 与下一 tick 可提供最终恢复，但当前没有崩溃窗口自动化证据，不能按关闭标尺 1 的“所有 Gate 路径”读作完整闭环。

### P1-2 未关闭：A→B 测试绕过 Gate，merge 协议也少终态重读

`internal/forgeworker/merge.go:54-72` 已在调用前重读 Change，并把 head 不同与远端 CAS conflict 收敛为 stale；这证明旧 Gate(A) 不会直接合并 B，是有效安全增量。

但完整 V7 仍缺：

- `internal/forgeworker/merge_test.go:14-21,67-68` 直接用 `db.EnqueueOperation` 手工建立 merge(B)，没有让 B 经生产 reconciler 重新冻结 facts、T3/T5、Gate 与 Shadow；测试名中的“new Gate operation”并非测试事实。
- stale 完成仅将旧 operation 标为 stale；没有断言或持久动作证明其触发 B 重过 Gate。周期扫描可能随后做到，但关闭标尺要求的是自动化全链证据。
- `specs/outbox.md` §9 要求 merge 成功后按 Change ID 重读 merged state/head/merge SHA 并持久证据；worker 在 `MergeChange` 返回后立即成功落账（`internal/forgeworker/merge.go:64-72`），没有终态重读，payload 也没有规格列出的 `gate_evaluation_id`。

因此只能确认 CAS/stale 半链，不能勾 V7。

### P1-3 未关闭：生产消费者把所有 merged 事实都记成手工绕过

`internal/intake/reconciler.go:87-98` 对任何 `ChangeMerged` 都无条件执行 `recordExternalMerge`，随后写 `GateBypassed:true`；`recordExternalMerge` 又无条件调用 `RecordHumanDecision(manual_merge)`（`125-142`）。该路径没有核对成功的 Sift `merge_change` operation 或其 Gate evaluation。于是 Sift 自己按 Gate 合并后，下一次 reverse-sync 也会被记为 `manual_merge + gate_bypassed`，破坏 V11 与 A6 的审计分类。

绑定也不是契约要求的显式因果身份。`internal/storage/external_decision.go:11-41` 只按 `(run_id, head_sha)` 查询，并在“恰好一条 calibration”时绑定；而 `GateCandidates` 会持续选择 `waiting_human` Run，每次 Gate 调用依法追加 calibration，同一 head 很容易有多条并导致永不结算。按 [`ledger.md` §3](../specs/ledger.md)，事实观察时必须已有不可变 binding，不能从 Run/head 猜测。

现有 `TestReconcilerOnceExternalMergeCompletesWaitingHuman` 只覆盖**没有 Gate calibration**时出现一条 fact 与一条未结算的人类审计记录；它没有覆盖二元 calibration、认证 revision、重复恢复，亦没有覆盖 Sift merge 不得标 bypass。P1-3 因而未关闭。

### P1-4 未关闭：阶段测试覆盖多个点，但没有证明阶段纵向同时成立

`TestReconcilerPhaseEvidence` 已覆盖两类 CI hard path、头漂移、T3/T5 正常与 fallback、Gate/Brain replay，是实质增量。但其 fixture 从 `SeedGateCandidateForTest` 和 `SeedCertificationForTest` 起步（`internal/gate/reconciler_test.go:45-54`）：

- 没有从 verified attempt 经 `SuccessReconciler`、`create_change` worker 到 Change facts；
- 认证是直接 seed，不是 Gate shadow → external/human decision → calibration settlement → certification projection；
- V7 的 worker 测试另行手工 enqueue B，未进入该纵向；
- 外部 merge 测试没有已绑定 calibration；
- `"gate_input_snapshot_ids":[` 的字符串包含断言不能证明数组非空或 T3/T5 各自链接正确。

故它能支持组件/局部生产边界，不足以关闭上一份报告要求的“Gate/Shadow/认证/回放/Change 创建自动化纵向证据”。

## WBS M4 六项

- [ ] **V5b / A5 / A6**：CI hard path 与 head drift 测试通过，但 policy 读取错误可退化为 missing，Sift merge 又会被 reverse-sync 错记为人工绕过；复合门禁不勾。
- [ ] **T3/T5 trace/Gate snapshot**：正常/fallback 已进入生产形 reconciler 与导出；但阶段断言没有严格证明每个来源的非空 snapshot link，且纵向安全读取仍有阻断，不单独改写阶段门禁。
- [ ] **V6**：Gate/Brain 导出重放通过；完整生产 cache/认证变化纵向未证。
- [ ] **V7**：旧 A stale 已证，B 重过 Gate 与 merge 成功终态重读未证。
- [ ] **V11 审计段**：外部 merge 可写审计记录，但显式因果绑定、认证结算及 Sift/external 分类错误未闭合。
- [ ] **五项同时可用**：阶段测试以 seed 跳过 Change 创建与认证结算，未形成同一纵向。

本轮不修改六个 checkbox，`M4 门禁通过` 与 M5 前置继续保持 `[ ]`。

## 执行证据

从 Issue #263 指定 worktree、基线 `a9e334b` 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**失败**；`internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 一次 fixture `signal: killed`。
- `CGO_ENABLED=0 go test -count=10 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。
- doctor 定向 `-count=10`：**通过**；launch wrapper crash suite 定向 `-count=10`：**通过**。

全仓失败仍符合上一份报告与 PR #262 已声明的 doctor 时序 flake；本次 FAIL 的决定性依据是上述可复现的协议与证据缺口，而不是该 flake。

## 再次关闭标尺

1. 区分 policy 文件确实不存在与 base/repo/git 读取失败；后者 fail closed，并覆盖生产 reconciler 测试。
2. 让 V7 自动化从 Gate(A) 生成 operation(A)，观察 B 后 stale，再由生产 Gate(B) 生成 operation(B)；成功后按规格重读并持久化终态证据。
3. 以不可变 Gate/merge operation 身份区分 Sift merge 与 external merge；外部事实携明确 calibration binding，并将 fact、manual decision/calibration/certification 与 `done + gate_bypassed` 做可崩溃、幂等的生产收敛测试。
4. 增加不靠 seed 跳过核心阶段的自动化纵向，至少串起 verified success、Change marker/ID、T3/T5、Gate/Shadow、HITL 或 merge、Ledger/认证与两类 replay。
5. 全仓标准门禁稳定通过，或继续把 doctor 时序失败作为有 owner 的独立稳定性工作项。

## Issue #263 对账

- [x] 读取 Issue #263、#258 与 PR #259–#262 的正文/评论。
- [x] 在指定 worktree、分支和基线复核 P1-1…P1-4。
- [x] 产出指定路径报告并逐项对照 WBS M4 六项。
- [x] 无充分证据的 WBS 项未勾选。

## 结论

**FAIL。** 四个 PR 已关闭“完全没有生产 M4 调用链”的主体缺口，但 P1-1 的 policy 读取仍可 fail open，P1-2 没有 B 重过 Gate/成功终态重读，P1-3 会把 Sift merge 错记为外部人工绕过且缺显式绑定，P1-4 仍以 seed 和分离测试跳过 Change 创建、认证结算与五项同链。P1-1…P1-4 尚不能整体核销，M4 与 M5 前置不得勾选。
