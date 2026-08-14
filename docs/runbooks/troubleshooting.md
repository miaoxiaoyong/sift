---
status: active
created: 2026-08-11
summary: 安装、升级、托管与 doctor 故障排查
---

# 安装与升级故障排查

先记录版本、平台、安装渠道和状态目录，避免混查两套安装：

```bash
uname -a
command -v sift
sift --version
printf 'SIFT_HOME=%s\n' "${SIFT_HOME:-$HOME/.sift}"
sift service status
sift doctor --offline
```

不要删除 `sift.db`、版本目录或 token 来“重试”。诊断退出码和配置边界见 [`../specs/config.md` §7](../specs/config.md)；安装原子性和版本握手见 [`../specs/release.md`](../specs/release.md)。

## 快速索引

| 症状 | 首先检查 | 处置 |
|------|----------|------|
| checksum 不匹配 | 下载文件名、release/tag、重复下载 | 删除该归档并从同一 Release 重新下载；不要继续解包 |
| `exec format error` | `uname -s`、`uname -m`、归档名 | 下载正确 OS/架构组合 |
| `sift install` 拒绝归档 | manifest、tar 内容、两个 `--version` | 使用官方未改动归档；不要手工重打包 |
| `already installed` | `$SIFT_HOME/bin/<version>` | 这是防覆盖保护；升级应安装新版本，不要覆盖旧目录 |
| `daemon unavailable` | service status、`siftd.sock`、daemon 日志 | 先恢复 daemon，再运行在线命令 |
| doctor `version:wrapper` error | 两个 binary 来源和版本 | 从同一个归档/同一次 brew 安装恢复整套 binary |
| service 安装后未启动 | 用户级 supervisor、日志、配置权限 | 按平台章节检查；无 supervisor 改用前台模式 |
| 升级后仍显示旧版本 | `command -v sift`、`current`、service restart | 修正 PATH/安装渠道并重启用户级单元 |
| 旧版本升级后拒绝数据库 | “schema newer”/migration 错误 | 不要删库；恢复能读取该 schema 的新 binary |

## 1. 下载或安装失败

### Checksum 不匹配

1. 确认归档和 `checksums.txt` 来自同一个 release。
2. 确认没有把网页错误页保存成 `.tar.gz`：

   ```bash
   file sift_<version>_<os>_<arch>.tar.gz
   tar -tzf sift_<version>_<os>_<arch>.tar.gz
   ```

3. 重新下载并校验。仍不匹配时停止安装并报告 release artifact；不要自行更新 checksum。

### `manifest`、SHA-256 或版本探测失败

官方归档应包含 `sift`、`sift-agent-wrapper`、`manifest.json`。安装器会拒绝未知 manifest 字段、路径穿越、链接、缺文件、hash 错误或 binary 版本不一致。确认归档未经代理、杀毒工具或手工解包重打；从 Release 重新下载原文件。

### `current` 切换失败

检查：

```bash
ls -la "${SIFT_HOME:-$HOME/.sift}/bin"
```

`current` 必须是安装器维护的相对 symlink，不能是目录或普通文件。若用户曾手工创建 `$SIFT_HOME/bin/current`，先停止 daemon，备份该异常路径，再移走它并重新运行原安装命令。不要编辑已激活的版本目录。

## 2. Daemon 或 service 不可用

### 通用检查

```bash
sift service status
ls -ld "${SIFT_HOME:-$HOME/.sift}"
ls -l "${SIFT_HOME:-$HOME/.sift}/logs"
sift doctor --offline
```

`SIFT_HOME` 必须是绝对路径，目录模式应为 `0700`，存在的 `config.yaml` 应为 `0600`。service 单元固定了安装时的 `SIFT_HOME`；shell 改成另一个值会让 CLI 去错误的 socket。

### macOS / launchd

```bash
launchctl list cn.hexai.sift
launchctl print "gui/$(id -u)/cn.hexai.sift"
tail -n 200 "${SIFT_HOME:-$HOME/.sift}/logs/siftd.err.log"
```

若单元未加载，重新运行 `sift service install`。重复安装会先卸载再 bootstrap 当前 `cn.hexai.sift`，使新 plist 环境生效；命令失败时不得把输出当作已重载。launchd 不继承终端 profile，服务 PATH 固定为 `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`，可用 `plutil -extract EnvironmentVariables.PATH raw -o - ~/Library/LaunchAgents/cn.hexai.sift.plist` 核验。Agent 可执行文件仍应在初始化时解析为绝对路径。

