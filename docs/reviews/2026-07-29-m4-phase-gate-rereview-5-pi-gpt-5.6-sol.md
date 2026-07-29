FAIL

# M4 阶段门禁五次定向复审

评审基线：`8370b0d`，分支 `docs/issue-279-m4-phase-gate-rereview-5-after-277-p1-closure`。本次按 Issue #279 定向复核上一份 [`2026-07-29-m4-phase-gate-rereview-4-pi-gpt-5.6-sol.md`](2026-07-29-m4-phase-gate-rereview-4-pi-gpt-5.6-sol.md) 的关闭标尺 1–5 是否由 #277 / PR #278 关闭，并按 [`WBS.md` M4](../WBS.md) 六项门禁及 active 的 [`policy.md`](../specs/policy.md)、[`gate.md`](../specs/gate.md)、[`ledger.md`](../specs/ledger.md) 裁决。已核对 #277、#279 的正文与编排评论、PR #278 正文、合入提交、完整 diff 和 CI；PR #278 无评论或 review 补充证据。

## 裁决

**M4 仍不通过，不得进入 M5。**

PR #278 已充分关闭 policy 错误矩阵的 alert 幂等，以及 V6 可变输入 hash/cache miss 和 T3/T5 fallback 的结构化 snapshot association；open/merged B 也都新增了生产 Gate 身份，exact inconclusive Gate 下的外部 merge 可写审计、不结算认证并收敛 `done + gate_bypassed`。这些增量足以按现有证据勾选 WBS 的 V5b、T3/T5 和 V6 三项。

但标尺 2–4 仍未全部满足。双平台 checked-in merge fixture 的值没有被 adapter 测试精确断言，worker 也只在 Fake 上断言非空值；更关键的是 external merge 仍先要求 Ledger binding/动作成功、最后才转 `done`，因此尚未进入 Gate 的 `queued/running` Run 或缺失/歧义 binding 的 Run 会被审计错误阻断，违反标尺 3 的“Forge 事实始终先收敛”。纵向中的第二次 Change worker 调用则不会 claim 已成功 operation，只是空转，没有重放 marker lookup 或证明同一 Change ID。V7、V11 与五项同时可用因此继续不勾，M4 总门禁仍为 FAIL。

## 关闭标尺 1–5 对账

### 1. 已关闭：policy 错误矩阵逐支证明 alert 幂等

`TestReconcilerPolicyReadErrorMatrixIsolatesOnlyBadProject`（`internal/gate/reconciler_test.go:321-418`）覆盖 missing policy、非法 policy、坏 base、坏 repo和既存 policy blob 的 `git show` 失败。每支对坏项目连续调用两次生产 reconciler（`:373-380`），随后断言隔离分支恰有一条 `project.isolated`、一个固定 key 的 `forge_alert` 且没有 Gate/merge；missing policy 则有两次 Gate、零隔离事件和零 alert（`:392-413`）。健康项目仍独立产生 Gate。标尺 1 已核销。

### 2. 未完全关闭：A/B 生产 Gate 成立，精确 CLI merge SHA 保留断言仍缺

`TestMergeWorkerRequiresProductionGateForReplacementHead` 与 `TestMergeWorkerRecognizesAlreadyMergedReplacementHeadFromProductionGate`（`internal/forgeworker/merge_test.go:46-115`）共用生产 reconciler：Gate(A) operation 在 B 出现后 stale，第二次生产 Gate 冻结 B；open B 由 worker 合并，merged B 在 Gate(B) 后由远端事实注入，两者 operation 均成功且 evidence 含 B 与非空 `merge_sha`。这关闭了 open/merged B 手工入队的缺口。

双平台部分仍未达到 #277 的明文断言。checked-in fixtures 虽新增 `merge_commit_sha`，值为 `merge-commit-github-7` / `merge-commit-gitlab-7`，但消费它们的 `merge_expected_head_cas_success` 只断言 `State` 与 `MergedAt`（`internal/forge/contract_suite_test.go:247-260`），不检查 `MergeSHA`。`TestV3AllVerbsDualPlatformMatrix` 改用代码内生成响应且只断言 `got.MergeSHA != ""`（`internal/forge/verb_matrix_test.go:55-60,248-253`）；worker evidence 同样来自 `forge.Fake` 并只判非空（`internal/forgeworker/merge_test.go:105-113`）。因此没有自动化证明 checked-in 的平台值被 adapter 精确解析并由 worker evidence 原值保留，标尺 2 只能部分核销。

### 3. 未关闭：exact inconclusive 已修复，“外部事实始终先收敛”仍不成立

`AppendExternalMergeFact` 现接受 exact `inconclusive` calibration；`TestReconcilerExternalInconclusiveMergeConvergesWithoutSettlement`（`internal/intake/reconciler_test.go:141-200`）证明 `code_review` 形态的生产 intake 可得到一条 fact/binding/human-action audit，calibration 保持未结算、认证不生成，Run 为 `done + gate_bypassed`，恢复 tick 不重复追加。这关闭了四次复审指出的 exact inconclusive 回归。

然而生产顺序仍与标尺 3 相反。`ReverseSyncCandidates` 明确返回 `queued|running|waiting_human`（`internal/storage/reverse_sync.go:23-29`）；非 Sift merge 先调用 `recordExternalMerge`，成功后才执行 `TransitionRun(... done)`（`internal/intake/reconciler.go:89-104`）。该函数又先调用只接受当前 `waiting_human` open Gate Interrupt 的 `WaitingHumanGateBinding`，再写 fact、再写 human decision（`:131-147`）；缺失 binding 直接报错，歧义也报错（`internal/storage/ledger.go:81-110`）。所以 Change 在 Gate 建立 Interrupt 前已被外部合并时，合法的 `queued/running` candidate 必然无法收敛；waiting-human binding 缺失/歧义或后续审计失败也会阻止权威事实转移。新增测试只覆盖 exact binding 的成功子集，没有覆盖“始终”。这是 V11 的生产 P1。

