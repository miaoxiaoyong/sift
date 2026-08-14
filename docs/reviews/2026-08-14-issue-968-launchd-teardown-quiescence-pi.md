---
title: "Issue #968 round-1 review — launchd reinstall teardown/bootstrap race — PASS WITH NOTES"
date: 2026-08-14
reviewer: pi / minimax-m3
issue: "https://github.com/xsift/sift/issues/968"
branch: "chore/issue-968-review"
tip: "f7c6881 fix(hosting): close launchd reinstall teardown/bootstrap race (#968)"
status: active
---

# Issue #968 独立复审（round 1）— launchd reinstall teardown/bootstrap race — PASS WITH NOTES

复审对象：`chore/issue-968-review`（tip `f7c6881 fix(hosting): close launchd reinstall teardown/bootstrap race (#968)`），base `origin/main`（`1ae3fae`）。Issue 无历史评论 / 无上一轮关闭包 / 无 MR；本包为完整关闭包（**第一轮**）。

## review_round: 1

## 验证命令（全部实跑，非推断）

- `go build ./...` → 通过
- `go vet ./internal/hosting/... ./cmd/sift/...` → 通过
- `gofmt -l .` → 干净
- `git diff --check origin/main...HEAD` → 干净
- `go test ./internal/hosting/... -count=1 -race` → `ok`（17.3s）
- `go test ./internal/hosting/... -run TestLaunchdInstall -count=1 -v` → 9 个子用例 PASS（ImmediateSuccess ×2、DelayedDisappearance、TransientBootstrapThenSuccess、RetryExhaustion、TeardownTimeoutNeverBootstraps、PermissionErrorDoesNotRetry、DomainErrorDoesNotRetry、AmbiguousStateDoesNotRetry；含 `TestLaunchdInstallPlanReplacesCurrentUnit` 旧 plan 契约保留用例）
- `go test ./cmd/sift/... -count=1 -timeout 360s` → `ok`（140.8s，含 `TestServiceInstallLaunchdReplacesFreshAndCurrentUnit` 改写为 bootout→print→bootstrap 序列、`TestServiceInstallLaunchdRetriesTransientBootstrap` 端到端瞬时重试契约）
- `go test ./internal/hosting/... ./cmd/sift/... -count=1 -race` → 双包 OK（首次跑命中 `TestNonInteractiveAgentAddReportsVersion` 单次 flake，重跑独立该用例 OK；与 #968 无关，与 #960/agent-add probing 配置态在 race 下偶发；本轮不阻塞）

## Acceptance 逐条核对（#968 八项需求 + #959/#965 保留）

