FAIL

# M3 P1 第四次定向复审

评审基线：`ee8a51b`，分支 `docs/issue-183-m3-directed-rereview-4-after-p1-1f2-p1-1g2`。本次按 Issue #183 只复核上一份 [`2026-07-29-m3-rereview-3-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-3-pi-gpt-5.6-sol.md) 的残余 P1 是否由 #179/#180（PR #181/#182）关闭，并确认此前关闭的 P1-2c/P1-1e 未回退；判定基准仍为 [`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、active 的 [`control-plane.md`](../specs/control-plane.md)、[`storage.md`](../specs/storage.md) 与 [`WBS.md` M3](../WBS.md)。Issue #183/#179/#180 均无评论。

PR #181 已把 grace 后的 KILL 从 Agent 直接子进程扩展为整个 wrapper 进程组，并加入忽略 TERM 的同组后代场景；核心缺陷已修正。但外置 reaper 的完成结果没有回到 wrapper 调用方，测试也在 wrapper `Wait` 返回后继续轮询，而非断言返回时整组已空，尚未满足 #179 的完整关闭条件。PR #182 只增加持久行计数及同参 permit 的并发 DB 重放；作者也明确声明完整 matrix 仍部分。它没有交付 #180 所要求的真实逐阶段杀进程、permit-loss/暂停旧 owner 交错和三面 owner 唯一性证据。M3 V4 因此仍不能关闭。

## P1 阻断

### P1-1f2 未完整关闭：组级 KILL 已有，但 wrapper 返回与消失确认未形成闭环

已关闭的核心缺陷：

- `internal/wrapper/wrapper.go:188-211` 的独立 reaper 对 `-pgid` 发 `SIGKILL`，再以有界 `kill(-pgid, 0)` 探测 `ESRCH`；不再只调用 `cmd.Process.Kill()`。
- `internal/wrapper/wrapper_integration_test.go:94-111` 构造同组、忽略 TERM 的后代并先核对其 PGID；测试清理没有替生产路径发组 KILL。

仍未闭合的关闭条件：

- `internal/wrapper/wrapper.go:149-170` 启动组外 reaper 后立即结束当前路径，且 `startProcessGroupReaper` 的错误被丢弃。reaper 的 `ReapProcessGroup` 若启动失败、探测超时或返回其他错误，调用方均不可见，wrapper 仍会退出；生产路径没有一个可观察结果证明整组已经消失。
- `internal/wrapper/wrapper_integration_test.go:109-111,159-161,191-207` 都先等待 wrapper 进程返回，再调用 `waitForGroupAbsence` 最多轮询一秒。这证明“最终可能消失”，没有按 #179 明列的条件证明“wrapper 返回时组已空”。
- 该异步结构是因为组级 `SIGKILL` 必然同时杀 wrapper，但这不免除契约；需要由可存活的监督者完成并暴露有界确认结果，或提供等价的、可被调用方验证的完成协议。

关闭条件：使组外 reaper 的启动/完成/失败成为监督路径可观察的结果，并让集成测试在 wrapper 返回点直接证明组已不存在；不得以测试侧追加轮询补生产确认。

### P1-1g2 未关闭：新增断言不等于真实 crash/permit-loss matrix

可复核证据：

- `internal/launchworker/e2e_crash_test.go:124-200` 仍只有 `after-rename`、`after-digest`、`before-spawn` 三个返回普通 Go error 的 worker hook；没有杀 worker/wrapper 后重启，也没有 prepare、spawn 后、control、started/result 边界。
- `internal/launchworker/e2e_crash_test.go:230-249` 的 `assertSingleLaunchOwner` 只统计单个 attempt 行上非空 `wrapper_instance_id` 与单个 claim 行上非空 `dispatch_id`。表结构本就只有该 attempt/claim 行；这个计数不能发现两个 OS wrapper/Agent 同时存活，也不是跨崩溃边界的全局 owner 计数。
- `internal/launchworker/e2e_crash_test.go:173-200` 的 spawn adapter 仅在重启后的新 `countingBackend` 中计数，崩溃前后不是同一个全局计数器；没有从进程身份面断言唯一 owner。
- `internal/storage/handoff_test.go:42-64` 新增的是两个 goroutine 对 `PermitSpawn` 的同参 DB 重放。它没有模拟 permit 响应丢失后的 wrapper RPC 重放、重放经过 one-shot guard 时 spawn adapter 仍为 1，或 permit 签发后暂停旧 owner再尝试换 owner。
- PR #182 body 已诚实注明“完整 prepare/control/started/result 杀进程 matrix 仍部分”。这正是 #180 的显式关闭条件，不宜降为 note。

