FAIL

# M3 phase review：Runtime / interrupt / recovery

评审基线：`8443f5e`，分支 `docs/issue-148-m3-phase-review-runtime-interrupt-recovery`。判定基准为 [`WBS.md` M3](../WBS.md)、[`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、[`PRD.md` §10.1](../PRD.md) 以及 active 的 [`control-plane.md`](../specs/control-plane.md)、[`storage.md`](../specs/storage.md)、[`interrupt.md`](../specs/interrupt.md)、[`config.md`](../specs/config.md) 与 [`outbox.md`](../specs/outbox.md)。Issue #148 无评论。

M3 不能通过。确定性 Interrupt 发射、隔离、受控终止内核、handoff 存储/RPC 端口与 worktree 证据已有可复核实现，但生产 wrapper 仍是明确的 M1 stub，daemon 也没有 `launch_agent` worker；同时启动恢复在没有逐项收敛全部 attempt/launch operation 的情况下即打开持久恢复屏障。这两项都直接落在“不得双起”和“恢复先于启动重放”的安全边界上，属于 P1 阻断。

## P1 阻断

### P1-1：生产启动链不存在，wrapper 契约与 handoff 只停留在端口/测试接缝

可复核证据：

- `cmd/sift-agent-wrapper/main.go:13-18` 除 `--version` 外只输出 `wrapper stub — not implemented`，不会读取并 unlink bootstrap、acquire session、写 control/task、请求 permit、经 one-shot launcher spawn Agent、确认 started、写 heartbeat/result 或转发进程组信号。
- `internal/runtime/runtime.go:66-145` 实现了 `ProcessBackend`、`Launcher` 和 `DirectLauncher`，但非测试生产代码没有调用 `ProcessBackend.Spawn`、`PermitGate.SpawnOnce` 或 `DirectLauncher.Start`。
- `internal/daemon/daemon.go` 只装配 Intake、reconciler、reply 与 forge comment worker；仓库中没有消费 `launch_agent` 的生产 worker。`ClaimLaunchOperation` 只有存储端口和测试调用。
- 因而 [`control-plane.md` §4.2–§7.6](../specs/control-plane.md) 冻结的 bootstrap v2、wrapper 本地状态机、control/heartbeat/result/log 契约均没有生产实现；WBS §3.1/§3.2 原 `[x]` 与同段“wrapper 主体仍为 stub”的文字自相矛盾。

影响：Sift 不能从 queued attempt 启动真实 Agent；V4 process 段和 V10a wrapper 端到端段没有成立，更无法证明 permit 响应丢失/重放时生产 wrapper 只 spawn 一次。`internal/runtime`/storage/control-plane 的单测证明了组件性质，不能替代生产调用链。

关闭条件：实现并装配唯一 `launch_agent` worker 与完整 wrapper 状态机；按 bootstrap/file/spawn/acquire/permit/started/极快退出各崩溃窗口做生产路径集成测试，并证明 Agent 只经 launcher 且保持 wrapper 直属进程组拓扑。

### P1-2：启动恢复屏障在存在未分类候选时被提前打开

可复核证据：

- [`storage.md` §12.8](../specs/storage.md) 要求扫描“全部非终态 attempt + 全部未完成 `launch_agent` operation”，逐项经确定性恢复动作收敛；只有无未分类候选时才可 `CompleteStartupRecovery`。
- `internal/daemon/termination.go:34-65` 的 `Recover` 只查询非终态 attempt：`pending` 直接跳过；live `starting/spawning` 直接 `continue`；没有查询未完成 launch operation，也没有实现规格中的 `ApplyStartupRecoveryAction(expectedGeneration, observationDigest, action)`。
- `cmd/siftd/main.go:57-63` 在该函数返回后无条件调用 `CompleteStartupRecovery`，随后装配 worker。存储层 `ClaimLaunchOperation` 的 CAS 屏障本身有效，但屏障的打开条件不成立。
- WBS §3.4 已诚实保留“完整恢复矩阵逐行”与多 wrapper/旧 generation/后端会话不一致为 `[ ]`，却把 M3 V4 组合门禁标为通过；两者不能同时成立。

影响：一旦补上 P1-1 的 launch worker，过期 launch lease 可在旧 bootstrap/wrapper/permit 状态尚未按矩阵分类时被 reclaim，破坏“恢复先于启动 operation 重放”的线性化保证并重新引入第二 owner 风险。当前缺 worker 只让风险暂不可触发，不构成协议正确性。

关闭条件：实现统一的恢复分类器和恢复写端口，扫描 attempt 与 launch operation 的并集；每行以 generation/operation version + observation digest CAS 收敛；仅在复查无未分类候选后打开本 boot 屏障；逐行覆盖 DESIGN §10.1 的 process 段。

## 其他缺口与注记

1. **受控终止的确认消失分支生产不可达。** `cmd/siftd/main.go` 未给 `TerminationCoordinator.ProcessGroupVerified` 注入资格谓词，故 Linux 即使确认进程组消失也统一转 `process_group_unverified` / `startup_stall`。这是安全的 fail-closed，但 WBS §3.7“确认消失后的结局按来源区分”不能标完成；真实资格仍按 WBS 归 M6/M7。
2. **hooks 闭环未接。** `project_hook_baselines` 没有生产写入方，Agent 结束后也没有自动复核；doctor 只能报告 baseline absent。WBS §3.8 对应项已改回 `[ ]`。
3. **完整 DESIGN §10.1 / §12 V4 集合未齐。** 现有测试覆盖 handoff 端口、live starting owner、stale heartbeat、身份 fail-closed、受控终止、迟到事实与 startup_stall 并发去重，但没有 pending/dispatch 恢复、starting 超时换代、spawning 各文件/进程组合、后端会话不一致、完整安全事件和真实 process wrapper 崩溃注入。按 issue 约束，不把这些局部测试称为完整恢复矩阵。
4. **doctor 既知时序 flake 再现。** 全量测试中的 `TestDoctorBaselineChecksConfiguredDependencies` 因共享 deadline 下 fixture 命令被 `signal: killed` 失败；单独重跑通过。它不是上述 P1 的原因，但意味着本次全量测试门禁并非全绿。

## M3 工作包复核

| WBS 工作包 | 结果 | 摘要 |
|---|---|---|
| 3.1 backend / launcher / wrapper | **NO** | backend 与 launcher seam 有单测；生产 wrapper 为 stub。 |
| 3.2 spawn handoff | **NO** | DB/RPC/one-shot 组件存在；无 launch worker 与 wrapper 调用链。 |
| 3.3 worktree / success evidence | **YES（M3 范围）** | base-only 读取、hooksPath 覆盖与成功证据测试通过；Change 创建按计划留 M4。 |
| 3.4 recovery matrix / qualification | **NO** | 局部 fail-closed 与 boot CAS 存在；完整矩阵和正确屏障打开条件缺失。 |
| 3.5 resolution / isolation | **YES（M3 范围）** | 三个当前生产入口共用 `ResolveAttemptRace`；Command 入口按计划留 M5。 |
| 3.6 generic Interrupt emitter | **YES** | 全 reason fallback、五件事事务、稳定生成键和并发去重有实现与测试。 |
| 3.7 controlled termination | **NO** | 终止核心与未确认分支可达；确认消失的生产分诊不可达。 |
| 3.8 hooks / doctor | **NO** | fingerprint reader 有实现；baseline writer 和结束后自动复核缺失。 |

## M3 门禁复核

| WBS M3 门禁 | 结果 |
|---|---|
| V4 process backend、handoff、恢复矩阵、受控终止与资格门控部分 | **NO** — P1-1/P1-2。 |
| V5a base/worktree 读取源 | **YES**。 |
| V10a wrapper 凭据部分 | **NO（端到端）** — server/存储拒绝矩阵与 one-shot 单测通过，但 wrapper 不存在；只能记组件段通过。 |
| 每个 PRD reason 的无 T4/T6 fallback | **YES**。 |
| 同一 startup_stall 并发发现唯一 Interrupt/扣费/operation | **YES**。 |
| 无法证明消失时可见且 worktree 隔离 | **YES** — 现有生产 recovery/timeout/operator 未确认分支可达；M5 retry 两段式不在本片。 |

## 执行证据

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**未通过**；仅 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 失败，表现为 fixture 命令在 deadline 下被 `signal: killed`。
- `CGO_ENABLED=0 go test -count=1 -run TestDoctorBaselineChecksConfiguredDependencies ./internal/controlplane`：**通过**。
- `go test -race ./internal/runtime ./internal/daemon ./internal/storage ./internal/controlplane ./internal/hooks ./internal/worktree`：目标包除同一 doctor flake 外通过；doctor 单独重跑通过。
- `CGO_ENABLED=0 go test -count=10 ./internal/runtime ./internal/daemon`：**通过**。
- handoff、startup_stall 与 worktree 聚焦测试重复 10 次：**通过**。

## PRD §10.1 诚实性声明

本次是 M3 阶段评审，不是 PoC 发布验收。PRD §10.1 的真实闭环、GitHub/GitLab 各一条、≥3 Run 配额合批、负样本、硬护栏、手机审批、多 Agent 真实链、kill/restart 真实记录与干净双 OS 发布证据均未形成完整集合；不得由本报告推导为已完成。M3 直接相关的恢复标准也因 P1-1/P1-2 不能宣称通过。

## WBS 诚实性修正

仅修改 [`WBS.md`](../WBS.md) 的明显错误勾选，不改实现：将生产 wrapper/launcher 链、完整 handoff、确认消失生产分诊、hooks 自动复核及 M3 V4 组合门禁改回 `[ ]`。既有局部实现证据和后续 M4–M7 归属保持不变。

## Issue #148 acceptance checklist

- [x] 对照 WBS M3 退出条件、门禁、AGENTS 验证命令与相关 active specs/实现证据。
- [x] 产出指定路径报告。
- [x] 给出 PASS / PASS WITH NOTES / FAIL，P1 阻断含可复核代码路径与关闭条件。
- [x] 如实声明 DESIGN §10.1/V4 与 PRD §10.1 全集未齐，不伪造完成。
- [x] 仅在当前 worktree 工作；未改实现代码。

## 结论

**FAIL。** Interrupt/隔离/终止内核的局部安全性质值得保留，但“可启动的 Runtime”与“先完整恢复、后开放 launch replay”是 M3 的两条主承诺，当前分别缺生产执行链和正确恢复闭环。关闭 P1-1、P1-2 并补齐 process 段逐行恢复证据后再复审；在此之前不得勾选 M3 前置、不得进入 M4。