FAIL

# M3 P1 第七次定向复审

评审基线：`a265fc9`，分支 `docs/issue-197-m3-directed-rereview-7-after-p1-1g5`。本次按 Issue #197 只复核上一份 [`2026-07-29-m3-rereview-6-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-6-pi-gpt-5.6-sol.md) 的残余 P1-1g 是否由 #195（PR #196）关闭，并确认 P1-1f3/P1-2c/P1-1e 未回退；关闭标尺采用 Issue #195 继承自 #190 的四项硬条件。Issue #197 的 conductor 评论指定了当前 worktree、分支、模型和基线；Issue #195 无评论，PR #196 有四条 CI 修复评论。

PR #196 已把同步从轮询文件推进为 execution wrapper 内的精确 hook，并新增 Linux 真实 `SIGSTOP`、生产 `RecoverStartup`、新 boot 候选 worker 与 phase/liveness 断言。新 suite 在 Linux `-count=10` 通过，目标五包在 Darwin/Linux 的 `-count=10` 与 race 也通过，原 marker 未再复现。因此同步点、暂停对象、生产恢复接管尝试和稳定性已有足够关闭证据。

但 #195 明列的第三项仍未满足：全局 spawn 日志保存的是 outer supervisor PID，持久投影保存的是 execution wrapper PID/PGID，新测试只数 spawn 日志行数，从未读取、关联或探测 outer PID；Agent 也只在一个窗口探测。上一轮指出的“outer supervisor、execution wrapper、Agent 身份未关联”因而仍然成立。三面证据尚不能按硬条件共同给出完整身份区间，P1-1g5 不能关闭。

## 已关闭的子条件

### 精确同步点与真实旧 owner 暂停

- `internal/wrapper/wrapper.go:120-125,176-181,195-204` 在 control 初写后/permit RPC 前、Agent control 补写后/`claim.started` RPC 前、`result.json` rename 前后设置 wrapper 内同步点；它们不再依赖同一路径的异步轮询来猜边界。
- `internal/wrapper/testhook.go:10-25` 由 execution wrapper 自身写 ready 后反复向自己发 `SIGSTOP`；`internal/daemon/paused_wrapper_recovery_test.go:83-94,216-232` 等到 procfs 状态 `T`，并分别断言 `starting`、`spawning`、`running`。因此暂停对象是持久 owner 对应的 execution wrapper，而不是 launch worker 的 `Backend.Spawn` 调用栈。
- `internal/daemon/paused_wrapper_recovery_test.go:31-39` 的四项覆盖 control 初写、control 补写/started 前、started 后且 result rename 前、result rename 后；后两项在 `running` phase 上建立了 `claim.started` 提交后的事实。

### 生产 recovery 接管尝试

- `internal/daemon/paused_wrapper_recovery_test.go:96-121` 创建新 boot 后调用生产 `TerminationCoordinator.RecoverStartup`，再打开恢复屏障并运行候选 `launchworker.Worker`；不再由测试直接写 `supervise` receipt。
- 恢复前后都探测旧 execution wrapper PID/PGID；Agent 存活窗口中也探测 Agent 仍在同一 PGID。候选 backend 调用数为 0、全局 spawn 行数仍为 1、持久 owner 数仍为 1，证明生产恢复在旧 execution owner 存活时保留监督而未启动 replacement。
- PR #196 同时让 wrapper 持久身份与 Linux procfs Inspector 使用同一 executable/start-time 口径，并在 started 仲裁写入 `heartbeat_at_ms`，因此 starting/spawning 与 heartbeat 新鲜的 running owner 均能走生产 `ownerIsLive` 分支，而不是因测试身份失配误入终止/重派。

### marker 与压力稳定性

- PR #196 删除了 post-spawn helper 中永久阻塞 `Backend.Spawn` 的 shim，精确暂停改由 wrapper hook 承担；原 `TestLaunchWorkerWrapperCrashSuite` 在本轮独立 `-count=20` 和五目标包组合 `-count=10` 中均通过。
- Linux 容器内精确暂停 suite 独立 `-count=10` 通过；带 init/reaper 的 Linux 目标五包 race 通过。此前 marker 为 0 的反例本轮没有复现。

## P1 阻断

### P1-1g5 未关闭：outer / execution / Agent 三面身份区间仍未关联

#195 的硬条件不是只证明“一个持久 owner + 一个 adapter 调用”，而是关联 outer supervisor、execution wrapper、Agent 的 PID/PGID，并以全局 spawn、持久投影和实际存活区间共同证明至多一个 owner。当前测试仍缺以下证据：

