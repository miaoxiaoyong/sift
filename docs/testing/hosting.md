---
status: active
created: 2026-08-11
summary: 托管单元生成、安装、自启与升级重启的测试方案（派生自 specs/hosting.md）
---

# 托管测试方案

派生自 [`../specs/hosting.md`](../specs/hosting.md)。覆盖 M8 §8.2（Issue #905）的验收面：三后端单元生成、崩溃自启字段、原子升级后重启、Homebrew formula 草稿一致性、不引入监听端口。

## 1. 测试金字塔

| 层 | 载体 | 内容 |
|----|------|------|
| 单元 | `internal/hosting` | 后端按 GOOS 分发；spec 解析（已装 release、拒绝相对 home / 不可执行 daemon / 非 SemVer current）；launchd/systemd 模板字段（KeepAlive、Restart=on-failure、ExecStart=current/sift daemon、SIFT_HOME、绝对日志路径）；**含 `&` / 空格 / `#` 的 SIFT_HOME 渲染两后端，XML 实体 / systemd 引号转义后经 `plutil -lint`（darwin）/ `systemd-analyze --user verify`（linux，可用时）校验且解析出的 ExecStart/SIFT_HOME 与原路径一致（`TestRenderEscapesUserPathsInUnits`）**；foreground 无单元；install/uninstall/restart/status plan（WriteFile + RunCmd）；**foreground status 实探 `siftd.sock` 并携带 `present|absent` 两态判定 + socket 路径（`TestPlanStatusForegroundProbesSocket`）**；原子写（temp+rename，无残留 staging）；Exec 在工具缺失时返回 `ErrNoBackend`；formula 草稿与归档名一致、装两二进制、指向 service install、no-port |
| 单元 | `cmd/sift` | `sift service` 参数纪律（无动作 exit 2、未知动作 exit 2、未装 release exit 1）；**foreground status 的 no-supervisor 报告在两种状态均输出可被 `[ -S "$SIFT_HOME/siftd.sock" ]` 验证的 `present|absent` 判定（`TestServiceStatusForegroundReportsSocketVerdict`）** |
| 静态 | `brew style` | `packaging/homebrew/sift.rb` 草稿 RuboCop 零违规（本地手测；发布管线断言） |
| 集成 | `scripts/hosting-smoke.sh` | 有 supervisor 的机器：install → 起 daemon → kill → 自启（新 pid / 仍注册）→ 装第二 release → `current` 切换 → `sift service restart`；无 supervisor：foreground 路径起停。**每个验收步都是硬门：daemon 未起 / supervisor 未产出新 PID / 未再注册 / restart 后新 release 未运行均 exit 非 0；正常路径断言 daemon 存活（socket present）、kill 后新 PID/新注册实例、restart 后新 release 正在运行（upgrade shim 写 release marker 为证）** |
| 集成 | `scripts/hosting-smoke-test.sh` | **可重复负例/正例 harness**：以 scrubbed PATH + fake systemctl/launchctl 驱动 smoke，零真实 supervisor/真实安装副作用；启动失败（foreground）、kill 无新 PID（systemd, linux）、restart 后无新 release（systemd, linux / launchd, darwin）负例断言非 0；happy 路径断言 0（launchd 场景需 Darwin，systemd 场景需 Linux，因 CLI backend 跟随 runtime.GOOS） |

## 2. 关键断言

