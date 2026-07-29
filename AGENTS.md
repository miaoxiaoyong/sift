# AGENTS.md — Sift 代理导航

## 先读

- 文档地图、命名约定、上下文加载规则：[docs/README.md](docs/README.md)

## 项目现状

M1–M4 已有 Go 实现：控制面/配置/Brain 壳、双平台 Forge 与 Intake、M3 process runtime，以及 M4 的有效策略、Gate/Shadow、Ledger/认证、回放与 Change 创建均已接通；相关规格保持 `active`。产品需求：[docs/PRD.md](docs/PRD.md)；架构与工作基线见 [docs/DESIGN.md](docs/DESIGN.md) / [docs/WBS.md](docs/WBS.md)。

[S2/M2 第四次定向复审](docs/reviews/2026-07-29-s2-m2-rereview-4-pi-gpt-5.6-sol.md) 结论为 **PASS WITH NOTES**（doctor 时序 flake 与双 issue consumer 集成测试粒度属非阻断注记），M2 门禁已通过。[M3 P1 第八次定向复审](docs/reviews/2026-07-29-m3-rereview-8-pi-gpt-5.6-sol.md) 已关闭 paused recovery 的最后 P1；随后 [M3 阶段门禁](docs/reviews/2026-07-29-m3-phase-gate-pi-gpt-5.6-sol.md) 以 **PASS WITH NOTES** 允许进入 M4。该结论只通过 V4 的 M3 process 首跑段；DESIGN §10.1 双后端完整矩阵仍按 WBS 留 M6、真实 Agent/version 资格留 M7，不得描述为完整 V4/M6 通过。[M4 第六次定向复审](docs/reviews/2026-07-29-m4-phase-gate-rereview-6-pi-gpt-5.6-sol.md) 结论为 **PASS WITH NOTES**：M4 门禁已通过，可进入 **M5：Attention、Command、Report、Brain 与指标**；PRD 状态图同步与并行时序 flake 属非阻断注记。不得将此读作 M5 已实现，或将 M4 的 V11 审计段描述为 M5 指标分母已闭合。

## 上下文规则（摘要）

- 默认只加载 `status: active | draft` 的文档；`done / abandoned / superseded`、`reviews/`、`CHANGELOG.md` 仅回溯类任务加载。
- 不全量加载 `docs/`；按 docs/README.md 的「按任务类型的默认上下文集」表选读。
- 引用不复制：事实只写一次，其余地方链接。
