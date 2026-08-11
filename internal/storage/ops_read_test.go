package storage

import (
	"context"
	"encoding/json"
	"testing"
)

// TestRunPSProjectsRunAttempt verifies ops.ps returns Run/attempt rows, open
// Interrupt / pending outbox counts and today's remaining attention quota.
func TestRunPSProjectsRunAttempt(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-ps", "proj-ps", now); err != nil {
		t.Fatal(err)
	}
	// A running run with an attempt + one open Interrupt.
	if err := db.SeedLaunchRunForTest(ctx, "runPS", "proj-ps", "cfg-ps", now, "/work"); err != nil {
		t.Fatal(err)
	}
	seedInterruptWithDelivery(t, db, ctx, "intPS", "runPS", "code_review", "normal", now)
	// The helper seeds a closed/delivered interrupt; for the ps open-count we
	// need an open one, so seed a second open interrupt directly.
	chargeID := "chg-open"
	if _, err := db.ExecForTest(ctx, `INSERT INTO budget_entries(id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES(?,'attention','run','runPS',?,1,'code_review','runPS',?,?)`, chargeID, now, "op-open", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO interrupts(id,run_id,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,nonce,nonce_issued_at_ms,version,status,dispatch_state,expires_at_ms,on_expire,escalation_count,max_escalations,charged_budget_entry_id,created_at_ms,updated_at_ms) VALUES('intOpen','runPS','gen-open','code_review','normal','h','b','[]','text','[]','n',?,1,'open','ready',?,? ,0,0,?, ?,?)`, now, now+1000, "hold", chargeID, now, now); err != nil {
		t.Fatal(err)
	}
	// A persisted attention 'normal' bucket: limit 5, consumed 2 → remaining 3.
	if _, err := db.ExecForTest(ctx, `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('attention','severity','normal',?,?,?,?,1,?)`, now, now+86400000, 5, 2, now); err != nil {
		t.Fatal(err)
	}

	report, err := db.RunPS(ctx, PSQuery{ConfiguredQuota: map[string]int{"low": 5, "normal": 5, "high": 5}})
	if err != nil {
		t.Fatalf("RunPS: %v", err)
	}
	if len(report.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(report.Runs))
	}
	r := report.Runs[0]
	if r.RunID != "runPS" || r.Status != "queued" {
		t.Fatalf("run = %+v, want runPS/queued", r)
	}
	if r.Attempt == nil || r.Attempt.AttemptNo != 1 || r.Attempt.Phase != "pending" || r.Attempt.AgentID != "agent" {
		t.Fatalf("attempt = %+v, want pending attempt 1 for agent", r.Attempt)
	}
	if r.OpenInterruptCount != 1 {
		t.Fatalf("open interrupt count = %d, want 1", r.OpenInterruptCount)
	}
	// 'normal' remaining = configured limit 5 − consumed 2 = 3.
	if report.AttentionRemaining["normal"] != 3 {
		t.Fatalf("normal remaining = %d, want 3", report.AttentionRemaining["normal"])
	}
	// 'high' has no persisted bucket → configured ceiling 5 fully remaining.
	if report.AttentionRemaining["high"] != 5 {
		t.Fatalf("high remaining = %d, want 5 (configured ceiling)", report.AttentionRemaining["high"])
	}
}

// TestRunPSExactRunFilter verifies a run_id filter returns only that run.
func TestRunPSExactRunFilter(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-ps2", "proj-ps2", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "runA", "proj-ps2", "cfg-ps2", "iA", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "runB", "proj-ps2", "cfg-ps2", "iB", now); err != nil {
		t.Fatal(err)
	}
	report, err := db.RunPS(ctx, PSQuery{RunID: "runB"})
	if err != nil {
		t.Fatalf("RunPS: %v", err)
	}
	if len(report.Runs) != 1 || report.Runs[0].RunID != "runB" {
		t.Fatalf("runs = %+v, want only runB", report.Runs)
	}
}

// TestRunTimelineIsBoundedAndKeyset verifies the event timeline is bounded,
// globally ordered by occurred_at_ms descending (seq tie-breaker), keyset
// paginated on (occurred_at_ms, seq), and filterable by type — even when seq
// and occurred_at_ms are interleaved (replay/backfill).
func TestRunTimelineIsBoundedAndKeyset(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-tl", "proj-tl", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "runTL", "proj-tl", "cfg-tl", "iTL", now); err != nil {
		t.Fatal(err)
	}
	// Append five events whose seq order (insertion) interleaves with
	// occurred_at_ms: global newest-first is +4, +3, +1, +2, +0.
	occurred := []int64{now + 1, now + 3, now + 2, now + 5, now + 4}
	for i, at := range occurred {
		typ := "run.transitioned"
		if i%2 == 0 {
			typ = "report.progress"
		}
		if _, err := db.AppendEvent(ctx, EventCmd{RunID: "runTL", Type: typ, Source: SourceAgent, PayloadJSON: []byte("{}"), OccurredAtMS: at, RecordedAtMS: at}); err != nil {
			t.Fatal(err)
		}
	}

	// Limit 2 → the two globally newest events, descending.
	page1, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL", Limit: 2})
	if err != nil {
		t.Fatalf("timeline page1: %v", err)
	}
	if len(page1.Events) != 2 {
		t.Fatalf("page1 = %d events, want 2", len(page1.Events))
	}
	if page1.Events[0].OccurredAtMS != now+5 || page1.Events[1].OccurredAtMS != now+4 {
		t.Fatalf("page1 not globally newest-first: %+v", page1.Events)
	}
	if !page1.HasMore {
		t.Fatal("page1 should report more events")
	}
	// Keyset from (page1.NextOccurredAtMS, page1.NextSeq) → next page.
	page2, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL", AfterOccurredAtMS: page1.NextOccurredAtMS, AfterSeq: page1.NextSeq, Limit: 10})
	if err != nil {
		t.Fatalf("timeline page2: %v", err)
	}
	if len(page2.Events) != 3 {
		t.Fatalf("page2 = %d events, want 3", len(page2.Events))
	}
	// Pages concatenate to the global occurred_at_ms descending order with no
	// duplicates or omissions.
	all := append(append([]Event{}, page1.Events...), page2.Events...)
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if prev.OccurredAtMS < cur.OccurredAtMS || (prev.OccurredAtMS == cur.OccurredAtMS && prev.Seq < cur.Seq) {
			t.Fatalf("concatenated pages not globally descending at %d: %+v", i, all)
		}
		if prev.Seq == cur.Seq {
			t.Fatalf("duplicate event across pages: %+v", all)
		}
	}
	if page2.HasMore {
		t.Fatal("page2 should report no more events")
	}

	// Type filter returns only matching events, still globally descending.
	typed, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL", Type: "report.progress"})
	if err != nil {
		t.Fatalf("timeline typed: %v", err)
	}
	if len(typed.Events) != 3 {
		t.Fatalf("typed events = %d, want 3", len(typed.Events))
	}
	for i, e := range typed.Events {
		if e.Type != "report.progress" {
			t.Fatalf("non-matching type %s in filtered timeline", e.Type)
		}
		if i > 0 && typed.Events[i-1].OccurredAtMS < e.OccurredAtMS {
			t.Fatalf("typed timeline not descending: %+v", typed.Events)
		}
	}
}

// TestRunTimelineLegacyAfterSeqPagination verifies the backward-compatible
// legacy cursor: an old ops.timeline caller sends only after_seq (no
// after_occurred_at_ms), so the seq must be resolved to its occurred_at_ms
// before the (occurred_at_ms, seq) keyset is applied. Each page returns the
// subsequent events; concatenated pages cover all events exactly once and stay
// globally occurred_at_ms descending — identical to the dual-cursor path.
func TestRunTimelineLegacyAfterSeqPagination(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	const now = testNow
	if err := db.SeedProjectForTest(ctx, "cfg-tl2", "proj-tl2", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "runTL2", "proj-tl2", "cfg-tl2", "iTL2", now); err != nil {
		t.Fatal(err)
	}
	// Interleaved seq/occurred_at_ms; global newest-first is +4, +3, +1, +2, +0.
	occurred := []int64{now + 1, now + 3, now + 2, now + 5, now + 4}
	for _, at := range occurred {
		if _, err := db.AppendEvent(ctx, EventCmd{RunID: "runTL2", Type: "run.transitioned", Source: SourceAgent, PayloadJSON: []byte("{}"), OccurredAtMS: at, RecordedAtMS: at}); err != nil {
			t.Fatal(err)
		}
	}

	// Page 1: no cursor.
	page1, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL2", Limit: 2})
	if err != nil {
		t.Fatalf("legacy page1: %v", err)
	}
	if len(page1.Events) != 2 || !page1.HasMore {
		t.Fatalf("legacy page1 = %+v, want 2 events with more", page1)
	}

	// Page 2: legacy cursor — only AfterSeq, AfterOccurredAtMS left at 0.
	page2, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL2", AfterSeq: page1.NextSeq, Limit: 2})
	if err != nil {
		t.Fatalf("legacy page2: %v", err)
	}
	if len(page2.Events) != 2 || !page2.HasMore {
		t.Fatalf("legacy page2 = %+v, want 2 events with more", page2)
	}
	if page2.Events[0].Seq == page1.Events[len(page1.Events)-1].Seq {
		t.Fatalf("legacy page2 repeats page1 tail event: %+v", page2.Events)
	}

	// Page 3: legacy cursor again, limit 10 to drain the stream.
	page3, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL2", AfterSeq: page2.NextSeq, Limit: 10})
	if err != nil {
		t.Fatalf("legacy page3: %v", err)
	}
	if len(page3.Events) != 1 || page3.HasMore {
		t.Fatalf("legacy page3 = %+v, want the final event", page3)
	}

	// Concatenation covers all five events exactly once, globally descending.
	all := append(append(append([]Event{}, page1.Events...), page2.Events...), page3.Events...)
	if len(all) != 5 {
		t.Fatalf("legacy pages cover %d events, want 5: %+v", len(all), all)
	}
	seen := map[int64]bool{}
	for i, e := range all {
		if seen[e.Seq] {
			t.Fatalf("legacy pages duplicate seq %d: %+v", e.Seq, all)
		}
		seen[e.Seq] = true
		if i > 0 && all[i-1].OccurredAtMS < e.OccurredAtMS {
			t.Fatalf("legacy pages not globally descending at %d: %+v", i, all)
		}
	}

	// The legacy path must agree with the dual-cursor path page-for-page.
	dual2, err := db.RunTimeline(ctx, TimelineQuery{RunID: "runTL2", AfterOccurredAtMS: page1.NextOccurredAtMS, AfterSeq: page1.NextSeq, Limit: 2})
	if err != nil {
		t.Fatalf("dual page2: %v", err)
	}
	if len(dual2.Events) != len(page2.Events) {
		t.Fatalf("legacy/dual page2 disagree: legacy %d vs dual %d events", len(page2.Events), len(dual2.Events))
	}
	for i := range dual2.Events {
		if dual2.Events[i].Seq != page2.Events[i].Seq {
			t.Fatalf("legacy/dual page2 disagree at %d: legacy %+v vs dual %+v", i, page2.Events, dual2.Events)
		}
	}
}

// TestRunPSIsJSONSerializable keeps the result shape wire-safe.
func TestRunPSIsJSONSerializable(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	report, err := db.RunPS(ctx, PSQuery{})
	if err != nil {
		t.Fatalf("RunPS empty: %v", err)
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal ps report: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty serialization")
	}
}
