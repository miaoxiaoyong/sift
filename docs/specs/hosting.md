---
status: active
created: 2026-08-11
summary: 用户级托管单元（launchd/systemd/foreground）的生成、安装与升级重启契约
---

# 托管规格

本文定义 M8 §8.2（Issue #905）的托管契约：把 `sift daemon` 作为**用户级**进程常驻的三条路径——macOS 的 launchd user agent、Linux 的 systemd user unit、无 supervisor 时的前台 fallback；崩溃自启；以及原子升级后的重启。实现为 `internal/hosting`、`cmd/sift` 的 `service` 子命令、`packaging/homebrew/sift.rb` 草稿与 `scripts/hosting-smoke.sh`；测试基准见 [`../testing/hosting.md`](../testing/hosting.md)。

需求与结构来源：[WBS M8 §8.2](../WBS.md)、[DESIGN §11 部署与运维](../DESIGN.md)（托管矩阵、原子升级段落）、[release.md §3](release.md)（`current` 切换）。

## 1. 范围与非范围

**范围（WBS §8.2）：**

- launchd **user agent** plist 模板 + 安装（`sift service install`）。
- systemd **user unit** + 前台 fallback（无 systemd 时前台跑 `sift daemon`）。
- 崩溃自启（`KeepAlive` / `Restart=on-failure`）+ 原子升级后重启（`current` 切换后 `sift service restart`）。
- Homebrew tap formula（**草稿**）与 Release 归档安装路径一致。
- 托管安装冒烟脚本（`scripts/hosting-smoke.sh`）。

**非范围：**

- Homebrew tap **实际发布**（需 Issue A 的 release 归档产物 + GitHub Release——留 §8.2 之后）。
- 跨用户 / 系统级部署（V0 是 user-level；DESIGN §11 明文 launchd user agent 而非 system daemon，systemd user unit 而非 system service）。
- A10 干净机安装证据（Human Gate，M7 通过后）。

## 2. 平台矩阵

平台差异只允许出现在托管单元的生成与探测这一处（DESIGN §11）。其余——路径（统一 `~/.sift/`）、两个属主 only 的 Unix socket、文件契约、恢复逻辑——两平台同码同行为。

| 平台 | 后端 | 单元位置 | 自启语义 |
|------|------|----------|----------|
| macOS | launchd user agent | `~/Library/LaunchAgents/cn.hexai.sift.plist` | `RunAtLoad` + `KeepAlive=true`（登录即起、崩溃即重启）；`ThrottleInterval=10` 防崩溃紧循环 |
| Linux | systemd user unit | `$XDG_CONFIG_HOME/systemd/user/sift.service`（默认 `~/.config/systemd/user/sift.service`） | `Restart=on-failure` + `RestartSec=10`；`loginctl enable-linger $USER` 后可在未登录时常驻 |
| 其它 / 无 supervisor | foreground | 无单元文件 | 用户在前台 / tmux / screen 直接跑 `sift daemon`；V0 **不承诺**自动常驻 |

后端由 `hosting.Detect(runtime.GOOS)` 选择（`darwin→launchd`、`linux→systemd`、其它→foreground）。CLI 在 `sift service install` 时探测平台工具（`launchctl` / `systemctl`）是否在 PATH 上；缺失则落回 foreground 提示，不报错——foreground 是受支持（只是不自启）的形态。

## 3. 单元契约

### 3.1 不变量（两后端共同）

- **不监听任何网络端口**：控制面是两个属主 only 的 Unix socket（运维 `siftd.sock` / 上报 `run.sock`，DESIGN §3.2）。单元内不得出现 `Sockets`（plist）或 `ListenStream`/`Socket`（unit）。
- **ExecStart 指向 `bin/current/sift daemon`**：跟随 `current` 符号链接，因此升级只需 `sift install` 新版本（原子切换 `current`）后再 `sift service restart`，**绝不逐文件覆盖正在使用的安装**（release.md §3）。
- **用户级**：launchd 是 user agent（非 system daemon，必须以用户身份跑才能用 `gh`/agent CLI 登录态）；systemd 是 user unit（非 system service）。
- **SIFT_HOME 固定**：单元以 `Environment=SIFT_HOME=…`（systemd）/ `SIFT_HOME` 键（plist）固定运行 home，避免从不同 shell 环境漂移。
- **日志落 `~/.sift/logs/`**（DESIGN §11）：`siftd.log` / `siftd.err.log`。

### 3.2 launchd plist 字段

| 字段 | 值 | 理由 |
|------|----|------|
| `Label` | `cn.hexai.sift` | 稳定句柄；`launchctl kickstart -k gui/<uid>/<Label>` 原子重启 |
| `ProgramArguments` | `[<home>/bin/current/sift, daemon]` | 跟随 `current` |
| `RunAtLoad` | `true` | 登录即起 |
| `KeepAlive` | `true` | 崩溃自启（§4） |
| `ThrottleInterval` | `10` | 防崩溃紧循环重启风暴 |
| `StandardOutPath` / `StandardErrorPath` | `<home>/logs/siftd{,err}.log` | DESIGN §11 日志路径 |
| 无 `Sockets` | — | 不开端口 |

### 3.3 systemd unit 字段

| 字段 | 值 | 理由 |
|------|----|------|
| `[Service] Type` | `simple` | 前台进程，systemd 直接跟踪 |
| `ExecStart` | `<home>/bin/current/sift daemon` | 跟随 `current` |
| `Restart` | `on-failure` | 崩溃自启，但干净退出不循环（§4） |
| `RestartSec` | `10` | 防崩溃紧循环 |
| `Environment` | `SIFT_HOME=<home>` | 固定运行 home |
| `[Install] WantedBy` | `default.target` | user unit 标准启用目标 |
| 无 `ListenStream` / socket 单元 | — | 不开端口 |
| 单元注释 | foreground fallback + `enable-linger` 指引 | 无 systemd 时降级路径可见 |

