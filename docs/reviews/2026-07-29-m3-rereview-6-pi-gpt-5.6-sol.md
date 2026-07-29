FAIL

# M3 P1 第六次定向复审

评审基线：`a5ed126`，分支 `docs/issue-193-m3-directed-rereview-6-after-p1-1g4`。本次按 Issue #193 只复核上一份 [`2026-07-29-m3-rereview-5-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-5-pi-gpt-5.6-sol.md) 的残余 P1-1g3/g4 是否由 #190（PR #192）关闭，并确认 P1-1f3/P1-2c/P1-1e 未回退；判定基准仍为 [`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、active 的 [`control-plane.md`](../specs/control-plane.md)、[`storage.md`](../specs/storage.md) 与 [`WBS.md` M3](../WBS.md)。Issue #193/#190 均无评论。

PR #192 新增了真实 OS wrapper 启动后的四个场景、跨进程 spawn 日志、DB wrapper PID/PGID 探测和一个新 boot 下的候选 worker；这是比 spawn 前 hook 更接近目标的增量。但测试没有把 control 初写/补写、started 前后的窗口同步住，所谓 pause-old-owner 实际暂停的是 launch worker 的测试 backend，而不是已获 permit 的 wrapper owner；恢复方也由测试直接写入 `supervise` receipt，并未走会尝试替换 owner 的生产恢复路径。实际进程身份只覆盖旧 execution wrapper 的 PID/PGID，没有覆盖 Agent 与新 owner 的存活区间。更直接地，#190 明列必须消除的 marker flake 在本轮组合 `-count=10` 和独立 `-count=20` 中均再次复现。因此 P1-1g4 与 M3 V4 仍不能关闭。

## P1 阻断

### P1-1g4 未关闭：边界、旧 owner 交错与三面身份仍不构成硬性证据

已交付的有效增量：

- `internal/launchworker/e2e_crash_test.go:211-346` 确实先启动编译后的生产 wrapper，再对其持久 wrapper identity 所在进程组发真实 `SIGKILL`；不再把 `beforeSpawn` hook 称作 post-spawn。
- `internal/launchworker/e2e_crash_test.go:283-287,362-374,414-425` 从 SQLite 取得旧 execution wrapper 的 PID/PGID，验证非 result 场景下该身份仍对应真实进程组，并在 replacement worker 前等待组消失。
- `internal/launchworker/e2e_crash_test.go:341-345,598-604` 用跨进程追加文件加 replacement backend 调用计数断言没有第二次 backend spawn。
- 原 marker 等待从“文件出现后立即采样”改成等待一行持久副作用，修掉了原先那一个窄采样竞态。

仍不满足 #190 的四项硬性关闭条件：

1. **四个名字不是四个可证明的 crash window。** `waitForInitialControl` 在 `e2e_crash_test.go:376-379` 只等路径存在，无法区分首次 control 与 Agent identity 补写后的同一路径；`waitForAgentControl` 在 `381-395` 轮询补写后的文件，但 wrapper 紧接着就发 `claim.started`，测试没有在 RPC 前阻塞，也没有断言 kill 时 phase 仍为 `spawning`。因此 `control-initial-write`、`control-agent-rewrite` 和 `claim-started` 可以全部实际落在 started 已提交之后。`waitForResult` 同样只有“文件已存在”点；矩阵没有分别同步并杀在 control 初写/补写、started 提交前后、result rename 前后的两侧。
2. **pause-old-owner 场景暂停错了对象。** helper 在 `e2e_crash_test.go:466-470` 设置 `backend.block=true`，而 backend 在 `587-611` 启动 outer wrapper、写 ready 后卡在 `select {}`，所以被暂停的是 launch worker 的 `Backend.Spawn` 调用栈；已取得 permit 的 execution wrapper/Agent 没有收到 `SIGSTOP`，仍继续运行。代码注释所称“Backend.Spawn has returned”也不成立。新 boot 又通过 `completeLaunchRecovery` 在 `479-503` 直接提交测试指定的 `supervise` action；候选 `RunOnce` 看到 acquire 已收敛为 succeeded 的 operation 而无 work 可 claim。`candidate.spawns==0` 因而不能证明“生产恢复尝试接管但被旧 owner 存活证据挡住”。
3. **三面 owner 证据仍不完整。** spawn 文件记录的是 backend 启动的 outer supervisor PID，DB identity 是其另建进程组中的 execution wrapper PID/PGID；测试没有把二者及 Agent PID/PGID 关联起来，也没有探测 Agent 的存活区间。replacement 侧只有 adapter 调用数为 0，因此没有新 wrapper/Agent 的实际身份区间。`assertSingleLaunchOwner` 仍只数单 attempt/claim 表中非空行。现有证据可说明“一个旧 wrapper 记录 + 没有第二次 adapter 调用”，尚不能按关闭条件从全局 spawn、持久投影、旧/新 wrapper/Agent 实际 PID/PGID 区间三面共同证明至多一个 owner。
4. **marker flake 未消除，组合稳定性硬条件直接失败。** `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage` 在 `TestLaunchWorkerWrapperCrashSuite` 得到 `agent-started` 行数 0；随后独立 `CGO_ENABLED=0 go test -count=20 -run '^TestLaunchWorkerWrapperCrashSuite$' ./internal/launchworker` 又失败两轮，同样在 `e2e_crash_test.go:96` 超时后为 0。把轮询目标从文件存在改成一行没有消除 Agent 偶尔根本未在 5 秒内产生 marker 的底层时序问题。race 组合通过不能抵消明确的 `-count` 反例。

