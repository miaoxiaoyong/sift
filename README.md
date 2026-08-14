# Sift

**给 Issue 打一个标签，让你本机已有的 Coding Agent 完成修改；Sift 负责把工作推进到经过 Gate、必要时由人审批的 PR/MR。**

```text
Issue + sift:run
        │
        ▼
本机隔离 worktree → Coding Agent → Checks / Gate → PR 或 MR → 人工决定
```

Sift 同时支持 GitHub 和 GitLab。它不是另一个看板，也不托管你的代码或 Agent 凭证；Forge 仍是事实源，AI 可以提出和实现变更，但不能绕过确定性策略与人工决策。

## 选择起点

| 我想…… | 下一步 |
|---|---|
| **第一次安全体验**，不碰现有项目 | 查看 [`xsift/bluff`](https://github.com/xsift/bluff)。可玩游戏 MVP 由 [#1](https://github.com/xsift/bluff/issues/1) 跟踪，Sift bootstrap/seed tasks 由 [#2](https://github.com/xsift/bluff/issues/2) 跟踪；两项尚未合入可运行代码，因此当前只是预览入口，**还不是可以完成首次 Run 的教程**。 |
| **接入已有项目** | 安装后进入仓库运行 `sift init`；完整步骤见 [Getting Started](docs/guides/getting-started.md#路径-b接入已有仓库)。 |

> 不要普通 Fork Bluff 来代替 Template：GitHub Fork 不复制 Issues，并且更容易把 PR 提回上游。Template 会创建一个属于你的独立仓库。

## 开始前需要

- macOS 或 Linux（amd64/arm64），以及 Git；
- 对应项目的官方 Forge CLI：[GitHub CLI `gh`](https://cli.github.com/) 或 [GitLab CLI `glab`](https://gitlab.com/gitlab-org/cli)，并已登录；
- 一个可从终端启动的 Coding Agent。向导可识别 Claude Code、Codex CLI、Cursor CLI、pi、Gemini CLI、Aider、Qwen Code、Cody 等，也可登记其他可执行文件；**只有 Cursor GUI、不含 `cursor` CLI 时不能作为后台 Agent 启动**；
- 该 Agent 所需的账号、API Key 或订阅。Sift 不附送模型额度，运行会消耗你自己的额度；开始前请确认供应商计费与 [`brain.daily_token_limit`](docs/specs/config.md#34-brain)。

先验证认证和 Agent：

```bash
gh auth status        # GitHub；未登录：gh auth login
# 或
glab auth status      # GitLab；未登录：glab auth login

claude --version      # 换成你实际使用的 Agent，例如 codex、cursor 或 pi
```

## 安装

安装器默认查询并安装 **latest release**，不固定旧版本：

```bash
curl -fsSL https://raw.githubusercontent.com/xsift/sift/main/scripts/install.sh | bash
export PATH="$HOME/.sift/bin/current:$PATH"
sift --version
```

默认不会修改 shell 配置。可将上面的 PATH 写入 `~/.zshrc` / `~/.bashrc`，或明确允许安装器添加：

```bash
curl -fsSL https://raw.githubusercontent.com/xsift/sift/main/scripts/install.sh | SIFT_AUTO_PATH=1 bash
```

如不接受 `curl | bash`，请按 [安装指南](docs/guides/installation.md) 下载 release 归档并先校验 SHA-256。

## 已有项目：最短真实路径

```bash
cd /path/to/your/repo
sift init
sift doctor --offline
sift service install            # 无 launchd/systemd 时会提示改用 sift daemon
```

然后给一个边界清楚、可丢弃的测试 Issue 加默认触发标签：

```bash
gh issue edit 42 --add-label "sift:run"       # GitHub
# 或：glab issue update 42 --label "sift:run" # GitLab
```

观察真实运行：

```bash
sift ps
sift timeline
sift logs <run-id>
```

需要人决定时，请复制 Sift 发布到 Issue/PR/MR 评论中的**完整命令**；批准命令包含本次 Run ID 和一次性 nonce，不能简写成 `/sift approve`。

详细的成功预期、失败恢复与清理步骤见 **[Getting Started](docs/guides/getting-started.md)**。

## 先说明安全边界

- **本地执行**：daemon、Agent、worktree 和 SQLite 状态都在你的机器；Sift 不监听 TCP/UDP 端口。
- **隔离修改**：Agent 在独立 worktree/分支工作，不直接改主工作区。失败时可 `sift kill <run-id>`，检查后丢弃分支/worktree；已推送内容仍需在 Forge 上按常规权限处理。
- **默认不自动合并**：`auto_merge` 默认是 `false`。即使显式开启，也必须通过策略认证、Gate 和 Forge 的 expected-head 能力检查。
- **AI 没有最终决定权**：策略、Gate、可信 operator 与 Forge 事实约束最终动作；不确定信息 fail closed。
- **凭证不托管**：Sift 调用你已登录的 `gh` / `glab` 和本机 Agent，不接管它们的凭证生命周期。
- **单 Coordinator**：同一仓库、同一触发标签只运行一个主动 Sift Coordinator。多个独立 daemon 没有共享锁，会重复消费同一 Issue；团队成员可以作为多个 operator 操作同一个 Coordinator。

完整边界和恢复方式见 [Getting Started：安全、成本与清理](docs/guides/getting-started.md#安全成本与清理)。

## Sift 做什么

- 从 Issue 出发，分解、执行、运行 Checks/Gate，并创建 Change；
- 在需要人时，把带证据和可执行命令的决策请求发回 Forge；
- 用 `ps`、`logs`、`timeline` 和 `metrics` 提供本地可观测性；
- 统一 GitHub/GitLab 差异，同时保留 Forge 作为 Issue、PR/MR、审批和合并事实源；
- 支持 process 与 tmux 运行后端，并保留审计、预算与恢复证据。

## 当前状态

Sift 是仍在快速演进的本地自动化工具。GitHub/GitLab 控制面、process/tmux 运行时、Gate、人工命令、查询 CLI、安装与用户级服务已经落地；真实环境、Agent 版本和平台能力仍可能不同，请把 `sift doctor` 与小型试跑作为接入门禁。内部 M1–M8 进度不再放在用户首屏；需要项目执行状态时查看 [`docs/STATUS.md`](docs/STATUS.md)。

## 文档

| 文档 | 内容 |
|---|---|
| [Getting Started](docs/guides/getting-started.md) | 两条首次使用路径、成功预期、失败恢复和清理 |
| [安装指南](docs/guides/installation.md) | release 校验、安装、升级与 service |
| [故障排查](docs/runbooks/troubleshooting.md) | daemon、socket、配置与升级问题 |
| [配置规格](docs/specs/config.md) | `~/.sift/config.yaml` 字段、默认值与预算 |
| [产品需求](docs/PRD.md) | 产品边界与完整需求 |

## 开发

需要 Go 1.25+：

```bash
go test ./...
go build -o sift ./cmd/sift
go build -o sift-agent-wrapper ./cmd/sift-agent-wrapper
```

发布用户应优先使用安装器或已校验的 release 归档，而不是只复制一个二进制。

## 许可

[MIT](LICENSE)
