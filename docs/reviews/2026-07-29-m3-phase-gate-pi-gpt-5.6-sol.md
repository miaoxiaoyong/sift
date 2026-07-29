PASS WITH NOTES

# M3 阶段门禁裁决

评审基线：`65b17f1`，分支 `docs/issue-205-m3-phase-gate-after-p1-pass-with-notes`，Issue #205 指定 worktree。本文是正式 **M3 phase gate**，不是第九轮 P1-1g 窄复审。判定基准为 [`WBS.md` M3 与验收权威表](../WBS.md)、[`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md) 及 [M3 P1 第八次定向复审](2026-07-29-m3-rereview-8-pi-gpt-5.6-sol.md)。

## 裁决

**允许 M3 以 PASS WITH NOTES 结案并进入 M4。**

理由不是把未完成项降级，而是 WBS 已明确把 V4 分阶段：验收权威表规定 **M3 首跑 process 段，M6 以双后端完整矩阵最终闭合，M7 形成真实 Agent/version 资格证据**。因此 M3 的退出谓词是 process Runtime 的生产链可运行，且未确认旧执行体消失时 fail closed、可见、隔离、不产生第二 owner；它不是提前完成 M6 的 tmux/backend-session 行或 M7 的真实资格矩阵。

前序 phase review 的两个 P1——生产 launch/wrapper 链缺失、恢复屏障过早开放——及随后逐轮发现的 dispatch 幂等、进程组回收、permit-loss、paused-owner 交错和三面身份取证，均已在 P1 关闭链中落实。第八次定向复审在真实 `SIGSTOP` 的四个 wrapper 边界，以 outer supervisor、持久 execution wrapper、Agent 的 OS 身份区间和 owner/claim/session/permit 投影共同证明生产 `RecoverStartup` 不重叠启动 replacement；本次标准与定向回归未发现新的 M3 P1。

## M3 退出证据

### 生产 Runtime 与 handoff

- `internal/launchworker/launch.go:55` 是持久 `launch_agent` operation 的生产消费者；bootstrap 准备、digest、lease 与 spawn 都走存储 CAS。
- `internal/wrapper/wrapper.go:30,80` 是生产 supervisor/execution 状态机：读取并删除 bootstrap，经 RPC 完成 acquire/permit/started，写 control/heartbeat/result，并监督 Agent 进程组。
- `internal/runtime/runtime.go` 的 `ProcessBackend` 只启动 wrapper，`Launcher`/`DirectLauncher` 是唯一 Agent spawn 接缝；Agent 环境只含非机密 `SIFT_RUN_DIR`。
- `internal/runtime/handoff.go:15-48` 的 one-shot permit gate 与存储/RPC 的 session、permit、generation 检查共同阻止 permit 响应重放二次 spawn。`TestHandoffPermitReplayAndStartedEvidence`、`TestProductionWrapperReplaysLostPermitResponseWithSameParameters`、`TestLaunchWorkerWrapperCrashSuite` 覆盖该链。

因此 WBS §3.1/§3.2 原先因生产链 P1 而保留的 `[ ]` 已不再反映当前代码，现改为 `[x]`；这不连带声称完整恢复矩阵完成。

### 恢复屏障、旧 owner 与可见冻结

- `cmd/siftd/main.go` 在 `RecoverStartup` 完成后才调用 `CompleteStartupRecovery`，之后才装配 launch worker。
- `internal/daemon/termination.go:52-103` 扫描 attempt/launch-operation 并集；每个候选经 `ApplyStartupRecoveryAction` 的 CAS 分类后才允许打开屏障。
- `internal/daemon/termination.go:107-134` 与 `internal/storage/recovery.go` 已实现 pending prepared-dispatch 复用、fencing generation 递增和 re-dispatch；`TestRecoverStartupRedispatchesPendingAttemptBeforeOpeningBarrier` 与 `TestRecoverStartupReusesValidatedPreparedBootstrap` 直接覆盖。WBS 中“pending re-dispatch 仍留 M6”的旧注记属于事实性过期，已修正。
- `TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier` 证明含糊证据在屏障前形成隔离、`waiting_human`、唯一 `startup_stall` 与可重放发布 operation，而不是只写分类收据。
- `TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner` 在 permit 前、started 前、result rename 前后暂停旧 execution wrapper；恢复前后及清理后核对 outer/execution/Agent 身份、进程组和 replacement 空区间。第八次定向复审已按既定关闭标尺判定该 P1 为关闭，本次 Darwin/Linux 定向重跑继续通过。

### 资格门控的 M3 语义

