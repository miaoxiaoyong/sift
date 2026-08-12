# Sift

本地多 Agent 任务编排中枢：把 GitHub / GitLab 的 Issue **筛**成已合并的变更，该自动的自动，该人审的绝不放过。

> 概念验证（PoC）：M1–M6 已完成，M7 PoC 已验证，M8 自动化核心持续完善中。

## 快速开始

从零开始，把本机接到一个 forge Issue 的自动编排。字段契约与默认值以 [docs/specs/config.md](docs/specs/config.md) 为准，安装与升级细节见 [docs/guides/installation.md](docs/guides/installation.md)。

### 1. 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | bash
```

安装指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | SIFT_VERSION=0.1.0 bash
```

安装器把 `sift` 与 `sift-agent-wrapper` 装到 `~/.sift/bin/<version>`，`~/.sift/bin/current` 指向最新版本。**默认不修改你的 shell 配置文件**（`curl|bash` 非交互，只打印 next-steps 提示）；如需自动追加 PATH（按 `$SHELL` 探测：zsh→`~/.zshrc`、bash→`~/.bashrc`，已含去重、可安全重跑）：

```bash
curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | SIFT_AUTO_PATH=1 bash
# 或本地：
./scripts/install.sh --add-to-path
```

### 2. 加 PATH

当前终端临时生效：

```bash
export PATH="$HOME/.sift/bin/current:$PATH"
sift --version
```

永久生效（zsh；bash 把 `~/.zshrc` 换成 `~/.bashrc`）：

```bash
echo 'export PATH="$HOME/.sift/bin/current:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

### 3. 登录 forge CLI

Sift 经官方 CLI 驱动 GitHub / GitLab，**不管理任何凭证**：

```bash
gh auth login       # GitHub
# 或
glab auth login     # GitLab
```

### 4. 初始化配置

使用向导生成并校验 `~/.sift/config.yaml`，避免手写配置出错：

```bash
sift init
```

向导会交互式选择 Agent 与 operator，并绑定当前目录项目；Forge 类型从项目 git remote 自动探测，无需手选。非交互环境也可通过选项传入：

```bash
sift init --agent claude --project . --forge github
```

如需自动化或高级配置，也可以手工维护配置文件；字段契约见 [docs/specs/config.md](docs/specs/config.md) §3.1–3.3。配置存在时，`~/.sift` 与 `config.yaml` 必须为属主读写（§2.1）：

```bash
chmod 700 ~/.sift
chmod 600 ~/.sift/config.yaml
```

### 5. 检查

```bash
sift doctor --offline   # 只读诊断，exit 0 表示健康
```

### 6. 启动

```bash
sift daemon             # 前台运行
# 或注册自启：
sift service install
```

### 7. 触发

给 Issue 打上 trigger label（默认 `sift:run`，可在配置 `labels` 覆盖）：

```bash
gh issue edit 42 --add-label sift:run
```

观察运行：

```bash
sift ps            # 运行中 Run / attempt、注意力余量与隔离状态
sift timeline      # append-only 事件时间线
```

---

## 一句话

**LLM 有话语权、没有决定权；Forge 是事实源；需要人时系统主动找人。**

## 形态

| 维度 | 说明 |
|------|------|
| **双平台** | GitHub 与 GitLab 对等，经 `gh` / `glab` 官方 CLI 集成，不管理任何凭证 |
| **零界面** | 不做看板。决策简报发到 Issue / PR 评论，一条 `/sift approve` 完成审批——手机上的 forge App 就是审批终端 |
| **零暴露面** | 守护进程不监听任何端口，自适应轮询取代 webhook |
| **本地执行** | 复用你已订阅的 coding agent，代码不出本机 |

## 文档

| 文档 | 内容 |
|------|------|
| [docs/PRD.md](docs/PRD.md) | 产品需求（问题、公理、范围、状态机、模块） |
| [docs/guides/installation.md](docs/guides/installation.md) | 安装、升级与配置引导 |
| [docs/specs/config.md](docs/specs/config.md) | 全局配置字段契约（`~/.sift/config.yaml`） |

## 核心设计

- **Issue → 变更流水线**：从 Issue 出发，经分解、执行、门禁、审查到合并，全程自动化编排
- **注意力调度**：只在必要的检查点打扰人，推送已嚼碎的决策简报而非原始日志
- **Agent 无关**：可替换底层 coding agent（Claude Code / Codex / Cursor 等），不绑定任何厂商
- **Forge 抽象**：GitHub / GitLab 统一抽象，事实源由 forge 驱动，LLM 仅提供建议

详细设计见 [docs/PRD.md](docs/PRD.md)。

## 开发

### 依赖

- Go 1.22+
- `gh` CLI（GitHub 集成）
- `glab` CLI（GitLab 集成）

### 从源码构建两个发布二进制

```bash
go build -o sift ./cmd/sift
go build -o sift-agent-wrapper ./cmd/sift-agent-wrapper
./sift daemon
```

### 本地开发

```bash
# 运行测试
go test ./...

# 构建（含两个发布二进制）
go build -o sift ./cmd/sift
go build -o sift-agent-wrapper ./cmd/sift-agent-wrapper
```

> 开发命令适用于本地源码检出；发布版本请优先使用上方安装器。

## 状态

Sift 仍是 PoC，但控制面、策略、运行时、注意力/命令/报告与发布自动化核心已经实质落地；真实生产规模与完整多平台发布证据仍在持续补齐。

## 许可

本项目使用 [MIT](LICENSE) 许可（待创建）。
