PASS WITH NOTES

# S2/M2 Forge & Intake 第四次定向复审（C1 后）

复审基线：`origin/main` / `ae1f256`（已合入 #83 C1），工作分支 `chore/s2-m2-rereview-4`。本次只复核[上次报告](2026-07-29-s2-m2-rereview-3-pi-gpt-5.6-sol.md)的 C1 关闭条件，并以 [`specs/forge.md`](../specs/forge.md)、PRD §4.5/§9.2 与 WBS M2 为判定基准。

C1 的生产正确性阻断已关闭：回复目标现在以 `(project_id, issue_id)` 持久化 cursor 与最近 marker generation，上下文可跨增量页和进程重启恢复；receipt 查询使用完整 `(project_id, forge_event_id)` 身份；双平台 daemon 已实际运行按 payload project 定向的 comment worker。B1、R2 与 Intake 旧 generation 门禁可更新为 PASS，M2 组合门禁通过。

## 执行证据

- `CGO_ENABLED=0 go test ./internal/forge ./internal/intake ./internal/daemon ./internal/storage ./internal/forgebudget ./internal/forgeworker -count=1`：**通过**。
- `CGO_ENABLED=0 go test ./internal/intake ./internal/daemon ./internal/storage -run 'TestReplyConsumer(UsesPersistedGenerationAfterRestart|PersistsGenerationAcrossPagesAndRestart|BindsRepliesToClarificationGeneration)|TestV7ReplyCursorIsolatedByIssue|TestDaemonTick(CommentWorkersClaimOnlyTheirPayloadProject|ExecutesDueGitHubAndGitLabRepliesWhileSkippingNotDuePoll)' -count=20`：**通过**。
- 两次 `CGO_ENABLED=0 go test ./... -count=1`：均仅 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 在共享 5 秒 deadline 下失败；目标包均通过。该测试单独重跑先复现一次 deadline，随后重跑通过（约 2 秒），确认是此前复审已记录的 doctor 时序 flake，不是 C1/M2 回归。

## C1 关闭条件复核

| C1 子项 | 结果 | 代码与测试证据 |
|---|---|---|
| generation 上下文跨增量页/重启可恢复 | **PASS** | migration `0003_reply_target_state.sql` 新增以 `(project_id, issue_id)` 为主键的 `forge_reply_state`，同时保存不透明 cursor、generation 与 marker 时间。`ReplyConsumer` 从 `ReplyState` 恢复上下文，先持久化更新后的 marker generation，再推进目标 cursor；崩溃发生在两次写入之间时只会重放，receipt 保证幂等，不会跳项。`TestReplyConsumerUsesPersistedGenerationAfterRestart` 覆盖 marker 与合法回复分属两次增量读取且中间重建 consumer；`TestReplyConsumerPersistsGenerationAcrossPagesAndRestart` 覆盖 generation 1 marker/旧回复、generation 2 marker/当前回复与重启，断言旧回复审计、当前回复推进。两项重复 20 次通过。 |
| cursor 按回复目标隔离 | **PASS** | 旧的项目级 `issue_comments` cursor 已从生产 reply path 移除；读取和保存均按 `project.ID + item.IssueID` 使用 `forge_reply_state`。`TestV7ReplyCursorIsolatedByIssue` 证明同项目 issue A/B 的 cursor 独立更新。 |
| receipt 使用完整项目身份 | **PASS** | `ApplyIntakeReply` 先从 intake item 取得 project，再以 `WHERE project_id=? AND forge_event_id=?` 查重，与表的唯一约束一致；跨项目相同 comment ID 不再互相吞掉。 |
| GitHub/GitLab daemon 实际执行 project-scoped comment claim | **PASS** | `TestDaemonTickCommentWorkersClaimOnlyTheirPayloadProject` 为双平台各装配一个 `CommentWorker` 并经 `Daemon.Tick` 执行，断言 GitHub/GitLab 各自只发送本项目 payload、无交叉领取；既有 dual-platform daemon 测试继续覆盖 due/not-due poll 与两平台 reply consumer。 |

## B1/B2 与 R1–R4 回看

| 项 | 结果 | 判定 |
|---|---|---|
| B1 — durable generation、目标级 cursor/receipt、双平台 daemon | **PASS** | C1 已补齐上次报告列出的四个生产阻断；跨页/重启旧代仲裁与项目定向 comment worker 均有执行证据。 |
| B2 — 双平台 V3 录制 fixture 矩阵 | **PASS，无回退** | C1 未改 Forge adapter/fixture 契约；目标包全量测试通过。 |
| R1 — 启动激活配置投影 | **PASS，无回退** | C1 未改启动激活链；daemon/storage 测试通过。 |
| R2 — 生产调度、回复消费与项目路由 | **PASS** | 目标级 durable state、完整 receipt identity 与双平台 project-scoped comment worker 已接生产路径；此前漏回复/串流阻断关闭。 |
| R3 — V3 双平台 13 动词契约 | **PASS，无回退** | B2 结论保持；Forge 测试通过。 |
| R4 — 启动能力探测接生产 | **PASS，无回退** | C1 未改能力探测链；daemon 测试通过。 |

## M2 门禁表

| WBS M2 门禁 | 结果 |
|---|---|
| V3 通过；V7 的 Forge/marker/CAS 部分通过 | **PASS** — B2 录制矩阵、marker/CAS 证据无回退；C1 补齐 reply 目标级幂等状态。 |
| 条件合并能力缺失时 `auto_merge` 被结构性禁用 | **PASS，无回退**。 |
| actor 缺失事件被忽略；坏项目不影响健康项目 | **PASS，无回退**。 |
| Intake crash marker 与旧 generation 回复仲裁测试通过 | **PASS** — crash marker 重放证据保留；generation 现可跨页/重启恢复，旧代只审计、当前代推进。 |
| V11 外部事实收敛首段通过 | **PASS，无回退**。 |

M2 组合门禁为 **PASS**，可进入 M3。

## 非阻断注记

1. 同项目双 issue 的回归目前在 storage 层直接验证 cursor 隔离；生产 `ReplyConsumer` 的跨页/重启测试使用单 issue。两组证据与生产循环代码足以证明本次正确性，但后续可把“双 issue + consumer”合成一条集成测试，降低未来接线回归风险。
2. `TestDoctorBaselineChecksConfiguredDependencies` 的共享 5 秒 deadline flake 本轮再次出现，并导致两次 `./...` 未全绿；目标包和 C1 聚焦重复测试均稳定通过。该 flake 不属于 M2/C1，但应在后续独立处理。

## 结论

C1 已把 generation identity、目标级 cursor、完整 receipt identity 和双平台 comment claim 全部接入持久化生产路径；上次可永久漏掉合法当前代回复的问题已关闭。B1/R2 与 Intake 仲裁门禁转为 PASS，B2/R1/R3/R4 及其余 M2 门禁无回退。仅保留测试组合粒度与既知 doctor 时序 flake 两项非阻断注记，因此结论为 **PASS WITH NOTES**。
