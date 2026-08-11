# testdata golden files

## ops_timeline_envelope_v1.json

`TestOpsTimelineJSONBackwardCompatible` (#921 B5) pins the `ops.timeline`
`--json`/`SIFT_JSON=1` contract evolution: the paging contract intentionally
grew from a `seq` cursor to an `(occurred_at_ms, seq)` keyset, which adds
exactly one additive result field — `next_occurred_at_ms`. Every pre-existing
field must stay byte-for-byte identical with the origin/main envelope for the
same fixed fixture.

### Provenance

Captured from **origin/main** (the last commit before the B2 pagination change
in the #921 branch), NOT from this branch. In origin/main, `sift timeline`
printed the raw RPC envelope by default (the ux-3 humanization landed later),
and the capture runs that default against a fixed fixture whose insertion order
equals its occurrence order (newest-first, like a backfill), so the legacy
seq-ASC and the B2 occurred-DESC full streams coincide and the whole legacy
envelope is byte-comparable.

Capture method (throwaway origin/main worktree, one-shot test never merged):

1. `git worktree add /tmp/sift-921-origin-main origin/main`
2. In `cmd/sift/`, add a temporary test that:
   - seeds `cfg-cli`/`proj-cli`, forge run `runCLI`, and 5 `report.progress`
     events with `OccurredAtMS`/`RecordedAtMS` = `1700000000000 + {5000, 4000,
     3000, 2000, 1000}` (insertion order = occurrence order),
   - runs `sift timeline --run runCLI --limit 100` (the raw envelope),
   - canonicalizes via `schema.Decode` (Closed) → set `request_id` to
     `00112233445566778899aabbccddeeff` → set each event `ID` to
     `golden-event-<seq>` → `json.MarshalIndent(resp, "", "  ")` + `\n`.
3. Copy the output here.

The two normalized leaves (`request_id`, per-event `id`) are inherently random
per call even between two origin/main runs; everything else is compared
byte-for-byte.

### Regeneration

Only regenerate from origin/main (or any pre-B2 commit) when the pre-B2
envelope is expected to change, then re-run
`go test ./cmd/sift/ -run TestOpsTimelineJSONBackwardCompatible`. Never
regenerate from this branch — the current implementation intentionally adds
`next_occurred_at_ms`, and the golden must keep pinning the legacy field set.
