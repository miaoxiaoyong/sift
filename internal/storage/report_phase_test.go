package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fullReportConfig is a realistic frozen config snapshot: production snapshots
// always carry every report/runtime knob because the effective config applies
// defaults at load time.
const fullReportConfig = `{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":4,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":2,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}`

// seedReportRun prepares a running attempt bound to a run token whose hash is
// sha256("report-token"). The phase is overridable to exercise not_ready and
// permanent rejects.
func seedReportRun(t *testing.T, phase string) (*DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, _ := openTestDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report','report-hash',1,?,1,?, 'test')`, fullReportConfig, testNow)
	mustExec(t, db, `UPDATE runs SET config_snapshot_id='cfg-report',status='running' WHERE id='run'`)
	insertTaskSpec(t, db, "task", "run", 1)
	insertAttempt(t, db, "run", 1, "task")
	insertAttemptClaim(t, db, "run", 1, "launch")
	mustExec(t, db, `UPDATE attempts SET phase=?,agent_pid=2,agent_started_at_ms=?,agent_executable='/agent',result_exit_code=CASE WHEN ?='finished' THEN 0 ELSE NULL END,result_signal=NULL,result_digest=CASE WHEN ?='finished' THEN 'abc' ELSE NULL END,result_observed_at_ms=CASE WHEN ?='finished' THEN ? ELSE NULL END WHERE run_id='run' AND attempt_no=1`, phase, testNow, phase, phase, phase, testNow)
	mustExec(t, db, `UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='bootstrap',run_token_hash=? WHERE run_id='run' AND attempt_no=1`, handoffHash("report-token"))
	return db, ctx
}

func reportCmd(kind string) ReportSubmitCmd {
	return ReportSubmitCmd{Token: "report-token", RunID: "run", AttemptNo: 1, Generation: 1, ReportKey: "0123456789abcdef0123456789abcdef", Kind: kind, Payload: payloadFor(kind), NowMS: testNow}
}

func TestReportSpawningReturnsNotReadyWithRetryPolicy(t *testing.T) {
	db, ctx := seedReportRun(t, "spawning")
	var nre *ReportNotReadyError
	_, err := db.RecordReport(ctx, reportCmd("progress"))
	if !errors.As(err, &nre) {
		t.Fatalf("spawning report error = %v, want ReportNotReadyError", err)
	}
	if nre.RetryPolicy.InitialDelayMS != 100 || nre.RetryPolicy.MaxDelayMS != 1000 || nre.RetryPolicy.TotalTimeoutMS != 10000 || nre.RetryPolicy.MultiplierMicros != 2000000 {
		t.Fatalf("retry policy = %#v", nre.RetryPolicy)
	}
	// not_ready must not consume a rate token or write a receipt/event.
	assertCount(t, db, "report_receipts", 0)
	assertCount(t, db, "events", 0)
	assertCount(t, db, "rate_limit_buckets", 0)
}

