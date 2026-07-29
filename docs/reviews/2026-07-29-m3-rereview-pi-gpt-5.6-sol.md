FAIL

# M3 P1-1/P1-2 定向复审

评审基线：`0b4a497`，分支 `docs/issue-156-m3-directed-rereview-after-p1-1-p1-2`。本次只复核 [`2026-07-29-m3-phase-review-pi-gpt-5.6-sol.md`](2026-07-29-m3-phase-review-pi-gpt-5.6-sol.md) 的 P1-1/P1-2 是否由 Issue #151/#152 及后续 Issue #155 的集成测试关闭；判定基准为 [`DESIGN.md` §8.4/§10.1/§12](../DESIGN.md)、active 的 [`control-plane.md` §4.2–§7.6](../specs/control-plane.md)、[`storage.md` §12.8](../specs/storage.md) 与 [`WBS.md` M3](../WBS.md)。Issue #151/#152/#155/#156 均无评论。

#151/#152/#155 已合入，生产代码也不再是空壳：daemon 装配了唯一 `launch_agent` worker，wrapper 能走 acquire → permit → spawn → started → heartbeat/result，且新增测试会运行真实 wrapper 二进制。但这些增量尚未关闭两项 P1。当前恢复代码把“写一条分类收据”等同于“候选已确定性收敛”，随后允许原 launch operation 被 reclaim；launch worker 自身也没有完成规格要求的 lease/dispatch 崩溃恢复。两者组合后，启动屏障仍不能证明“旧执行体已分类后才重放”。

## P1 阻断

### P1-2 仍未关闭：恢复动作只是收据，屏障会在候选未收敛时打开

可复核证据：

- `internal/daemon/termination.go:52-78` 对 attempt 调用既有 `recoverAttempt` 后，仅写 `Action: "attempt_" + phase`；对每个未完成 launch operation 无条件写 `Action: "launch_operation_held"`。该 operation 分支没有观测 bootstrap/control/process，也没有结合对应 attempt 选择复用 dispatch、递增 generation、终结 operation、冻结并打扰人等确定性动作。
- `internal/daemon/termination.go:81-84` 对所有 `pending` attempt 直接返回 `no_execution_body`，没有检查规格点名的 operation 派发状态、bootstrap 是否已读、wrapper/acquire 是否在途，也没有递增 fencing generation、作废旧 dispatch 或重新入队。它因此不能区分 DESIGN §10.1 的两条 `pending` 行。
- `internal/storage/recovery.go:49-117` 的 `ApplyStartupRecoveryAction` 只校验 generation 或 operation version并插入 `startup_recovery_actions`；它不修改 attempt、claim、dispatch 或 operation，因而不是 [`storage.md` §12.8](../specs/storage.md) 所要求的“逐项经确定性恢复动作收敛”。`Action` 还是任意非空字符串，而非可验证的 closed action 集。
- `internal/storage/recovery.go:138-166` 只要存在上述收据，就从本 boot 的候选查询中排除对象；`internal/storage/boot.go:56-68` 随即允许打开屏障。`internal/storage/boot_test.go:38-46` 甚至明确证明：给未完成 launch operation 写一条 `launch_operation_held` 收据后，屏障打开，原 operation 可立即被 `ClaimLaunchOperation` 认领。测试锁定的是“分类即放行”，不是“收敛后放行”。
- 生产路径的文件布局不一致：`internal/launchworker/launch.go:69` 与 wrapper 测试使用 `runs/<run>/<attempt>/...`，而 `internal/daemon/termination.go:186-197` 从 `runs/<run>/attempts/<attempt>/...` 读取 control/result。重启时恢复协调器因此无法观测生产 wrapper 写下的身份与结果，不能按 observation digest 正确分类。

影响：一个已准备 dispatch、已写 bootstrap、已 spawn wrapper 或 acquire 在途的 operation，都可能仅凭收据被视为“已恢复”；屏障打开后旧 operation 可被 reclaim，而旧 wrapper/process 事实没有按矩阵收敛。P1-2 的核心安全承诺仍不成立。

关闭条件：让恢复 action 在同一 CAS 端口内执行实际的、closed 的收敛变更；按 attempt 与 launch operation 的并集观测统一的生产 run-dir；覆盖 pending/starting/spawning 与 dispatch/bootstrap 各状态，并断言屏障打开时每个候选已成为正常监督、终态、可安全重派或 frozen + 可见 Interrupt，而不只是拥有一条收据。

### P1-1 仍未关闭：launch dispatch 与 wrapper 状态机没有达到生产崩溃安全

可复核证据：