若配置修复后仍处于失败状态：

```bash
sift service restart
```

不要用 system-wide LaunchDaemon；Sift 需要当前用户的 forge/Agent CLI 登录态。

### Linux / systemd user

```bash
systemctl --user status sift.service
journalctl --user -u sift.service -n 200 --no-pager
tail -n 200 "${SIFT_HOME:-$HOME/.sift}/logs/siftd.err.log"
```

出现 “Failed to connect to bus” 时，先确认当前用户有 systemd user session。无人登录仍需常驻：

```bash
loginctl enable-linger "$USER"
sift service install
```

没有 systemd user manager 的发行版使用：

```bash
sift daemon
```

这是受支持的前台 fallback，但不会自动重启。

### Socket 文件存在但仍不可用

文件存在不等于 daemon 正在监听；崩溃可能遗留旧 socket。以 `sift service status` 和实际在线 `sift doctor` 为准。确认没有第二个 daemon 竞争 `$SIFT_HOME/siftd.lock`，然后通过 supervisor 重启。不要为了绕过单实例锁而删除正在使用的 lock/socket。

## 3. Doctor 报告

```bash
sift doctor             # daemon 可用时的完整在线视图
sift doctor --offline   # daemon 不可用时的只读视图
```

退出码语义见 [`../specs/config.md` §7](../specs/config.md#7-sift-doctor-基线退出码)。

常见检查：

- `version:wrapper`：wrapper 缺失/不可执行是 warning；与 `sift` 版本不同是 error。恢复同一归档中的一对 binary。
- `permissions:*`：按报告中的路径收紧属主权限；先确认路径确属当前安装，避免 chmod 错目录。
- Agent/forge CLI：确认 executable 在 service 的 PATH 可见，并以同一 OS 用户完成 `gh auth status` 或 `glab auth status`。
- tmux：仅配置使用 tmux backend 时要求可用；不要通过改成 process backend 掩盖已运行 attempt 的资格问题。
- policy/hooks/isolation/outbox/channel：按报告中的 project/run 标识修复根因；不要通过清库清除诊断。
- `operator-token-readable-by-agent` / `unsafe-local`：这是 V0 明示的同 UID 安全边界，不是 chmod 能完全消除的误报；不要把 warning=1 宣称为沙箱闭合。

在线 doctor 失败且离线 doctor 正常，通常说明 daemon 未运行、CLI 与 service 使用不同 `SIFT_HOME`，或 operator socket/token 不匹配。

## 4. 升级故障

### CLI 已升级，daemon 仍是旧版本

检查 PATH 和 service：

```bash
command -v sift
sift --version
readlink "${SIFT_HOME:-$HOME/.sift}/bin/current"
sift service restart
sift service status
```

归档安装应从 `.../bin/current` 使用 CLI。Homebrew 安装应使用 brew 的 `bin`，不要同时让旧的 `$SIFT_HOME/bin/current` 排在 PATH 前面。

### Wrapper 版本不一致

不要复制单个文件。归档渠道安装新的完整归档；Homebrew 渠道运行 `brew reinstall sift` 或 `brew upgrade sift`。然后重启 service 并重新运行 doctor。

### 数据库 migration 失败或 schema 较新

立即停止反复重启，保留整个 `SIFT_HOME` 和日志。迁移只前向执行；较旧 daemon 正确行为是拒绝较新的 schema。恢复最后一个已成功打开该数据库的 release，或升级到更新 release。禁止删除 `schema_migrations`、手工改版本号或用空库覆盖生产状态。

### 需要回退

Binary 回退只有在数据库尚未迁移到旧版本不认识的 schema 时才安全。先停止 daemon 并保存完整 `SIFT_HOME`，再依据 release 说明判断兼容性。`current` 指回旧目录并不能逆转 migration；不确定时不要启动旧 binary。

## 5. 收集报告材料

报告安装/升级问题时附上（先移除 token、私有 repo 路径和 Agent 日志中的秘密）：

```text
OS 与架构
安装渠道、release 版本、归档 SHA-256
command -v sift
SIFT_HOME（可脱敏）
sift service status
sift doctor / sift doctor --offline 输出与退出码
siftd.err.log 或 user-service journal 的相关时间段
```

不要上传 `operator.token`、`control.json`、`bootstrap.json`、完整数据库或未经检查的 Agent 日志。