func TestReportRunningAcceptsProgressGoalCompleted(t *testing.T) {
	db, ctx := seedReportRun(t, "running")
	for i, kind := range []string{"progress", "goal", "completed"} {
		cmd := reportCmd(kind)
		cmd.ReportKey = keyFor(i)
		cmd.Payload = payloadFor(kind)
		result, err := db.RecordReport(ctx, cmd)
		if err != nil || result.Disposition != "accepted" {
			t.Fatalf("%s report = %#v, %v", kind, result, err)
		}
	}
	assertCount(t, db, "report_receipts", 3)
	assertCount(t, db, "events", 3)
	// Non-blocker kinds never write a charge or interrupt.
	assertCount(t, db, "budget_entries", 0)
	assertCount(t, db, "interrupts", 0)
	var status string
	if err := db.db.QueryRow(`SELECT status FROM runs WHERE id='run'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("completed changed runs.status to %q", status)
	}
}

func TestReportWrongGenerationIsStale(t *testing.T) {
	db, ctx := seedReportRun(t, "running")
	cmd := reportCmd("progress")
	cmd.Generation = 2
	_, err := db.RecordReport(ctx, cmd)
	if !errors.Is(err, ErrReportStale) {
		t.Fatalf("wrong generation error = %v, want ErrReportStale", err)
	}
}

func TestReportWrongTokenIsUnauthorized(t *testing.T) {
	db, ctx := seedReportRun(t, "running")
	cmd := reportCmd("progress")
	cmd.Token = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err := db.RecordReport(ctx, cmd)
	if !errors.Is(err, ErrReportUnauthorized) {
		t.Fatalf("wrong token error = %v, want ErrReportUnauthorized", err)
	}
}

func TestReportFinishedPhaseIsPermanentConflict(t *testing.T) {
	for _, phase := range []string{"pending", "starting", "finished", "orphaned"} {
		t.Run(phase, func(t *testing.T) {
			db, ctx := seedReportRun(t, phase)
			_, err := db.RecordReport(ctx, reportCmd("progress"))
			if !errors.Is(err, ErrReportConflict) {
				t.Fatalf("%s report error = %v, want ErrReportConflict", phase, err)
			}
			assertCount(t, db, "report_receipts", 0)
		})
	}
}

func TestReportKeyConflictIsPermanent(t *testing.T) {
	db, ctx := seedReportRun(t, "running")
	first := reportCmd("progress")
	first.Payload = map[string]any{"message": "first"}
	if _, err := db.RecordReport(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Payload = map[string]any{"message": "second"}
	_, err := db.RecordReport(ctx, second)
	if !errors.Is(err, ErrReportConflict) {
		t.Fatalf("same key diff digest error = %v, want ErrReportConflict", err)
	}
	assertCount(t, db, "report_receipts", 1)
}

func TestReportSameKeySameDigestIsDuplicate(t *testing.T) {
	db, ctx := seedReportRun(t, "running")
	cmd := reportCmd("progress")
	cmd.Payload = map[string]any{"message": "once"}
	first, err := db.RecordReport(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.RecordReport(ctx, cmd)
	if err != nil || second.Disposition != "duplicate" || second.ReceiptID != first.ReceiptID {
		t.Fatalf("duplicate = %#v, %v", second, err)
	}
	assertCount(t, db, "report_receipts", 1)
	// Duplicate must not consume a second rate token.
	var available int
	if err := db.db.QueryRow(`SELECT available_units FROM rate_limit_buckets WHERE kind='report' AND scope_id='run:run:attempt:1'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 3 {
		t.Fatalf("duplicate consumed a token: available = %d, want 3", available)
	}
}

func TestReportSemanticWindowDedupe(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	// dedupe_window = 30s so a new key with the same (kind, digest) inside the
	// window returns the original receipt as a duplicate.
	cfg := `{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":4,"dedupe_window":30000000000,"events_per_minute":60,"interrupts_per_run_daily_quota":2,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}`
	mustExec(t, db, `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report','report-hash',1,?,1,?, 'test')`, cfg, testNow)
	mustExec(t, db, `UPDATE runs SET config_snapshot_id='cfg-report',status='running' WHERE id='run'`)
	insertTaskSpec(t, db, "task", "run", 1)
	insertAttempt(t, db, "run", 1, "task")
	insertAttemptClaim(t, db, "run", 1, "launch")
	mustExec(t, db, `UPDATE attempts SET phase='running',agent_pid=2,agent_started_at_ms=?,agent_executable='/agent' WHERE run_id='run' AND attempt_no=1`, testNow)
	mustExec(t, db, `UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='bootstrap',run_token_hash=? WHERE run_id='run' AND attempt_no=1`, handoffHash("report-token"))
	first := reportCmd("progress")
	first.ReportKey = "0123456789abcdef0123456789abcdef"
	first.Payload = map[string]any{"message": "same"}
	r1, err := db.RecordReport(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	// New key, identical payload, within the window -> duplicate.
	second := first
	second.ReportKey = "1123456789abcdef0123456789abcdef"
	r2, err := db.RecordReport(ctx, second)
	if err != nil || r2.Disposition != "duplicate" || r2.ReceiptID != r1.ReceiptID {
		t.Fatalf("semantic duplicate = %#v, %v", r2, err)
	}
	assertCount(t, db, "report_receipts", 1)
	// Outside the window (received_at_ms < cutoff) -> accepted as a new report.
	third := first
	third.ReportKey = "2123456789abcdef0123456789abcdef"
	third.NowMS = testNow + 31000
	r3, err := db.RecordReport(ctx, third)
	if err != nil || r3.Disposition != "accepted" {
		t.Fatalf("post-window report = %#v, %v", r3, err)
	}
	assertCount(t, db, "report_receipts", 2)
}

func TestReportPayloadTooLargeRejected(t *testing.T) {
	db, ctx := seedReportRun(t, "running")
	cmd := reportCmd("progress")
	cmd.Payload = map[string]any{"message": strings.Repeat("x", 65536)}
	_, err := db.RecordReport(ctx, cmd)
	if !errors.Is(err, ErrReportPayloadTooLarge) {
		t.Fatalf("oversized payload error = %v, want ErrReportPayloadTooLarge", err)
	}
	assertCount(t, db, "report_receipts", 0)
}

func TestReportPayloadRejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{"a\tb", "a\u0001b", "a\x7fb"} {
		db, ctx := seedReportRun(t, "running")
		cmd := reportCmd("progress")
		cmd.ReportKey = "0123456789abcdef0123456789abcdef"
		cmd.Payload = map[string]any{"message": bad}
		_, err := db.RecordReport(ctx, cmd)
		if !errors.Is(err, ErrReportInvalid) {
			t.Fatalf("control char %q error = %v, want ErrReportInvalid", bad, err)
		}
	}
}

func keyFor(i int) string {
	return string(rune('0'+i)) + "123456789abcdef0123456789abcdef"
}

func payloadFor(kind string) map[string]any {
	switch kind {
	case "progress":
		return map[string]any{"message": "working"}
	case "goal":
		return map[string]any{"goal": "ship it"}
	case "completed":
		return map[string]any{"summary": "done"}
	default:
		return map[string]any{}
	}
}
