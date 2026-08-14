---
status: done
created: 2026-07-30
summary: M5 收口第三波依赖与门禁顺序
---

# M5 收口 Wave 3

跟踪 Issue：[#830](https://github.com/xsift/sift/issues/830)。判定基线为 [WBS M5](../WBS.md#M5AttentionCommandReportBrain-与指标)；字段与事务契约继续以对应 active specs 为准。

## 目标与诚实边界

本波只关闭 WBS 已明确留下的 T7 生产接线、`rerun_checks` request-start worker、Attention once-charge 全生命周期证据，并在这些前置合入后执行 M5 综合门禁。组件完成不等于 M5 通过；只有最终门禁 Issue 可依据完整证据更新结论。

连续两个无关 PR 的 CI 首轮均命中 `TestProductionWrapperCrashWindows/started` 时序 flake，因此先修测试证据边界，避免用重跑构造虚假门禁稳定性。

## 串行切片

| 顺序 | Issue | 交付 | 后置条件 |
|---|---|---|---|
| 1 | [#831](https://github.com/xsift/sift/issues/831) | wrapper started 崩溃窗确定性证据 | 20 次连续定向通过，非 timeout/retry 掩盖 |
| 2 | [#832](https://github.com/xsift/sift/issues/832) | Forge `rerun_checks` worker 与 §8.5 request-start | 双平台、三崩溃窗、生产 OutboxTick |
| 3 | [#833](https://github.com/xsift/sift/issues/833) | Interrupt/Channel/Command/restart once-charge 证据 | Attention 与 Forge API 收费独立 |
| 4 | [#834](https://github.com/xsift/sift/issues/834) | 确定性聚合 → T7 → inert draft | fallback 无 draft，A7 边界不扩张 |
| 5 | [#835](https://github.com/xsift/sift/issues/835) | M5 综合门禁与 full-fake V9 | 独立评审后才更新 M5 结论 |

## 每片统一门禁

1. 定向测试及适用的 `-race`/重复运行。
2. `go test ./...`、`go vet ./...`、`git diff --check`。
3. 自审调用图、唯一写口、崩溃/重放语义。
4. CI 全绿后合并；已知 flake 必须记录并修复，不以 blanket retry 作为证据。
5. 一个 Issue 合并后再开始下一个，避免 storage/daemon/Forge 并行冲突。
