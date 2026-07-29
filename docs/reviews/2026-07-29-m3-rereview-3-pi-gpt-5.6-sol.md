FAIL

# M3 P1 第三次定向复审

评审基线：`baedfca`，分支 `docs/issue-177-m3-directed-rereview-3-after-p1-2c-p1-1e-f-g`。本次按 Issue #177 只复核上一份 [`2026-07-29-m3-rereview-2-pi-gpt-5.6-sol.md`](2026-07-29-m3-rereview-2-pi-gpt-5.6-sol.md) 的 P1 是否由 #169/#170/#171/#172（PR #173/#175/#174/#176）关闭；判定基准仍为 [`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、active 的 [`control-plane.md`](../specs/control-plane.md)、[`storage.md`](../specs/storage.md) 与 [`WBS.md` M3](../WBS.md)。Issue #177 及四个修复 Issue 均无评论。

PR #173 已关闭 frozen 投影必错 SQL 与静默停滞；PR #175 已关闭相同 digest reclaim 的永久停滞；PR #176 也使原 production-shaped e2e 在独立 `-count=10`/`-race` 中通过。但 PR #174 的 KILL 只发给 Agent 直接子进程而不是进程组，仍能留下忽略 TERM 的后代；同时 #172 明列的完整逐阶段 crash/permit-loss matrix 绝大部分未交付。P1 与 M3 V4 门禁因此仍不能关闭。

## P1 阻断

### P1-1f 未关闭：KILL 没有覆盖整个进程组

可复核证据：

- `internal/wrapper/wrapper.go:153-163` 先对 `-syscall.Getpgrp()` 发 TERM，但 grace 到期后调用的是 `cmd.Process.Kill()`，只向 Agent 直接子进程发 KILL；随后只等待该直接子进程。进程组内忽略 TERM 的孙进程可继续存活。
- `internal/wrapper/wrapper.go:144-146` 关于“直接子进程被 reap 后不会留下受控执行体”的注释不成立：同组后代不因其父进程被 KILL 而必然退出。
- `internal/wrapper/wrapper_integration_test.go:92-165` 两个新增测试都只运行一个无后代的 TERM-ignoring shell，所以无法执行上述分支。
- 本次以同一测试 helper 临时构造 `trap '' TERM HUP` 的 Agent shell及同样忽略 TERM/HUP 的同组子 shell，等待 `agent_identity` 后终止 wrapper；`go test ./internal/wrapper -run TestIssue177DescendantRepro -count=1` 稳定失败为 `descendant keeps process group ... alive after wrapper exit`。临时复现文件测试后已删除，未进入工作树。

这仍违反 DESIGN §8.4 的“post-spawn 任意错误/信号下监督并回收整个进程组”，也没有满足 Issue #171“wrapper 退出时进程组不再存活”的关闭条件。

关闭条件：grace 到期后向已验证的 wrapper 进程组发 `SIGKILL`，并有界确认整组消失；测试至少包含一个忽略 TERM 的同组后代，断言 wrapper 返回时 `kill(-pgid, 0)` 为 `ESRCH`，且测试清理不能替生产路径杀遗留进程。

### P1-1g 未关闭：逐阶段 crash/permit-loss matrix 仍缺失

`internal/launchworker/e2e_crash_test.go:123-193` 新增的定向套件覆盖 rename 后、digest 后、spawn 前三个 worker hook，并证明恢复后的测试 backend 调用一次、Agent marker 一行。这足以支持 P1-1e 的窄窗口，但不等价于 Issue #172 的关闭条件：

- hook 返回普通 Go error，不是杀 worker/wrapper 后重启；
- 未覆盖 prepare、wrapper spawn 后、control rewrite、started、result 边界；
- 没有 permit 响应丢失/重放或 permit 后暂停旧 owner 的 production-shaped 交错；
- `TestLaunchWorkerWrapperCrashSuite`（`:23-121`）仍只是正常 handoff 后 Agent 自杀，再 tick 已成功 operation；
- `backend.spawns == 1` 只统计重启后的内存 backend，不能替代跨崩溃边界的全局 wrapper/owner 唯一性断言。

