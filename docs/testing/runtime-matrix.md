---
status: active
created: 2026-07-31
summary: M6 V2/V4 逐行证据清单
---

# Runtime V2/V4 测试矩阵

派生自 [`specs/runtime.md`](../specs/runtime.md)、[DESIGN §10.1](../DESIGN.md#101-启动期恢复矩阵) 与 [§12 V2/V4](../DESIGN.md#12-验证策略)。本文件是 M6 的 coverage inventory，不复制恢复语义；动作冲突时以 DESIGN/runtime spec 为准。

状态含义：`existing` 表示当前已有精确证据；`partial` 表示已有单 backend/单边界证据，M6 仍需合取；`planned` 表示由列出的 Issue 新增。#852 必须把所有 `partial/planned` 更新为可定位的 test name 后才可宣布 M6 通过。

## 1. V2 事务与崩溃边界

| ID | 边界 | 当前精确证据 | 状态/Owner | Backend 维度 |
|---|---|---|---|---|
| V2-01 | Run transition：投影/事件同事务 | `TestV2TransitionCrashAtomicity` | existing | backend-neutral，一次 |
| V2-02 | 当前基础写族：Forge receipt、Task Spec、Brain charge、outbox claim/complete | `TestV2CurrentWritePortsCrashAtomicity` 对应 subtests | existing | backend-neutral，一次 |
| V2-03 | launch operation lease claim/reclaim 与 boot recovery barrier | `TestV2OutboxClaimLeaseBackoffAndStableKey`、`TestLaunchClaimWaitsForCurrentBootRecoveryBarrier`、`TestBackendV2CrashHarness/V2/*/lease`（真杀 worker 后 replacement 收敛） | covered #849 | process/tmux 同 harness |
| V2-04 | dispatch prepare 提交前/后 | `TestLaunchWorkerWrapperCrashSuite/after_prepare`、`TestBackendV2CrashHarness/V2/*/prepare`（redispatch 后 generation=2、旧代三动词 stale） | covered #849 | 两 backend |
| V2-05 | bootstrap temp/write/rename/digest 回填前后 | `TestLaunchWorkerWrapperCrashSuite/after_bootstrap_write|after_bootstrap_digest`、`TestWriteControlFileIsPrivateAndReplacesContents`、`TestBackendV2CrashHarness/V2/*/bootstrap`（reuse_dispatch 恢复） | covered #849 | 两 backend |
| V2-06 | backend 接受 wrapper 前/响应丢失后 | `TestLaunchWorkerWrapperCrashSuite/before_spawn`、`TestLaunchWorkerKilledAfterRealWrapperSpawn`、`TestBackendV2CrashHarness/V2/tmux/acquire`（同绑定 tmux respawn 拒绝） | covered #849；tmux binding/reclaim 由 #845 | 两 backend |
| V2-07 | acquire 请求提交/响应丢失 | `TestProductionWrapperCrashWindows/acquired`、`TestLaunchWorkerKilledAtHandoffBoundaries/acquire`、`TestBackendV2CrashHarness/V2/*/acquire`（杀进程组后同 tuple 重放收敛、同代第二 wrapper conflict、replacement owner=0） | covered #849 | 两 backend |
| V2-08 | permit 请求提交/响应丢失，spawn adapter count=1 | `TestHandoffPermitReplayAndStartedEvidence`、`TestPermitGateConsumesReplayedPermitBeforeSpawn`、`TestProductionWrapperReplaysLostPermitResponseWithSameParameters`、`TestV2HandoffReplayIsBackendParameterized`、`TestBackendV2CrashHarness/V2/*/permit`、`TestBackendV2HandoffResponseReplayAndSpawnOnce`（spawnCalls==1） | covered #849 | 两 backend |
| V2-09 | Agent spawn 后、identity/control 写前 | `TestProductionWrapperCrashWindows/spawned`、`TestLaunchWorkerKilledAtHandoffBoundaries/permit`、`TestBackendV2CrashHarness/V2/*/spawn`（杀组后 started 重放→running→duplicate） | covered #849 | 两 backend |
| V2-10 | Agent identity/control 写后、started 提交/响应前 | `TestProductionWrapperCrashWindows/started`、`TestLaunchWorkerKilledAtHandoffBoundaries/started`、`TestBackendV2CrashHarness/V2/*/started` | covered #849 | 两 backend |
| V2-11 | 极快退出/result 与 started 交错 | `TestProductionWrapperCrashWindows` fast-exit cases、`TestBackendV2CrashHarness/V2/*/fast-exit`（Agent SIGKILL 后 result 收敛、重放零 spawn） | covered #849 | 两 backend |
| V2-12 | Interrupt 五件事：Run、Interrupt、charge、event、publication 全有或全无 | `TestV2InterruptFivePartCrashMatrix`（run/charge/interrupt/admission/binding/event/outbox/delivery/target） | covered #849 | backend-neutral，一次；逐写点 SQLite crash injection |
| V2-13 | `startup_stall` retry success：absence、旧 attempt/resolution/isolation、Interrupt、Run、新 attempt/claim/launch/ack/event 全有或全无 | `TestV2RetryProbeSuccessCrashMatrix`（probe/old attempt resolution/interrupt close/run UPDATE/run.transitioned event/final command.event/final outcome/isolation release/old attempt orphaned/successor/claim/launch/ack 十三个可区分写点）、`TestV2RetryProbeSuccessProjection`、`TestBackendV2OwnerReplacementBarriers`（disappearance→frozen→retry→replacement owner=1 生产路径） | covered #849 | backend-neutral，一次；逐写点 SQLite crash injection |
| V2-14 | 人工态 started/result 与决定的单事务仲裁 | `TestV4HumanStateInterleavings`、`TestResolveAttemptRace*` | partial → #851 | 两 backend 交错调用图 |
| V2-15 | hooks baseline/recheck 与诊断写入崩溃重放 | `TestHookCrashReplayRecordsOneStableDrift`、`TestHookRecheckCrashReplayReceiptIsAtomicWithTerminalResult` | existing #848 | backend-neutral |

## 2. DESIGN §10.1 恢复矩阵逐行

| ID | attempt/观测 | 当前精确证据 | 状态/Owner |
|---|---|---|---|
| R01 | pending：operation 未派发/lease 过期，无 wrapper/control | `TestRecoverStartupRedispatchesPendingAttemptBeforeOpeningBarrier` | partial → #849/#850 双 backend |
| R02 | pending：bootstrap 已读/acquire 在途，wrapper 匹配、无 control | launch crash suite 覆盖局部，未形成双 backend recovery row | planned #850 |
| R03 | starting：session owner/control 在、无 permit | `TestRecoverKeepsLiveStartingOwner` | partial → #850 双 backend |
| R04 | starting：owner 与进程组均不存在 | 无精确具名 row | planned #850 |
| R05 | spawning：owner 匹配、Agent identity 未落盘 | `TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner`、launch kill boundaries | partial → #849/#850 双 backend |
| R06 | spawning：Agent identity/live，started 未提交或响应丢失 | handoff/production wrapper tests 覆盖局部 | planned #850 双 backend补 started/接管监督 |
| R07 | spawning：Agent 已退出且 identity-matched result 在 | fast-exit tests 覆盖生产 wrapper，恢复补 started+result 未双 backend合取 | planned #850 |
| R08 | spawning：Agent identity 在，process/result 不在；wrapper live/dead 分支 | 无完整双分支证据 | planned #850 |
| R09 | spawning：wrapper 死、进程组存在、Agent identity 缺失/不可信 | `TestTerminatorSignalsOnlyVerifiedIdentityAndProvesAbsence` 为 termination seam | partial → #850 生产恢复行 |
| R10 | spawning：wrapper/进程组均不存在、无 Agent identity | 无精确具名 row | planned #850 |
| R11 | running：result success | 现有 success/Change 链覆盖结果消费，未作为双 backend recovery row | planned #850 |
| R12 | running：result failed | 现有 termination/result tests 局部覆盖 | planned #850 |
| R13 | running：process identity 匹配、heartbeat 新鲜 | live starting owner 有证据；running row 无精确双 backend test | planned #850 |
| R14 | running：process 存在、heartbeat 过期 | `TestRecoverRoutesStaleHeartbeatThroughTermination` | partial → #850 双 backend |
| R15 | running：tmux session 在、wrapper 不在 | 无 backend observer | planned #847（端口）/#850（收敛） |
| R16 | running：wrapper 在、tmux session 不在 | 无 backend observer | planned #847（继续监督+诊断）/#850 |
| R17 | 任意：process identity 无法确认，不向不确定 PID 发信号 | `TestTerminatorNeverSignalsReusedOrUncertainPID`、`TestPlatformProcessInspectorRequiresMatchingControlNonce`、`TestTerminationUnconfirmedFreezesAndMakesStartupStallVisible` | partial → #850 精确断言一次 startup_stall/waiting_human/frozen |
| R18 | 任意：多个 wrapper 竞争同 attempt | `TestHandoffPermitReplayAndStartedEvidence`、handoff security event tests | partial → #849 两 backend |
| R19 | 任意：旧 generation wrapper 苏醒 | handoff stale/conflict tests与 security event | partial → #849 两 backend |

R01–R19 的 tmux 参数化断言只能读取 wrapper/control/result/process-group 作为裁定证据。R15/R16 的 session observation 只决定诊断分支，不能改变这条规则。任何“absence 后 orphan/new attempt”的行（含 R02/R04/R07/R08/R10/R15）还必须合取 exact `process-group-verified` gate；unverified/unknown 不执行该结局，而按 X07/X08/X16 收敛为单条人工分支。

## 3. DESIGN §12 V4 扩展向量

| ID | 向量 | 当前精确证据 | 状态/Owner |
|---|---|---|---|
| X01 | kill siftd / wrapper / Agent / backend session | process crash tests覆盖前三类局部；无 tmux | planned #849/#850 |
| X02 | acquire/permit/started replay；permit replay spawn count=1 | `TestProductionWrapperReplaysLostPermitResponseWithSameParameters`、`TestPermitGateConsumesReplayedPermitBeforeSpawn` | partial → #849 双 backend |
| X03 | permit 后暂停旧 owner；replacement 前存在明确空区间 | `TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner` | partial → #849 双 backend |
| X04 | spawning 中 operator retry/kill | termination storage/control tests局部 | planned #851 |
| X05 | spawn 后证据缺失 + process group residual | termination seam tests | planned #850 |
| X06 | PID/PGID reuse | `TestTerminatorNeverSignalsReusedOrUncertainPID` | partial → #850 production recovery row |
| X07 | process group 拒绝消失 | `TestTerminatorEscalatesAndFailsClosedWhenGroupRemains` | partial → #850 exact one Interrupt/waiting_human/frozen |
| X08 | identity unknown | `TestPlatformProcessInspectorRequiresMatchingControlNonce`、`TestTerminationUnconfirmedFreezesAndMakesStartupStallVisible` | partial → #850 exact convergence |
| X09 | process/tmux 下 Agent direct child + same PGID，PTY active | `TestProductionWrapperKeepsAgentInWrapperProcessGroup`、`TestProductionTmuxWrapperKeepsAgentInWrapperProcessGroup` | existing | process/tmux production topology；tmux 需显式安装 |
| X10 | kill 无 successor；retry absence 后仅一个 successor | `TestTerminationKillAfterAbsenceFailsWithoutNewAttempt`、`TestTerminationRetryAfterAbsenceCreatesNewAttempt` | partial → #851 双 backend/并发 |
| X11 | Interrupt commit 前/后 × started，decision 前/后 × started | `TestV4HumanStateInterleavings` 四格 storage seam | partial → #851 两 backend生产调用图 |
| X12 | X11 的 late `result.json` 对称重放 | `TestResolveAttemptRacePersistsLateResult`/decision tests，未四格 | planned #851 |
| X13 | retry probe failure 保持同一 Interrupt、nonce rotate、escalation+1、cap hold、无 marker | `TestStartupStallProbeFailure*`、`TestAdvanceInterruptStartupStallAtLimitHoldsRatherThanAutoRejecting` | partial → #851 生产 discoverer合取 |
| X14 | retry probe success 原子回 queued + 一个 attempt | happy path + V2-13 单边界 | planned #849（逐写 crash）/#851（双 backend结果） |
| X15 | probe in flight 时合法 started fact wins + invalidation ack | attempt race/command tests局部 | planned #851 |
| X16 | detached descendant → process-group-unverified，禁自动 retry | 无 production qualification store/gate | planned #847/#850 |
| X17 | timeout/recovery/kill/retry 四 discoverer 并发 | concurrent Interrupt generation seam局部 | planned #851 |
| X18 | 每个真实 Agent CLI/version topology qualification | 无正式记录 | M7；M6 #847 只交 synthetic mechanism |
| X19 | observational attach 不参与 adjudication | 无 | planned #846；#852 检视 recovery 零引用 attach/session result |
| X20 | backend session binding response-loss/同名冲突 | 无 | planned #845/#849 |

## 4. 平台边界

| 平台项 | M6 结论 | 后续归属 |
|---|---|---|
| Linux process identity (`/proc`) | 已有 production inspector；M6 双 backend matrix 使用并加固 | #850 |
| Darwin identity unknown | 必须 fail closed、不发信号、一次 startup_stall；这是合法安全结局 | #850 逻辑/替身；M8 V15 原生完整恢复 |
| 真实 Agent executable/version 资格 | M6 仅冻结 key/store/query并使用 synthetic fixture | M7 正式证据 |
| 四 OS/arch build | 持续回归，不等于 tmux/PTY 原生运行矩阵 | M8 V15 完整发布 |

## 5. M1–M3 carryover 对账

| WBS 未勾项 | 当前判断 | 归属 |
|---|---|---|
| M1 §1.3 V1/V2 current write ports | V1 已闭合；V2 随新增 write port 继续增长，M6 需补 V2-12/13/15 后再回填 | #848/#849/#852 |
| M2 §2.5 V11 后续闭合 | M4 审计/Ledger与 M5 指标分母已有阶段证据，属于 stale checkbox | #852 仅据既有 review/test 回填 |
| M3 §3.4 完整恢复矩阵 | process 首跑已过，R01–R19 双 backend 未闭合 | #849–#851 |
| M3 §3.4 competition/old generation/heartbeat/backend mismatch | 前三项局部已有；backend mismatch 未实现 | #847/#849/#850 |
| M3 §3.7 confirmed-absence production branch | storage分诊已有，生产 qualification predicate 无 durable true path | #847 机制；M7 写真实资格 |
| M3 §3.8 hooks baseline/automatic recheck | reader/doctor 已有，production writer/recheck 无 | #848 |
| Darwin native inspector | 当前明确 fail closed；不属于双 backend 逻辑差异 | M8 V15 每 OS 完整恢复 |

## 6. M6 门禁查询

#852 必须机械检查：

1. 本文件所有 `partial/planned` 均替换为合入后的精确 test 名与 PR/Issue；不得仅改状态词。
2. process/tmux 共用测试由同一 table/harness 生成，log 中各 backend/row 都有 PASS sentinel；tmux 缺失不得 skip。
3. V2-12/V2-13 有逐写点 crash injection，不以 happy path、单个末写触发器或 transaction 实现目测代替。
4. R17、X07、X08 精确断言一条 `startup_stall`、一次 attention charge、Run `waiting_human`、isolation frozen，且零不确定 PID signal。
5. X09 在 PTY active 的两个 backend 都核对 OS PPID/PGID，不只检查 Go 调用图。
6. X16 的 unverified negative path生产可达；M7 前不得伪造真实 Agent verified 记录。
7. 独立评审逐行抽查本 inventory 与实际 `go test -list`/CI sentinel 一致后，方可更新 WBS M6。
