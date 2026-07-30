package storage

import (
	"context"
	"testing"
)

func TestRecordBlockerReportKeepsRunningRun(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report','report-hash',1,'{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":4,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":2,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}',1,?, 'test')`, testNow)
	mustExec(t, db, `UPDATE runs SET config_snapshot_id='cfg-report',status='running' WHERE id='run'`)
	insertTaskSpec(t, db, "task", "run", 1)
	insertAttempt(t, db, "run", 1, "task")
	insertAttemptClaim(t, db, "run", 1, "launch")
	mustExec(t, db, `UPDATE attempts SET phase='running',agent_pid=2,agent_started_at_ms=?,agent_executable='/agent' WHERE run_id='run' AND attempt_no=1`, testNow)
	mustExec(t, db, `UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='bootstrap',run_token_hash=? WHERE run_id='run' AND attempt_no=1`, handoffHash("report-token"))

	result, err := db.RecordReport(ctx, ReportSubmitCmd{
		Token: "report-token", RunID: "run", AttemptNo: 1, Generation: 1,
		ReportKey: "0123456789abcdef0123456789abcdef", Kind: "blocker",
		Payload: map[string]any{"blocker_summary": "blocked", "attempted_summary": "tried", "recommended_action": "ask"}, NowMS: testNow,
	})
	if err != nil || result.Disposition != "accepted" {
		t.Fatalf("RecordReport = %#v, %v", result, err)
	}
	var status, interruptID string
	if err := db.db.QueryRow(`SELECT r.status,rr.direct_interrupt_id FROM runs r JOIN report_receipts rr ON rr.run_id=r.id WHERE r.id='run'`).Scan(&status, &interruptID); err != nil {
		t.Fatal(err)
	}
	if status != "running" || interruptID == "" {
		t.Fatalf("status/interrupt = %q/%q", status, interruptID)
	}
	assertCount(t, db, "report_receipts", 1)
	assertCount(t, db, "interrupts", 1)

	submit := func(key string) error {
		_, err := db.RecordReport(ctx, ReportSubmitCmd{
			Token: "report-token", RunID: "run", AttemptNo: 1, Generation: 1,
			ReportKey: key, Kind: "blocker",
			Payload: map[string]any{"blocker_summary": "blocked", "attempted_summary": "tried", "recommended_action": "ask"}, NowMS: testNow,
		})
		return err
	}
	if err := submit("1123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("second blocker = %v", err)
	}
	for _, key := range []string{"2123456789abcdef0123456789abcdef", "3123456789abcdef0123456789abcdef"} {
		if err := submit(key); err == nil || err.Error() != "report: report_interrupt_quota_exhausted" {
			t.Fatalf("quota report %s error = %v", key, err)
		}
	}
	var available int
	if err := db.db.QueryRow(`SELECT available_units FROM rate_limit_buckets WHERE kind='report' AND scope_id='run:run:attempt:1'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 1 {
		t.Fatalf("rate token replay available = %d, want 1", available)
	}
	assertCount(t, db, "report_quota_exhaustions", 1)
}
