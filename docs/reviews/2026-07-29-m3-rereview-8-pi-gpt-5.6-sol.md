PASS WITH NOTES

# M3 P1 第八次定向复审

评审基线：`e7041db`，分支 `docs/issue-201-m3-directed-rereview-8-after-p1-1g6`。本次按 Issue #201 只复核上一份 [`2026-07-29-m3-rereview-7-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-7-pi-gpt-5.6-sol.md) 的残余 P1-1g5/g6 是否由 #199（PR #200）关闭，并确认 P1-1f3/P1-2c/P1-1e、精确同步、真实 `SIGSTOP`、生产恢复与 marker 稳定性未回退。Issue #201 的 conductor 评论指定了当前 worktree、分支、模型与基线；Issue #199 有一条 conductor 评论，PR #200 无讨论评论，合入 CI 六项全绿。

PR #200 补齐了上一轮唯一硬阻断：Linux paused recovery matrix 现在读取全局 spawn 日志中的 outer supervisor PID/PGID，把它与持久 execution wrapper、Agent 身份和父子/进程组拓扑关联，并在恢复前、生产恢复及候选 worker 运行后、清理后核对存活或空区间；同一断言同时检查 owner/claim/session/permit 投影。Linux 新 suite `-count=10`、目标五包 Darwin/Linux 的标准 `-count=10` 与 race 均通过。因此 P1-1g6 关闭，本轮没有继续阻断该窄关闭标尺的 P1。

本轮有两个非阻断注记。第一，已退出 Agent 的历史 PGID 由 execution PGID 构造而非从 `control.json` 读取；当前 matrix 仍以 live Agent 窗口实测同组、以持久 Agent PID + absence 证明 result 两侧的“曾存在后消失”，足以满足本次窄关闭标尺，但若测试拓扑未来可变，宜把 control 中完整 Agent identity 纳入历史断言。第二，在同时并发运行全仓测试、五包 `-count=10` 与五包 race 的高负载复审方式下，旧 launch marker 等待曾超时；同一标准命令单独重跑全绿，marker suite 独立 `-count=20` 全绿，Linux 组合也全绿，且 PR #200 未改 launchworker/wrapper 生产或 marker 代码。因此记为既有时间预算敏感性，不判定为本次回退。

## P1-1g6 关闭证据

### outer / execution / Agent 三面身份关联

- `internal/daemon/paused_wrapper_recovery_test.go:88-103` 从 spawn 文件读取 outer 身份，读取 DB execution/Agent 身份，断言 outer 与 execution PID/PGID 均不同、两者存活状态正确，并从 `/proc/<execution>/stat` 断言 execution 的 parent 正是 outer。
- `pausedSpawnIdentity` 不再只数日志行：它解析唯一 outer PID并调用 `Getpgid`；`pausedIdentity` 从 attempts 投影读取 execution PID/PGID 与 Agent PID。Agent 已启动且 DB 尚未接收 started 的窗口从 `control.json` 取得 PID，随后 `assertPausedAgentInterval` 用 OS `Getpgid` 证明 live Agent 属于 execution 进程组。
- 四窗口保持上一轮的精确边界：permit RPC 前 Agent 为空；started RPC 前 Agent 已存活；result rename 前后 Agent 已退出。因而 Agent 的未启动空区间、live 区间和曾登记后消失区间均有显式分支，而非由单个 backend 计数代替。

### 恢复前后、replacement 空区间与清理

- `internal/daemon/paused_wrapper_recovery_test.go:105-137` 调生产 `TerminationCoordinator.RecoverStartup`，完成恢复屏障后运行候选 launch worker；随后断言 spawn 文件仍恰有一个 outer、候选 backend 调用为 0，显式形成 replacement outer 空区间。
- 候选运行后再次探测旧 outer 存活、旧 execution 仍为 stopped 且存活、按场景探测 Agent live/absent，并重跑持久投影核对；这覆盖“生产恢复没有签发并启动第二 owner”，不是仅在恢复前取一次快照。
- `internal/daemon/paused_wrapper_recovery_test.go:139-155` 清理 execution 进程组与 outer 后分别等待 execution、outer、已记录 Agent 消失，并再次保持 replacement 为空。

### OS 事实与持久投影共同核对