PR #176 body 已诚实注明“完整逐阶段 crash matrix 覆盖仍有限”。本次独立压力运行通过，说明原有 flake 已显著改善，但稳定化不能替代 #172 明列且 DESIGN V4 要求的故障注入范围。该范围是上一轮 FAIL 的显式 P1 关闭证据，不宜降为 note。

关闭条件：以可控进程/同步点在 prepare、rename、digest、spawn、control、started/result 边界杀 worker 或 wrapper并重启，加入 permit 响应丢失/相同参数重放与 permit 后旧 owner 暂停交错；从持久投影、进程身份与 spawn adapter 三面断言最终至多一个 owner。

## 已确认关闭的增量

- **P1-2c 已关闭**：`internal/daemon/termination.go:75-92` 令 frozen 候选先走 `RecordTerminationObservation`；该唯一发射事务落完整 frozen 投影、Run `waiting_human`、一次 Interrupt/预算/comment operation。`internal/storage/recovery.go:121-131` 只在该安全后置条件成立时写 boot receipt。损坏 bootstrap、宽权限 bootstrap、control-only 三例在 `internal/daemon/termination_recovery_test.go` 覆盖，且断言 barrier 打开前投影完整。
- **P1-1e 已关闭**：`internal/storage/launch.go` 允许同 dispatch 的相同 digest 回填为幂等写，不同 digest 仍以零影响行拒绝；storage 单测与真实 worker 的 rename/digest/pre-spawn reclaim 测试均通过。
- **原 e2e 稳定化有效**：PR #176 等待 durable running/succeeded，并在测试清理中 wait/reap wrapper；本次独立定向 `-count=10` 与 `-race` 均通过。此结论只说明已有场景稳定，不把它扩写成完整 crash matrix。

## 执行证据

以下命令均在并发压力任务结束后独立执行：

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**通过**。
- `CGO_ENABLED=0 go test -count=10 ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- `go test -race ./internal/launchworker ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**通过**。
- frozen、digest 与现有 wrapper 定向测试各 `-count=10`：**通过**。
- 上述同组 TERM-ignoring 后代复现：**失败（确认生产缺口）**；测试清理发组 KILL，临时文件已删除。

补充说明：最初把全量、`-count=10` 与 race 三组同时并发执行时，launch e2e 和既知 doctor 时序测试在资源竞争下失败；相同正式命令随后独立执行均通过，因此不把该并发运行计作 #176 的阻断证据。生产进程组遗留与 matrix 缺口不依赖此现象。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 完整 wrapper 契约、§3.2 完整 handoff、§3.4 完整恢复矩阵和 M3 V4 门禁均保持 `[ ]`；现有结果不足以新增勾选。

## Issue #177 acceptance checklist

- [x] 自行读取 Issue #177 全文与评论，并回溯上一份 FAIL、#169–#172 与 PR #173/#175/#174/#176；相关 Issue 均无评论。
- [x] 定向复核 frozen+Interrupt、digest 幂等、TERM→KILL reap 与 e2e 稳定化。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告并给出可复核代码与测试路径。
- [x] 如实区分现有 production-shaped 场景、三个 worker hook 与完整逐阶段 crash/permit-loss matrix。
- [x] 仅在 issue worktree 工作；未修改实现代码，未推送、未合并。

## 结论

**FAIL。** P1-2c 与 P1-1e 已关闭，已有 e2e 的独立压力运行也稳定；但 P1-1f 仍会在 TERM grace 后只 KILL 直接 Agent，留下同组后代，且 P1-1g 的逐阶段崩溃与 permit-loss 关闭证据仍明显不足。修正组级 KILL/消失确认并补齐 #172 matrix 前，P1-1 与 M3 V4 门禁不得勾选。
