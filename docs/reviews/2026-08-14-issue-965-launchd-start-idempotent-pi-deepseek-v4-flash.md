---
title: "Issue #965 round-1 review — launchd service start idempotence + no-GUI diagnosis — PASS"
date: 2026-08-14
reviewer: pi / deepseek-v4-flash
issue: "https://github.com/xsift/sift/issues/965"
branch: "bugfix/issue-965-make-launchd-service-start-idempotent-and-diagno"
status: active
---

# Issue #965 独立复审（round 1）— launchd start 幂等 + 无 GUI 会话诊断 — PASS

复审对象：`bugfix/issue-965-make-launchd-service-start-idempotent-and-diagno`（tip `528c684 fix: make launchd service start idempotent`），base `origin/main`（`52f0c96`）。Issue 无历史评论 / 无上一轮关闭包 / 无 MR，本包为完整关闭包。

## review_round: 1

## 验证命令（全部实跑，非推断）

- `go test ./internal/hosting/...` → `ok`（6.9s；darwin 实跑，非 skip）
- `go test ./cmd/sift/...` → `ok`（144.5s；`TestServiceStartLaunchdIsIdempotentWithoutRewritingUnit` darwin 实跑）
- `go build ./...`、`go vet ./internal/hosting/... ./cmd/sift/...` → 通过
- `gofmt -l`、`git diff --check origin/main...HEAD` → 干净
- 真实 launchctl 语义探测（本机 macOS，全部只读）：
  - `launchctl print gui/<uid>/<缺失label>` → `Could not find service "…" in domain for user gui: 501`，**exit 113**（`isLaunchdUnloaded` 按文本命中 → bootstrap fallback）
  - `launchctl print gui/999999/…`（域不存在）→ `Could not find domain for user gui: 999999`，**exit 112**（`launchdDomainHint` 按文本命中 → SSH/前台提示）
  - `launchctl kickstart gui/<uid>/<缺失label>` → `Could not find service …` exit 113（真实失败传播，无假成功）
  - `launchctl bootout gui/<uid>/<缺失label>` → `Boot-out failed: 3: No such process` exit 3（#959 既有 `IsAlreadyUnloaded` 的 exit-3 门控在本机仍成立，无回归）

## Acceptance 逐条核对（#965 两项需求 + #959 保留）

| 验收 | 证据 | 结果 |
|---|---|---|
| `sift service start` 幂等：已加载 label 用 `kickstart`，未加载但保留 plist 存在用 `bootstrap`；不再用旧 `launchctl load`（`Load failed: 5` 假成功来源） | `planStart` launchd 分支 = `ProbeCmd: launchctl print gui/<uid>/<Label>` → 命中则 `kickstart`；`isLaunchdUnloaded`（仅 "no such process"/"could not find service" 文本，权限类显式排除）→ `bootstrap` fallback；`execLaunchdStart` 三路出口全部经 `launchdDomainHint` | ✅ |
| 与 install/reload 语义分离：start 不 bootout、不重写 unit | start plan `WriteFile` 为空；`Write` 对空 `WriteFile` 早退；CLI 测试断言 plist 内容前后一致（`retained plist` 未被改写） | ✅ |
| typed failure propagation：真实 launchctl 非零失败不得被当作"未加载"吞掉 | `isLaunchdUnloaded` 对 `permission denied`/`operation not permitted` 返回 false → 原样传播；kickstart/bootstrap 失败均传播 exit 1；测试覆盖 kickstart permission denied 与 GUI domain 缺失 | ✅ |
| SSH/无 GUI 会话：返回可行动提示而非晦涩成败 | `launchdDomainHint` 命中 "could not find domain"/"domain unavailable" → `log into the macOS desktop and retry, or run sift daemon in the foreground`；`TestLaunchdStartReportsMissingGUIActionably` 断言 SSH/foreground 字样 | ✅ |
| 文档化 gui/<uid> 行为 | `specs/hosting.md` start 行、`runbooks/troubleshooting.md`（GUI domain 不存在段 + `sift daemon` 前台指引）、`testing/hosting.md` 同步 | ✅ |
| 保留 #959：固定 PATH 与 install 替换行为 | `launchdTemplate` PATH 零改动；install plan（bootout→bootstrap replacement）零改动；`Plan` 仅新增 `ProbeCmd/FallbackCmd/StartUnit` 三字段 + start 专用 dispatch，`BeforeCmd` 仍仅 install 使用 | ✅ |
| `go test ./internal/hosting/... ./cmd/sift/...`、build、vet 通过 | 上述实跑 | ✅ |

## Scope 核对