| #968 验收项 | 证据 | 结果 |
|---|---|---|
| 1. bootout 成功后有界等待 launchctl 确认 absent 才 bootstrap | `planInstall` 写入 `AbsenceProbe: []string{"launchctl","print","gui/<uid>/<Label>"}`；`execLaunchdInstall` 经 `waitForLaunchdAbsent` 有界轮询 `launchdTeardownTimeout=5s / launchdTeardownPoll=250ms`；`TestLaunchdInstallDelayedDisappearance` 实测 print 探针 2 次（present → absent）后 bootstrap 1 次 | ✅ |
| 2. bootstrap 瞬时 exit 5 且缺席仍确认时有界重试 | 重试预算 `launchdBootstrapRetries=2`（共 3 次）、`launchdBootstrapBackoff=500ms`，重试前重探 absence（`execLaunchdInstall` 中 `runCommand(plan.AbsenceProbe)`）；`TestLaunchdInstallTransientBootstrapThenSuccess` 断言 print=2、bootstrap=2 | ✅ |
| 3. 权限 / no-GUI domain / malformed plist / ambiguous-or-present 一律不重试 | `isServiceAbsent` 与 `isTransientBootstrapFailure` 共用 hard-marker 黑名单（`permission denied` / `operation not permitted` / `could not find domain` / `domain unavailable` / `invalid property list` / `malformed`），均优先于 transient 文本；`TestLaunchdInstallPermissionErrorDoesNotRetry` / `TestLaunchdInstallDomainErrorDoesNotRetry` / `TestLaunchdInstallAmbiguousStateDoesNotRetry` 各自断言 bootstrap 调用数 = 1 | ✅ |
| 4. 耗尽返回非 0 且可操作错误（重跑 install） | `execLaunchdInstall` 末尾 `fmt.Errorf("hosting: launchctl bootstrap failed %d consecutive times with transient exit 5 ...; aborting install — rerun \`sift service install\`")`；teardown 超时 `fmt.Errorf("launchd label %s is still loaded %s after bootout; ...; aborting install — rerun \`sift service install\`")`；`TestLaunchdInstallRetryExhaustion` 断言错误含 `rerun/retry sift service install` 提示 | ✅ |
| 5. 注入 clock/sleeper/runner，测试确定性且毫秒级 | 包级 `sleep = time.Sleep` 可被 `pinLaunchdPacing` 替换为 `sleepRecorder`；`launchdTeardownTimeout/Poll` 与 `launchdBootstrapRetries/Backoff` 均为可变包级变量，`pinLaunchdPacing` 将窗口缩小到 `60ms/5ms`（deadline/poll）和 `5ms` 回退（重试），且 `t.Cleanup` 完整还原；全 install 序列测试总时长 8.7s | ✅ |
| 6. 命令序列测试覆盖：immediate success（fresh + current-loaded）、delayed disappearance、transient → success、retry exhaustion、permission 无重试、domain 无重试；外加 teardown timeout（never bootstraps）与 ambiguous-state | `hosting_test.go` 共 8 个新 `TestLaunchdInstall*`，全部 PASS | ✅ |
| 7. 保留 #959（固定 PATH / labels）+ #965（loaded→kickstart / unloaded→bootstrap / 真实失败传播 / SSH-no-GUI 提示） | `Plan` 仅新增 `AbsenceProbe` 一字段；`BeforeCmd` / `ProbeCmd` / `RunCmd` / `FallbackCmd` / `StartUnit` 排版零改动；`planInstall` / `planStart` / `BeforeCmd` 在 `Exec` 中的执行序与 `IsAlreadyUnloaded` 仅 fresh install 的 bootout exit-3 容忍面均未改动；`launchdTemplate` PATH 与 label 零改动 | ✅ |
| 8. hosting spec / testing / runbook 同步 | `specs/hosting.md §install` 行改写（说明有界等待 + 瞬时退避 + 边界重试不重试规则 + Issue 锚点）；`runbooks/troubleshooting.md` 新增「重复 sift service install 时 ...」段；`testing/hosting.md §1 金字塔 + §13 安装 teardown quiescence 序列与 §14 smoke 失败条件 + §3 fake launchctl 描述全部同步；hosting-smoke-test.sh `write_fake_launchctl` 增加 `print` case 回答 install 缺席探针；hosting-smoke.sh `socket_responds` 切换为 `sift doctor --json` 显式判定（防 stale socket 通过，仅观 doctor stdout 的 `"ok"` envelope） | ✅ |

## Scope 核对

- 改动面 = `internal/hosting/{hosting.go,hosting_test.go}`、`cmd/sift/main_test.go`、3 份 docs、`scripts/hosting-smoke{,-test}.sh`，共 8 文件 553 增 30 删；**无 daemon/brain/forge/operator 控制面**改动 ✅
- launchd 模板、label / LegacyLabel、`launchdTemplate` PATH、`osUserUID()` helper 全部零改动 ✅
- systemd 后端零改动（`planInstall` systemd 分支原样）✅
- 前台 backend（无 supervisor）`sift service install` 仍走 `ErrNoBackend` → foreground 提示 exit 0，语义未变 ✅
- start 路径零改动（`execLaunchdStart` 与 `planStart` 无改动），`AbsenceProbe` 字段仅 launchd install plan 设置 ✅
- 失败自愈路径：teardown 超时/权限/no-GUI/耗尽均返回非 0，plist 保留（未触发 `WriteFile` 写入重置），重跑 install 可自愈 ✅
- `BeforeCmd` bootout 失败（非 `IsAlreadyUnloaded` 形态）仍按既有 `Exec` 行为直接返回，**不**经过 `AbsenceProbe`（设计正确：bootout EIO 不在 #968 已知瞬时口径内）✅

## Finding 列表

### [P2-1] production pacing 全局变量 + 注入 sleeper 在 `t.Parallel()` 下不安全
- 一句话：`launchdTeardownTimeout/launchdTeardownPoll/launchdBootstrapRetries/launchdBootstrapBackoff/sleep` 均为包级可变状态，`pinLaunchdPacing(t)` 用 `t.Cleanup` 还原。`t.Parallel()` 启用时，多 goroutine 共享并互踩这些全局变量，cleanup 顺序在失败路径下也会泄漏到下一个测试；当前 8 个新测试均无 `t.Parallel()` 所以安全，但本质脆弱。
- 关闭标尺：把 pacing 包装成 `pacing` 结构体（timeout/poll/retries/backoff/sleep）按 `Exec(plan, pacing...)` 或 plan 字段注入；新测试改用结构体，cleanup 改为 plan 局部变量，现状全局变量保留为兼容默认 → YES；或加显式 `if t.Parallel() { t.Fatal("pacing globals not safe under parallel; file an issue") }` 防回归。
- 证据缺口：无（功能正确），仅并发模式风险。
- fixer=same

### [P2-2] "service absent" / "transient bootstrap" 仍按 stderr 文本分类
- 一句话：`isServiceAbsent` 与 `isTransientBootstrapFailure` 以文本匹配为契约（`no such process` / `could not find service` / `service not found` / `does not exist`；`input/output error` / `eio`）；launchd 未来文本漂移会导致瞬时/缺席分类静默失准。
- 关闭标尺：在 `isServiceAbsent` 加上 launchctl 文档化的 service-not-found 退出码（如 113 / `exitErr.ExitCode()!=0 && exitErr.ExitCode()!=3` 黑名单）作为补充门控，使契约不仅依赖文本；新增 release 时跑一遍真实 launchctl 仍为权威证据 → YES。
- 证据缺口：无；纯鲁棒性，与 #965 P2-1 同类。
- fixer=same

### [P2-3] `launchctl print` 失败时 `isServiceAbsent` 把 domain 不可用列入 hard-marker，但 host-label 已被 bootout 成功删除的语义边界未断言
- 一句话：`isServiceAbsent` 把 `could not find domain` 列入 hard-marker（不视为 absent），正确避免了 `launchdDomainHint` 漂移；但 `TestLaunchdInstallImmediateSuccess` 没专门覆盖"bootout 成功 + print 返回 domain-not-found" 序列，hosting 层只覆盖了 bootout-success + print-absent。
- 关闭标尺：`TestLaunchdInstallImmediateSuccess` 增加子用例 `bootoutAbsent: true` 已隐含 bootout 成功 print-absent；新增 `TestLaunchdInstallDomainUnavailableAfterBootoutAbortsBeforeBootstrap`（脚本 print 返 `Could not find domain` exit 112）→ bootstrap 调用数 = 0 且错误含 SSH/foreground 提示 → YES。
- 证据缺口：当前用例通过不等于语义边界就位；纯测试覆盖广度。
- fixer=same

### [P2-4] `launchdTeardownTimeout=5s` 操作员不可配，繁忙系统可能误判
- 一句话：5s 阈值固定为编译期常量，源自从 #968 复现观察窗口；CI / 重载期间 launchd 可能 > 5s 完成 teardown，触发"did not quiesce"假失败 → 用户重跑。`launchdBootstrapRetries=2 / launchdBootstrapBackoff=500ms` 同理。
- 关闭标尺：把三类阈值接受 env override（`SIFT_LAUNCHD_INSTALL_TIMEOUT` / `SIFT_LAUNCHD_INSTALL_RETRIES` / `SIFT_LAUNCHD_INSTALL_BACKOFF`），smoke 套件断言 env 注入生效；或留 #968 DEFER 化后续 host-pacing 调优 issue → YES。
- 证据缺口：仅推理（无繁忙 macOS 实测）；不在 #968 scope。
- fixer=same（backlog / 后续 host-tuning issue）

### [DEFER-1] 真实 macOS 上 5/5 连续 `sift service install` 端到端需硬件与本机 GUI 域
- 一句话：脚本化 fake launchctl 已覆盖全部 8 个语义分支，但"daemon 真实起 / socket 真实 healthy / supervisor 真实接管" 只能在 macOS 上真测；与 #959 DEFER-1 / #965 DEFER 同类。
- 关闭标尺：M7 / M8 阶段在干净 mac 上 `for i in 1..5; do sift service install && sleep 2; done` 后 `sift doctor --json` 返回 ok、`[ -S "$SIFT_HOME/siftd.sock" ]` 存在 → YES。
- 证据缺口：本环境非 macOS；CI runner 默认 ubuntu。
- fixer=same（backlog / M7-M8 真实发布证据）

### [DEFER-2] 跨域（legacy user/<uid> + 新 gui/<uid>）共存未处理
- 一句话：#959 P2-1 / #965 DEFER-2 已记；本轮只修同 label bootout→bootstrap，不涉及清理旧域残留；与 #968 无耦合。
- 关闭标尺：后续 legacy-domain-migration issue 覆盖 start 前清理或文档警告 → YES。
- 证据缺口：仅推理（需 v0.5.5 经 SSH `launchctl load` 复现路径）；与 #968 同源 race 不同维度。
- fixer=same（backlog）

### [DEFER-3] 用 `launchctl print` 而非 `launchctl print-cache` 探缺席
- 一句话：launchd 在 macOS 12+ 提供 `print-cache` 专用于 label 是否在缓存中存在；`print` 会实际查询 job state。负载高时 `print` 可能更慢。Fake launchctl 单元测试不受影响，smoke harness 也不依赖真实 launchd。
- 关闭标尺：实测 `print-cache` 与 `print` 在 absent 情形下的延迟与文本一致后切换 → YES；或留 #968 后续 perf 调优。
- 证据缺口：纯性能维度，非 #968 必需。
- fixer=same（backlog / perf follow-up）

## Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | 是 |
| P1 | 0 | 是 |
| P2 | 4 | 否（记录） |
| DEFER | 3 | 否（backlog） |

## Verdict

**PASS WITH NOTES** — P0/P1 全关（各 0 项）。`sift service install` 在 darwin 上现以 bootout → 有界 print 等待 teardown quiescence → bootstrap 三段式重写，瞬时 exit 5（Input/output error）有有界退避 + 缺席重探；权限 / no-GUI domain / malformed / ambiguous-or-present 一律不重试立即失败；耗尽与超时均返回可操作的 `rerun sift service install` 错误；#959 PATH / #965 start 语义与所有既有 plist 写入路径零回归。8 个新 hosting 层 + 1 个 CLI 端到端 `TestServiceInstallLaunchdRetriesTransientBootstrap` + 改写的 `TestServiceInstallLaunchdReplacesFreshAndCurrentUnit` 实跑 PASS，build / vet / gofmt / race 干净，`internal/hosting` race=ok。P2/DEFER 仅记录不进当前 MR；可进入 MR 创建与合并流程。须在合并前由实施方在真 macOS 上完成 5/5 连续 install 复现（#968 acceptance #1 / M7-M8 真实发布证据）。
