FAIL

# S2/M2 Forge & Intake 第三次定向复审（B1/B2 后）

复审基线：`origin/main` / `ea02c37`，工作分支 `chore/s2-m2-rereview-3`。本次只复核[上次报告](2026-07-29-s2-m2-rereview-2-pi-gpt-5.6-sol.md)的 B1/B2 关闭条件，并以 [`specs/forge.md`](../specs/forge.md)、PRD §4.5/§9.2 与 WBS M2 为判定基准。

B2 已关闭，R3 可由 FAIL 更新为 PASS；B1 增加了 durable clarification marker 对账、评论 cursor 和双平台 daemon tick 测试，但 marker 所确定的 generation 没有跨增量页/进程重启持久化，合法回复可被永久跳过，且 cursor/receipt 的作用域仍有项目内与跨项目冲突。因此 B1、R2 及 Intake 旧 generation 门禁仍未关闭，M2 组合门禁为 **FAIL**。

## 执行证据

- `CGO_ENABLED=0 go test ./internal/forge ./internal/intake ./internal/daemon ./internal/storage ./internal/forgebudget ./internal/forgeworker -count=1`：**通过**。
- `CGO_ENABLED=0 go test ./internal/intake ./internal/daemon ./internal/forge -run 'TestReplyConsumerBindsRepliesToClarificationGeneration|TestDaemonTickExecutesDueGitHubAndGitLabRepliesWhileSkippingNotDuePoll|TestV3(AllVerbsDualPlatformMatrix|RecordedMutationAndCommentMatrix)|TestContractRegressionSuite|TestAssemble(ProbesAndRecordsAutoMergeCapabilityOnEveryStartup|RecordsAmbiguousCapabilityAndFailsOnStorageError)' -count=10`：**通过**。
- 两次 `CGO_ENABLED=0 go test ./... -count=1`：第一次通过；第二次仅 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 在 5 秒 deadline 下失败。单独运行 `CGO_ENABLED=0 go test ./internal/controlplane -run TestDoctorBaselineChecksConfiguredDependencies -count=1` 通过。该 doctor 并行时序 flake 与前两次复审一致，不是本次 FAIL 原因。
- 临时最小复现（未入库，执行后删除）：generation 1 marker 在第一次 `RunOnce` 单独到达并推进 cursor；模拟重启后第二次仅返回其后的合法 `/sift approve`。`CGO_ENABLED=0 go test ./internal/intake -run TestRereview3RestartAfterMarkerBeforeReply -count=1` **按预期失败**：`reply after restart was skipped; awaiting=1`。

## B1/B2 关闭条件复核

| 项 | 结果 | 代码证据与判定 |
|---|---|---|
| B1 — 回复 generation 来自 durable generation；增量 receipt/cursor；双平台 daemon 执行证据 | **FAIL** | 正向修复成立：`internal/intake/replies.go` 不再把 `item.Generation` 直接赋给所有历史评论，而是用 `IntakeReplyOperations` 取得 durable comment operation，并以 operation marker/digest 绑定 generation；同一返回页内 generation 1 旧回复会进入 `intake.reply_ignored`，generation 2 当前回复才推进，`TestReplyConsumerBindsRepliesToClarificationGeneration` 覆盖此路径。阻断仍在：① `latestGeneration`/`latestMarkerAt` 每次 `RunOnce`、每个返回页都从零开始，`SaveForgeCursor` 却会在只看到 marker 后推进。下一页或重启后若只返回回复，consumer 因未在**当前页**再次看到 marker 而忽略该合法回复，并继续把 cursor 推过它；generation identity 因而不是跨重启 durable。② 所有 awaiting issue 共用 `(project_id, stream='issue_comments')` cursor；`RunOnce` 又逐 issue 读取并推进它。Issue A 的目标内 cursor 会被传给 Issue B，无法满足 Forge spec 的目标级不透明 cursor 语义，可造成同项目回复漏读。③ `ApplyIntakeReply` 去重查询只按 `forge_event_id`，而表的身份和唯一约束是 `(project_id, forge_event_id)`；两个项目可合法出现相同评论 ID，后者会被误判为已消费。④ 新 daemon 测试证明 GitHub due、GitLab not-due issue 调度及两平台 reply path，但每个平台只有当前 generation；没有首次/重启旧代+当前代场景，且 `Daemon.Comments` 为空，没有执行 B1 要求的项目定向 comment claim。故生产重启安全与完整双平台执行关闭条件均未满足。 |
| B2 — 录制 fixture 闭合双平台 V3 矩阵 | **PASS** | `internal/forge/testdata/fixtures/{github,gitlab}` 已加入双平台 `CommentTarget`、`CreateChange`、`ListChangeComments` 正常响应；GitLab 创建响应缺 head 后按 ID 重读 fixture，重读仍缺 head 的 fixture 也在。`TestV3RecordedMutationAndCommentMatrix` 在真实 Adapter 边界逐平台断言正常归一、缺 comment ID → `ErrContractViolation`、重读仍缺 head → `ErrContractViolation`、评论 actor 缺失丢弃；既有 suite 继续覆盖 marker 冲突、unknown、GitHub combined-status envelope 与 merge CAS。`TestV3AllVerbsDualPlatformMatrix` 保持 13×2 正常路径可见。B2 要求已闭合。 |

