# M5 #798 LLM severity-only downgrade + visual rejects voice rereview

**Verdict: PASS** (Sol / deepseek-v4-pro, review_round 1)  
**PR:** #799 (`22d94a9`, merge `926e5b5`) · **Issue:** #798 · **Base:** `origin/main` @ `926e5b5`

## Scope

Close WBS §5.2: LLM 只能建议 severity 降级；`min_modality: visual` renderer 拒绝语音路径.

## Findings

| ID | Severity | Status |
|----|----------|--------|
| `recomputeCertification` Window<=0 静默跳过（附带变更） | P2 | Record only |
| escalate→`Severity(next, downgraded)` 无独立新测（#711 已覆盖） | P2 | Record only |
| `Severity()` 非累加由调用方纪律保证 | P2 | Record only — 注释+三路径测试已证实 |

P0/P1: **0**.

## Evidence

- Fix: `Severity(base, suggestedDowngrade)` 唯一 severity 写口；T6 仅 `SuggestedDowngrade bool`；visual 通道在 T6 前按 capabilities 过滤，零兼容 → held + `no_compatible_channel`.
- Tests: `severity_downgrade_visual_test.go`（含 `TestSeverityIsAtMostOneDowngradeAndNeverUpgrades`、`TestEmitInterruptVisualModalityHoldsRatherThanRoutingToVoice`、并发/重放收敛）`-count=1` PASS.
- Upgrade path: `AdvanceInterrupt` 复用冻结 `suggested_downgrade` + `Severity(next, downgraded)`；#711 升级复用测仍绿.

## Notes

Do **not** claim M5 complete. §5.2 once-charge lifecycle、§5.1 T7 生产调用器、§5.4 residuals、#748+ 仍开。P2 记入 backlog。