1. **后端分发**：`Detect("darwin")=launchd`、`"linux"=systemd`、其它=foreground（`TestDetectSelectsBackendByGOOS`）。
2. **不监听端口（§3.1）**：launchd 模板无 `Sockets`/`Listener`；systemd 模板无 `ListenStream`/`Socket`（`TestRenderLaunchdTemplateContract` / `TestRenderSystemdTemplateContract`）。
3. **ExecStart 跟随 current**：两模板的 ProgramArguments/ExecStart 均为 `<home>/bin/current/sift daemon`，不指向具体版本目录——这是升级只需 restart 的契约基础（同上两测试）。
4. **崩溃自启字段**：launchd `KeepAlive`+`true`；systemd `Restart=on-failure`（**非** `always`，干净退出不循环）+ `RestartSec`（同上）。
5. **用户级**：systemd `WantedBy=default.target` 且注释含 `enable-linger` 指引（同上）。
6. **foreground fallback**：`NewSpecFor(home,"freebsd")` backend=foreground、`UnitPath=""`、`Render()` 返回 nil；install plan 无 WriteFile/RunCmd，仅 Hint（`TestPlanInstallForegroundHasNoWrite`）。
7. **foreground status 实探 socket（§5 契约）**：`planStatus` 对 `<home>/siftd.sock` 做 `os.Stat` + `ModeSocket` 判定，plan 携带 `Status=present|absent` 与 `SocketPath`，两态各有测试（present 用真实 Unix socket）；CLI 的 no-supervisor 报告把判定输出为 `operator socket <path>: present|absent`，可被 `[ -S "$SIFT_HOME/siftd.sock" ]` 复核（`TestPlanStatusForegroundProbesSocket` / `TestServiceStatusForegroundReportsSocketVerdict`）。
8. **单元 escaping（R1 P1-2）**：SIFT_HOME 含 `&`/空格/`#` 时，launchd 值经 XML 实体转义（`&amp;`/`&lt;`/`&gt;`）、systemd 的 ExecStart/Environment 值双引号包裹并转义 `\`/`"`；`plutil -lint` + `plutil -extract`（darwin）与 `systemd-analyze --user verify`（linux）通过且解析出的路径与原值逐字符一致（`TestRenderEscapesUserPathsInUnits`）。
9. **原子写**：`Write` 经 temp+rename 落盘且无 `.sift-unit-*` 残留；nil content = 删除（幂等）（`TestWrite*`）。
10. **工具缺失可移植**：`Exec` 对 nil RunCmd 或 PATH 上找不到工具返回 `ErrNoBackend`，CLI 据此打印 foreground 提示而非硬失败（`TestExecForegroundPlanReturnsErrNoBackend` / `TestExecMissingToolReturnsErrNoBackend`）。
11. **formula 一致性**：归档名 `sift_<release>_darwin_arm64.tar.gz`、pin version + sha256、装两二进制、指向 `sift service install`、声明 no-port（`TestFormulaConsistentWithReleaseArchive`）。
12. **CLI 纪律**：`sift service` 无动作 / 未知动作 exit 2；未装 release exit 1 并提示先 install（`TestService*`）。
13. **smoke 失败条件（R1 P1-3）**：`hosting-smoke.sh` 对「daemon 未启动（foreground 立即退出 / supervised wait 超时）、kill 后 supervisor 未产出新 PID（systemd）/ 未再注册（launchd）、restart 后新 release 未运行（marker 缺失）」均 exit 非 0；`hosting-smoke-test.sh` 以 fake supervisor 对每个负例断言非 0、对 happy 路径断言 0。

## 3. 验收映射

- **三路径安装并起 daemon**：单元覆盖三后端的 Render/Plan/Write；smoke 脚本在有 supervisor 的机器上端到端（launchd 实测通过；systemd 需 user manager 环境）。
- **崩溃自启可验证**：smoke 脚本 kill 后断言 supervisor 给出新 pid（systemd MainPID 变化）/ launchd 仍注册；foreground 如实跳过。
- **升级后重启可验证**：smoke 脚本装第二 release → 断言 `current → <new>` → `sift service restart` 重载；模板 ExecStart 跟随 current 的契约由单元锁定。
- **formula 草稿一致**：`TestFormulaConsistentWithReleaseArchive` + `brew style` 零违规。
- **不引入端口**：模板断言 + 既有 `TestV10ZeroNetworkListeners`（`internal/controlplane`）不回归。

## 4. 运行

```bash
go test ./internal/hosting/... ./cmd/sift/...
go run ./tools/hosting formula --version 0.1.0-dev --sha256 0 > packaging/homebrew/sift.rb
brew style packaging/homebrew/sift.rb                       # RuboCop（本地）
./scripts/hosting-smoke.sh                                   # 需两个 release 二进制在 PATH；自动探测 supervisor
./scripts/hosting-smoke-test.sh                              # 可重复负例/正例 harness（fake supervisor，无副作用）
```

smoke 脚本在无 user manager 的 CI runner（如默认 GitHub Actions ubuntu）上会落回 foreground 路径并如实报告「autorestart not promised in this mode」；systemd 自启需 `XDG_RUNTIME_DIR` + `systemctl --user is-system-running` 可达，A10 干净机（M7 后）才是完整证据。harness 的 systemd 场景需在 Linux 上跑（CLI backend 跟随 runtime.GOOS），launchd 场景需在 macOS 上跑；负例在对应平台均非 0。

## 5. 已知缺口（留后续 / Human Gate）

- A10 干净 macOS 与 systemd Linux 各自的完整托管冒烟证据——Human Gate（M7 通过后）。
- systemd user unit 在无 DBUS_SESSION 的 headless CI 上的自启——留需要 user manager 的环境。
- Homebrew tap 实际发布（归档产物 + GitHub Release）——§8.2 非范围。
