FAIL

# S2/M2 Forge & Intake 第二次定向复审（R1–R4 后）

复审基线：`origin/main` / `2da92d9`，工作分支 `chore/s2-m2-rereview-2`。本次只复核[上次复审](2026-07-29-s2-m2-rereview-pi-gpt-5.6-sol.md)的 R1–R4 关闭条件，并以 [`specs/forge.md`](../specs/forge.md)、PRD §4.5/§9.2 与 WBS M2 门禁为判定基准。

R1、R4 已关闭；R2 的配置、收费键、项目路由和按 `next_poll_at_ms` 跳过未到期轮询均已接入，但回复消费者把所有历史评论绑定为**当前** clarification generation，旧代回复仍可被误接纳，且没有要求中的 GitHub+GitLab daemon 执行级测试。R3 的两个已知适配器缺陷已修正，13 动词双平台正常路径矩阵也已建立；但上次明确要求的录制 fixture / 关键 fail-closed 矩阵仍未闭合，故 R2、R3 仍为阻断。

## 执行证据

- `CGO_ENABLED=0 go test ./internal/forge ./internal/intake ./internal/daemon ./internal/storage ./internal/forgebudget ./internal/forgeworker -count=1`：**通过**。
- `CGO_ENABLED=0 go test ./internal/daemon -run 'TestEmptyDBDaemonTickPersistsForgeIntakeAndT1|TestAssembleProbesAndRecordsAutoMergeCapabilityOnEveryStartup|TestAssembleRecordsAmbiguousCapabilityAndFailsOnStorageError' -count=10`：**通过**。
- `CGO_ENABLED=0 go test ./internal/forge -run 'TestV3AllVerbsDualPlatformMatrix|TestGitLabCreateChangeRereadsMissingHeadSHA|TestContractRegressionSuite' -count=10`：**通过**。
- 两次 `CGO_ENABLED=0 go test ./... -count=1`：均仅 `internal/controlplane/TestDoctorBaselineChecksConfiguredDependencies` 在 5 秒 deadline 下失败；单独运行 `CGO_ENABLED=0 go test ./internal/controlplane -run TestDoctorBaselineChecksConfiguredDependencies -count=1` 通过。

全套测试继续呈现前次已记录的 doctor 并行时序 flake；它不属于本轮 R1–R4，也不是本次 FAIL 的原因。

## R1–R4 关闭条件复核

| 项 | 结果 | 代码证据与判定 |
|---|---|---|
| R1 — 启动激活配置投影 | **PASS** | `cmd/siftd/main.go` 在 worker 组装前调用 `DB.ActivateConfig`；`internal/storage/config_activation.go` 在同一事务写入/复用 canonical snapshot、禁用已移除项目并 upsert 当前项目。`internal/daemon/daemon_test.go::TestEmptyDBDaemonTickPersistsForgeIntakeAndT1` 从空 DB 激活最小配置，再用 fixture Runner 执行真实 daemon tick，断言 Forge 预算、receipt、cursor、intake item、T1 trace 与 queued Run 均落库。 |
| R2 — 生产调度、回复消费与项目路由 | **FAIL** | 已关闭部分：`internal/intake/reconciler.go` 设置稳定 tick/project 收费键；`internal/forgeworker/comment.go` 与 `internal/storage/outbox.go::ClaimOutboxOperationKindProject` 按 payload `project_id` 领取，避免跨平台 worker 抢单；`internal/intake/poller.go` 消费 `forge_cursors.next_poll_at_ms`，slow/active/idle 间隔会实际阻止提前请求；`internal/daemon/daemon.go::Tick` 已调用 reply consumer。未关闭部分：`internal/intake/replies.go` 对 `ListIssueComments(..., "")` 返回的每条历史评论都把 `Generation` 赋成 `item.Generation`；它既不从对应的 durable clarification comment operation/marker取得回复所属 generation，也不解析任何 generation identity。于是 daemon 在 generation 2 首次看到 generation 1 的旧 `/sift approve` 时，会把它伪装成 generation 2 交给 `ApplyIntakeReply`；`internal/storage/intake_reply.go` 的 CAS 因收到“当前代”而会接纳。现有 `internal/storage/intake_reply_test.go` 只直接构造正确的旧代命令，没有覆盖生产 consumer。`internal/daemon/daemon_test.go` 也没有上次关闭条件要求的 GitHub+GitLab 双项目执行级 tick；双平台测试仅检查组装对象，空库执行测试只有 GitHub。 |
| R3 — V3 双平台 13 动词契约 | **FAIL** | 代码缺陷已修：`internal/forge/cli.go::CreateChange` 在创建响应有 ID 但缺 SHA 时按 ID 重读，`TestGitLabCreateChangeRereadsMissingHeadSHA` 覆盖该路径；`GetChecks` 以 combined-status envelope 解码 `/status`，`internal/forge/testdata/fixtures/github/statuses_contract.json` 与 `TestContractRegressionSuite` 覆盖真实响应形状。`internal/forge/verb_matrix_test.go::TestV3AllVerbsDualPlatformMatrix` 也让 13 动词逐项跑 GitHub/GitLab 正常路径。然而 Forge spec §10 和上次关闭条件要求录制 fixture 入库，并让显式矩阵同时证明关键 fail-closed；当前矩阵由 `matrixRunner` 动态拼装/catch-all 响应，仅验证成功路径。`testdata/fixtures` 仍没有 `CommentTarget`、`CreateChange`、`ListChangeComments` 的双平台正常响应 fixture，也没有这三项在显式矩阵中的关键拒绝断言。故实现修正通过，但 V3 的测试契约关闭条件尚未全部满足。 |
| R4 — 启动能力探测接生产 | **PASS** | `internal/daemon/daemon.go::assemble` 对每个 enabled project 在构造 workers 前调用 `ProbeAndRecordAutoMergeCapability`；`internal/forge/cli.go` 将歧义保留为 false，并只在证明成功时设置进程内 capability；`internal/storage/project_capability.go` 持久化 `capabilities_json`、checked time 与审计事件。`TestAssembleProbesAndRecordsAutoMergeCapabilityOnEveryStartup` 覆盖重启重探；`TestAssembleRecordsAmbiguousCapabilityAndFailsOnStorageError` 覆盖歧义仍启动且 false、存储失败拒绝启动。Merge 仍同时检查进程内与持久化证明。 |

