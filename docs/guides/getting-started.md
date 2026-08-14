---
status: active
created: 2026-08-14
summary: 从安全试跑或已有仓库完成首个 Sift Run
---

# Getting Started

本指南把首次使用分成两条真实路径：

- **路径 A：Bluff Template**——安全试炼场，但 onboarding bootstrap 尚未完成；
- **路径 B：已有仓库**——现在可用，建议先选一个可丢弃的小 Issue。

安装和升级细节以 [安装指南](installation.md) 为准；这里聚焦从零到首个 Run。

## 0. 前置检查

### 系统与 Git

Sift 发布包支持 macOS/Linux 的 amd64/arm64。先确认当前目录是有 `origin` 的 Git 仓库：

```bash
git --version
git remote get-url origin
```

**成功预期**：两条命令均输出版本或远端 URL。

**失败恢复**：先安装 Git；若没有 `origin`，进入正确的 clone，或按团队约定添加 GitHub/GitLab 远端。`sift init` 依靠远端识别项目，无法识别时不会猜测目标仓库。

### Forge CLI 与认证

GitHub 项目安装 [GitHub CLI](https://cli.github.com/)，GitLab 项目安装 [GitLab CLI](https://gitlab.com/gitlab-org/cli)。只需检查对应平台：

```bash
gh --version && gh auth status
# 或
glab --version && glab auth status
```

**成功预期**：CLI 输出版本，`auth status` 显示当前 host 和登录用户。

**失败恢复**：

```bash
gh auth login       # GitHub
# 或
glab auth login     # GitLab
```

公司自建实例应登录项目 remote 对应的 host。Sift 复用官方 CLI 的登录，不保存或刷新 Forge token。

### Coding Agent 与额度

Sift 必须能从终端启动 Agent，而不只是打开 IDE。向导会自动探测 Claude Code、Codex CLI、Cursor CLI、pi、Gemini CLI、Aider、Qwen Code 和 Cody；未收录的 CLI 也可用可执行文件名登记。

```bash
claude --version       # 示例；换成 codex、cursor、pi 等实际命令
```

**成功预期**：命令无需图形交互即可输出版本。只有 Cursor GUI 而没有 PATH 中的 `cursor` CLI，不满足后台启动条件。

**失败恢复**：安装并登录 Agent CLI，确认它在 daemon 可见的 PATH 中；也可以在 `sift init` 中输入绝对可执行路径。Agent 的账号、API Key、订阅和模型费用由你负责。Sift 配置中的 Brain token/API/attention 预算用于自身调度和 fail-closed，但不应当作供应商账单上限；首次测试请选小任务并同时检查供应商侧限额。

## 1. 安装 Sift

默认安装 latest release：

```bash
curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | bash
export PATH="$HOME/.sift/bin/current:$PATH"
sift --version
sift-agent-wrapper --version
```

**成功预期**：两个命令输出相同 release 版本，安装目录为 `~/.sift/bin/<version>`，稳定入口是 `~/.sift/bin/current`。

**失败恢复**：

- `sift: command not found`：重新执行 PATH export，并把它写入 `~/.zshrc` 或 `~/.bashrc`；
- 不接受 `curl | bash`：按 [release 归档安装](installation.md) 下载并校验 SHA-256；
- wrapper 版本不同：不要单独复制二进制，重新安装同一份完整 release。

## 路径 A：Bluff Template

[`hexai-cn/bluff`](https://github.com/hexai-cn/bluff) 是计划用于独立、安全试跑的入门项目。可玩游戏 MVP 由 [#1](https://github.com/hexai-cn/bluff/issues/1) 跟踪，Template bootstrap、合法 policy 和 seed Issues 由 [#2](https://github.com/hexai-cn/bluff/issues/2) 跟踪；两项尚未合入可运行代码。

### 当前可做

1. 打开 Bluff，点击 **Use this template**；
2. 选择 **Create a new repository**，创建到你自己的账号/组织；
3. clone 这个新仓库，而不是普通 Fork，也不要在 `hexai-cn/bluff` 上游操作；
4. 阅读项目 README 和 `.sift/policy.yaml`。

**成功预期**：新仓库不属于 `hexai-cn/bluff`，有独立的 Issues、分支和权限边界。

**当前限制**：在 #2 完成前，不要运行或转述尚不存在的 `scripts/bootstrap.sh`，也不要期待 seed Issues 已被创建。当前没有可作为证据的 Sift 全链路截图/GIF。本路径到此是预览，不宣称已经完成首次 Run。

### #2 完成后的路径

以下是 **#2 验收通过后** 才可执行的目标流程；届时以 Bluff README 中的实际命令为准：

```bash
./scripts/bootstrap.sh
sift init
sift doctor --offline
sift service install
```

bootstrap 应幂等创建 label/seed Issues，并拒绝误操作上游。在它实际合入并验证前，请走下面的路径 B。

## 路径 B：接入已有仓库

### 2. 选择安全的首个 Issue

选择一个范围小、验收清楚、允许关闭 PR/MR 并丢弃分支的 Issue。避免把首次试跑用于发布、迁移、密钥、支付、生产配置或大规模重构。

默认触发 label 是 `sift:run`。如果仓库尚无此 label，先在 Forge 的 Labels 页面创建；自定义 label 以配置为准。

**成功预期**：Issue 描述包含目标、范围、验收方式，仓库主工作区无你不想混入的临时修改。

### 3. 初始化

```bash
cd /path/to/your/repo
sift init
```

向导会：

1. 从 `origin` 探测 GitHub/GitLab 和项目；
2. 探测 PATH 中的 Agent 并让你选择；
3. 从已登录的 Forge CLI 记录 operator；
4. 写入 `~/.sift/config.yaml`。

**成功预期**：末尾显示已写入配置；`sift agent list` 和 `sift project list` 能看到所选 Agent/项目。

**失败恢复**：

- 无法解析项目：检查 `git remote get-url origin`，并确认 host 是 GitHub/GitLab；
- 未发现 Agent：输入可执行文件名或绝对路径，或先修复 service 可见的 PATH；
- operator 缺失：先完成 `gh auth login` / `glab auth login`，再运行 `sift init`；
- 写错配置：重新运行向导会备份原文件为 `config.yaml.bak`；字段级修复见 [配置规格](../specs/config.md)。

### 4. 离线检查

```bash
sift doctor --offline
sift status
```

**成功预期**：doctor 没有 error；`status` 能读取本地配置状态。doctor 的退出码是 0=无问题、1=至少一个 warning、2=至少一个 error。

**失败恢复**：不要忽略 warning。按每条提示修复 PATH、文件权限、Agent 或项目配置后重跑；配置文件存在时应保持：

```bash
chmod 700 "${SIFT_HOME:-$HOME/.sift}"
chmod 600 "${SIFT_HOME:-$HOME/.sift}/config.yaml"
```

### 5. 启动 Coordinator

推荐用户级服务：

```bash
sift service install
sift service status
sift doctor
```

macOS 使用 launchd，Linux 优先 systemd user unit。没有可用 supervisor 时，`service install` 会给出 foreground 提示，此时在一个保持打开的终端运行：

```bash
sift daemon
```

**成功预期**：service 为运行状态，在线 `sift doctor` 可连接 daemon。Sift 只创建本地 Unix sockets，不监听网络端口。

**失败恢复**：

```bash
sift service restart
sift service status
sift doctor
```

若仍失败，查看 [故障排查](../runbooks/troubleshooting.md)。前台模式终端退出后 daemon 也会停止，不承诺崩溃自启。

### 6. 触发首个 Run

给测试 Issue 加 label：

```bash
gh issue edit 42 --add-label "sift:run"       # GitHub
# 或
glab issue update 42 --label "sift:run"      # GitLab
```

也可以在 Forge 网页/App 中添加同名 label。

**成功预期**：轮询后 Issue 出现在 `sift ps`，Forge 上可看到 Sift 推进状态所产生的标签或评论。它不是即时 webhook；短暂等待轮询属于正常情况。

**失败恢复**：确认 label 拼写、daemon、项目配置、Forge 认证和 API 权限，然后运行：

```bash
sift doctor
sift ps -a
sift timeline --limit 20
```

同一 Issue 不要同时由两台机器上监听相同 label 的 daemon 消费；这不是负载均衡，会产生重复 Run 和远端副作用。

### 7. 观察进度

```bash
sift ps
sift timeline --limit 20
sift logs <run-id>
sift worktree <run-id>
```

**成功预期**：`ps` 显示 queued/running/waiting_human/done/failed 中的状态；`timeline` 显示 append-only 事件；有 attempt 后 `logs` 和 `worktree` 返回对应信息。

**失败恢复**：

- daemon 未连接：先恢复 service/foreground daemon；
- Run failed：用 `sift logs <run-id>` 和 `sift timeline --run <run-id>` 找原因，修复环境后执行 `sift retry <run-id>`；
- Agent 卡住或方向错误：执行 `sift kill <run-id>`，再检查隔离 worktree，不要直接合并其分支。

### 8. 审批或拒绝

需要人工决定时，Sift 会在 Forge 评论中给出带 Run ID 和一次性 nonce 的完整命令，例如命令形态为：

```text
/sift approve <run-id> <nonce>
```

把评论里**原样提供的完整命令**回复到对应 Issue/PR/MR。不要手写 nonce，也不要只回复 `/sift approve`。其他可用动作同样以评论提供的命令和选项为准。

**成功预期**：Sift 回写 command acknowledgement，并继续 Gate/Change 流程。默认 `auto_merge=false`，Gate 通过不等于已自动合并；最终以 Forge 上的 PR/MR 状态为准。

**失败恢复**：命令过期、目标不符、operator 不可信或 nonce 错误时会 fail closed。回到最新 Sift 评论复制当前命令；不要复用旧评论中的命令。

## 安全、成本与清理

### 停止和丢弃试跑

```bash
sift kill <run-id>       # 停止活跃 Run
sift worktree <run-id>   # 找到并检查隔离 worktree
sift rm <run-id>         # 终态后从默认列表归档，历史仍保留
```

若 Run 仍在执行而你确定要终止并归档，可用 `sift rm -f <run-id>`。归档不是删除 Forge 上的分支或 PR/MR；远端清理由你按仓库正常流程执行。删除本地 worktree 前先确认 Agent 已停止、需要的 diff 已保存，并使用 Git 的标准 worktree 流程。

### 不会替你做的事

- Sift 不保证 Agent 产出正确，必须看 diff、Checks 和 Gate 证据；
- Sift 不提供模型额度，也不承诺配置预算等于供应商账单硬上限；
- Sift 不接管 `gh` / `glab`、Agent 或云模型的凭证；
- Sift 不让多个独立 Coordinator 安全抢同一 Issue 池；一个仓库/触发 label 只保留一个主动 Coordinator；
- Sift 不把 kill/归档当成已推送远端内容的自动回滚。

### 默认信任保证

- Agent 在隔离 worktree/分支执行；
- `auto_merge` 默认关闭；
- 未知 Checks、review、mergeability 或输入会 fail closed；
- AI 的建议不能覆盖 policy、Gate 或可信 operator 决定；
- Forge 是 Issue、Change、审批和合并状态的最终事实源。

完成首个小 Run 后，再逐步扩大任务范围、调整 `.sift/policy.yaml` 和预算；不要在未观察真实 Agent/Forge 行为前直接开启自动合并。