## R1–R4 回退检查

| 项 | 结果 | 判定 |
|---|---|---|
| R1 — 启动激活配置投影 | **PASS，无回退** | B1/B2 提交未改启动激活链；目标包测试通过，既有空 DB daemon tick 证据保留。 |
| R2 — 生产调度、回复消费与项目路由 | **FAIL** | due/not-due 与双平台回复执行已有新增证据，但 B1 所述跨页/重启 generation 丢失、cursor/receipt 作用域错误仍可漏掉合法回复；双平台 daemon 测试也未实际运行 comment claim。 |
| R3 — V3 双平台 13 动词契约 | **PASS** | B2 已补齐上次缺少的三类录制 fixture 与关键 fail-closed 断言；13 动词双平台正常矩阵及既有 marker/unknown/merge CAS 套件均通过。 |
| R4 — 启动能力探测接生产 | **PASS，无回退** | B1/B2 未改探测链；启动逐项目探测、歧义 false、持久化失败拒启的重复测试通过。 |

## M2 门禁表

| WBS M2 门禁 | 结果 |
|---|---|
| V3 通过；V7 的 Forge/marker/CAS 部分通过 | **PASS** — B2 使录制 fixture 与关键 fail-closed 矩阵闭合；marker 冲突、unknown 与 merge expected-head CAS 证据保留。 |
| 条件合并能力缺失时 `auto_merge` 被结构性禁用 | **PASS** — 无回退。 |
| actor 缺失事件被忽略；坏项目不影响健康项目 | **PASS** — 双平台 actor fixture 与项目隔离证据保留。 |
| Intake crash marker 与旧 generation 回复仲裁测试通过 | **FAIL** — comment worker crash replay 保留通过；单页旧代 CAS 通过，但 marker 与回复跨增量页/重启时当前代合法回复会被永久跳过，生产仲裁未闭合。 |
| V11 外部事实收敛首段通过 | **PASS** — 外部 merge → `done + gate_bypassed` 证据保留。 |

M2 组合门禁仍为 **FAIL**，M3 前置不得勾选。

## 阻断关闭条件

### C1 — B1 必须跨增量页与重启保存目标级 generation 上下文

- 将“该回复前最近一个已对账 clarification/duplicate-confirmation marker 所属 generation”作为可恢复状态：marker 与回复分属不同增量页、进程在两页之间重启时，回复仍必须携正确 generation 进入 `ApplyIntakeReply`；不得要求每页重复出现 marker，也不得回退为当前 projection generation。
- cursor 必须按具体回复目标隔离（至少 project + issue target），不能让同项目多个 issue 共享一个目标内不透明 cursor；receipt 去重必须使用 `(project_id, forge_event_id)` 完整身份。
- 增加生产 consumer/daemon 测试：generation 1 marker/旧回复、generation 2 marker/当前回复跨页到达，并在 marker 后重建 consumer/模拟 daemon 重启；断言旧回复仅 `reply_ignored`、当前回复推进、cursor 重放不丢不重。同项目至少两个 awaiting issue，验证 cursor 不串流。
- 扩展 GitHub+GitLab daemon 执行级测试，实际装配并运行各项目 `CommentWorker`，断言 payload `project_id` 的 comment operation 只被对应项目领取；同时保留 due/not-due poll 与 reply consumer 断言。

## 结论

B2 已把 V3 录制 fixture 与关键 fail-closed 矩阵补齐，R3 可判通过；R1/R4 也未回退。但 B1 目前只在“marker、旧回复、新 marker、当前回复同一页”时正确。增量页或重启切在 marker 与回复之间时，generation 上下文归零且 cursor 前移，合法当前代回复会被永久漏掉；cursor/receipt 作用域也不足以支撑多 issue/多项目。因此本轮结论仍为 **FAIL**。
