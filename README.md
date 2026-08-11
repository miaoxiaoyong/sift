# Sift

本地多 Agent 任务编排中枢：把 GitHub / GitLab 的 Issue **筛**成已合并的变更，该自动的自动，该人审的绝不放过。

> 概念验证（PoC）：M1–M6 已完成，M7 PoC 已验证，M8 自动化核心持续完善中。

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | bash
```

安装指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | SIFT_VERSION=0.1.0 bash
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

## 核心设计

- **Issue → 变更流水线**：从 Issue 出发，经分解、执行、门禁、审查到合并，全程自动化编排
- **注意力调度**：只在必要的检查点打扰人，推送已嚼碎的决策简报而非原始日志
- **Agent 无关**：可替换底层 coding agent（Claude Code / Codex / Cursor 等），不绑定任何厂商
- **Forge 抽象**：GitHub / GitLab 统一抽象，事实源由 forge 驱动，LLM 仅提供建议

详细设计见 [docs/PRD.md](docs/PRD.md)。

## 快速开始

安装完成后，启动 daemon：

```bash
sift daemon
```

从源码运行或开发时，也可以按下面的方式构建两个发布二进制：

```bash
git clone https://github.com/miaoxiaoyong/sift.git
cd sift
go build -o sift ./cmd/sift
go build -o sift-agent-wrapper ./cmd/sift-agent-wrapper
./sift daemon
```

## 开发

### 依赖

- Go 1.22+
- `gh` CLI（GitHub 集成）
- `glab` CLI（GitLab 集成）

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
