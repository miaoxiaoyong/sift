# M5 §5.7 metrics/CLI/timeline Sol re-review (#767 round 2)

- **Issue**: [#767](https://github.com/miaoxiaoyong/sift/issues/767)
- **Implement PR**: [#769](https://github.com/miaoxiaoyong/sift/pull/769) (merge `0b63a6c`)
- **Fix PR**: [#774](https://github.com/miaoxiaoyong/sift/pull/774) (merge `adbf11f`)
- **Agent**: pi-deepseek-v4-pro
- **Round**: 2（delta；round 1 = NEED-FIX）
- **Verdict**: **PASS**

## Context

Parallel Sol path [#770](https://github.com/miaoxiaoyong/sift/issues/770) recorded **PASS WITH NOTES** on #769 and synced WBS §5.7 via #772. A second Sol pass on the same tip found three closable defects (below). Round-1 closing package was posted on #767; #774 closed all P0/P1. This document is the authoritative delta PASS for the conductor that drove #767 → #769 → #774.

## Round-1 findings → closed in #774

| ID | Level | Summary | Closed by |
|---|---|---|---|
| P0-1 | P0 | `weightedAttention()` dual `WHERE` when `ProjectID` set | Single WHERE + parenthesized `deliveredExists`; project path `WHERE r.project_id=? AND (...)` |
| P1-1 | P1 | Zero trigger→started latency dropped (`started <= observed`) | Guard → `started < observed`; `TestTriggerStartedLatencyZeroAllowed` |
| P1-2 | P1 | No project-scoped `Metrics()` coverage | `TestMetricsProjectScoped` across nine series |

## UNCHANGED-PASS

- V11: `gate_bypassed` excluded from false-release denominator; counted in bypass rate
- Frozen snapshot reason weights; response interval never human-minutes
- `sift ps` / `logs` / `metrics` / `timeline` operator surfaces
- Nine §10.2 series with fail-closed coverage notes
- Honest gaps: false-release numerator=0 until revert/fix events; dispatch accuracy structural; real P50&lt;60s stays M7

## STILL-OPEN (P2, non-blocking)

| ID | Summary |
|---|---|
| P2-1 | `RunTimeline.HasMore` uses unscoped `COUNT(*)` |
| P2-2 | `llmCost()` ignores `MetricsQuery.ProjectID` |

## Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0（已关） | — |
| P1 | 0（已关） | — |
| P2 | 2 | 否（记录） |
| DEFER | 0 | — |

## Evidence

`go test ./internal/storage/ ./internal/controlplane/ ./cmd/sift/ -count=1` green after #774.

## Follow-up

WBS §5.7 checkboxes already synced via #772 against #770; amend wave note to cite this round-2 PASS and #774. Do **not** claim M5 complete. Critical fuse, Channel `ops.ps`/`ops.doctor` endpoint-level reopen, and M5 phase gate remain open. Do not start #748+ code-opt.
