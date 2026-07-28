# AGENTS.md — Sift 代理导航

## 先读

- 文档地图、命名约定、上下文加载规则：[docs/README.md](docs/README.md)

## 项目现状

M1–M2 已有 Go 实现：控制面/配置/Brain 壳之上，已接双平台 Forge 适配、Intake 调度与回复消费、outbox comment worker、API 预算与 V3/V7/V11 首段证据；[`specs/forge.md`](docs/specs/forge.md) 与五份基础规格保持 `active`。产品需求：[docs/PRD.md](docs/PRD.md)；架构与工作基线见 [docs/DESIGN.md](docs/DESIGN.md) / [docs/WBS.md](docs/WBS.md)。

[S2/M2 第四次定向复审](docs/reviews/2026-07-29-s2-m2-rereview-4-pi-gpt-5.6-sol.md) 结论为 **PASS WITH NOTES**（doctor 时序 flake 与双 issue consumer 集成测试粒度属非阻断注记）。M2 门禁已通过；下一步进入 **M3：Runtime 与启动停滞安全闭环**。不得把 M2 Intake/Forge 描述为 M3 process backend / Interrupt 发射核心已实现。

## 上下文规则（摘要）

- 默认只加载 `status: active | draft` 的文档；`done / abandoned / superseded`、`reviews/`、`CHANGELOG.md` 仅回溯类任务加载。
- 不全量加载 `docs/`；按 docs/README.md 的「按任务类型的默认上下文集」表选读。
- 引用不复制：事实只写一次，其余地方链接。
