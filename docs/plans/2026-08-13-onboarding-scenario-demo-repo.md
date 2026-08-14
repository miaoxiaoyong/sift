---
status: active
created: 2026-08-13
summary: 首次体验阻断分析与 Bluff Template 建设方案
---

# 初次体验推演 + 样板仓库方案（防劝退）

## 推演方法

代入“一个用终端 Coding Agent 干活的普通后端开发者，GitHub 刷到 Sift”的视角，逐阶段找阻断点。stars、About 与仓库描述是会变化的外部展示信号，不作为仓库内的长期能力事实。

## 当前建设状态（2026-08-14）

- [`xsift/bluff`](https://github.com/xsift/bluff) 的可玩游戏、合法 Sift policy、bootstrap 脚本和 6 个 seed tasks 已合入 main，仓库已设为 GitHub Template；
- 从 Template 创建独立仓库、首次 bootstrap 生成 6 个带完整 labels 的 Issues，以及重跑后仍保持 6 个 Issues，已经过 live 实测；不公开临时 smoke 仓库地址；
- 建设结论仅升级为「Template/bootstrap 路径可用且重跑幂等性已验证」。真实 Coding Agent→PR→人工审批链尚未验收，不放置或暗示该全链路已经通过的截图/GIF。

## 动线与阻断点

### 阶段 0：刷到 → 点进来？
- 缺少社会证明会降低点击意愿，但 stars 是外部、动态指标，不能靠文档伪造。
- 首屏“多 Agent 任务编排中枢”是架构师黑话，普通开发者要的是“它替我自动做 Issue→PR/MR 吗”。
- 缺真实动图/截图；证据产生前只能用诚实的最小流程图代替。
- About、topics 与 README 的 GitHub/GitLab 口径需要由 conductor 在 GitHub 仓库设置中同步。

### 阶段 1：点进来 → 装？
- "PoC" 打在第二行 → 吓退认真想用的人。
- curl|bash 陌生 0-star 项目有戒心（checksum 校验没强调）。
- 前置依赖（gh/glab + AI agent）没在显眼处。

### 阶段 2：装完 → 首次 `sift`
- **PATH 不自动加** → 首次命令找不到（劝退高发）。
- **没有"3 分钟看到价值"的路径** → 要配真 forge+agent+项目+等，反馈周期长，多数放弃。

### 阶段 3：init 配置 → 配通？
- 要绑真项目+真 Issue，但用户怕 AI 乱改真仓库 → 缺安全试炼场。
- agent 门槛：向导可探测多种终端 Agent；只有 Cursor GUI 而没有 `cursor` CLI 时不能作为后台可执行 Agent。
- 成本焦虑：跑 Issue 烧 Claude 额度，缺预算保护 assurance。

### 阶段 4：跑起来 → 信任
- 后台跑 AI 改代码，用户焦虑，缺实时感/主动通知。
- 审批要手机 forge App，初体验者想先终端试。
- 失败可恢复性：AI 搞砸了怎么回滚。

## 核心判断

CLI 易用性（help/completion/status/wizard/kill 免参）已扎实，但那是**已决定试的人**的体验。**决策点更靠前**：刷到→愿意装→第一次看到价值。那两步最薄弱。

**最大劝退**：装完看不到价值就放弃（配置周期长 + 怕碰真仓库 + 怕花额度）。

## 方案：公开 Template Repository（用户创建独立副本即用）

**不是** `sift demo` 命令（fake forge 是大工程；共享 demo 仓库有任务抢占问题），也不主推普通 Fork（GitHub Fork 不复制 Issues，且容易把 PR 错提回上游）。**而是**公开的 GitHub Template Repository [`xsift/bluff`](https://github.com/xsift/bluff)：把上下文工程文档（PRD/DESIGN/WBS/STATUS + AGENTS.md）按规范组织好，引导用户点 **Use this template** 创建自己的独立仓库 → 接 Sift → 跑自己的 seed Issue。README 面向中文用户可解释为“从模板复制一份自己的仓库”。

### 为什么这个方案最优
1. **真实可跑**：从 Template 创建后是用户自己的独立仓库，无抢占、无 fake-forge 工程。
2. **展示上下文工程价值**：样板里 PRD/DESIGN/WBS/STATUS 已按规范组织（sift 自己就是范本——`docs/README.md` 明确"文档服务人和 AI 代理"），sift 一上来就有"嚼碎的上下文"，直接体现"结构化上下文喂 AI"的核心价值。
3. **模板即用 = 自然引导用自己的仓库开始**：不碰用户现有真项目，也不会继承上游 Fork 网络带来的误提 PR 风险。
4. **成本可控**：跑的是用户自己的 Issue + 自己的 agent 额度，且样板 Issue 小而明确。

### 样板仓库该有什么
- **规范的上下文工程文档**：`docs/PRD.md`（需求/状态机）、`docs/DESIGN.md`（架构/为什么）、`docs/WBS.md`（任务分解/验收）、`docs/STATUS.md`（执行跟踪）、`docs/README.md`（文档地图/上下文加载规则）、`AGENTS.md`（agent 导航/上下文规则）。参考 sift 自身结构。
- **一个小而完整的项目**：让 sift 跑 Issue→分解→执行→门禁→PR→审批 全链路有实际意义（不是空壳）。
- **`.sift/policy.yaml`**（仓内策略范本）+ seed Issues（带 trigger label，涵盖简单/需审查两种）。
- **README 引导（当前可执行路径）**：Use this template → clone 自己的仓库 → `gh auth status`（未认证才 `gh auth login`）→ `./scripts/bootstrap.sh` 幂等创建 labels/6 个 seed Issues → `pnpm install && pnpm dev` 本地试玩 → `sift init` → `sift doctor --offline` → `sift service install`（或前台 daemon）→ 在 seed Issue 添加 `sift:run` → 用 `sift ps` / `timeline` / `logs` / `worktree` 观察 → 需要审批时只复制 Sift 评论中带 Run ID 与 nonce 的完整命令。
- **前置依赖检查说明**：gh/glab 安装与认证、一个可从终端启动的 Coding Agent。

### gh/glab 未配置的处理
README/引导里**前置**：`gh auth status`（或 glab）检查，未登录给 `gh auth login` 引导；sift init/doctor 也探测并提示。样板 README 把这一步做成醒目的"第 0 步"。

## 优化点清单（防劝退，按价值排序）

| 优先级 | 优化点 | 阻断 |
|--------|--------|------|
| 🔴 P0 | **Bluff Template Repository（创建独立副本即用）** | 看不到价值+怕碰真仓库（最大劝退） |
| 🔴 P0 | README 顶部：用户视角价值主张 + 真实动图/截图（证据前用流程图）+ 前置依赖前置 | 首屏吸引力 |
| 🟠 P1 | 安装后 PATH 更主动 | 首次命令找不到 |
| 🟠 P1 | Agent CLI/GUI 边界 + 自有额度与预算边界 | 配置门槛+成本焦虑 |
| 🟡 P2 | 运行实时感（终端进度/通知） | 后台焦虑 |
| 🟡 P2 | 审批终端路径 + "变更可回滚、不动主分支"安全保证 | 信任 |
| ⚪ P3 | About/README 平台口径统一；PoC 措辞软化 | 信任细节 |

## Conductor 外部操作

这些项目不由仓库文件伪造，也不在文档提交中代替执行：

- GitHub About 描述同时写明 GitHub/GitLab 与本地 Coding Agent；
- topics 建议：`ai-agents`、`coding-agent`、`github`、`gitlab`、`developer-tools`、`automation`、`golang`；
- 真实 Agent→PR→人工审批链独立验收后，才采集对应运行截图/GIF并升级 README 结论；
- 社会证明通过真实使用、发布和分享积累，不创建虚假 badge、stars 或运行证据。

## 非目标
- 不做 `sift demo` 命令（fake forge 大工程；公开 demo 仓库有抢占）。
- 样板仓库是独立仓库，本仓提供 README/get-started 引导并指向它；已验证的 Template/bootstrap 不得外推为真实 Agent→PR→人工审批链通过。
