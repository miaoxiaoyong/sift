FAIL

# M3 P1 第五次定向复审

评审基线：`ab76c25`，分支 `docs/issue-189-m3-directed-rereview-5-after-p1-1f3-p1-1g3`。本次按 Issue #189 只复核上一份 [`2026-07-29-m3-rereview-4-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-4-pi-gpt-5.6-sol.md) 的残余 P1 是否由 #185/#186（PR #187/#188）关闭，并确认 P1-2c/P1-1e 未回退；判定基准仍为 [`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、active 的 [`control-plane.md`](../specs/control-plane.md)、[`storage.md`](../specs/storage.md) 与 [`WBS.md` M3](../WBS.md)。Issue #189/#185/#186 均无评论。

PR #187 已使组外 reaper 的启动后结果可被监督进程观察，并让生产 wrapper 只在收到完成确认后返回；P1-1f3 关闭。PR #188 把 prepare/rename/digest/调用 backend 前四个 worker 窗口从普通 Go error 提升为真实 SIGKILL/restart，并加入 permit 响应丢失同参重放，是有效增量。但它没有覆盖 wrapper 的 control/started/result 杀进程恢复，也没有 permit 后暂停旧 owner 的换 owner 交错；所谓 `spawn` 同步点实际位于 `Backend.Spawn` 调用之前。三面 owner 证据仍没有跨崩溃观测实际进程身份，且原 launch e2e 在本次定向 `-count=10` 中再次出现一轮 marker 为 0。PR #188 body 也如实声明这些缺口。P1-1g3 与 M3 V4 因此仍不能关闭。

## 已关闭

### P1-1f3 已关闭：reaper 完成/失败在 wrapper 返回前可观察

- `internal/wrapper/wrapper.go:30-67` 新增位于 execution wrapper 进程组外的监督层。execution wrapper 被组级 KILL 后，监督层读取 `reaper-result.json`，`waitForReaper` 未得到成功结果就不把执行路径当作已完成。
- `internal/wrapper/wrapper.go:244-290` 在启动 reaper 前原子写 pending 结果；reaper 对整组发 `SIGKILL`、有界探测 `kill(-pgid, 0)`，再原子写成功或错误。启动失败会留下 pending 并由监督层有界报错，完成与执行失败也不再静默。
- `internal/wrapper/wrapper_integration_test.go:118-174,228-243` 保留忽略 TERM 的同组后代/Agent 覆盖，在 `cmd.Wait` 返回后立即用单次 probe 断言 `ESRCH`，没有测试侧追加轮询；无效 PGID 的 reaper 错误也由监督路径观察。
- 上述 wrapper 定向测试独立 `-count=10` 通过，组合 target `-count=10` 与 race 也通过。

这满足 #185 明列的启动/完成/失败可观察与“wrapper 返回点组已空”关闭条件。

## P1 阻断

### P1-1g3 未关闭：真实 kill matrix 仍止于 spawn 前，缺旧 owner 交错与进程身份面

已交付的有效增量：

- `internal/launchworker/e2e_crash_test.go:128-208` 以子测试进程运行真实 worker，在 prepare、bootstrap rename、digest 和 spawn 前同步后发 SIGKILL，再创建新 boot/worker 恢复；这些不再是 hook 返回普通 error。
- `internal/wrapper/wrapper_integration_test.go:94-116` 真实丢弃第一次 permit socket 响应，断言第二次 params 完全相同且 Agent marker 只有一行，覆盖了 wrapper 内 permit replay/one-shot 的窄路径。
- 新增 kill-boundary 测试独立 `-count=10` 通过；目标包组合 `-count=10` 与 race 也各有一次完整通过。

仍不满足 #186 硬性关闭条件：

