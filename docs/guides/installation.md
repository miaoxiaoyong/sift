---
status: active
created: 2026-08-11
summary: 用户通过发布归档或 Homebrew 安装 Sift
---

# 安装 Sift

Sift 支持 macOS/Linux 的 amd64 与 arm64。一个 release 归档同时包含同版本的 `sift` 和 `sift-agent-wrapper`；目标机不需要 Go。分发边界见 [PRD §9.3](../PRD.md)，归档和安装校验契约见 [`../specs/release.md`](../specs/release.md)。

## 安装前

准备以下依赖：

- 当前平台对应的 Sift release 归档；
- 已登录的 forge CLI：GitHub 项目用 `gh`，GitLab 项目用 `glab`；
- 配置中使用的 Agent CLI；
- 仅在配置选择 tmux backend 时需要 `tmux`。

默认状态目录是 `~/.sift`。如需修改，安装、service 和日常 CLI 必须始终使用同一个绝对路径：

```bash
export SIFT_HOME=/absolute/path/to/sift-home
```

## 路径 A：Release 归档

### 1. 下载并校验

从同一个 forge Release 下载：

```text
sift_<version>_<os>_<arch>.tar.gz
checksums.txt
```

确认本机组合：

```bash
uname -s   # Darwin -> darwin, Linux -> linux
uname -m   # x86_64 -> amd64, arm64/aarch64 -> arm64
```

在下载目录只校验当前归档（完整 checksum 文件还会列出其他平台产物）：

```bash
archive=sift_<version>_<os>_<arch>.tar.gz
grep "  ${archive}$" checksums.txt | sha256sum --check -   # Linux
grep "  ${archive}$" checksums.txt | shasum -a 256 -c -    # macOS
```

命令必须输出 `${archive}: OK`；没有匹配行也属于失败。

### 2. 引导安装

先把下载的归档解到临时目录，仅用其中的 `sift` 调用内置安装器：

```bash
tmp=$(mktemp -d)
tar -xzf sift_<version>_<os>_<arch>.tar.gz -C "$tmp"
"$tmp/sift" --version
"$tmp/sift" install "$PWD/sift_<version>_<os>_<arch>.tar.gz"
rm -rf "$tmp"
```

安装器会再次校验 manifest、当前平台两个二进制的 SHA-256 和版本，然后写入：

```text
$SIFT_HOME/bin/<version>/
$SIFT_HOME/bin/current -> <version>
```

未设置 `SIFT_HOME` 时，上述根目录是 `~/.sift`。

将稳定入口加入 PATH：

```bash
export PATH="${SIFT_HOME:-$HOME/.sift}/bin/current:$PATH"
sift --version
sift-agent-wrapper --version
```

把 PATH 设置写入 shell profile 时，保留 `current` 路径，不要写死某个版本目录。

## 路径 B：Homebrew（macOS）

Homebrew 渠道只有在项目发布页明确给出**已发布 tap 名和正式 formula**后才可使用：

```bash
brew tap <published-tap>
brew install <published-tap>/sift
sift --version
```

当前仓库只定义 formula 草稿生成链；草稿中的开发版本和占位 SHA-256 不能安装，也不能冒充已发布渠道。发布页未列出 tap 时，请使用路径 A，不要从源码仓库复制草稿 formula。Homebrew 会把配对的两个二进制安装到同一 `bin`，升级使用 `brew upgrade sift`，不再调用 `sift install` 管理 Cellar 文件。

## 配置与启动

推荐使用向导生成并校验配置，避免手写出错：

```bash
sift init
```

向导会交互式选择 Agent 与 operator，并绑定当前目录项目；Forge 类型从项目 git remote 自动探测，无需手选。非交互环境也可通过选项传入（`sift init --agent claude --project . --forge github`，`--forge` 仅作覆盖）。绑定其他项目请 `cd <项目> && sift project add`。

`~/.sift/config.yaml` 可缺省，但要接入项目必须配置 Agent、项目、可信 operator 和 forge。如需手工维护或自动化，字段、默认值及最小示例以 [`../specs/config.md`](../specs/config.md) 为准；项目仓库中的 `.sift/policy.yaml` 以 [`../specs/policy.md`](../specs/policy.md) 为准。配置文件存在时应限制为属主读写：

```bash
chmod 700 "${SIFT_HOME:-$HOME/.sift}"
chmod 600 "${SIFT_HOME:-$HOME/.sift}/config.yaml"
```

归档安装后注册用户级服务：

```bash
sift service install
sift service status
```

- macOS 使用 launchd user agent；
- 有 systemd 的 Linux 使用 systemd user unit；无人登录也需常驻时运行 `loginctl enable-linger "$USER"`；
- 没有可用 supervisor 时，CLI 会提示前台执行 `sift daemon`。前台模式不承诺崩溃自启。

Sift 不监听 TCP/UDP 端口，只创建 `$SIFT_HOME/siftd.sock` 和 `$SIFT_HOME/run.sock`。

## 安装后检查

```bash
sift --version
sift-agent-wrapper --version
sift service status
sift doctor
```

`doctor` 退出码为：0 无问题；1 至少一个 warning；2 至少一个 error。warning 不应被静默忽略，error 必须在使用前修复。初次配置或 daemon 尚未运行时可使用只读诊断：

```bash
sift doctor --offline
```

离线诊断不会启动 daemon、修改数据库或执行迁移。故障处理见 [`../runbooks/troubleshooting.md`](../runbooks/troubleshooting.md)。

## 升级

归档渠道：

```bash
sift install /path/to/sift_<new-version>_<os>_<arch>.tar.gz
sift service restart
sift --version
sift doctor
```

Homebrew 渠道发布后，先按该 tap 的 release 说明处理 daemon，再升级整套 formula：

```bash
brew update
brew upgrade sift
sift --version
sift-agent-wrapper --version
```

macOS v0.1.0 用户按上述归档升级路径执行 `sift service restart` 时会自动迁移旧 launchd label：清理 `com.miaoxiaoyong.sift` 及其 plist，并使用 `com.xsift.sift`；迁移一次且幂等，不会创建重复 agent。无需先额外执行 `sift service install`。

不要在 tap 尚未发布时猜测 service 命令。两个渠道都应整套升级，禁止只替换 `sift` 或只替换 wrapper。数据库迁移只前向执行；降级 binary 可能因数据库 schema 较新而拒绝启动。