> `Restart=on-failure`（非 `always`）是刻意的：干净退出（exit 0，如配置拒绝启动的进程级失败）不应被紧循环重启。崩溃（非零 / 信号）才重启。

## 4. 崩溃自启与升级后重启

**崩溃自启：** launchd `KeepAlive=true` 在进程消失时立即重启（受 `ThrottleInterval` 节流）；systemd `Restart=on-failure` 在非零退出 / 致命信号时重启（受 `RestartSec` 节流）。foreground 后端无此能力——V0 如实声明不承诺。

**原子升级后重启（DESIGN §11 升级段落）：**

1. `sift install <new-archive>`：把新版本两二进制 + manifest 装到 `bin/<new-release>/`，校验后原子切换 `current → <new-release>`（release.md §3，temp+rename，绝不逐文件覆盖）。
2. 从 v0.1.0 升级的 macOS 用户执行 `sift service restart`（或 `sift service install`）时会一次性迁移 launchd label：卸载旧的 `com.miaoxiaoyong.sift` agent 并删除旧 plist，再使用 `cn.hexai.sift`。旧 label 不存在时该步骤幂等，因此按本节升级路径不会留下重复 agent。
3. `sift service restart`：
   - launchd：`launchctl kickstart -k gui/<uid>/<Label>`（原子重启，按 label）。
   - systemd：`systemctl --user restart sift.service`。
   - foreground：提示用户停止并重新运行（无 supervisor）。

重启后 `current` 已指向新版本，因此单元的 ExecStart 重新解析即运行新 release；旧版本目录保持原样，可回滚（再 `sift install` 旧归档）。

## 5. CLI（`sift service`）

```
sift service <install|uninstall|start|stop|restart|reload|status>
```

| 动作 | 行为 |
|------|------|
| `install` | 生成单元文件（temp+rename 原子写）→ 探测平台工具存在则加载并启动（launchd `launchctl load`；systemd `daemon-reload` + `enable --now`）；工具缺失则打印 foreground 提示并 exit 0（可移植）。**要求已 `sift install` 一个 release**：单元必须指向真实二进制。 |
| `uninstall` | 停 / 卸载单元（launchd `bootout`/`unload`；systemd `disable --now`）→ 删除单元文件（幂等）。 |
| `start` | launchd 加载保留的 plist；systemd `systemctl --user start sift.service`；foreground 提示在终端运行 `sift daemon`。 |
| `stop` | launchd `bootout`（防 `KeepAlive` 立即拉起）；systemd `systemctl --user stop sift.service`；foreground 提示停止该前台进程。 |
| `reload` | V1 等价于 `restart`，明确输出 SIGHUP 热重载尚未实现。 |
| `status` | 默认人话输出 `✓ 运行中` / `✗ 未运行` / `✗ 未安装`，包含 backend、可用时的 PID 和 operator socket；底层查询为 launchd `launchctl list <Label>` / systemd `systemctl --user status`，foreground 实探 socket。 |
| `restart` | 见 §4 升级后重启。 |

`sift service install` 把 ExecStart 指向 `bin/current/sift`，故**升级路径 = `sift install` + `sift service restart`**，二者解耦（install 不碰版本目录；restart 会按上面的兼容迁移规则处理旧 launchd label）。

## 6. Homebrew formula 草稿

`packaging/homebrew/sift.rb` 是 **草稿**：由 `internal/hosting.Formula(release, sha256)`（命令行 `go run ./tools/hosting formula`）渲染，归档名与布局与 [`release.md` §2](release.md) 一致，安装两个 release 二进制（`sift` + `sift-agent-wrapper`，DESIGN §8.4 配对），`caveats` 指向 `sift service install`，`test do` 断言 `sift --version` 与 release 一致。

- 草稿用 `0.1.0-dev` + 占位 sha256，`brew style`（RuboCop）零违规。
- 发布时由 release 管线用真实归档 sha256 重渲染后放入 tap 仓库（tap 实际发布是本 Issue 非范围）。
- 本地干跑：`go run ./tools/hosting formula --version <v> --sha256 <h> > /tmp/sift.rb && brew style /tmp/sift.rb`。

Homebrew 由 Cellar 链接承担 `current` 切换的等价语义（`brew link`），与 Release 归档的 `~/.sift/bin/current` 是两条并列安装路径（DESIGN §11 矩阵）。

## 7. 验收映射

| Issue #905 验收 | 判据 |
|-----------------|------|
| launchd/systemd/foreground 三路径均可安装并起 daemon | `hosting.NewSpec` + `Render`/`Plan` 单元（§3 字段、§2 后端分发）；`scripts/hosting-smoke.sh` 在有 supervisor 的机器上跑通 install→up→kill→restart |
| 崩溃自启可验证 | smoke 脚本 kill 后断言 supervisor 给出新 pid（systemd）/ 仍注册（launchd）；foreground 路径如实声明不承诺 |
| 升级后重启可验证 | smoke 脚本装第二 release → 断言 `current` 切换 → `sift service restart` 重载；`TestRenderSystemd/LaunchdTemplateContract` 断言 ExecStart 跟随 `current` |
| Homebrew formula 草稿与归档安装路径一致 | `TestFormulaConsistentWithReleaseArchive`（归档名、两二进制、service install 指引、no-port）；`brew style` 零违规 |
| 不引入监听端口 | `TestRender*TemplateContract` 断言无 `Sockets`/`ListenStream`/`Socket`；既有 `TestV10ZeroNetworkListeners`（`internal/controlplane`）不回归 |