- 测试表只有 `prepare/rename/digest/spawn` 四项；`internal/launchworker/e2e_crash_test.go:239-240` 的 `spawn` 实际绑定 `beforeSpawn`，而 `internal/launchworker/launch.go:141-146` 显示同步发生在 `Backend.Spawn` **之前**。没有杀死已启动 wrapper 的场景，也没有 control 初写/补写、`claim.started` 提交前后、`result.json` 前后的真实 kill/restart。
- permit 丢失测试使用内存 `wrapperServer`，不经过真实 storage/daemon 投影；它证明同参重放和单次 Agent marker，但没有在 permit 已签发后暂停旧 wrapper，同时启动恢复/新 owner 并证明旧 owner 或进程组消失前新 owner不能出现。
- `backend.spawns == 1` 只统计重启后的新 `countingBackend`；`assertSingleLaunchOwner` 仍只数单个 attempt/claim 表中非空行；Agent marker也只在恢复后观测。三者没有跨崩溃记录旧、新 wrapper/Agent 的实际 PID/PGID 与存活区间，因而不能从“跨重启 spawn adapter + 持久投影 + 实际进程身份”三面证明最终至多一个 owner。
- PR #188 body 明确写明“control/started/result 杀进程与 pause-old-owner 交错未齐”。这些正是 #186 的硬性关闭条件和 DESIGN §12 V4 显式场景，不宜降为 NOTES。
- 稳定性仍有反例：在组合 target `-count=10` 通过后，定向执行 `TestLaunchWorkerKilledAtHandoffBoundaries|TestLaunchWorkerWrapperCrashSuite` 的 `-count=10` 时，`TestLaunchWorkerWrapperCrashSuite` 一轮在 `e2e_crash_test.go:100` 得到 `controlled agent starts = 0`。该单测随后独立 `-count=20` 通过，说明仍是时序 flake；#186 要求组合 `-count`/`-race` 稳定，不能计作关闭。

关闭条件：以真实 wrapper/worker 子进程和同步点补齐 backend spawn 后、control、started、result 边界的 kill/restart；构造 permit 后暂停旧 owner，并让恢复方尝试接管，断言旧 wrapper/进程组未确认消失前无新 owner；用跨重启全局 spawn 计数、持久投影及实际 PID/PGID 存活区间共同证明至多一个 owner；消除 launch marker flake并使组合 `-count`/`-race` 稳定。

## P1-2c/P1-1e 回归确认

- **P1-2c 未回退**：`TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier` 独立 `-count=10` 通过；frozen 候选仍在 recovery barrier 打开前收敛完整 `startup_stall` 投影。
- **P1-1e 未回退**：`TestRecordBootstrapDigestIsIdempotentForSameDispatch` 独立 `-count=10` 通过；新真实 worker kill/restart 套件的 rename/digest 场景独立 `-count=10` 也通过，相同 dispatch/digest reclaim 仍可继续启动。

## 执行证据

以下命令均在当前 issue worktree 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**失败一次**，既知 `TestDoctorBaselineChecksConfiguredDependencies` fixture 超时得到 `signal: killed`；`CGO_ENABLED=0 go test ./internal/controlplane` 随后通过。该已知 doctor 时序 flake不作为本轮 P1 判定依据，但全量命令不伪报为通过。
- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- `go test -race -count=1 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- launch 两个定向场景合跑 `-count=10`：**失败一次**，见上述 marker flake；`TestLaunchWorkerWrapperCrashSuite` 随后独立 `-count=20` 通过，`TestLaunchWorkerKilledAtHandoffBoundaries` 独立 `-count=10` 通过。
- reaper/permit-loss 四组 wrapper 定向测试、P1-2c 与 P1-1e 定向测试各自 `-count=10`：**通过**。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 完整 wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵和 M3 V4 门禁均保持 `[ ]`；PR #187 可关闭本轮窄 P1-1f3，但 PR #188 尚不足以把这些完整项勾选。

## Issue #189 acceptance checklist

- [x] 自行读取 Issue #189 全文与评论，并回溯上一份 FAIL、#185/#186 与 PR #187/#188；相关 Issue 均无评论。
- [x] 定向复核 P1-1f3 reaper 启动/完成/失败可观察及 wrapper 返回点整组消失。
- [x] 定向复核 P1-1g3 的真实 kill/restart、permit-loss、pause-old-owner 与三面 owner 证据，并按作者声明诚实判定。
- [x] 确认 P1-2c/P1-1e 未回退。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，给出代码、测试与命令证据。
- [x] 仅在 issue worktree 工作；未修改实现代码，未推送、未合并。

## 结论

**FAIL。** P1-1f3 已关闭：监督者在返回前可观察 reaper 完成/失败，测试直接证明组已空。P1-1g3 只有 prepare/rename/digest/backend spawn 前的真实 worker kill/restart及一个 fake-server permit-loss 窄场景；control/started/result、permit 后暂停旧 owner、跨重启实际进程身份面仍缺，且 launch marker flake再次出现。P1-2c/P1-1e 未回退；补齐上述硬性 matrix 并稳定压力运行前，M3 V4 不得勾选。