- `assertPausedProjection` 断言全 Run 只有一个带 `wrapper_instance_id` 的 owner，并 join `attempts`/`attempt_claims` 核对 DB execution PID/PGID、instance、session 与各边界应有的 permit/Agent 投影。
- permit 前明确要求 permit、持久 Agent、观测 Agent 全空；permit 后要求 permit 已持久；started 后或 result 窗口把持久 Agent PID与观测身份对齐。结合唯一 outer spawn、outer→execution 父子关系、Agent 同组/消失事实及 replacement 空区间，足以共同证明 matrix 内至多一个 owner。

## 未回退确认

- **精确 sync / 真实 SIGSTOP / 生产 recovery**：四个 wrapper 内 hook 场景在 Linux 新 suite `-count=10` 全部通过；execution wrapper 的 `/proc` 状态仍严格为 `T`，恢复仍经生产 `RecoverStartup` 与真实候选 worker。
- **P1-1f3**：permit-loss 与 TERM/reaper 定向组 `-count=10` 通过；目标五包 Darwin/Linux race 通过。
- **P1-2c**：`TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier -count=10` 通过。
- **P1-1e**：`TestRecordBootstrapDigestIsIdempotentForSameDispatch -count=10` 通过。
- **marker**：`TestLaunchWorkerWrapperCrashSuite -count=20` 独立通过；标准五包组合在 Darwin 单独重跑及 Linux `--init` 下 `-count=10` 通过。高并发复审负载下的超时详见注记，不把它误写成从未出现。

## 执行证据

以下命令均从 Issue #201 指定 worktree 发起：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：高并发执行时**失败**于既知 doctor fixture `signal: killed`，并同时出现一次 launch marker 超时；`go test -count=10 ./internal/controlplane -run '^TestDoctorBaselineChecksConfiguredDependencies$'` 随后通过。
- Darwin 标准单独重跑：`CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**；同五包 `CGO_ENABLED=1 go test -race -count=1`：**通过**。
- Darwin marker suite：`CGO_ENABLED=0 go test -count=20 ./internal/launchworker -run '^TestLaunchWorkerWrapperCrashSuite$'`：**通过**。
- Linux/arm64 Docker `--init`：`TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner -count=10`：**通过**；目标五包 `-count=10`：**通过**；目标五包 race：**通过**。
- P1-1f3 定向 wrapper 组、P1-2c 与 P1-1e 各自 `-count=10`：**通过**。
- PR #200 合入 CI：vet/test、schema drift、Darwin/Linux amd64/arm64 build 全部**通过**。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 完整 backend/wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵及 M3 V4 门禁继续保持 `[ ]`。本轮只关闭 paused recovery matrix 的 P1-1g6 证据缺口；pending re-dispatch、backend session liveness、生产 `ProcessGroupVerified` 和 DESIGN §10.1 其余行仍不能由这份窄测试代替。

## Issue #201 acceptance checklist

- [x] 自行读取 Issue #201 全文、Agent 建议、conductor 评论，并回溯上一份 FAIL、Issue #199 与 PR #200 的描述、评论、变更和 CI。
- [x] 定向复核 P1-1g5/g6 三面身份区间、持久投影和“至多一个 owner”关闭标尺。
- [x] 确认 P1-1f3/P1-2c/P1-1e、精确 sync、SIGSTOP、生产 recovery 与 marker 的实现和定向回归状态。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，给出代码、测试、CI 与命令证据。
- [x] 诚实核对 WBS；仅在指定 issue worktree 工作，未修改实现代码。

## 结论

**PASS WITH NOTES。** PR #200 已把 outer supervisor、持久 execution wrapper 与 Agent 的身份/拓扑、恢复前后存活或空区间、清理后消失，以及 owner/claim/session/permit 投影放进同一 Linux paused recovery matrix；上一轮唯一 P1-1g5/g6 硬阻断因此关闭。P1-1f3/P1-2c/P1-1e 与同步、SIGSTOP、生产恢复均未回退。已退出 Agent 的历史 PGID取证仍可更直接，高并发复审负载也再次暴露旧 marker/doctor 时间预算敏感性；两者在本轮标准与定向复跑全绿、且不破坏窄安全证明，故记 NOTE，不扩大为完整 M3 V4 通过或误勾 WBS。