- 改动面 = `internal/hosting/{hosting.go,hosting_test.go}`、`cmd/sift/main_test.go`、三份 docs + 新 plan 文档，共 7 文件 196 行；无 daemon/brain/forge 生产改动 ✅
- systemd 后端零改动（`planStart` systemd 分支原样）✅
- Label/LegacyLabel 未动；`osUserUID()` 复用既有 helper ✅
- 前台 backend（无 supervisor）`sift service start` 仍走 `ErrNoBackend` → foreground 提示 exit 0，语义未变 ✅
- 失败自愈路径：start 失败不落盘（start 无 WriteFile），plist 保留，重跑 install/start 可自愈 ✅

## Finding 列表

### [P2-1] 假 launchctl 桩 exit code 与真实 launchctl 脱节（3 vs 112/113）
- 一句话：`hosting_test.go`/`main_test.go` 的 `print` 失败桩用 exit 3，实测本机真实 launchctl 为 exit 113（缺 label）/112（缺 domain）——代码按文本匹配故行为正确，但桩若不改，未来若有人给 `isLaunchdUnloaded` 加 exit-code 门控会在测试里假绿、真实机上漏判。
- 关闭标尺：把桩改为 `exit 113`（print 缺 label）与 `exit 112`（缺 domain）后 `go test ./internal/hosting/... ./cmd/sift/...` 仍 PASS → YES。
- 证据缺口：无（功能由文本匹配保证）；纯测试保真问题。
- fixer=same

### [P2-2] launchd 错误分类逻辑双实现
- 一句话：新 `isLaunchdUnloaded`（纯文本）与既有 `IsAlreadyUnloaded`（exit-3 + "no such process" 门控）平行维护两套 launchctl 错误口径，未来漂移风险。
- 关闭标尺：合并为单一分类器（bootout 的 "no such process"+exit3 与 print 的 "could not find service" 同函数处理，权限类同排除）且两包测试全过 → YES；或加注释互引差异。
- 证据缺口：无。
- fixer=same

### [P2-3] bootstrap 失败传播与 CLI 层失败退出无回归断言
- 一句话：plan 文档声称"覆盖 bootstrap/kickstart failure"，实际 hosting 层仅测了 kickstart permission denied，bootstrap 自身失败未覆盖；CLI 层 `sift service start` 失败（exit != 0 + stderr 报错）也无回归测试。
- 关闭标尺：`hosting_test.go` 加 bootstrap 失败用例（桩 bootstrap 返回 permission/其它非零 → `Exec` 报错）；`main_test.go` 加 start 失败场景断言 `run(...) != 0` 且 stderr 含错误 → YES。
- 证据缺口：行为正确（代码路径明确传播），缺自动化钉住。
- fixer=same

### [DEFER-1] 真实 SSH/无 GUI 会话端到端不可在本环境复现
- 一句话：本机有 GUI 域（`gui/501` 存在），缺 domain 的匹配已用 uid 999999 实测（exit 112 "Could not find domain" → hint 命中）验证了文本侧，但登录用户自身无 GUI 域的 SSH 全链（print→hint→exit 非 0）无自动化证据。
- 关闭标尺：真实 SSH 会话（无 loginwindow）手测 `sift service start` 输出登录 GUI/前台提示且 exit != 0 → YES；或 M8 发布验证一并核销。
- 证据缺口：CI/本机均无"无 GUI 域"环境；与 #959 DEFER-1 同类。
- fixer=same（backlog / M8 发布证据）

### [DEFER-2] 旧 `launchctl load`（#959 前）残留 `user/<uid>` 域实例与 gui 域 bootstrap 短时并存
- 一句话：#959 P2-1 已记录跨域并存类问题，start 的 gui 域 bootstrap 路径同样暴露（旧 user 域实例可能持 SIFT_HOME 锁，新 gui 域实例争锁）；本轮未处理且不属 #965 范围。
- 关闭标尺：后续 issue 覆盖 start 前 legacy user-domain 清理或文档警告 → YES。
- 证据缺口：仅推理（旧域实例需 v0.5.5 经 SSH `launchctl load` 遗留）；无实测。
- fixer=same（backlog）

## Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | 是 |
| P1 | 0 | 是 |
| P2 | 3 | 否（记录） |
| DEFER | 2 | 否（backlog） |

## Verdict

**PASS** — P0/P1 全关（各 0 项）。`sift service start` 已按 label 语义分离（loaded→kickstart / unloaded→bootstrap / 真实失败→传播），不再有 `launchctl load` 假成功；SSH/无 GUI 域输出可行动提示；#959 固定 PATH 与 install 替换行为零改动。测试/build/vet/gofmt 实跑通过，真实 launchctl 文本匹配验证（exit 112/113/3）与测试桩语义一致。P2/DEFER 仅记录，不进当前 MR；可进入 MR 创建与合并流程。
