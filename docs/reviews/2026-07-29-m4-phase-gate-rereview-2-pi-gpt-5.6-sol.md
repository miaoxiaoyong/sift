FAIL

# M4 阶段门禁二次定向复审

评审基线：`b221118`，分支 `docs/issue-267-m4-phase-gate-rereview-2-after-265-p1-closure`。本次按 Issue #267 定向复核上一份 [`2026-07-29-m4-phase-gate-rereview-pi-gpt-5.6-sol.md`](2026-07-29-m4-phase-gate-rereview-pi-gpt-5.6-sol.md) 的 P1-1…P1-4 是否由 #265 / PR #266 关闭，并按 [`WBS.md` M4](../WBS.md) 六项门禁及 active 的 [`policy.md`](../specs/policy.md)、[`ledger.md`](../specs/ledger.md)、[`outbox.md`](../specs/outbox.md) 裁决。已核对 #265、#267 的编排评论、PR #266 的合入提交与完整 diff；PR #266 未提供额外正文或评论证据。

## 裁决

**M4 仍不通过，不得进入 M5。**

PR #266 已修正 policy 缺失判别的实现主体，给 merge payload 加入 Gate evaluation identity，补了 merge 后重读，并能区分一类成功的 Sift merge 与外部 merge；全仓、定向重复与 race 门禁本轮均通过。但 P1-2 的 A→B 测试仍直接手工入队 B，且已合并预读分支会把“merged B”误认成旧 A operation 的成功；P1-3 移除了外部事实 binding 的生产调用，导致手工合并永远只能写未结算审计，不能产生要求的 calibration/certification；P1-4 阶段测试仍从 seed 的 Change/认证开始。P1-1 也缺关闭标尺要求的错误分支生产测试和项目隔离收敛。因此 P1-1…P1-4 均不能整体核销。

## P1 对账

### P1-1 未完全关闭：读取实现已 fail closed，但错误路径与隔离未取证

`internal/gate/reconciler.go:208-228` 现在先把 base 解析为 commit，再以 `ls-tree` 的成功空结果作为唯一 missing 情形；base、repo、`ls-tree` 或既存对象的 `git show` 失败均返回错误。这关闭了上一轮“任意 git show 失败伪装成 missing”的代码缺口。

但 PR #266 只把阶段 fixture 从空目录换成正常 Git repo（`internal/gate/reconciler_test.go:59,111-120`），没有覆盖不存在 base、不可读 repo、`git show` 失败时不产生 Gate/merge operation 的生产 reconciler 测试。错误也只是从 Gate reconciler 返回并中止 daemon 当前 tick（`internal/daemon/daemon.go:157-160`），没有按 `policy.md` §3.1 将坏项目持久隔离、同时继续健康项目。故实现方向正确，但尚不满足上一轮关闭标尺 1 的自动化证据及 active policy 的单项目隔离语义。

### P1-2 未关闭：B 仍未重过 Gate，已合并快路径仍错误，终态证据不完整

PR #266 已让生产 Gate 把真实 `recorded.EvaluationID` 写入 merge payload（`internal/gate/reconciler.go:85-90`），并在远端 merge 调用返回后重读 Change（`internal/forgeworker/merge.go:66-81`）。这是有效增量。

但完整 V7 仍缺：