生产 `cmd/siftd/main.go` 尚未注入 `ProcessGroupVerified`，不是 M3 可以忽略的成功路径，而是已实现的保守门控：未取得 Agent/version 资格时，进程组消失不被当作执行体消失的充分证明；恢复、超时、kill/retry 都冻结并发一次 `startup_stall`，不自动 retry。`internal/daemon/termination.go:336-363` 和 `TestTerminationUnconfirmedFreezesAndMakesStartupStallVisible` 锁定该行为。

按 WBS R11、V4 权威表与 M6/M7 工作包，真实资格真值和脱组构造分别在 M6/M7 闭合；M3 要求的是默认 unverified 时禁止危险自动恢复。故它是本阶段通过的安全门控，不是 P1。

## 明确保留到后续片的 NOTE

以下项目仍为 `[ ]`，但不是 M3 退出 P1：

1. **DESIGN §10.1 / V4 完整双后端矩阵**：tmux backend、backend session liveness 及两个后端同套断言在 WBS M6 最终闭合。当前不得称为“完整 V4 通过”。
2. **生产 `ProcessGroupVerified` 真值与真实 Agent/version 拓扑资格**：按 WBS R11/V4/M7 闭合；在此之前生产路径保持 fail closed。
3. **§3.7 确认消失后的生产自动分诊**：存储端口与测试已存在，但资格真值未接前生产不可达，随上项闭合。
4. **§3.8 hooks baseline 写入与 Agent 结束后自动复核**：当前 doctor 能报告 baseline absent/漂移，但生产 writer 未接；V5b 硬护栏按 WBS 留 M4。
5. **完整 V2 与 M5 人工 `startup_stall` retry/reject/hold 两段式**：验收权威表分别留 M6/M5，M3 只要求发射、隔离和事实仲裁核心。

这些归属来自现行 WBS；本次没有把已 defer 到 M6/M7 的工作重新列为 M3 硬阻断。

## 执行证据

均从 Issue #205 指定 worktree、基线 `65b17f1` 发起：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：除既知 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 共享 deadline 下 fixture `signal: killed` 外全部通过；该测试随后 `CGO_ENABLED=0 go test -count=10 ./internal/controlplane -run '^TestDoctorBaselineChecksConfiguredDependencies$'`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- `CGO_ENABLED=1 go test -race -count=1 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- Linux Docker `--init`：`CGO_ENABLED=0 go test -count=10 ./internal/daemon -run '^TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner$'`：**通过**。

全仓标准命令仍会偶发 doctor 时序失败，故保留 NOTE，不虚报全绿；它与 M3 Runtime 生产链及单-owner 安全证明无代码交集，独立十轮通过，也与历次记录一致，不升为 M3 P1。

## WBS / AGENTS 对账

- WBS §3.1/§3.2 改为 `[x]`，移除“生产 wrapper 仍是 stub”的过期描述。
- WBS 修正 pending re-dispatch 已实现的事实；§3.4 完整矩阵仍保持 `[ ]`。
- M3 V4 **首跑 process 段**门禁改为 `[x]`，同时明示完整双后端 V4 仍在 M6；M4 前置据此改为 `[x]`。
- `AGENTS.md` 更新为本阶段门禁 PASS WITH NOTES，并保留“不得描述为完整 V4/M6 通过”的边界。

这不是修改验收标准来迁就实现：WBS 的验收权威表在本次之前就已规定 V4 的 M3 首跑、M6 最终闭合和 M7 真实资格。本次只是让 M3 门禁勾选和局部任务状态与该既有分阶段规则及当前实现一致。

## Issue #205 acceptance checklist

- [x] 自行读取 Issue #205 全文、Agent 建议、acceptance/约束及 conductor 评论。
- [x] 以正式 M3 phase gate 裁决，而非重复 P1-1g 窄复审。
- [x] 回答是否可 PASS WITH NOTES 进入 M4，并以代码、测试与命令给出依据。
- [x] 核对仍未完成项；未把 M6/M7 defer 项重写为 M3 P1。
- [x] 在指定路径产出本报告并诚实修正 WBS/AGENTS。
- [x] 只在 Issue #205 worktree 与约定分支工作；未 push、未合并。

## 结论

**PASS WITH NOTES。** M3 process Runtime 的生产启动链、handoff、恢复屏障、受控终止 fail-closed 路径、可见隔离及 paused-owner 单一性证据已达到进入 M4 的安全门槛；当前没有剩余 M3 P1。M3 门禁可以关闭，M4 可以开始。完整 DESIGN §10.1/V4 双后端矩阵、生产资格真值、hooks 自动复核与 doctor 时序敏感性继续按上述归属跟踪，任何后续文档均不得把本结论扩写为完整 V4、M6 或 PoC 发布验收通过。
