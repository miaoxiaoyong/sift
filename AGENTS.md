# AGENTS.md — Sift 代理导航

## 先读

- 文档地图、命名约定、上下文加载规则：[docs/README.md](docs/README.md)

## 项目现状

M1–M2 已有 Go 实现：控制面/配置/Brain 壳之上，已接双平台 Forge 适配、Intake 调度与回复消费、outbox comment worker、API 预算与 V3/V7/V11 首段证据；[`specs/forge.md`](docs/specs/forge.md) 与五份基础规格保持 `active`。产品需求：[docs/PRD.md](docs/PRD.md)；架构与工作基线见 [docs/DESIGN.md](docs/DESIGN.md) / [docs/WBS.md](docs/WBS.md)。

[S2/M2 第四次定向复审](docs/reviews/2026-07-29-s2-m2-rereview-4-pi-gpt-5.6-sol.md) 结论为 **PASS WITH NOTES**（doctor 时序 flake 与双 issue consumer 集成测试粒度属非阻断注记），M2 门禁已通过。[M3 P1 第八次定向复审](docs/reviews/2026-07-29-m3-rereview-8-pi-gpt-5.6-sol.md) 已关闭 paused recovery 的最后 P1；随后 [M3 阶段门禁](docs/reviews/2026-07-29-m3-phase-gate-pi-gpt-5.6-sol.md) 以 **PASS WITH NOTES** 允许进入 M4。该结论只通过 V4 的 M3 process 首跑段；DESIGN §10.1 双后端完整矩阵仍按 WBS 留 M6、真实 Agent/version 资格留 M7，不得描述为完整 V4/M6 通过。

## 上下文规则（摘要）

- 默认只加载 `status: active | draft` 的文档；`done / abandoned / superseded`、`reviews/`、`CHANGELOG.md` 仅回溯类任务加载。
- 不全量加载 `docs/`；按 docs/README.md 的「按任务类型的默认上下文集」表选读。
- 引用不复制：事实只写一次，其余地方链接。
