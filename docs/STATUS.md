---
status: active
created: 2026-08-04
last_updated: 2026-08-07
summary: Sift 总体计划执行情况。工作包分解见 WBS.md。
---

# Sift — 总体计划执行情况

> **文档分工**：本文跟踪**总体计划的执行**(里程碑状态、门禁裁决、整体进度、遗留与下一步)。**任务/工作包分解见 [`WBS.md`](WBS.md)**(里程碑、工作包、验收标准、范围/依赖,不含状态)。需求语义见 [PRD](PRD.md),结构理由见 [DESIGN](DESIGN.md)/ADR,字段契约见 `specs/`,执行步骤见 `plans/`。

## 里程碑执行状态

| 里程碑 | 状态 | 门禁裁决(独立复审) | 产出要点 |
|---|---|---|---|
| M1 骨架 | ✅ 完成 | 第二次定向复审 PASS WITH NOTES | Go/SQLite、状态机、outbox、控制面、配置、Brain 壳/T1/T2、fake 骨架链 |
| M2 Forge/Intake | ✅ 完成 | 第四次定向复审 PASS WITH NOTES | GitHub/GitLab 适配、Intake、actor 闸门、API 预算 |
| M3 Runtime | ✅ 完成(process 首跑段) | M3 phase gate PASS WITH NOTES | process backend、wrapper handoff、恢复、泛型 Interrupt 发射核心 |
| M4 Gate/Shadow/Ledger/回放 | ✅ 完成 | 第六次定向复审 PASS WITH NOTES | 有效策略、Gate/Shadow、Ledger/认证、回放、Change 创建 |
| M5 Attention/Command/Report/Brain/指标 | ✅ 完成 | #835 独立复审 PASS WITH NOTES | Interrupt 全功能、Command、Report、Channel、九项指标 |
| **M6 tmux + 完整故障矩阵** | ✅ **完成** | **#852/#873 PASS WITH NOTES** | tmux 第二后端、PTY、V2/V4 双后端全矩阵、阶段门归档 |
| M7 真实 Agent + 双 Forge + PoC 取证 | ⏸ 未启动(人工门禁 A1–A9) | — | 真实 Agent/version、双 forge、并发、凭证 spike |
| M8 发布 | ⏸ 未启动 | — | 单归档、四组合冒烟、托管、升级、Homebrew |

> 各复审原文见 [`reviews/`](reviews/)。M6 阶段门归档:[`reviews/2026-08-04-m6-phase-gate-pi-minimax-m3.md`](reviews/2026-08-04-m6-phase-gate-pi-minimax-m3.md)。

## 整体进度

- **版本退出条件**:M6 已闭,剩 **M7(真实链/PoC 取证)+ M8(发布)**。
- **当前焦点**:M7 前置准备(人工:真实 Agent/version、双 Forge 凭证、手机审批、≥3 并行真实 Run 环境);M7 前可并行推进技术债结构优化。
- **诚实边界(不声称已闭合)**:真实 Agent/version 资格、真实 GitHub+GitLab Forge、手机审批、≥3 并行真实 Run、P50<60s —— 均为 M7 人工门禁 A1–A9,PoC 发布证据 M7/M8。

## 遗留 / 延期(已知缺口)

- **#863 daemon effective-backend 接线**:生产 run→首 attempt 创建路径尚未接出 skeleton,属 daemon 架构 + 真实 Agent 集成,**留 M7**。effective-backend 路由逻辑已在测试层证明,缺生产可达证据。
- **#883 性能 profile**:需 M7 真实负载,无真实负载前不做(优化即猜测)。
- **既有 flake**:`TestProductionTmuxWrapperCrashHarness` 等 wrapper crash-window 在 race 下偶发(AGENTS.md 已记非阻断),非功能缺陷。

## 维护轮(M6 后)

- 基线 CI 红修复(#869 backend routing fixture / #876 detached-descendant PID 竞态 / #886 real-wrapper crash-harness)—— 均为"不等 CI"期间 Linux-only 漏网,已修。
- staticcheck 清零(#874)。
- 文档规范:STATUS.md/WBS.md 分离(本文)。
- 技术债结构优化 backlog:父 epic [#878](https://github.com/miaoxiaoyong/sift/issues/878),子任务 #879(storage 拆分,进行中)/#880(SQL 中心化)/#881(已合)/#882(策略化)/#883(M7 绑定)。

## 规划评审处置(D0.3 历史归档)

WBS 的 D0.3 评审处置(R1–R16 发现→里程碑落点)属规划历史,原文见各 [reviews/](reviews/)(codex-gpt5 / cursor-opus5 / glm-5.2 / kimi-code / pi-gpt-5.6-sol D0.2 独立复评 / D0.3 定向复核),结论为 `active`。具体发现→落点映射见 WBS 各里程碑小节引用。

## 下一步

1. **M7 人工前置**(非 codegen 可自给):真实 Agent CLI/version + 凭证、真实 GitHub+GitLab 项目、手机审批设备、≥3 并行真实 Run。
2. M7 前可并行 #879/#880/#882 结构优化(每步 CI 绿、行为保持)。
3. M8 发布需 M7 PoC 证据齐全。