1. `pausedRecoveryBackend.Spawn` 在 `internal/daemon/paused_wrapper_recovery_test.go:151-171` 启动的是 `cmd/sift-agent-wrapper` 的 **outer supervisor**，spawn 文件记录 `cmd.Process.Pid`。生产 `wrapper.Run` 随后另起 `--run` execution wrapper；DB 的 `wrapper_pid/wrapper_pgid` 是后者。
2. `pausedIdentity` 在 `internal/daemon/paused_wrapper_recovery_test.go:190-204` 只读取 DB execution wrapper 与 Agent PID；`lineCount(spawns)` 只数换行。测试从未解析 outer PID，未断言它与 DB execution PID 不同且具有父子/监督关系，也未在恢复前后 probe outer PID 的存活区间。
3. Agent 实际 PID/PGID 只在 `control-rewrite` 场景检查；control-initial 尚未 spawn Agent 属合理空区间，但 result 两侧只把已退出 Agent 标成 `agentLive=false`，没有记录其已存在后消失的区间。replacement 侧只以 backend 调用数 0 表示空区间，也没有形成旧 outer/execution/Agent 与新 owner 空区间的统一身份记录。
4. `owners == 1` 仍只统计单 attempt 上 `wrapper_instance_id IS NOT NULL`；它不能补足缺失的 outer OS 身份。因而本轮能证明“一个 spawn 日志行 + 一个持久 execution owner + 特定窗口的 execution/Agent 存活”，不能证明 #195 要求的三类进程身份关联及完整存活区间。

关闭条件：在 paused recovery matrix 中读取全局 spawn 日志的 outer PID，关联并分别探测 outer supervisor、DB execution wrapper PID/PGID 与 Agent PID/PGID 在恢复前后及清理后的区间；对 Agent 未启动/已退出和 replacement 未签发/未启动给出显式空区间断言，并把这些 OS 事实与持久 owner/claim/permit 投影共同核对。不得仅以 spawn 行数或 backend 调用数代替身份关联。

## P1-1f3/P1-2c/P1-1e 回归确认

- **P1-1f3 未回退**：permit-loss 与三组 reaper/TERM 定向测试 `-count=10` 通过；Linux 带 init 的目标 race 中 wrapper 包通过。裸容器 PID 1 不回收孤儿时组探测会把 zombie 视为仍存在，增加 `--init` 后通过，这是容器执行环境注记，不是本轮代码回退证据。
- **P1-2c 未回退**：`TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier` 独立 `-count=10` 通过；frozen 候选仍在 barrier 前产生完整 `startup_stall` 投影。
- **P1-1e 未回退**：`TestRecordBootstrapDigestIsIdempotentForSameDispatch` 独立 `-count=10` 通过；五目标包组合压力亦通过。

## 执行证据

以下命令均从当前 issue worktree 发起：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**失败一次**，仍是既知 `TestDoctorBaselineChecksConfiguredDependencies` fixture 超时并得到 `signal: killed`；随后 `CGO_ENABLED=0 go test -count=10 ./internal/controlplane` 通过。该注记不改变本轮身份硬条件的 FAIL。
- Darwin：`CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**；`CGO_ENABLED=0 go test -race -count=1` 同五包：**通过**；marker suite 独立 `-count=20`：**通过**。
- Linux/arm64 容器：`TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner -count=10`：四窗口十轮全部**通过**；目标五包 `-count=10`：**通过**。
- Linux/arm64 容器带 `--init`：目标五包 `-race -count=1`：**通过**。首次不带 init 的 race 因容器 PID 1 不回收被杀子进程而在组消失断言失败；带 reaper 后通过。
- P1-1f3 四组定向测试、P1-2c、P1-1e 各自 `-count=10`：**通过**。
- PR #196 合入前最终 CI：vet/test、schema drift、Darwin/Linux amd64/arm64 build 全部通过；Linux CI 对新 suite 有一次非跳过执行。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 完整 wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵和 M3 V4 门禁继续保持 `[ ]`。即使补齐本轮窄 P1-1g 身份证据，WBS 已记录的 pending re-dispatch、backend session liveness、生产 `ProcessGroupVerified` 等更大范围缺口仍不允许勾选 M3 V4；本轮 P1-1g5 本身也尚未关闭。

## Issue #197 acceptance checklist

- [x] 自行读取 Issue #197 全文、Agent 建议、conductor 评论、#193/#190 关闭标尺，并回溯上一份 FAIL、#195 与 PR #196 全部评论。
- [x] 定向复核精确 sync 点、真实 execution wrapper `SIGSTOP`、生产 recovery 接管尝试、三面身份区间与 marker/`-count=10` 稳定性。
- [x] 确认 P1-1f3/P1-2c/P1-1e 的实现与定向回归状态。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，给出代码、测试、CI 与命令证据。
- [x] 诚实核对 WBS；仅在指定 issue worktree 工作，未修改实现代码。

## 结论

**FAIL。** PR #196 已关闭上一轮的同步点、暂停对象与生产恢复旁路问题，且 marker、Linux 精确 suite、目标组合 `-count=10`/race 本轮稳定；P1-1f3/P1-2c/P1-1e 未回退。但全局 spawn 面仍只保存并计数 outer supervisor PID，持久面只读取 execution wrapper，测试没有关联或探测 outer supervisor，也没有形成三类进程及 replacement 空区间的完整身份时间线。该项是 #195 明列的硬性条件，不能降为 NOTE；补齐前 P1-1g5、M3 V4 与 M3 门禁不得勾选。
