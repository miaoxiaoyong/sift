---
status: active
date: 2026-08-04
reviewer: pi-minimax-m3
scope: "#852 M6 consolidated phase gate independent audit"

# M6 Consolidated Phase Gate — Independent Audit

## 结论

**PASS WITH NOTES**。M6 自动化阶段门的双 backend 逻辑、PTY/wrapper 拓扑、V2/V4 矩阵和 traceability 证据已具名且可定位；建议开启 M7。该结论不声称真实 Agent/version、真实 GitHub/GitLab、手机端或发布证据，这些仍属于 M7/M8。

本次审核为独立审核，不复用此前 per-slice reviewer 的结论作为唯一依据；逐项抽查了源码测试名、矩阵、WBS、DESIGN §10.1/§12，并执行门禁命令。

## 1. M6 WBS 证据表

| WBS 项 | 具名证据 | 判定 |
|---|---|---|
| tmux 只承载 wrapper；Agent 为 wrapper 直接子进程且同 PGID | `internal/wrapper/wrapper_integration_test.go:TestProductionTmuxWrapperKeepsAgentInWrapperProcessGroup`；#845 commit `9734c56`；process 对照 `TestProductionWrapperKeepsAgentInWrapperProcessGroup` | PASS |
| wrapper-owned PTY 中继到 `agent.log`/pane，session 非事实源 | #844 commit `1bb435b`；wrapper PTY topology/relay tests；tmux lifecycle 与 observer 断言见 #845/#846 | PASS |
| process/tmux 同一 V2/V4 套件 | `internal/launchworker/v2_crash_harness_test.go:TestBackendV2CrashHarness`（table backend）；`internal/daemon/recovery_matrix_test.go:TestRecoveryRowsBackendParameterized`；`TestV4KillRetryBackends`、`TestV4HumanStateInterleavings`、`TestV4FourDiscoverersConverge` | PASS |
| DESIGN §12 V4 全矩阵 | `docs/testing/runtime-matrix.md` V2-01..15、R01..R19、X01..X20；#849 `d32cb1c`、#850 `7bb1709`、#851 `c642791`；逐行 test names 均可定位 | PASS |
| 脱组后代/资格门控 | #847 commit `abcf2e8`；`TestProcessGroupQualification...` 资格 store/gate tests；unverified negative path与 `process_group_unverified` convergence in recovery tests | PASS |
| 双后端 owner、kill/retry cardinality | `TestV4KillRetryBackends/{process,tmux}`；`TestBackendV2OwnerReplacementBarriers`；`TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner` | PASS |
| `sift attach` 只读观察 | #846 commit `be36e3d`；attach closed-RPC/argv/zero-domain-write tests；runtime spec §6 | PASS |

Process/tmux assertions use the same parameterized harnesses and the same wrapper/control/result/process-group evidence chain; tmux session observation is diagnostic only.

## 2. DESIGN traceability inventory

### §10.1 recovery rows

R01–R19 in `docs/testing/runtime-matrix.md` are all marked `covered #850` or covered by the shared #849/#851 suites, with named subtests for both `{process,tmux}`. I spot-checked the source test table and the recovery implementation: session presence/absence never independently changes ownership, completion, retry, or claim state; exact wrapper/control/result/process-group evidence remains authoritative. Unknown identity and unverified process-group outcomes converge to one `startup_stall`, `waiting_human`, frozen isolation, and no unsafe signal.

### §12 V2/V4 vectors

V2-01..V2-15 and X01..X20 are named in `docs/testing/runtime-matrix.md`. The matrix explicitly leaves X18 real Agent qualification to M7 and platform-native Darwin completion to M8; these are intentional scope boundaries, not uncovered M6 rows. X19/X20 are covered by #846/#845 and the shared recovery/handoff suites. No M6 row remains `partial` or `planned` in the current inventory.

## 3. Reproducibility and race evidence

Executed from this worktree:

- `go generate ./...`: PASS
- `go test ./...`: first run had one transient `TestBackendV2CrashHarness/V2/tmux/bootstrap` killed-wrapper crash-window failure; the same named vector rerun with `-count=3`: PASS
- `go vet ./...`: PASS (in the combined gate command)
- `git diff --check`: PASS (in the combined gate command)
- `go test -race ./internal/daemon ./internal/launchworker ./internal/runtime ./internal/wrapper ./internal/storage`: daemon, launchworker, runtime, wrapper passed; the aggregate command timed out during storage after the already-passing packages. The targeted storage race set `go test -race ./internal/storage -run 'TestV2|TestResolveAttemptRace|TestRecord|Test.*Crash' -count=1`: PASS.

The first failure is the known wrapper crash-window timing flake documented by AGENTS and is non-blocking after repeated green execution. No race report was emitted by the completed/targeted race runs.

Baseline #869 is verified by the green `TestProductionBackendFreezeRoutingAndInherit` coverage in the backend routing suite. #863 (daemon effective-backend wiring) is explicitly treated as a known M7-deferred, out-of-scope item; it is not counted as M6 evidence or hidden as complete.

## 4. M1–M3 carryover reconciliation

Based on actual evidence, not the M6 stage-gate inference:

- M1 §1.3 V1/V2 current write ports: **close/stale checkbox may be checked**. V1 and current write-port crash evidence are present; M6 additions V2-12/13/15 are named and tested.
- M2 §2.5 V11 follow-up: **close/stale checkbox may be checked**. Existing M4 audit/Ledger and M5 metric-denominator evidence is cited by the prior phase reviews and current tests; this is not inferred solely from M6.
- M3 §3.4 complete recovery matrix: **check** based on R01–R19 dual-backend evidence.
- M3 §3.4 competition/old generation/heartbeat/backend mismatch: **check** for the M6-delivered synthetic/runtime matrix evidence; real qualification remains M7.
- M3 §3.7 confirmed-absence production branch: **do not claim real Agent qualification**; storage/production mechanism is tested with synthetic qualification and the real Agent/version record remains M7.
- M3 §3.8 hooks baseline/automatic recheck: **check** based on #848 `365a9fa`, `TestHookCrashReplayRecordsOneStableDrift`, and `TestHookRecheckCrashReplayReceiptIsAtomicWithTerminalResult`.

WBS checkbox changes are intentionally limited to items whose current text can point to the above named evidence; no M7/M8 manual evidence is backfilled.

## 5. Explicit non-claims and residue

1. No real Agent CLI/version qualification is claimed; synthetic qualification mechanism only. Formal evidence remains M7.
2. No real Forge project evidence is claimed; GitHub/GitLab adapters and fixtures are not a real PoC run.
3. No phone approval, real HITL device round-trip, or release/archive/clean-machine evidence is claimed; these remain M7/M8.
4. #863 daemon effective-backend wiring is a known deferred scope-out item, explicitly retained for M7.
5. The single observed wrapper crash-window timing flake is recorded as a known non-blocking note; repeated named-vector execution was green.
6. The full aggregate race command did not complete within the time budget; targeted storage race coverage and completed runtime packages were green. This is a reproducibility note, not a claim of an unexecuted full race gate.

## 6. Recommendation

**Recommend opening M7**, because the M6 automated gate is PASS WITH NOTES. Opening M7 must not be described as a complete PoC release: M7 still owns real Agent/version qualification, real Forge runs, concurrent real runs, and phone evidence; M8 owns release and clean-machine evidence.