- `internal/forgeworker/merge_test.go:14-21,51,67` 对 A、B 都直接调用 `db.EnqueueOperation`；B 没有经过生产 Gate 重新冻结 facts、T3/T5、Shadow 与 Gate evaluation，测试仍不能证明“新 head 重新过 Gate”。
- worker 在预读发现 `current.State == merged` 时立即把 operation 记为 succeeded（`internal/forgeworker/merge.go:59-60`），没有先验证 `current.HeadSHA == expected_head_sha`。因此旧 operation(A) 观察到已合并的 B 会被误记成功，违反 `outbox.md` §9“只有 expected head 已合并才 succeeded；head 不同应 stale”。现有测试只覆盖 open B，未覆盖 merged B。
- `mergeEvidence` 只保存 `change_id/head_sha/state`（`internal/forgeworker/merge.go:99-102`）；Forge `Change` 模型也没有 merge SHA，仍未满足 `outbox.md` §9 的 merged state/head/**merge sha** 终态证据。

### P1-3 未关闭：Sift merge 分类有增量，但 external calibration binding 被彻底跳过

`internal/intake/reconciler.go:88-101` 现在先用成功的 `merge_change` operation 区分 Sift merge，新增测试也证明该 fixture 不写 `gate_bypassed` 或 manual decision。这关闭了上一轮“所有 merged 都无条件记为人工绕过”的直接错误。

但 external 路径在创建 `forge_change_merged` fact 后直接调用 `RecordHumanDecision`（`internal/intake/reconciler.go:134-141`），不再调用任何 `BindExternalDecision`。仓库中 `BindExternalDecision` 只剩定义（`internal/storage/ledger.go:37-54`），无生产调用者。按 `ledger.md` §3/§5，缺 immutable external binding 时只能写 `calibration_decision=null`，不能结算 calibration、不能重算 certification；这正是当前实现的实际结果。`TestReconcilerOnceExternalMergeCompletesWaitingHuman` 只断言 fact 与 human-decision 行各一条，没有先建二元 Gate calibration，也没有断言 binding、settlement、certification revision 或重复恢复。

新增的 Sift merge 测试本身也手工入队一个不存在真实 Gate evaluation 的 payload，再手工把 operation 完成（`internal/intake/reconciler_test.go:90-107`）；它能证明分类分支，却不能证明 Gate→worker→reverse-sync 的显式因果纵向。

### P1-4 未关闭：没有新增五项同链证据

`TestReconcilerPhaseEvidence` 仍以 `SeedGateCandidateForTest` 和 `SeedCertificationForTest` 起步（`internal/gate/reconciler_test.go:49-53`）。PR #266 对该测试的实质变化只是创建一个合法空 policy Git repo；它仍跳过：

- verified success → `SuccessReconciler` → `create_change` worker → Change marker/ID；
- Gate shadow → external/HITL human decision → calibration settlement → certification projection；
- Gate(A) → stale → Gate(B) → merge(B) → reverse-sync；
- 同一链上的 Gate 与 Brain 两类 replay。

其 Brain link 断言仍只是匹配 `"gate_input_snapshot_ids":[`（`internal/gate/reconciler_test.go:93-95`），不能单独证明 T3/T5 各自的数组非空。组件测试可证明单个 T3 link，但不能替代要求的阶段纵向。

## WBS M4 六项

- [ ] **V5b / A5 / A6**：硬路径与 head drift 局部测试存在，policy 读取实现已改为报错；但错误分支生产测试、坏项目隔离和完整安全纵向仍无证据。
- [ ] **T3/T5 trace/Gate snapshot**：正常/fallback、版本字段和组件级 link 已有证据；阶段测试仍未严格证明 T3/T5 各自的非空 snapshot association，关闭证据未增强。
- [ ] **V6**：纯函数、每次 Gate 记录、导出与 replay 的组件/生产边界证据存在；完整 cache miss 与认证 revision 变化纵向仍未证明。
- [ ] **V7**：B 未经生产 Gate，merged-B 快路径会把旧 A 记成功，merge SHA 终态证据缺失。
- [ ] **V11 审计段**：外部 merge 可收敛 `done + gate_bypassed` 并写审计行，但没有 external binding、二元 calibration settlement 或 certification projection。
- [ ] **五项同时可用**：阶段测试继续 seed Change 与认证，并把 create-change、认证结算和 merge/reverse-sync 留在分离 fixture。

本轮不修改六个 checkbox，`M4 门禁通过` 与 M5 前置继续保持 `[ ]`。

## 执行证据

从 Issue #267 指定 worktree、基线 `b221118` 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/gate ./internal/replay ./internal/forgeworker ./internal/intake ./internal/storage ./internal/daemon`：**通过**。

本轮未复现 doctor 时序 flake；FAIL 仅由上述协议与证据缺口决定。

## 关闭标尺

1. 为 policy missing、坏 base、坏 repo、既存路径 `git show` 失败增加生产 reconciler 测试；错误必须不产生 Gate/merge，并按 policy 契约只隔离坏项目。
2. 让 V7 测试从 Gate(A) 生成 operation(A)，观察 open/merged B 时 A 均 stale，再由生产 Gate(B) 生成 operation(B)；成功证据包含 merged state、expected head 与 merge SHA。
3. external merge fact 创建时携不可变 calibration binding；以生产测试证明 fact、manual decision、calibration、certification、`done + gate_bypassed` 在崩溃/重复下幂等收敛，同时 Sift merge 不写 bypass。
4. 增加不靠 seed 跳过阶段核心的自动化纵向，串起 verified success、Change marker/ID、T3/T5、Gate/Shadow、HITL 或 merge、Ledger/认证及两类 replay；结构化断言每个真实参与的 T3/T5 trace 都有非空 snapshot link。

## Issue #267 对账

- [x] 读取 Issue #267、#265 与 PR #266 可获得的正文/评论及合入 diff。
- [x] 在指定 worktree、分支和基线复核 P1-1…P1-4。
- [x] 产出指定路径报告并逐项对照 WBS M4 六项。
- [x] 无充分证据的 WBS 项未勾选。

## 结论

**FAIL。** PR #266 修正了 policy fail-open 的实现主体、merge 后重读及 Sift/external 分类的第一层，但没有关闭 V7 的 B 重过 Gate，新增了仍会误认 merged B 的旧 operation 快路径，external merge 又没有任何生产 binding，阶段测试也仍从 seed 的 Change/认证开始。P1-1…P1-4 尚不能整体核销，M4 与 M5 前置不得勾选。
