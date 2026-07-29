PASS WITH NOTES

# M4 阶段门禁六次定向复审

评审基线：`eb54516`，分支 `docs/issue-283-m4-phase-gate-rereview-6-after-281-p1-closure`。本次按 Issue #283 定向复核上一份 [`2026-07-29-m4-phase-gate-rereview-5-pi-gpt-5.6-sol.md`](2026-07-29-m4-phase-gate-rereview-5-pi-gpt-5.6-sol.md) 的后续关闭标尺 1–3 是否由 #281 / PR #282 关闭，并按 [`WBS.md` M4](../WBS.md)、[`DESIGN.md` V7/V11](../DESIGN.md) 及 active 的 [`forge.md`](../specs/forge.md)、[`gate.md`](../specs/gate.md)、[`ledger.md`](../specs/ledger.md)、[`outbox.md`](../specs/outbox.md) 裁决。已核对 #281/#283 正文与编排评论、PR #282 正文、合入提交、完整 diff、六个 required checks；PR #282 无 review 或 comment 补充证据。

## 裁决

**M4 通过，允许进入 M5。**

PR #282 已关闭五次复审剩余三个标尺：双平台 checked-in fixture 的确定 SHA 由 adapter 精确断言，merge worker evidence 也由非空断言提升为原值精确断言；无-seed 纵向真实留下可恢复的 `create_change` operation，第二 worker 实际 marker-hit 并收敛到同一 Change ID，远端 Create 总调用一次；external merge 则把 Forge fact 与可选 Ledger binding/settlement 解耦，生产矩阵覆盖 `queued/running`、exact binary、exact inconclusive、missing 与 ambiguous binding，均收敛为 `done + gate_bypassed`，且只在合法精确 binding 下写审计/结算。

据此，前轮已勾的 V5b、T3/T5、V6 不回退；本轮可勾 V7、V11、五项同链及 M4 总门禁。没有阻断项。

## 后续关闭标尺 1–3 对账

### 1. 已关闭：精确 CLI merge SHA

GitHub/GitLab checked-in merged fixtures 分别使用 40 位确定值 `111…111` 与 `222…222`；共享 contract suite 将它们冻结为各平台 `wantMergeSHA`（`internal/forge/contract_suite_test.go:59-98`），`merge_expected_head_cas_success` 在 adapter 完成 merge 后重读时精确比较 `c.MergeSHA`（`:249-267`），不再只判非空。双平台 verb matrix 也精确比较相同平台值。

worker 边界的 open-B 与 already-merged-B 两条生产 Gate 路径都精确断言 evidence 中 `merge_sha == "merge-"+headB`（`internal/forgeworker/merge_test.go:43-114`）。adapter 解析与 worker 原值保留仍是分层契约测试而非单个 CLI-to-worker fixture，但两个边界都已由精确值锁定，足以关闭本标尺。

### 2. 已关闭：Forge 事实先收敛，Ledger 只做精确可选结算

`AppendExternalMergeFact` 不再要求 Gate/calibration identity，先以稳定 idempotency key 写权威 fact，重复调用返回既有 event ID（`internal/storage/ledger.go:37-63`）；`BindExternalMergeFact` 独立校验同 Run 的精确 Gate/calibration，并幂等附着 immutable binding（`:66-103`）。生产 reconciler 即使 `WaitingHumanGateBinding` 缺失或歧义，也先写 fact；binding、审计或结算错误不会阻止后续 `TransitionRun(... done, GateBypassed=true)`（`internal/intake/reconciler.go:87-103,128-148`）。状态入口已允许外部事实的 `queued → done`（`internal/storage/transition.go:206-218`）。

`TestReconcilerExternalMergeFactsFirstWithoutExactBinding`（`internal/intake/reconciler_test.go:203-289`）逐支覆盖：

- `queued`、`running`：一条 fact、零 binding/decision，Run 为 `done + gate_bypassed`；
- exact binary：一条精确 binding、一条 manual-merge decision，并由既有 Ledger/纵向测试证明 calibration/certification 结算；
- exact inconclusive：保留 binding 与 human-action audit，但不补全 calibration、不生成认证；
- missing/ambiguous：只保留一条未绑定 fact，不猜样本；
- 恢复 tick 后 fact 数仍为一。

这满足 V11 的 M4 审计段；指标查询分母仍按 WBS 留 M5，不构成本片延后。

### 3. 已关闭：真实 Change marker recovery replay

`TestM4VerticalVerifiedSuccessToExternalMerge` 现在让第一次 `CreateChange` 先在远端成功，再返回 transient 模拟响应丢失/本地尚未持久化的崩溃窗口（`internal/gate/reconciler_test.go:196-223,305-333`）。第一次 worker 后 Run 的 Change ID 仍为空、operation 为 due 的 `retryable`，但远端 body 已含由真实 operation key 与 payload digest 生成的 marker。第二 worker 在到期时实际 claim operation、调用 `FindChangeForCreateOperation` 命中 marker，并严格断言：

