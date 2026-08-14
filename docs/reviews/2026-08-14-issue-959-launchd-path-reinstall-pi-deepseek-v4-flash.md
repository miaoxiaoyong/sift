---
title: "Issue #959 round-1 review — launchd PATH + reinstall reliability — PASS"
date: 2026-08-14
reviewer: pi / deepseek-v4-flash
issue: "https://github.com/xsift/sift/issues/959"
branch: "bugfix/issue-959-make-launchd-service-path-and-reinstall-reliable"
status: active
---

# Issue #959 独立复审（round 1）— launchd PATH + 重复 install 可靠性 — PASS

复审对象：`origin/bugfix/issue-959-make-launchd-service-path-and-reinstall-reliable`（tip `9a4e4b7 fix(hosting): reload launchd unit on install`），base `origin/main`（`fc003f8`）。

## review_round: 1

Issue 无历史评论 / 无上一轮关闭包 / 无 MR，本包为完整关闭包。

## 验证命令（全部实跑，非推断）

- `go test ./internal/hosting/...` → `ok`（18.9s；含 `plutil -lint` darwin 用例 `TestRenderEscapesUserPathsInUnits` 真实通过）
- `go test ./cmd/sift/...` → `ok`（116.1s；新增 `TestServiceInstallLaunchdReplacesFreshAndCurrentUnit{/fresh,/current-loaded}`、`TestServiceInstallLaunchdSurfacesBootstrapFailure` 实跑非 skip）
- `go build ./...`、`go vet ./internal/hosting/... ./cmd/sift/...` → 通过
- `gofmt -l`、`git diff --check` → 干净

## Acceptance 逐条核对

| 验收 | 证据 | 结果 |
|---|---|---|
| plist 含闭集 PATH + SIFT_HOME | `launchdTemplate` `EnvironmentVariables{PATH,SIFT_HOME}`；`TestRenderLaunchdTemplateContract` 断言 PATH 恰为 `LaunchdPath`；plutil lint 通过 | ✅ |
| 重复 install 应用新 plist、无 `Load failed` 假成功 | install plan = `bootout gui/<uid>/cn.hexai.sift`（exit3+No such process 视为 fresh）→ `bootstrap gui/<uid> <plist>`；`Exec` 传播 bootstrap 失败；CLI 层 3 测试 + `Exec` 层 2 测试覆盖 fresh/current-loaded/command-failure | ✅ |
| daemon 侧 doctor 可解析 Homebrew gh/glab | launchd 以 plist `EnvironmentVariables` 作进程 env → daemon 不重置 PATH → `doctor.commandCheck`/`forge-cli:*` 的 `exec.LookPath` 走该 PATH，`/opt/homebrew/bin`/`/usr/local/bin` 命中；agent/brain/tmux probe 同受益 | ✅（构造性，见 DEFER-1） |
| `go test ./cmd/sift/... ./internal/hosting/...`、build、vet 通过 | 上述实跑 | ✅ |
| 无 launchd label 回归 | `Label=cn.hexai.sift`、`LegacyLabel=com.miaoxiaoyong.sift` 未动；migration 幂等路径未动；systemd 后端零改动（无 BeforeCmd） | ✅ |

## Scope 逐条核对

- 闭集 PATH（含 Apple Silicon/Intel Homebrew + system bins），不复制交互 shell PATH ✅
- init 探测的 agent 可执行保持绝对路径，未以 PATH 展开替代配置正确性（本改动零触及 init/config normalize；docs 重申「初始化时解析为绝对路径」）✅
- launchd install 幂等替换/重载已加载单元 ✅（`bootout`→`bootstrap` 是 launchd 官方 replacement 模式）
- launchctl 失败不得假报成功 ✅（bootstrap 失败传播 exit 1；bootout 仅 exit3+No such process 容错）
- Label/LegacyLabel 保留 ✅
- paused-run recovery 跨受控重启保持 ✅（daemon 启动时 `RecoverStartup` boot-barrier + `Recover`；`cmd/sift/daemon.go` `signal.NotifyContext(..., SIGTERM)` 优雅退出；bootout→bootstrap 即受控重启）
- render/plan/CLI 测试齐备（fresh、current-replacement、legacy cleanup 既有测试、command failure、PATH 内容）✅
- hosting spec/troubleshooting 已随行为更新 ✅

