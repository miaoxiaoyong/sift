---
status: active
created: 2026-07-30
verified: 2026-07-30
summary: 代码精简整改任务清单 — 原因、必要性、证据支撑
---

# 代码精简整改任务清单

> **基线（撰写时）**：`b6d89c0`（2026-07-30）— 仍在历史中，相对当前 HEAD 已过时  
> **核实 HEAD**：≈ `29803d5`（2026-07-30）  
> **当前规模**：Go 生产代码 ~24,235 行，测试 ~20,355 行，Markdown 文档 ~27,211 行  
> **目标**：生产代码从 ~24K 行压到 ~11K 行（减 ~55%），同时不丢失已验证的功能和门禁  
>
> **使用方式**：每个任务对应一个独立的可勾选项。建议按 P0→P1→P2→P3 顺序推进（见下方核实结论：部分 P1 删除项已 REJECT）。

---

## 核实结论（2026-07-30）

相对撰写基线 `b6d89c0`，在 HEAD ≈ `29803d5` 上复核。跟踪 meta：[#746](https://github.com/miaoxiaoyong/sift/issues/746)。

| 任务 | 判定 | 说明 |
|------|------|------|
| **T004** 删除 T6/T7 | **REJECT / 勿执行** | T6 已生产接线（`SetInterruptT6`）；WBS §5.1 经 #721 PASS / #733 PASS。删除会撤销 Sol 已审 M5 工作。PRD「立项信号」≠「已核销实现可删」。**不建实现 issue。** |
| **T005** 删除 replay | **REJECT / 勿执行** | WBS §4.5 已核销（`internal/replay`）；PRD 要求 Gate 配套 replay。体量声称（~260 LOC、仅 `reconciler_test` 引用）为真，但删除违反已关闭的 M4 验收。**不建实现 issue。** |
| **T006** forgebudget | **REVISE** | 真实 LOC：`charger.go`=67、`charger_test.go`=125（合计 192）。原文证据 2458/4480 **为假**。可后续简化/内联；**不得**删除 M2 API 预算能力。时机：M5 wave-2 预算 §5.6 之后或一并做。 |
| **T007** storage 文件合并 | **ACCEPT（P2 refactor）** | 非测试 `.go` ≈40（非 71）；含测试 ≈73；约 17.7k LOC。合并只降**文件数**，**不降 LOC**。时机：M5 phase gate 后或专门重构日；与触碰 storage 的工作串行。 |
| **T008** config 合并 | **ACCEPT（P2）** | 16 个生产文件；生产 ~2703 LOC（3855 含测试）。时机：T007 之后，或无冲突时可并行。 |
| **T009** decode+contract→schema | **ACCEPT（P2/P3）** | 优先 M5 gate 之后。 |
| **T010** 删除 genschema | **ACCEPT（P3 optional）** | ~459 LOC。倾向移出主树或文档注明再生路径，而非硬删失去 regenerate。时机：空闲。 |
| **T011** 单实现接口 | **ACCEPT（P3 audit）** | `channelworker` 为 **2** 个接口而非 1。时机：空闲。 |

**文档缺口**：正文与执行建议写「P0→P3」，但曾出现工作区删光 **P0 / T001–T003** 的脏状态，与叙述不一致。本次核实**保留**下方 P0 节；P0 尚未拆 GitHub 跟踪 issue，属跟踪缺口（非 REJECT）。

**执行口径**：勿再按原文 P1「这周删 backlog」推进 T004/T005；可执行项为 T006（修订范围）、T007–T011。P2 文件合并预期是导航改善，不是用合并「砍掉一半行数」。

---

## P0 — 流程优化（今天可做，防止问题继续产生）

### T001 改 review prompt：增量模式

| 维度 | 内容 |
|------|------|
| **做什么** | 修改 project skill 中的 review 提示词，要求 AI reviewer 只输出「与上次 review 相比新增的 finding」。上次已通过的 finding 不再重复。每条 finding 固定格式：`[P0/P1/P2/DEFER] 描述` |
| **为什么** | 当前每轮 review 生成完整独立文件，9 轮 rereview = 9 份结构几乎相同的文档。M5 command spec 审了 9 轮，产生 9 份独立 .md，每份都在重新分析同样的问题 |
| **证据** | review 文档 185 份，~15,820 行，占全部 .md 的 58%，是 Go 生产代码的 65%。M5 command spec rereview 序列（9 轮）和 M5 channel webhook worker rereview 序列（13 轮）是典型案例 |
| **必要性** | 不改变 review 流程，§10 分析的「复审驱动膨胀」会持续发生。当前每轮 review 的产出量 ≈ 初始 review，没有边际递减 |
| **预估效果** | review 文档量减少 60-70%，从 ~16K 行降到 ~5K 行 |
| **风险** | 低。只改提示词，不改工具。如果 reviewer 不遵守，可以在后续 review 中纠正 |
| **核实** | 正文保留；本次未拆实现 issue（跟踪缺口，见上） |

### T002 改 review prompt：scope gate

| 维度 | 内容 |
|------|------|
| **做什么** | review 输出中每条 finding 强制标注优先级标签 `[P0/P1/P2/DEFER]`，并在 review 末尾提供「scope summary」表格列出各优先级数量 |
| **为什么** | 当前 reviewer 从不建议 defer，author 从不拒绝。结果：每条 review finding 都被实现，即使它属于 V1 范围 |
| **证据** | 需与核实结论对齐：不得再以「PRD backlog ⇒ 可删已验收实现」为由推进 T004/T005 |
| **必要性** | 没有 scope gate，PRD 的「明确不做」和「Backlog」章节就形同虚设；同时须防止误删已核销实现 |
| **预估效果** | 防止未来继续产生无立项的膨胀代码 |
| **风险** | 低。需要人工复核 reviewer 的优先级判断 |
| **核实** | 正文保留；本次未拆实现 issue（跟踪缺口，见上） |

### T003 首次提交前的新增/删除比检查

| 维度 | 内容 |
|------|------|
| **做什么** | 在 `pre-commit hook`（或相当于 git commit 前的手动检查步骤）中加入新增/删除行比警告（例如 >5:1） |
| **为什么** | AI 代码生成倾向只有新增，从不主动删除或重构 |
| **证据** | storage 等包文件数膨胀；合并文件≠自动减 LOC（见 T007 核实） |
| **必要性** | 防止代码单向膨胀 |
| **预估效果** | 每次提交的新增/删除比从 ~10:1 降到 ~3:1（目标，非保证） |
| **风险** | 低。只是警告，不是硬拒绝 |
| **核实** | 正文保留；本次未拆实现 issue（跟踪缺口，见上） |

---

## P1 — 原「删除 Backlog」项（已核实；两删项 REJECT）

### T004 删除 brain 中的 T6/T7 实现 — **REJECT / 勿执行**

| 维度 | 内容 |
|------|------|
| **原提案** | 从 `internal/brain/t4t6t7.go` 移除 T6/T7 执行路径，改为 `ErrNotImplemented` |
| **原为什么（已否决）** | PRD §5.3 立项信号尚未触发 |
| **核实为什么拒绝** | T6 已生产接线（`SetInterruptT6`）；WBS §5.1 经 #721 PASS / #733 PASS；删除会撤销 Sol 已审 M5 工作。PRD「立项信号」≠「已核销实现可删」 |
| **证据（保留参考）** | `t4t6t7.go` 等体量仍大，但这不能成为删除已验收路径的理由 |
| **预估效果** | 不适用 — **不执行** |
| **风险** | 若误执行：高（破坏 M5 验收） |

### T005 删除 replay 包 — **REJECT / 勿执行**

| 维度 | 内容 |
|------|------|
| **原提案** | 删除 `internal/replay/`，并去掉 `reconciler_test` 引用 |
| **原为什么（已否决）** | 尚无足够 Change 流过 Gate |
| **核实为什么拒绝** | WBS §4.5 已核销；PRD 要求 Gate 配套 replay。删除违反已关闭的 M4 验收 |
| **证据（更正后仍成立的部分）** | `internal/replay/` ~260 LOC；生产引用面窄（测试侧 `reconciler_test`）— **体量声称为真，删除结论为假** |
| **预估效果** | 不适用 — **不执行** |
| **风险** | 若误执行：高（破坏 M4 验收） |

### T006 forgebudget：简化/内联（修订）— **REVISE，勿删能力**

| 维度 | 内容 |
|------|------|
| **做什么** | 可简化或内联 `internal/forgebudget/` 的实现组织；**保留** M2 API 预算能力（配置上限 + 计数/降级）。禁止「直接删模块」 |
| **为什么** | 独立包对 V0 可能偏重，但预算是已存在能力，不是可丢的 backlog |
| **证据（已更正）** | `charger.go`=**67** LOC，`charger_test.go`=**125** LOC，合计 **192**。原文 2458 / 4480 **为假**。引用方含 `internal/daemon/daemon.go` |
| **必要性** | 若做，目标是降低抽象噪音，不是取消预算 |
| **预估效果** | 组织简化；LOC 降幅有限（总量本就 ~192） |
| **风险** | 中。不得在 M5 §5.6 预算波次前破坏 API 预算行为 |
| **时机** | M5 wave-2 预算 §5.6 之后，或与之同一波 |

---

## P2 — 机械合并（需专门安排；合并不砍 LOC）

### T007 storage 包：薄 CRUD 文件合并（~40→~20）— **ACCEPT P2**

| 维度 | 内容 |
|------|------|
| **做什么** | 将 `internal/storage/` 中薄 CRUD 生产文件按领域合并为约 20 个文件，不改变外部 API。建议分组表可作起点，执行前按当前树重扫 |
| **为什么** | 单机 SQLite 持久层不需要过度 entity-per-file；文件数造成导航噪音 |
| **证据（已更正）** | 非测试 `.go` ≈**40**（不是 71）；含测试 ≈**73**；约 **17.7k** LOC。合并降低**文件数**，**不降低 LOC** |
| **必要性** | 改善可导航性与评审面；不要用「从 16K→8K 行」作为成功标准 |
| **预估效果** | ~40 生产文件 → ~20；LOC 基本不变 |
| **风险** | 中。与 storage 改动串行；建议 M5 phase gate 后或专门重构日 |
| **时机** | post-M5-gate 或 dedicated day |

**建议的合并分组**（草案；执行前按仓库实况调整）：

| 新文件 | 包含旧文件 | 预估行数 |
|--------|-----------|---------|
| `runs.go` | interrupt.go, advance_interrupt.go, termination.go, launch.go, transition.go, assignment.go | ~1,500 |
| `events.go` | appendevent.go, events.go, outbox.go | ~400 |
| `forge_io.go` | intake.go, intake_m2.go, intake_reply.go, change.go, ready_change.go, reverse_sync.go, forgebudget.go, report.go, report_quota.go, gate.go, gate_candidate.go | ~1,500 |
| `brain_io.go` | brain.go, ledger.go, proposal.go | ~700 |
| `channel_io.go` | channel.go, channel_batch.go | ~350 |
| `system.go` | boot.go, migrate.go, storage.go, security.go, doctor.go, testseed.go | ~600 |
| `scheduler.go` | scheduler.go | ~200 |
| `command.go` | command.go, command_probe.go | ~300 |
| `handoff.go` | handoff.go, attempt_race.go | ~400 |
| `recovery.go` | recovery.go | ~300 |
| `config_activation.go` | config_activation.go | ~100 |
| `reverse_sync.go` | reverse_sync.go | ~100 |
| 测试文件 | 合并到对应的 *_test.go（如 `forge_io_test.go`） | 测试 LOC 不因合并而减少 |

### T008 config 包：16 个 production 文件合并到 ~5 个 — **ACCEPT P2**

| 维度 | 内容 |
|------|------|
| **做什么** | 将 `internal/config/` 的生产文件从 16 个合并到约 5 个，按功能分组：类型定义、加载、校验、规范化、激活 |
| **为什么** | 配置解析层文件过碎，增加导航成本 |
| **证据（已更正）** | 16 个 production 文件；生产 LOC ≈**2703**（**3855 含测试**）。`normalize.go` 仍是最大文件之一 |
| **必要性** | 合并文件数；勿默认「行数腰斩」 |
| **预估效果** | 16 → ~5 生产文件；LOC 降幅次要 |
| **风险** | 中。全局引用多，需全量测试 |
| **时机** | T007 之后，或确认无冲突时并行 |

### T009 decode + contract 合并为 internal/schema — **ACCEPT P2/P3**

| 维度 | 内容 |
|------|------|
| **做什么** | 将 `internal/decode/` 与 `internal/contract/`（及合理子包边界）收敛为 `internal/schema/`，保留 JSON schema 校验与 envelope 解码 |
| **为什么** | 两包职责重叠，import 面分散 |
| **证据** | decode 引用 contract 类型；schemagen/genschema 与运行时校验闭环可同域 |
| **必要性** | 降低包数与 import 噪音 |
| **预估效果** | 包数下降；LOC 降幅视重复代码而定（勿写死腰斩） |
| **风险** | 中。需更新多包 import |
| **时机** | 优先 M5 gate 之后 |

---

## P3 — 基础设施清理（有余力再做）

### T010 迁移/外置 schema 生成器（非硬删 schemas）— **ACCEPT P3 optional**

| 维度 | 内容 |
|------|------|
| **做什么** | 将 `internal/contract/genschema`、`schemagen`、`internal/brain/genschemas` 等生成入口移出主构建路径（out-of-tree / `tools/` / 文档说明），**保留** regenerate 能力与已提交的 `.schema.json`。不把「手写维护且丢掉生成器」当作默认 |
| **为什么** | 运行时不需要生成器在主模块热路径上；但失去再生路径代价高于 ~459 LOC |
| **证据** | 生成器合计 ~**459** LOC |
| **必要性** | optional chore；空闲时做 |
| **预估效果** | 主树更干净；不删除 schema 产物 |
| **风险** | 中（若硬删生成器）。外置+文档则低 |
| **时机** | idle |

再生入口与 drift 检查见 [JSON Schema 再生](../dev/schema-generation.md)。

### T011 审计单实现接口 — **ACCEPT P3 audit**

| 维度 | 内容 |
|------|------|
| **做什么** | 审计 `internal/attempt`、`internal/launchworker`、`internal/channelworker` 等单实现接口，评估是否改为直接结构体；有测试/替换价值则保留 |
| **为什么** | 过早接口增加间接层；但测试 mock 可能依赖接口 |
| **证据（已更正）** | attempt / launchworker 各约 1 接口；**channelworker 为 2 个接口**（不是 1） |
| **必要性** | 审计后决定，禁止无差别删除 |
| **预估效果** | 视审计结果；可能只改少量调用点 |
| **风险** | 低–中（测假体） |
| **时机** | idle |

审计结论见 [单实现接口审计](../analysis/2026-07-30-single-implementation-interface-audit.md)：收敛 `attempt.Runner`，保留 `launchworker.Backend` 与 `channelworker` 的两个 side-effect 接口。

---

## 执行建议

### 顺序选择（核实后）

| 策略 | 适用场景 | 理由 |
|------|---------|------|
| **先门禁后重构** | 默认 | M5 phase gate / §5.6 预算波次完成前，不做 T007–T009 大搬；T006 跟 §5.6 |
| **P2 文件合并日** | gate 后专门一天 | T007 → T008（或无冲突并行）；预期收益是文件数不是 LOC |
| **P3 空闲** | 有余力 | T010（外置生成器）、T011（接口审计） |
| **禁止** | — | 执行 T004/T005 删除；用错误 LOC 当证据；把 P2 合并当成「砍 50% 行数」 |

### 人工复核要求

1. **REJECT 项**：不得以本计划原文为由开删除 T6/T7 或 replay 的 PR
2. P2 任务（合并）：先在分支上跑完整测试套件，确认 green 后再合入 main；成功标准看文件数/可导航性
3. P0 任务：仍可做流程优化，但需单独开 issue（当前为跟踪缺口）
4. T006：diff 必须证明 API 预算行为保留

### 成功标准（修订）

| 指标 | 当前（核实） | 目标 |
|------|-------------|------|
| Go 生产代码 | ~24K 行 | 不再以「删 T6/T7/replay」凑数；整体压缩另议 |
| storage 生产文件数 | ~40 | ~20（LOC 基本不变） |
| config 生产文件数 | 16 | ~5 |
| forgebudget 证据 | 已更正为 67+125 | 禁止再引用 2458/4480 |
| T004/T005 | REJECT | 保持已核销实现 |