- Run 的 Change ID 等于第一次远端创建的精确 ID，且非空；
- `CreateChange` 总调用次数为 1；
- marker-hit 次数为 1。

这不再是成功 operation 上的 no-op 第二调用，已覆盖标尺要求的 recovery 路径。marker 同时升级为 key + payload digest 精确匹配，避免同 key 异 payload 被接管。

## WBS M4 六项

- [x] **V5b / A5 / A6**：沿用五次复审已确认的 hard path、head drift、policy 读取错误隔离与 alert 幂等证据。
- [x] **T3/T5 trace/Gate snapshot**：沿用五次复审已确认的正常与 fallback 版本化 trace、结构化 snapshot association。
- [x] **V6**：沿用五次复审已确认的纯函数、整份输入 cache miss、每次 calibration 与导出重放证据。
- [x] **V7**：双平台 merge SHA 精确解析、A/B 生产 Gate stale/no-op、worker evidence 原值保留，以及真实 create marker recovery 均有自动化证据。
- [x] **V11 审计段**：facts-first 矩阵覆盖全部指定状态/binding 分类；exact binary 结算、exact inconclusive 只审计、missing/ambiguous 不伪造样本，Run 均收敛 `done + gate_bypassed`。
- [x] **五项同时可用**：无-seed 纵向从 M3 成功事实经过真实 Change 创建及 marker recovery、Gate/Shadow/HITL、T3/T5 snapshot link、外部合并、Ledger/认证和 Gate/Brain replay；没有 M4 延后项。

本轮据证据把后三项与 `M4 门禁通过` 改为 `[x]`；前三项保持 `[x]`。

## 非阻断注记

1. `ledger.md` §5 与实现为满足 facts-first 已覆盖 `queued → done`，但 [`PRD.md` §4.1](../PRD.md) 的状态图仍只列 `queued → running | waiting_human | failed`，与其同节“`done` 只表示 Change 已合并”及 §4.5 的事实收敛语义存在文档漂移。应在后续文档修订中把 direct external-fact edge 补入 PRD；当前实现遵循 A2/§4.5 的权威事实语义，不阻断 M4。
2. marker 崩溃窗以“远端成功后响应丢失并落 `retryable`”建模，而不是在测试进程中 kill worker、等待 executing lease reclaim；实际 marker lookup、同一 ID 和单次 Create 均已发生，通用 lease reclaim 另有 outbox 测试，因此不影响本标尺裁决。
3. `recordExternalMerge` 对可选 binding/settlement 错误采取 best-effort 吸收，以保证 Forge 事实不被审计阻断；现有 exact 路径与 crash/replay幂等证据通过。后续可补 settlement 错误的诊断/重试可观测性，但不得再把它变成 `done` 的前置条件。
4. 本轮并行全仓测试在本机负载下各出现一次 doctor fixture `signal: killed` 与 launchworker marker 未及时出现；串行全仓、两项各自 `-count=10`、M4 定向 `-count=10`、race 及 PR required CI 均通过。两处均未由 #282 修改，不改变本轮 M4 语义裁决。

## 执行证据

从 Issue #283 指定 worktree、基线 `eb54516` 执行：

- PR #282 的四平台 build、schema drift、vet + test 六个 required checks：**全部通过**。
- `git diff 3821c21..eb54516 --check`：**通过**。
- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test -p=1 -count=1 ./...`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/forge ./internal/forgeworker ./internal/gate ./internal/intake ./internal/storage ./internal/replay ./internal/daemon`：**通过**。
- 四组本轮核心用例各 `-count=10`（双平台 contract、A/B merge worker、facts-first matrix、无-seed vertical）：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/forge ./internal/forgeworker ./internal/gate ./internal/intake ./internal/storage ./internal/replay ./internal/daemon`：**通过**。
- doctor 与 launchworker 两个并行负载失败用例分别单独 `-count=10`：**通过**。

## Issue #283 对账

- [x] 读取 #281/#283、编排评论、PR #282 正文、提交、完整 diff、checks 及可用 review/comment。
- [x] 在指定 issue worktree、分支与 `eb54516` 基线复核关闭标尺 1–3。
- [x] 产出指定路径报告并逐项对照 WBS M4 六项。
- [x] 保持前三项已勾；只在证据充分后勾选 V7、V11、五项同链及 M4 总门禁。

## 结论

**PASS WITH NOTES。** #282 已关闭精确 CLI merge SHA、Forge facts-first V11 全分类，以及真实 Change marker recovery 三项剩余标尺；M4 六项门禁均有充分生产/自动化证据，M4 通过并可进入 M5。注记限于 PRD 状态图同步、错误可观测性和测试粒度/并行时序，不构成 M4 阻断。