稳定性也未满足 #180 的 `-count` 条件：独立运行

`CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`

时，`TestLaunchWorkerWrapperCrashSuite` 有一轮在 `e2e_crash_test.go:99` 得到 `controlled agent starts = 0`。随后该单测独立 `-count=20` 通过，说明是时序/资源敏感 flake，而不是确定性功能失败；但 #180 明列稳定性为关闭条件，不能计作通过。

关闭条件：按 #180 原文以可控子进程和同步点覆盖 prepare、rename、digest、spawn、control、started/result 的真实 kill/restart；覆盖 permit 响应丢失/同参重放和 permit 后暂停旧 owner；用跨重启 spawn adapter 计数、持久投影与实际进程身份共同证明最终至多一个 owner，并使组合 `-count`/`-race` 稳定。

## 已关闭项回归确认

- **P1-2c 未回退**：`TestRecoverStartupFrozenCandidatesEmitStartupStallBeforeOpeningBarrier` 独立 `-count=10` 通过；frozen 候选仍先经 `RecordTerminationObservation` 收敛完整投影，再允许 recovery barrier 打开。
- **P1-1e 未回退**：`TestRecordBootstrapDigestIsIdempotentForSameDispatch` 与 `TestLaunchWorkerReclaimsPreparedBootstrapAfterCrashWindows` 独立 `-count=10` 通过；相同 dispatch/digest reclaim 仍幂等，不同窗口均可继续启动。

## 执行证据

以下正式命令均在当前 issue worktree 执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/wrapper`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**失败一次**，见上述 launch marker flake；同一正式命令未计为通过。
- `CGO_ENABLED=0 go test -run '^TestLaunchWorkerWrapperCrashSuite$' -count=20 -v ./internal/launchworker`：**通过**。
- `go test -race -count=1 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- P1-2c/P1-1e 三组定向测试各自 `-count=10`：**通过**。

最初曾把 targeted `-count=10` 与 race 并发运行，wrapper 的一秒测试等待在资源竞争下失败；该并发结果不作为正式判据。随后 wrapper 独立 `-count=10` 通过。上面另列的 launch marker 失败发生在并发压力命令结束后的独立组合 `-count=10`，故如实保留。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 完整 wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵和 M3 V4 门禁均已保持 `[ ]`；PR #181/#182 的增量不足以新增勾选，现有文字也未把完整 matrix 误报为完成。

## Issue #183 acceptance checklist

- [x] 自行读取 Issue #183 全文与评论，并回溯上一份 FAIL、#179/#180 与 PR #181/#182；相关 Issue 均无评论。
- [x] 定向复核 P1-1f2 组级 SIGKILL/消失确认与 P1-1g2 crash/permit-loss owner matrix。
- [x] 确认 P1-2c/P1-1e 未回退。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，给出可复核代码、测试与命令证据。
- [x] 如实区分组级 KILL 主路径、异步确认缺口、DB 重放/行计数与完整真实进程 matrix。
- [x] 仅在 issue worktree 工作；未修改实现代码，未推送、未合并。

## 结论

**FAIL。** PR #181 修正了“只 KILL 直接 Agent”的核心错误，但组外 reaper 的完成/失败对监督者不可见，测试也没有证明 wrapper 返回时整组已空；P1-1f2 尚未按原关闭条件形成闭环。PR #182 只加强了既有测试的行计数和 permit DB 重放，完整逐阶段真实 crash/permit-loss matrix 仍明显缺失，且组合 `-count=10` 仍出现一次 launch marker flake。P1-2c/P1-1e 未回退；在 P1-1f2/P1-1g2 按上述条件补齐前，M3 V4 不得勾选。