- `internal/launchworker/launch.go:57-84` 只在 `PrepareLaunchDispatch` 前校验一次 lease；写 bootstrap 前、spawn wrapper 前均未重新确认 expected lease，也未回填 bootstrap digest。它违反 [`control-plane.md` §4.4](../specs/control-plane.md) 的固定顺序和“每个外部步骤前重验 lease”要求。
- `internal/storage/launch.go:43-45` 遇到已准备的 dispatch 直接返回 stale worker；同时数据库只存 nonce/token hash，worker 没有复用明文的路径。故 worker 在 prepare 提交后、bootstrap rename 前崩溃时，新 worker既不能复用同 dispatch，也没有实现“确认无 wrapper/control 后递增 generation 重发”，该 operation 会停滞。这一关键窗口没有 launchworker 测试；`internal/launchworker` 当前没有任何 `_test.go`。
- `internal/wrapper/wrapper.go:53-57` 对 permit 只调用一次 RPC。响应丢失时不会用同 session/candidate/params 重试；孤立的 `PermitGate` 单测只能证明第二次显式调用 `StartOnce` 被拒，不能证明生产 wrapper 的 permit 响应丢失/重放状态机。
- `internal/wrapper/wrapper.go:94-108` 在 Agent 已 spawn 后，只要 control rewrite 或 `claim.started` 失败就立即返回；没有等待、终止或回收已启动 Agent。`cmd/sift-agent-wrapper/main.go:25` 还使用 `context.Background()`，wrapper 没有安装信号转发/进程组回收逻辑。单独终止 wrapper 或 started 拒绝可留下继续写 worktree、但已失去 wrapper 监督的 Agent，未逐条实现 DESIGN §8.4 wrapper 契约第 7 条。
- `internal/wrapper/wrapper_integration_test.go:23-89` 的所谓 crash-window 测试只是省略文件、放宽权限、让 fake RPC 返回错误或使用不存在的 executable；它没有在 bootstrap rename、spawn、control rewrite、started/result 等边界杀 worker/wrapper并重启恢复，也没有模拟 permit 响应丢失/重放。`wrapperServer` 对每个请求只返回一次普通响应。
- 新拓扑测试并不稳定：`CGO_ENABLED=0 go test -count=10 ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage` 中 `TestProductionWrapperKeepsAgentInWrapperProcessGroup` 失败 3 次；`go test -race` 的同一测试也失败。即使把一秒轮询上限视作测试时序问题，Issue #155 要求的稳定集成证据也尚未形成。

影响：当前代码证明了“正常 happy path 可以启动一个进程”与“一个内存 gate 的第二次调用会失败”，没有证明 production lease/dispatch/restart 与 wrapper post-spawn 失败窗口不双起、不遗留无人监督执行体。P1-1 的生产链和崩溃窗口关闭条件仍未满足。

关闭条件：补齐 dispatch digest与每步 lease CAS、已准备 dispatch 的恢复策略；实现 permit 同参数重试、post-spawn 任意错误/信号下的进程组监督与回收；以真实 DB/control-plane/launch worker/wrapper/受控 Agent 做崩溃注入，覆盖 Issue #151/#155 列出的窗口、响应丢失/重放和重启后 owner 唯一性。

## 已确认的增量

- `cmd/sift-agent-wrapper` 已从 stub 接到 `internal/wrapper.Run`。
- `internal/daemon.Daemon` 只有一个 `Launch` 槽位，`cmd/siftd` 在 recovery barrier 后装配一个 `launch_agent` worker。
- Agent 正常路径只经 `runtime.PermitGate.StartOnce` → `runtime.DirectLauncher.Start`；拓扑测试在单次普通执行中验证 Agent 与 wrapper 同进程组。
- `StartupRecoveryPending` 确实查询全部非终态 attempt 与全部未完成 `launch_agent` operation，`ApplyStartupRecoveryAction` 也具备 generation/operation version CAS 和 observation digest 收据；缺的是收据背后的确定性收敛动作与生产观测。
- `CompleteStartupRecovery` 与 `ClaimLaunchOperation` 的 boot-scoped DB 屏障本身仍有效；失败点是屏障的完成谓词过弱。

## 执行证据

- `CGO_ENABLED=0 go vet ./...`：**通过**。
- `CGO_ENABLED=0 go test ./...`：**未通过**；既知 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 时序 flake 再现（fixture agent 命令 `signal: killed`），其余包本次通过。
- `CGO_ENABLED=0 go test -count=10 ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**未通过**；wrapper 拓扑测试 3 次超时，其余目标包通过。
- `go test -race ./internal/wrapper ./internal/runtime ./internal/daemon ./internal/storage`：**未通过**；同一 wrapper 拓扑测试超时，其余目标包通过。

## WBS 诚实性

未修改 [`WBS.md`](../WBS.md)。§3.1 的生产 wrapper/launcher 项、§3.2 完整 handoff 项、§3.4 完整恢复矩阵项与 M3 V4 组合门禁当前均保持 `[ ]`，与本次结论一致；没有可诚实新增的完成勾选。

## Issue #156 acceptance checklist

- [x] 自行读取 Issue #156 全文与评论，并回溯 #151/#152/#155；均无评论。
- [x] 定向复核上一份 FAIL 的 P1-1/P1-2及可用集成测试。
- [x] 在指定路径产出 PASS / PASS WITH NOTES / FAIL 报告，并给出可复核代码与测试路径。
- [x] 如实说明集成套件已合入但不足以关闭崩溃窗口，不把组件/happy-path 测试称为完整生产证据。
- [x] 仅在 issue worktree 工作；未修改实现代码，未推送、未合并。

## 结论

**FAIL。** #151/#152/#155 把两个 P1 从“完全缺实现”推进到“已有 production-shaped happy path 与 boot-scoped 分类账”，但 P1-2 的分类账尚未执行确定性恢复动作，P1-1 的 dispatch 与 wrapper 也未覆盖关键崩溃/终止窗口。尤其是“给 operation 写 held 收据后立即允许 reclaim”与启动安全目标相反；在修正该完成谓词并补齐端到端崩溃证据前，两项 P1 均不能关闭，M3 前置不得勾选。
