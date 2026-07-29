FAIL

# M3 P1-1/P1-2 第二次定向复审

评审基线：`33973ee`，分支 `docs/issue-167-m3-directed-rereview-after-p1-2b-p1-1b-c-d`。本次只复核上一份 [`2026-07-29-m3-rereview-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-pi-gpt-5.6-sol.md) 的 P1-1/P1-2 是否由 Issue #159/#160/#163/#165（PR #161/#162/#164/#166）关闭；判定基准仍为 [`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、active 的 [`control-plane.md` §4.2–§5.6/§10](../specs/control-plane.md)、[`storage.md` §12.8/§16.1](../specs/storage.md) 与 [`WBS.md` M3](../WBS.md)。四个 Issue 均无评论。

后续增量已经修正生产 run-dir，加入 closed action 名称、pending generation 重派、逐外部步骤 lease 重验、bootstrap digest、permit 同参重试和 prepared-dispatch 复用入口；方向正确。但两个可复现的生产窗口仍会停滞或留下未回收执行体，#166 的新增测试也不是其标题和关闭条件要求的逐阶段崩溃注入。P1-1/P1-2 因而仍不能关闭。

## P1 阻断

### P1-2 仍未关闭：pending 含糊证据无法收敛为 frozen + 可见 Interrupt

可复核证据：

- `internal/daemon/termination.go:95-116` 把损坏、宽权限或其他含糊 bootstrap/control 证据分类为 `Action: "frozen"`，这是 DESIGN §10.1 要求 fail closed 的正确方向。
- 但 `internal/storage/recovery.go:121-125` 的实际动作只写 `isolation_state='frozen'`，没有同时写 `isolation_reason` 与 `isolated_at_ms`。这必然违反 `internal/storage/migrations/0001_initial_schema.sql:204-206` 的 CHECK 及 `0004_m3_resolution_isolation_discussion.sql:108-128` 的 `attempts_isolation_shape_update` trigger，导致 `ApplyStartupRecoveryAction` 返回 SQL 错误、`RecoverStartup` 退出，daemon 无法完成启动恢复。
- 该分支也没有经唯一 `EmitInterrupt` 入口创建可见的 `startup_stall`，没有把 Run 转为 `waiting_human`、扣一次配额并创建可重放 comment operation。因此即使补齐隔离列，仍未达到 Issue #159 的“frozen + 可见 Interrupt”关闭条件和 DESIGN §10.1 的“转人工必须是一次可见打扰”。
- `internal/daemon/termination_recovery_test.go:108-174` 只覆盖“无文件则 redispatch”和“有效 bootstrap 则 reuse”；没有覆盖损坏/宽权限/bootstrap-control 含糊证据并断言一条 Interrupt、完整隔离投影及 barrier 可安全打开。现有测试因此没有执行到上述必错 SQL。

影响：一个真实的损坏 bootstrap 或含糊 control 现场不会静默放开 launch lease，但会令 siftd 每次启动都在恢复阶段失败；它既没有确定性收敛成可操作的人工态，也没有可见打扰。P1-2 的关闭条件仍不成立。

关闭条件：让 `frozen` action 在同一 CAS 端口内落完整隔离投影，并复用唯一 Interrupt 发射事务产生恰好一条 `startup_stall`；增加损坏、宽权限、control-only/其他含糊文件证据测试，断言 barrier 打开前候选已 frozen、Run 为 `waiting_human`、Interrupt/配额/comment operation 完整且幂等。

### P1-1 仍未关闭：prepared dispatch 的 digest 后崩溃窗口永久停滞

可复核证据：

- 首个 worker 在 `internal/launchworker/launch.go:111-118` 依次记录 bootstrap digest、重验 lease、spawn wrapper。若进程在 digest 已提交后、spawn 前崩溃，启动恢复会正确验证并保留该 dispatch。
- 新 lease owner 在 `internal/launchworker/launch.go:70-88` 读取并经 `ResumeLaunchDispatch` 验证 prepared bootstrap；但随后无条件再次调用 `RecordBootstrapDigest`（`:111-113`）。`internal/storage/launch.go:43-51` 只允许 `bootstrap_digest IS NULL` 的 UPDATE，已存在相同 digest 时影响 0 行并返回 `ErrRejectedStaleWorker`。
- 所以“digest 已记录 → spawn 前崩溃”不会复用也不会安全重发；每次 lease reclaim 都在同一点失败。`TestRecoverStartupReusesValidatedPreparedBootstrap` 只断言恢复后 generation/operation/dispatch 未变，没有继续让真实 worker reclaim 并 spawn，因而漏掉该窗口。

影响：这正位于 Issue #160/#163 点名的 prepare → bootstrap → spawn 崩溃链；operation 可永久保持可 reclaim 但不可执行，P1-1 的 crash-safe dispatch 尚未闭合。

关闭条件：使相同 dispatch + 相同 digest 的回填成为提交幂等 no-op（不同 digest仍拒绝），并以真实 worker 覆盖“rename 后、digest 后、spawn 前”各窗口，重启后断言最终恰好一个 wrapper/Agent owner。

### P1-1 仍未关闭：post-spawn 错误没有完成进程组回收

可复核证据：

- `internal/wrapper/wrapper.go:109-132` 在 control rewrite 或 `claim.started` 失败时只对进程组发送一次 `SIGTERM`，随后立即返回；没有等待进程组消失，也没有在 grace 后升级 `SIGKILL`。忽略/延迟 TERM 的 Agent 可在 wrapper 退出后继续写 worktree，违反 DESIGN §8.4 wrapper 契约第 7 条和 Issue #160 的“post-spawn 任意错误/信号下进程组监督与回收”。
- `internal/wrapper/wrapper_integration_test.go` 的 `started` case 使用普通 `/bin/sh`，只数 spawn，不构造忽略 TERM 的子进程并证明 wrapper 返回前整个组已消失；信号路径同样没有这项断言。

关闭条件：wrapper 在 post-spawn 失败与收到终止信号时执行有界 TERM → KILL → wait/reap，并以忽略 TERM 的受控 Agent 断言 wrapper 退出时进程组不再存活。

## #166 崩溃套件不足且不稳定

`internal/launchworker/e2e_crash_test.go:21-117` 确实使用真实 SQLite、control-plane socket、launch worker、编译出的 wrapper 与真实 Agent，这是有价值的 production-shaped happy path；但测试只让 Agent 在正常 acquire → permit → spawn → started 后自行 `SIGKILL`，随后再 tick 一次已成功的 worker。它没有：

- 在 prepare、bootstrap rename、digest、wrapper spawn、control rewrite、started/result 边界杀 worker/wrapper并重启；
- 模拟 permit 响应丢失/重放与暂停旧 owner；
- 重启 daemon 后证明 owner 唯一。

这与 Issue #165 的关闭条件不等价，且 PR #166 body 已诚实注明“逐阶段 crash matrix / permit 丢失交错未齐”。定向稳定性证据本次也未形成：

- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**失败**；`TestLaunchWorkerWrapperCrashSuite` 多次出现 marker 为 0 或 durable phase 仍为 `spawning`，`TestProductionWrapperKeepsAgentInWrapperProcessGroup` 也超时。
- `go test -race ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**失败**；`TestLaunchWorkerWrapperCrashSuite` marker 为 0。
- 单独 `CGO_ENABLED=0 go test -count=10 ./internal/launchworker -run TestLaunchWorkerWrapperCrashSuite`：**失败**；出现 phase 仍为 `spawning`，并有 wrapper 在测试清理 run-dir 后才尝试写 `result.json`，说明测试没有等待/回收真实 wrapper。
- 单独 `CGO_ENABLED=0 go test -count=10 ./internal/wrapper -run 'TestProductionWrapper(CrashWindows|KeepsAgentInWrapperProcessGroup)'`：本次通过；这不能补足 launchworker e2e 的缺口。

## 已确认的增量

- PR #161 将收据 action 收窄为 closed set，并在同一事务中实现 pending generation 递增、旧 operation stale、新 operation 入队；生产 run-dir 已统一为 `runs/<run>/attempts/<attempt>/`。
- PR #162 在 bootstrap 写入与 wrapper spawn 前重验 expected lease，持久化 bootstrap digest；wrapper permit 会以相同 session/candidate/params 重试，Agent 只经相邻 one-shot gate 进入 launcher。
- PR #164 能从文件验证 dispatch secrets/digest，并提供 prepared bootstrap 的复用入口；“digest 已存在时二次回填失败”是剩余的窄窗口，不否定这些增量。
- `converge_operation` 会把不再绑定安全 pending claim 的未完成 operation 标 stale；boot-scoped claim barrier 本身仍有效。

## 执行证据

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**失败**；既知 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 时序 flake 再现；新增 `TestLaunchWorkerWrapperCrashSuite` 也失败（marker 为 0）。
- 上述 `-count=10` 与 `-race` 定向套件：**失败**，详见前节。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 的完整 wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵和 M3 V4 门禁均已保持 `[ ]`；在两个 P1 仍阻断且崩溃套件不稳定时，没有可诚实新增的完成勾选。

## Issue #167 acceptance checklist

- [x] 自行读取 Issue #167 全文与评论，并回溯 #159/#160/#163/#165 及 PR #161/#162/#164/#166；相关 Issue 均无评论。
- [x] 定向复核上一份 FAIL 的 P1-1/P1-2，并核查 #166 作者自述未齐的测试范围。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，给出可复核代码与测试路径。
- [x] 如实区分 production-shaped happy path 与逐阶段端到端崩溃注入，不把 #166 称为完整关闭证据。
- [x] 仅在 issue worktree 工作；未修改实现代码，未推送、未合并。

## 结论

**FAIL。** P1-2 的正常 redispatch/reuse 路径已有实质收敛，但含糊证据的 frozen action 违反数据库隔离约束且没有可见 Interrupt；P1-1 则仍有“digest 已记录、spawn 前崩溃”的永久停滞窗口和 post-spawn TERM-only 回收缺口。#166 不是逐阶段 crash matrix，且在 `-count=10`/`-race` 下不稳定。修正这些生产窗口并补足真实崩溃/响应丢失交错证据前，P1-1/P1-2 与 M3 V4 门禁不得勾选。