关闭条件：在生产路径可观测的同步点分别杀在 control 初写/补写、`claim.started` 提交前后、`result.json` rename 前后；permit 后真实暂停 execution wrapper owner（而非 launch worker/backend），让生产 recovery/termination 路径实际尝试接管并证明旧 wrapper/Agent 组消失前不会签发或启动新 owner；关联 outer wrapper、execution wrapper 与 Agent 的 PID/PGID，并记录旧/新 owner 存活区间；修复 marker 根因，使目标包组合 `-count`/`-race` 稳定。

## P1-1f3/P1-2c/P1-1e 回归确认

- **P1-1f3 实现未回退**：PR #192 只改测试文件，#187 的监督/reaper 生产代码未变。四组 reaper/permit-loss 定向测试独立 `-count=10` 通过。它们与其他压力命令并行时曾有一轮 `TestProductionWrapperReapsTERMIgnoringGroupAfterStartedFailure` 在 5 秒内未发布 identity；独立重跑通过，作为时序稳定性注记，不改变本轮由 P1-1g4 明确阻断的结论。
- **P1-2c 未回退**：`TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier` 独立 `-count=10` 通过；frozen 候选仍在 recovery barrier 打开前产生完整 `startup_stall` 投影。
- **P1-1e 未回退**：`TestRecordBootstrapDigestIsIdempotentForSameDispatch` 独立 `-count=10` 通过；prepare/rename/digest/spawn 前真实 worker kill/restart与新增 post-spawn suite 合跑 `-count=10` 也有一次完整通过。

## 执行证据

以下命令均在当前 issue worktree 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**失败一次**，既知 `TestDoctorBaselineChecksConfiguredDependencies` fixture 超时得到 `signal: killed`；其余包（含 launchworker/wrapper）在该轮通过。
- 新 post-spawn suite 单次：**通过**；与原两个 launch crash suites 合跑 `-count=10`：**通过**；同三组 race：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**失败**，`TestLaunchWorkerWrapperCrashSuite` marker 为 0；其余四包通过。
- `go test -race -count=1 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- `TestLaunchWorkerWrapperCrashSuite` 独立 `-count=20`：**失败两轮**，marker 为 0。
- P1-1f3 四组 wrapper 定向测试独立 `-count=10`：**通过**；P1-2c 与 P1-1e 定向测试各自 `-count=10`：**通过**。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 完整 wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵和 M3 V4 门禁均保持 `[ ]`。PR #192 的测试增量不足以勾选这些完整项。

## Issue #193 acceptance checklist

- [x] 自行读取 Issue #193 全文与评论，并回溯上一份 FAIL、#190 与 PR #192；相关 Issue 均无评论。
- [x] 定向复核 P1-1g3/g4 的 post-spawn kill matrix、pause-old-owner、三面身份与稳定性硬条件。
- [x] 确认 P1-1f3/P1-2c/P1-1e 的实现与定向回归状态。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，给出代码、测试与命令证据。
- [x] 诚实核对 WBS；仅在 issue worktree 工作，未修改实现代码，未推送、未合并。

## 结论

**FAIL。** PR #192 已把测试从 spawn 前 hook 推进到真实 wrapper 已启动后的 OS 进程组，但没有建立精确的 control/started/result 两侧同步点；pause-old-owner 实际暂停 launch worker backend，候选新 boot 也没有经过生产恢复接管判定；旧/新 wrapper/Agent 的完整 PID/PGID 存活区间仍缺。最关键的是 #190 要求消除的 marker flake在组合 `-count=10` 与独立 `-count=20` 中再次复现。P1-1f3/P1-2c/P1-1e 实现未回退；补齐真实交错与身份证据并稳定压力运行前，P1-1g4、V4 与 M3 门禁不得勾选。