## 额外核对

- `Plan` 新增 `BeforeCmd` 字段：全部构造点均具名字段，无位置构造破坏；仅 launchd install plan 设置 BeforeCmd，uninstall/stop/restart/status/update.go 路径不受影响。
- `IsAlreadyUnloaded` 对 `runCommand` 的 `%w` 包装错误经 `errors.As` 解包仍成立（exit 3 + 消息含 "no such process"，CombinedOutput 覆盖 stderr）。uninstall/stop 既有分支不受新包装影响。
- systemd daemon-reload 条件由 `name == "systemctl"` 改为 `plan.RunCmd[0] == "systemctl"`，语义等价。
- 失败时序自愈：新 plist 先落盘 → bootout 旧 job → bootstrap 失败时 job 未加载但 plist 在盘、错误 loud（exit 1）；重跑 install 或 `sift service start`（`launchctl load` 对未加载 label 正常）可自愈，无静默坏态。

## Finding 列表

### [P2-1] 无 GUI（Aqua）会话 / SSH 场景的 `bootstrap gui/<uid>` 行为未文档化
- 一句话：`launchctl bootstrap gui/<uid>` 要求用户已登录图形会话；纯 SSH（无 loginwindow）下新 install 会以 `Bootstrap failed: 5: Input/output error` 硬失败（旧 `launchctl load` 会落到会话域“看似成功”，但 agent 随会话消亡、也不登录自启——旧行为本就偏离产品契约）；另 v0.5.5 曾经 SSH 安装的 agent 留在 `user/<uid>` 域，gui 域 bootout 对其为 No such process（被容错），若随后在 GUI 会话重复 install 可能短暂并存两个域实例争 SIFT_HOME 锁。
- 关闭标尺：`grep -q "GUI" docs/specs/hosting.md && grep -q "SSH\|ssh" docs/runbooks/troubleshooting.md` → YES（hosting.md §3 注明 gui 域前提 + SSH/无图形会话走 foreground 提示；troubleshooting 加一行同场景指引）。
- 证据缺口：真实无 GUI 会话环境的失败/回退行为无自动化测试（launchctl 假桩仅覆盖 exit3/exit1，未覆盖 gui 域不存在）；跨域并存仅推理未实测。
- fixer=same（文档级补充，实现语义正确）。

### [P2-2] `sift service start` 对已加载 label 仍存在 `Load failed: 5` 假成功（越界遗留）
- 一句话：`planStart` 仍用 `launchctl load`，label 已加载时 launchctl 打印 `Load failed: 5: Input/output error` 却 exit 0，CLI 报成功——与本 issue 为 install 修复的假成功同类，但超出本 issue Scope（仅 install），留 backlog。
- 关闭标尺：开后续 issue 将 start 改为 label 域语义（`bootstrap`/`kickstart` 幂等路径）或至少容错 `Load failed: 5` → YES。
- 证据缺口：无；行为由既有 `launchctl load` 语义决定。
- fixer=same（后续 issue）。

### [DEFER-1] 真实 macOS 安装后 daemon 侧 doctor 解析 gh/glab 无自动化集成证据
- 一句话：验收「真实安装后 daemon 侧 doctor 可解析 Homebrew gh/glab」仅由 plist PATH 内容断言（单元级代理）保证，真实 launchd 环境变量注入 → 进程 env → `LookPath` 全链无 CI 集成测试。
- 关闭标尺：`scripts/hosting-smoke.sh` 增加 PATH 解析断言（真实 supervisor 机器：install 后 `launchctl print gui/<uid>/cn.hexai.sift` 或 daemon doctor 输出含 gh/glab 绝对路径）→ YES；或 M8 发布验证人工确认后关闭。
- 证据缺口：CI 无真实 launchd 环境；属发布级验证，非本轮可自动化。
- fixer=same（backlog / M8 发布证据）。

## Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | 是 |
| P1 | 0 | 是 |
| P2 | 2 | 否（记录：P2-1 文档补充、P2-2 后续 issue） |
| DEFER | 1 | 否（backlog：M8 发布验证） |

## Verdict

**PASS** — P0/P1 全关（各 0 项）；接受项全部满足，测试/build/vet/gofmt 实跑通过，无 label 回归、无假成功残留。P2/DEFER 仅记录，不进当前 MR。可进入 MR 创建与合并流程。
