package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func seedReportQuotaRun(t *testing.T, quota int) (*DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, _ := openTestDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report','report-hash',1,'{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":8,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":`+fmt.Sprint(quota)+`,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}',1,?, 'test')`, testNow)
	mustExec(t, db, `UPDATE runs SET config_snapshot_id='cfg-report',status='running' WHERE id='run'`)
	insertTaskSpec(t, db, "task", "run", 1)
	insertAttempt(t, db, "run", 1, "task")
	insertAttemptClaim(t, db, "run", 1, "launch")
	mustExec(t, db, `UPDATE attempts SET phase='running',agent_pid=2,agent_started_at_ms=?,agent_executable='/agent' WHERE run_id='run' AND attempt_no=1`, testNow)
	mustExec(t, db, `UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='bootstrap',run_token_hash=? WHERE run_id='run' AND attempt_no=1`, handoffHash("report-token"))
	return db, ctx
}

func submitBlocker(ctx context.Context, db *DB, key string) error {
	_, err := db.RecordReport(ctx, ReportSubmitCmd{Token: "report-token", RunID: "run", AttemptNo: 1, Generation: 1, ReportKey: key, Kind: "blocker", Payload: map[string]any{"blocker_summary": "blocked", "attempted_summary": "tried", "recommended_action": "ask"}, NowMS: testNow})
	return err
}

func TestReportQuotaT4AcceptanceAndPersistedBytes(t *testing.T) {
	db, ctx := seedReportQuotaRun(t, 1)
	var got InterruptT4Input
	db.SetInterruptT4(func(_ context.Context, in InterruptT4Input) (InterruptT4Output, error) {
		got = in
		return InterruptT4Output{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"reject", "hold"}, RecommendedOptionID: "hold"}, nil
	})
	if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "report_interrupt_quota_exhausted") {
		t.Fatalf("quota report = %v", err)
	}
	if got.AttemptNo != nil || got.Reason != InterruptFailureReview || got.Headline != "报告打扰额度已耗尽" || got.Brief != "事实：failure_class=report\\_interrupt\\_quota\\_exhausted；failure_evidence_ref=sift://event/"+eventIDForQuota(t, db)+"；recommended_action=hold。建议：hold" {
		t.Fatalf("T4 input = %#v", got)
	}
	if fmt.Sprint(got.Options) != "[{reject 停止 Run Run 停止 需人工重新发起} {hold 暂缓决定 保持 Interrupt 人工 held Run 继续运行}]" || fmt.Sprint(got.Fragments) != "[请人工处理 额度已耗尽]" {
		t.Fatalf("T4 quota variant = %#v", got)
	}
	var headline, brief, options, links string
	if err := db.db.QueryRow(`SELECT headline,brief_markdown,options_json,links_json FROM interrupts WHERE reason='failure_review'`).Scan(&headline, &brief, &options, &links); err != nil {
		t.Fatal(err)
	}
	if headline != "报告打扰额度已耗尽" || brief != "结论：额度已耗尽；要点：请人工处理；建议：暂缓决定（hold）" || options != `[{"effect":"Run 停止","id":"reject","label":"停止 Run","risk":"需人工重新发起"},{"effect":"保持 Interrupt 人工 held","id":"hold","label":"暂缓决定","risk":"Run 继续运行"}]` || links != `[{"label":"failure_evidence_ref","target":"sift://event/`+eventIDForQuota(t, db)+`"}]` {
		t.Fatalf("persisted quota T4 bytes = %q / %q / %q / %q", headline, brief, options, links)
	}
}

func TestReportQuotaT4InvalidOutputsFallBack(t *testing.T) {
	for _, out := range []InterruptT4Output{
		{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"hold", "reject"}, RecommendedOptionID: "hold"},
		{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"reject", "hold", "retry"}, RecommendedOptionID: "hold"},
		{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"reject", "hold"}, RecommendedOptionID: "retry"},
	} {
		db, ctx := seedReportQuotaRun(t, 1)
		db.SetInterruptT4(func(context.Context, InterruptT4Input) (InterruptT4Output, error) { return out, nil })
		if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
		if err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef"); err == nil {
			t.Fatal("quota report unexpectedly succeeded")
		}
		var headline, brief, options string
		if err := db.db.QueryRow(`SELECT headline,brief_markdown,options_json FROM interrupts WHERE reason='failure_review'`).Scan(&headline, &brief, &options); err != nil {
			t.Fatal(err)
		}
		if headline != "报告打扰额度已耗尽" || brief != "事实：failure_class=report\\_interrupt\\_quota\\_exhausted；failure_evidence_ref=sift://event/"+eventIDForQuota(t, db)+"；recommended_action=hold。建议：hold" || options != `[{"effect":"Run 停止","id":"reject","label":"停止 Run","risk":"需人工重新发起"},{"effect":"保持 Interrupt 人工 held","id":"hold","label":"暂缓决定","risk":"Run 继续运行"}]` {
			t.Fatalf("fallback bytes = %q / %q / %q", headline, brief, options)
		}
	}
}

