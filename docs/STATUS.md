---
status: active
created: 2026-08-04
last_updated: 2026-08-10
summary: Sift 总体计划执行情况。工作包分解见 WBS.md。
---

# Sift — 总体计划执行情况

> **文档分工**：本文跟踪**总体计划的执行**(里程碑状态、门禁裁决、整体进度、遗留与下一步)。**任务/工作包分解见 [`WBS.md`](WBS.md)**。需求语义见 [PRD](PRD.md)，结构理由见 [DESIGN](DESIGN.md)/ADR，字段契约见 `specs/`，执行步骤见 `plans/`。

## 里程碑执行状态

| 里程碑 | 状态 | 门禁裁决 | 产出要点 |
|---|---|---|---|
| M1 骨架 | ✅ 完成 | PASS WITH NOTES | Go/SQLite、状态机、outbox、控制面、配置、Brain 壳/T1/T2、fake 骨架链 |
| M2 Forge/Intake | ✅ 完成 | PASS WITH NOTES | GitHub/GitLab 适配、Intake、actor 闸门、API 预算 |
| M3 Runtime | ✅ 完成 | PASS WITH NOTES | process backend、wrapper handoff、恢复、泛型 Interrupt 发射核心 |
| M4 Gate/Shadow/Ledger/回放 | ✅ 完成 | PASS WITH NOTES | 有效策略、Gate/Shadow、Ledger/认证、回放、Change 创建 |
| M5 Attention/Command/Report/Brain/指标 | ✅ 完成 | PASS WITH NOTES | Interrupt 全功能、Command、Report、Channel、九项指标 |
| M6 tmux + 完整故障矩阵 | ✅ 完成 | PASS WITH NOTES | tmux 第二后端、PTY、V2/V4 双后端全矩阵、阶段门归档 |
| **M7 真实 Agent + PoC 取证** | 🔬 **PoC 已验证** | — | **Pi Brain+Agent 双 forge 端到端跑通** |
| **M8 发布** | 🔄 **§8.1 已启动** | — | 单归档 + 版本/安装/握手（#903）；托管、升级、Homebrew 待续 |

## M7 PoC 验证成果(本轮)

**Pi 作为 Brain(T1/T2 分类)+ Agent(编码执行)在 Sift 下端到端跑通**，双 forge(GitHub + GitLab):

| 步骤 | GitHub | GitLab |
|---|---|---|
| issue 发现(trigger label) | ✅ | ✅ |
| Brain T1(Pi: triage) | ✅ valid | ✅ valid |
| Brain T2(Pi: agent 分配) | ✅ valid → pi-coder | ✅ valid → pi-coder |
| launch dispatch(qualification + wrapper spawn) | ✅ | ✅ |
| wrapper handoff(claim.acquire → permit → started) | ✅ | ✅ |
| Pi Agent 实际编码(task.json → result) | ✅ | ✅ |
| Gate 评估 | ✅ waiting_human | ✅ running |

### 本轮发现并修的 8 个生产 bug

| # | bug | 根因 |
|---|---|---|
| 1 | forge pathPart `%2F` | GitHub `/` 被整体编码 → API 404 |
| 2 | T2 + launch 不 wire | production daemon 缺 T2 调用 + launch enqueue |
| 3 | launch op 不 complete | 无 CompleteOutboxAttempt → 重试风暴 |
| 4 | GitLab labels 格式 | GitLab `["sift"]` vs GitHub `[{"name":"sift"}]` |
| 5 | ErrNoRows 中断 tick | FindPendingIntake 无行 error → 后续 project starve |
| 6 | brain envelope 字段名 | `result_text` 非 `result`(pi-brain-wrapper.py 适配) |
| 7 | qualification 空 env | bun 脚本需 PATH 里找 node(pi-run.sh 适配) |
| 8 | run.sock 路径 | 3x vs 4x filepath.Dir，wrapper 永远找不到 daemon |

### 结构优化(本轮)

| 项 | PR | 内容 |
|---|---|---|
| siftd+sift 合并 | #900 | 3→2 二进制 |
| storage 6 域拆分 | #891-#897 | 全 <500 行 |
| config/forge 拆分 | #885 | <500 行 |
| 类型 dispatch 策略化 | #899 | lookup table |
| 代码质量评审 | #890 | deadcode 4 移除 + findings 报告 |
| 兼容层清理 | #898 | WorktreeManager/ReconcilerScheduler |
| SQL 中心化评估 | #880 | 评估完毕，无安全提取项 |
| 文档规范 | #888 | STATUS.md(执行)+ WBS(纯分解) |

## 遗留 / 延期

- **M7 完整门禁**:≥3 并行 Run + P50<60s 测量、手机端审批证据、凭证存储 spike
- **#883 性能 profile**:M7 真实负载绑定
- **wrapper handoff 精调**:`waiting_human` 上的 `kill`/`retry`/`approve` 操作验证
- 既有 flake:`TestProductionTmuxWrapperCrashWindows` race 下偶发(非阻断)

## 下一步

1. **M7 剩余验收**:人工前置(并行 Run 环境、手机设备、凭证策略)
2. **M8 §8.1 已完成（#903）**:release 版本（ldflags 注入）+ GoReleaser 四组合单归档/manifest/校验和 + `~/.sift/bin/<version>/` 原子安装 + doctor 握手可见性；契约见 [`specs/release.md`](specs/release.md)
3. **M8 后续**:托管（launchd/systemd，§8.2）、Homebrew tap、干净机验收与文档（§8.3）