### 4. 未关闭：marker 身份已验证，所谓 worker replay 实为 no-op

无-seed 纵向现在读取真实 `create_change` operation key/payload，按生产 digest 计算 marker 并验证 create body（`internal/gate/reconciler_test.go:206-214`），关闭了 marker/key/digest 缺失。

但第一轮 Change worker 已把 operation 完成到 `succeeded`；第二次 `RunOnce`（`:215-221`）不能 claim 它，因为 claim 查询只选择 due 的 `pending|retryable` 或 lease 过期的 `executing` operation（`internal/storage/outbox.go:190-214`）。该调用因此没有执行 `FindChangeForCreateOperation`，测试也未保存首个 Change ID 后精确比较、未断言 marker hit、未断言 `CreateChange` 调用次数。它即使完全不走 worker recovery 逻辑也会通过，故没有证明“重放 worker 后仍为同一 Change ID”。标尺 4 仍未核销。

### 5. 已关闭：V6 vectors 与 fallback snapshot association 均已结构化取证

`TestInputHashCoversMutableGateInputsAndPathsIncompleteDoesNotHash`（`internal/gate/gate_test.go:51-86`）逐项改变同 head 的 Checks、review、mergeability、risk value/source 和 certification revision并断言完整输入 hash 变化；`Cache.EvaluateCached` 只使用该 hash 与 Gate version，既有 cache 行为测试覆盖 hit/miss。`TestReconcilerAssemblyEvidence` 的正常、T3 fallback 和 T5 fallback 分支直接查询 `brain_gate_input_links`，以 `COUNT(non-null gate_input_snapshot_id)` 断言实际 T3/T5 call 均关联 Gate snapshot（`internal/gate/reconciler_test.go:27-133`）。标尺 5 已核销。

## WBS M4 六项

- [x] **V5b / A5 / A6**：`.sift/**`、GitHub/GitLab CI hard path、head drift、policy 读取错误隔离及逐支 alert 幂等均有生产/自动化证据。
- [x] **T3/T5 trace/Gate snapshot**：正常与确定性 fallback 均版本化留 trace，并以结构化关系证明非空 snapshot association。
- [x] **V6**：纯函数、整份输入 hash/cache miss、每次 Gate calibration、冻结导出及 Gate/Brain replay 均有证据。
- [ ] **V7**：A/B 生产 Gate 与 stale/no-op 主体成立；checked-in CLI merge SHA 的精确保留断言和真实 Change worker marker recovery replay仍缺。
- [ ] **V11 审计段**：exact binary/inconclusive binding 路径成立，但 pre-Gate 或无可结算 binding 的外部事实仍可被 Ledger 前置步骤阻断，未满足“始终收敛”。
- [ ] **五项同时可用**：无-seed 纵向已有主体与 marker body，但没有实际 marker recovery replay，且 V11 权威事实路径仍留阻断分支，不能声称无延后项。

本轮同步把前三项改为 `[x]`；后三项、`M4 门禁通过` 与 M5 前置继续保持 `[ ]`。

## 执行证据

从 Issue #279 指定 worktree、基线 `8370b0d` 执行：

- PR #278 的四平台 build、schema drift、vet + test 六个 required checks：**全部通过**。
- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：两次均仅出现既知 doctor fixture 在全仓并行负载下 `signal: killed`；其余 package 通过。
- `CGO_ENABLED=0 go test -p=1 -count=1 ./...`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/controlplane -run '^TestDoctorBaselineChecksConfiguredDependencies$'`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/gate ./internal/replay ./internal/forge ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/gate ./internal/replay ./internal/forge ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。

并行 doctor 时序 flake 与前序记录一致，不改变上述 V7/V11/marker 语义裁决。

## 后续关闭标尺

1. 让双平台 checked-in merged fixtures 使用 SHA 形态的确定值；在 contract suite 精确断言 adapter `MergeSHA`，并让 worker 测试精确断言 evidence 保留同一值，而非仅判非空。
2. 将 Forge merge 的 Run 事实收敛与可选 Ledger 结算解耦或置于同一保证事实优先的事务边界：`queued/running`、exact binary、exact inconclusive、missing/ambiguous binding 均须幂等得到 `done + gate_bypassed`；只有 exact binary 结算，exact inconclusive 只审计，缺失/歧义不得伪造样本。
3. 在无-seed 纵向真实制造“远端 Create 已成功、operation 尚可恢复”的崩溃窗口；第二 worker 必须实际 marker-hit，保持精确同一 Change ID，且 `CreateChange` 总调用次数仍为一。

## Issue #279 对账

- [x] 读取 Issue #277/#279、编排评论、PR #278 正文、提交、完整 diff、checks 及可用 review/comment。
- [x] 在指定 issue worktree、分支和基线复核关闭标尺 1–5。
- [x] 产出指定路径报告并逐项对照 WBS M4 六项。
- [x] 只勾选已有充分证据的前三项；M4 总门禁与 M5 前置未虚假勾选。

## 结论

**FAIL。** #278 关闭了 policy alert、V6/fallback association，并修复 exact inconclusive external merge；但精确 CLI merge SHA 断言、真实 Change marker recovery replay仍缺，且生产 Intake 继续让 Ledger binding/动作先于 Forge 权威 `done` 转移，导致 pre-Gate 或缺失/歧义 binding 的外部 merge不能保证收敛。M4 不得结案，M5 不得开始。