## M2 门禁更新

| WBS M2 门禁 | 结果 |
|---|---|
| V3 通过；V7 的 Forge/marker/CAS 部分通过 | **FAIL** — marker/CAS 行为保留通过；13 动词正常矩阵已补，但 R3 的录制 fixture 与关键 fail-closed 契约矩阵未闭合，V3 不能判通过。 |
| 条件合并能力缺失时 `auto_merge` 被结构性禁用 | **PASS** — 启动逐项目探测、false 持久化、重启重探与双重 merge 检查均已接生产。 |
| actor 缺失事件被忽略；坏项目不影响健康项目 | **PASS** — 双平台 actor fail-closed fixture、Poller/Reconciler 项目隔离测试与逐项目生产组装成立。 |
| Intake crash marker 与旧 generation 回复仲裁测试通过 | **FAIL** — comment worker crash replay 仍通过；storage CAS 单测通过，但生产 ReplyConsumer 会把历史评论重标为当前 generation，未证明旧代回复仲裁。 |
| V11 外部事实收敛首段通过 | **PASS** — Reconciler 具稳定收费键并进入 `Daemon.Tick`；项目投影已激活，组件测试覆盖外部 merge → `done + gate_bypassed` 及关闭事实。 |

M2 组合门禁仍为 **FAIL**，M3 前置不得勾选。

## 新的阻断关闭条件

### B1 — 回复 generation 必须来自回复所针对的 durable generation（R2）

- 为 clarification/duplicate-confirmation 评论建立可由回复消费路径确定的 generation identity（例如受控 marker/nonce 与 durable operation 对账）；禁止用当前 `intake_items.clarification_generation` 给所有历史评论补代号。
- ReplyConsumer 使用增量 cursor/receipt，且把历史旧代回复作为 `reply_ignored` 审计，不得推进当前 intake。
- 增加生产 consumer 测试：generation 1 评论发出后推进到 generation 2，daemon 首次/重启后同时读到旧回复与当前回复，断言旧回复只审计、仅当前回复可推进。
- 增加 GitHub+GitLab 两项目 daemon 执行级测试，实际执行 due/未 due 调度、reply consumer 与项目定向 comment claim；不得只检查对象数量和指针。

### B2 — 用录制 fixture 闭合显式 13×2 V3 矩阵（R3）

- 将双平台 `CommentTarget`、`CreateChange`、`ListChangeComments` 的正常响应以录制 fixture 放入 `internal/forge/testdata/fixtures/{github,gitlab}/`；GitLab CreateChange 需保留缺 SHA 后重读 fixture，GitHub Checks 保留 combined-status envelope。
- 在显式双平台矩阵中为上述动词及其关键边界加入 fail-closed 断言（至少缺 comment ID、创建重读后仍缺 head、comment actor 缺失丢弃），并继续覆盖 marker 冲突、unknown 归一与 merge CAS。

## 结论

R1 已使 `siftd` 能从空库按配置建立真实项目身份并执行收费 Intake/T1；R4 已把能力探测真正前移到 worker 启动前。R2 的大部分生产接线也已落地，R3 的两个已知适配器 bug 已修。但旧 generation 回复在真实 consumer 中仍可被错误提升为当前代，且 V3 仍未满足规格要求的录制 fixture / 关键 fail-closed 显式矩阵。因此本轮结论为 **FAIL**。
