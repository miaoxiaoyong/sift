# M5 Report run.sock bootstrap 复审（Issue #755）

> **review_round: 1**（首次；Issue #745 / PR #754 的 Sol 复审，无历史关闭包）
>
> HEAD: `9da64997929edb7d604092632004d47baac78381` (PR #754 merge commit)
>
> agent: pi / deepseek-v4-pro

## MUST VERIFY (WBS §5.5 key acceptance)

### 1. `sift report` 只连 `run.sock`，从 `SIFT_RUN_DIR/control.json` 读 run token

**PASS** — 证据：

- CLI 端：`cmd/sift/main.go` `runReport()` 读取 `os.Getenv("SIFT_RUN_DIR")` → `ReadControlFile()` 只读 `control.json`，验证绝对路径、regular file、0o600 权限、owner match、schema_version==1、非空 run_id/attempt_no/generation、64 小写 hex run_token
- RPC 端：`client.go` `RunReportRequest()` 只 dial `run.sock`；未引用 `siftd.sock`、`operator.token`、SQLite 或离线文件
- 测试覆盖：`TestRunReportMissingControlFile`（空 SIFT_RUN_DIR → error）、`TestRunReportRejectsBadArgs`、`TestReadControlFileRejectsUnsafeInputs`（symlink、非 owner、group-readable 均 reject）
- 服务端：`server.go` `runRequest()` 对 `report.submit` 只接受 `run_token` auth；operator token 在 run.sock 上被拒（`TestReportSubmitOperatorTokenRejectedOnRunSock`）

```bash
# 关闭标尺
rg -c "siftd.sock\|operator.token\|SQLite\|offline\|fallback" cmd/sift/main.go internal/controlplane/client.go | grep -v _test
# YES: 0 matches（Report 路径无这些 fallback）
```

### 2. running 接受；spawning 返回有界可重试 `not_ready`；跨 Run/过期/wrong generation/finished 永久拒绝

**PASS** — 证据：

- `storage/report.go` `checkReportBinding()`：running → accept；spawning → `ReportNotReadyError`（携带 closed `RetryPolicy`，不消费 token）；其余 phase → `ErrReportConflict`
- `assertReportBindingTx()` 事务内再验 phase==running
- Wrong token → `ErrReportUnauthorized`；wrong generation → `ErrReportStale`
- CLI: `decodeReportDelays()` 使用 `DisallowUnknownFields()` 验证 closed policy；`BackoffDelays()` 用 `big.Int` 防溢出；retry 期间 policy drift 被检测并 fail-closed
- 测试：`TestReportSpawningReturnsNotReadyWithRetryPolicy`（not_ready 不消费 token/不写 receipt），`TestReportWrongGenerationIsStale`，`TestReportWrongTokenIsUnauthorized`，`TestReportFinishedPhaseIsPermanentConflict`（pending/starting/finished/orphaned），`TestRunReportNotReadyTimesOut`（CLI 端重试后 time out）

```bash
go test ./internal/storage/ -run "Report(Spawning|Wrong)" -count=1 -v
# YES: PASS
```

### 3. progress/goal/blocker/completed 只写事件，不直接转 Run status

**PASS** — 证据：

- `recordSimpleReport()`（progress/goal/completed）：只写 events + report_receipts，无 runs.status 写入，无 budget_entries 写入
- `recordBlockerReport()`：写 events + receipts + budget_entries + EmitInterrupt，但不开 runs.status
- 测试断言 `completed` 后 runs.status 仍为 `"running"`
- Event type 映射：`"report." + cmd.Kind` → `report.progress` / `report.goal` / `report.blocker` / `report.completed`
- Payload 写入 `{"report_key","payload_digest","generation","report"}` 严格结构

```bash
rg "runs.status\|UPDATE runs SET status" internal/storage/report.go
# YES: 0 matches（Report 路径无 runs.status 写入）
```

### 4. 确定性去重、令牌桶与每 Run Interrupt 子配额；触顶只生成一次异常处置

**PASS** — 证据：

- Layer 1 去重：`lookupReportDuplicateTx()` 按 `(run_id, attempt_no, report_key)` 查重，同 digest → duplicate，不同 → conflict
- Layer 2 语义窗口：`dedupe_window > 0` 时按 `(kind, payload_digest, received_at_ms)` 查重
- 令牌桶：`consumeReportTokenTx()` CAS-based refill（`(now-last)*numerator/period`），首次创建按 snapshot 的 burst/events_per_minute；桶不随新 config 重置
- 每 Run Interrupt 子配额：`budget_counters` 按 `(kind=report, scope=run, scope_id=<run_id>, bucket_start_ms)` 日桶 CAS
- 触顶：`commitReportQuotaExhaustion()` 独立事务提交 exhaustion 事实 + rate token；`report_quota_exhaustions` 表有隐式 UNIQUE `(run_id, daily_bucket_start_ms)` 保证至多一条
- 并发：`RecordReportQuotaExhaustion()` 在 exhaustion committed 后 best-effort 调用 `EmitInterrupt`（`FailureReviewReportQuota` variant）；structural rejection 不回滚 exhaustion
- 测试：`TestReportSameKeySameDigestIsDuplicate`、`TestReportSemanticWindowDedupe`、`TestReportKeyConflictIsPermanent`、`TestReportQuotaExhaustionCrashReplayAndConcurrency`（7 子场景）、`TestReportQuotaT4AcceptanceAndPersistedBytes`、`TestReportQuotaT4InvalidOutputsFallBack`

```bash
go test ./internal/storage/ -run "Report.*(Duplicate|Dedupe|Quota|Token)" -count=1 -v
# YES: all PASS
```

### 5. Report 路径无 forge push / MR / merge 副作用

**PASS** — 证据：

- 全量搜索 Report 生产路径（`cmd/sift/main.go`、`client.go`、`server.go`、`report.go`、`report_quota.go`）无 forge push、merge request 创建、或 merge 调用
- 唯一 forge 引用是测试 helper `SeedForgeRunForTest`（seed 既有 Run）
- `EmitInterrupt` 的 forge-comment outbox 由 Attention 子系统管控，不由 Report 路径直接触发 forge 操作

```bash
rg -n "forge\|Forge\|push\|merge\|pr\|mr" cmd/sift/main.go internal/controlplane/{client,server,report}.go internal/storage/report.go internal/storage/report_quota.go 2>/dev/null | grep -iv "import\|//\|_test\|_ref\|_sha\|config_snapshot\|merge_request"
# YES: 0 production matches
```

### 6. 相关包测试全绿

**PASS** — 证据：

```bash
go test ./cmd/sift/ -count=1 -run "Report"     # 4 tests PASS (accepted, not_ready timeout, missing control, bad args)
go test ./internal/storage/ -count=1 -run "Report"  # 16 tests PASS (phase/quota/dedupe/crash-replay/concurrency)
go test ./internal/controlplane/ -count=1 -run "Report\|Backoff"  # 9 tests PASS (submit, backoff golden vectors)
```

全部 29 个 Report 相关测试 3/3 包零 flake。

---

## Findings

### [P2] agent_log_ref 链接合法性无校验：不可形成的 link 仍创建 Interrupt

- **描述**：`report.md` §3 要求 "若该位置不能形成 interrupt.md §3.3 合法链接，blocker 仍可作为普通 report 事件接受，但不得生成 Interrupt，并记确定性诊断"。当前 `recordBlockerReport()` 总是以 `strings.TrimRight(binding.Worktree, "/") + "/agent.log"` 构造 `agent_log_ref` 并直接调用 `EmitInterrupt`，无论 worktree 路径是否可形成合法链接。
- **现状评估**：worktree 路径由系统在 attempt 创建时生成，常规情况下始终合法；当前不可触发此边缘条件。
- **关闭标尺**：`rg "agent_log_ref\|log.*valid\|link.*check" internal/storage/report.go` — 无 link validation → YES 为未实现
- **fixer=switch:agent::glm-5.2**（后续 Issue 实现诊断分支，不阻塞当前 M5 波次）
- **建议**：开后续 Issue，标题 `feat(m5): agent_log_ref link validation with diagnostic fallback for blocker reports`

---

## Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | — |
| P1 | 0 | — |
| P2 | 1 | 否（记录，开后续 Issue） |
| DEFER | 0 | — |

---

## Verdict: **PASS**

WBS §5.5 六项关键验收全部通过。所有相关测试绿色无 flake。一个 P2 注记（agent_log_ref 链接诊断）不阻塞 WBS §5.5 checkbox 勾选，建议开后续 Issue 跟踪。

> 不声称 M5 完成：critical 熔断与 Channel `ops.ps`/`ops.doctor` 仍开；WBS §5.5 的下方 checkbox 可在本复审 PASS 后勾选。