func TestReportQuotaExhaustionCrashReplayAndConcurrency(t *testing.T) {
	t.Run("exhaustion transaction rollback consumes no rate token", func(t *testing.T) {
		db, ctx := seedReportQuotaRun(t, 1)
		if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `CREATE TRIGGER fail_quota_fact BEFORE INSERT ON report_quota_exhaustions BEGIN SELECT RAISE(ABORT, 'injected exhaustion crash'); END`)
		if err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "injected exhaustion crash") {
			t.Fatalf("quota crash = %v", err)
		}
		assertCount(t, db, "report_quota_exhaustions", 0)
		var available int
		if err := db.db.QueryRow(`SELECT available_units FROM rate_limit_buckets WHERE kind='report' AND scope_id='run:run:attempt:1'`).Scan(&available); err != nil || available != 7 {
			t.Fatalf("rate token after rolled-back exhaustion = %d, %v", available, err)
		}
	})
	t.Run("security-event cut rolls back the exhaustion transaction", func(t *testing.T) {
		db, ctx := seedReportQuotaRun(t, 1)
		if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `CREATE TRIGGER fail_quota_security_event BEFORE INSERT ON events WHEN NEW.type='security.report_quota_exhausted' BEGIN SELECT RAISE(ABORT, 'injected security event crash'); END`)
		if err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "injected security event crash") {
			t.Fatalf("security event cut = %v", err)
		}
		assertCount(t, db, "report_quota_exhaustions", 0)
		var available int
		if err := db.db.QueryRow(`SELECT available_units FROM rate_limit_buckets WHERE kind='report' AND scope_id='run:run:attempt:1'`).Scan(&available); err != nil || available != 7 {
			t.Fatalf("rate token after rolled-back security event = %d, %v", available, err)
		}
	})
	t.Run("emission rollback retains only committed exhaustion", func(t *testing.T) {
		db, ctx := seedReportQuotaRun(t, 1)
		if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `CREATE TRIGGER fail_quota_binding BEFORE INSERT ON interrupt_command_effect_bindings WHEN NEW.reason='failure_review' BEGIN SELECT RAISE(ABORT, 'injected quota emission crash'); END`)
		if err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef"); err == nil {
			t.Fatal("quota report unexpectedly succeeded")
		}
		assertCount(t, db, "report_quota_exhaustions", 1)
		assertCount(t, db, "interrupts", 1)
		assertCount(t, db, "report_emission_diagnostics", 1)
		var diagnosticPayload string
		if err := db.db.QueryRow(`SELECT e.payload_json FROM report_emission_diagnostics d JOIN events e ON e.id=d.event_id`).Scan(&diagnosticPayload); err != nil || !strings.Contains(diagnosticPayload, `"disposition":"structural_rejected"`) {
			t.Fatalf("structural rejection diagnostic = %q, %v", diagnosticPayload, err)
		}
		mustExec(t, db, `DROP TRIGGER fail_quota_binding`)
		if err := submitBlocker(ctx, db, "2123456789abcdef0123456789abcdef"); err == nil {
			t.Fatal("quota replay unexpectedly succeeded")
		}
		assertCount(t, db, "report_quota_exhaustions", 1)
		assertCount(t, db, "interrupts", 2)
	})
	t.Run("each post-exhaustion emission cut rolls back only the emission", func(t *testing.T) {
		for _, table := range []string{"interrupts", "attention_admissions", "budget_entries", "events", "outbox_operations", "interrupt_deliveries", "interrupt_command_effect_bindings"} {
			t.Run(table, func(t *testing.T) {
				db, ctx := seedReportQuotaRun(t, 1)
				if err := submitBlocker(ctx, db, "0123456789abcdef0123456789abcdef"); err != nil {
					t.Fatal(err)
				}
				trigger := "fail_quota_" + strings.ReplaceAll(table, "_", "")
				when := ""
				if table == "events" {
					when = " WHEN NEW.type='interrupt.emitted'"
				}
				mustExec(t, db, "CREATE TRIGGER "+trigger+" BEFORE INSERT ON "+table+when+" BEGIN SELECT RAISE(ABORT, 'injected quota emission cut'); END")
				err := submitBlocker(ctx, db, "1123456789abcdef0123456789abcdef")
				if err == nil || (table == "interrupt_command_effect_bindings" && !strings.Contains(err.Error(), "report_interrupt_quota_exhausted")) || (table != "interrupt_command_effect_bindings" && !strings.Contains(err.Error(), "injected quota emission cut")) {
					t.Fatalf("%s cut = %v", table, err)
				}
				if table == "interrupt_command_effect_bindings" {
					assertCount(t, db, "report_emission_diagnostics", 1)
				}
				assertCount(t, db, "report_quota_exhaustions", 1)
				assertCount(t, db, "interrupts", 1)
				assertCount(t, db, "attention_admissions", 1)
				assertCount(t, db, "budget_entries", 2)
				assertCount(t, db, "outbox_operations", 1)
				assertCount(t, db, "interrupt_deliveries", 1)
				assertCount(t, db, "interrupt_command_effect_bindings", 1)
				var domainEvents int
				if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='interrupt.emitted'`).Scan(&domainEvents); err != nil || domainEvents != 1 {
					t.Fatalf("domain events after %s cut = %d, %v", table, domainEvents, err)
				}
				mustExec(t, db, "DROP TRIGGER "+trigger)
				if err := submitBlocker(ctx, db, "2123456789abcdef0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "report_interrupt_quota_exhausted") {
					t.Fatalf("%s replay = %v", table, err)
				}
				assertCount(t, db, "report_quota_exhaustions", 1)
				assertCount(t, db, "interrupts", 2)
				assertCount(t, db, "attention_admissions", 2)
				assertCount(t, db, "budget_entries", 3)
				assertCount(t, db, "outbox_operations", 2)
				assertCount(t, db, "interrupt_deliveries", 2)
				assertCount(t, db, "interrupt_command_effect_bindings", 2)
			})
		}
	})

	t.Run("four writers create one exhaustion and one generation interrupt", func(t *testing.T) {
		db, ctx := seedReportQuotaRun(t, 1)
		keys := []string{"0123456789abcdef0123456789abcdef", "1123456789abcdef0123456789abcdef", "2123456789abcdef0123456789abcdef", "3123456789abcdef0123456789abcdef"}
		var wg sync.WaitGroup
		errs := make(chan error, len(keys))
		for _, key := range keys {
			wg.Add(1)
			go func(key string) { defer wg.Done(); errs <- submitBlocker(ctx, db, key) }(key)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !strings.Contains(err.Error(), "report_interrupt_quota_exhausted") {
				t.Fatalf("writer error = %v", err)
			}
		}
		assertCount(t, db, "report_quota_exhaustions", 1)
		assertCount(t, db, "interrupts", 2)
		assertCount(t, db, "attention_admissions", 2)
		assertCount(t, db, "interrupt_command_effect_bindings", 2)
		assertCount(t, db, "outbox_operations", 2)
		assertCount(t, db, "interrupt_deliveries", 2)
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM interrupts WHERE reason='failure_review' AND generation_key IS NOT NULL`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("quota interrupts = %d, %v", n, err)
		}
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='security.report_quota_exhausted'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("security events = %d, %v", n, err)
		}
		if err := db.db.QueryRow(`SELECT available_units FROM rate_limit_buckets WHERE kind='report' AND scope_id='run:run:attempt:1'`).Scan(&n); err != nil || n != 6 {
			t.Fatalf("rate tokens = %d, %v", n, err)
		}
		var eventID, binding, bindingDigest, interruptID, admissionInterruptID, eventPayload, generationKey, failureDigest string
		var bucketStart, bucketEnd int64
		if err := db.db.QueryRow(`SELECT q.security_event_id,q.daily_bucket_start_ms,q.daily_bucket_end_ms,b.binding_json,b.binding_digest,i.id,a.interrupt_id,q.generation_key,q.failure_digest,e.payload_json
			FROM report_quota_exhaustions q
			JOIN interrupts i ON i.generation_key=q.generation_key
			JOIN interrupt_command_effect_bindings b ON b.interrupt_id=i.id
			JOIN attention_admissions a ON a.interrupt_id=i.id
			JOIN events e ON e.id=q.security_event_id
			WHERE q.run_id='run'`).Scan(&eventID, &bucketStart, &bucketEnd, &binding, &bindingDigest, &interruptID, &admissionInterruptID, &generationKey, &failureDigest, &eventPayload); err != nil {
			t.Fatal(err)
		}
		wantBinding := `{"arm":"report_quota_failure_review","daily_bucket_end_ms":` + fmt.Sprint(bucketEnd) + `,"daily_bucket_start_ms":` + fmt.Sprint(bucketStart) + `,"run_id":"run","security_event_id":"` + eventID + `"}`
		if binding != wantBinding || admissionInterruptID != interruptID || generationKey == "" || failureDigest == "" {
			t.Fatalf("quota object identity = binding %q, interrupt/admission %q/%q, generation/digest %q/%q", binding, interruptID, admissionInterruptID, generationKey, failureDigest)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(eventPayload), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["failure_class"] != "report_interrupt_quota_exhausted" || payload["generation_key"] != generationKey || payload["failure_digest"] != failureDigest {
			t.Fatalf("security event payload = %s", eventPayload)
		}
		var digestOK int
		if err := db.db.QueryRow(`SELECT lower(hex(sift_sha256(?)))=?`, binding, bindingDigest).Scan(&digestOK); err != nil || digestOK != 1 {
			t.Fatalf("quota binding digest = %d, %v", digestOK, err)
		}
	})
}

func eventIDForQuota(t *testing.T, db *DB) string {
	t.Helper()
	var id string
	if err := db.db.QueryRow(`SELECT security_event_id FROM report_quota_exhaustions WHERE run_id='run'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
