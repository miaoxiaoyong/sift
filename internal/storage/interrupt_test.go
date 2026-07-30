package storage

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func interruptQuota() map[InterruptSeverity]int {
	return map[InterruptSeverity]int{SeverityLow: 3, SeverityNormal: 5, SeverityHigh: 5}
}

func emitTestInterrupt(t *testing.T, ctx context.Context, db *DB, cmd EmitInterruptCmd) (Interrupt, error) {
	t.Helper()
	if cmd.Reason != InterruptCodeReview && cmd.Reason != InterruptMergeConflict {
		return db.EmitInterrupt(ctx, cmd)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE runs SET change_id=?, change_head_sha=? WHERE id=?`, cmd.Generation.ChangeID, cmd.Generation.HeadSHA, cmd.RunID); err != nil {
		return Interrupt{}, err
	}
	if cmd.Reason == InterruptMergeConflict {
		return db.EmitInterrupt(ctx, cmd)
	}
	r := gateRecord(cmd.NowMS)
	r.RunID, r.HeadSHA = cmd.RunID, cmd.Generation.HeadSHA
	r.GateInputHash = strings.Repeat("f", 63) + "0"
	r.EffectivePolicyHash = strings.Repeat("e", 64)
	_, in, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, r, cmd)
	return in, err
}

func TestInterruptGenerationVectors(t *testing.T) {
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	shaC := strings.Repeat("c", 64)
	oid := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		reason InterruptReason
		g      InterruptGeneration
		want   string
	}{
		{InterruptDesignApproval, InterruptGeneration{TaskSpecSnapshotID: "task-01"}, "2eff88491a846f04025bc5a7019be780e96b00172adfa1b35154e71a77a27a83"},
		{InterruptGuardrailViolation, InterruptGeneration{PolicySnapshotID: "policy-01", ViolationCode: "rule-01", SubjectDigest: shaA}, "da9fc5161aa8f8a58f30b8c4e55833f4c4d23888112f19154b0de7c95968572e"},
		{InterruptCodeReview, InterruptGeneration{ChangeID: "change-01", HeadSHA: oid}, "7389e85b479a5c919062677e5a9a9e9f3465db0473b2d41171479be736a83e59"},
		{InterruptAgentBlocked, InterruptGeneration{AttemptNo: 1, Generation: 2, ReportID: "report-01"}, "ebc17dc66d66fb86c9d48d7e79c86a632e44f0fd0248b5c5713b6a9e95825643"},
		{InterruptMergeConflict, InterruptGeneration{ChangeID: "change-01", HeadSHA: shaA, ConflictDigest: shaB}, "56378c8559b5f6bdcebb3e097ff7385c78c0eabdcb1a56ae5effac50f0cdf1a3"},
		{InterruptFailureReview, InterruptGeneration{AttemptNo: 1, Generation: 2, FailureDigest: shaC}, "98da21cd0a751c6f54f043302d88fa93b08f15c98a406ecac4b09d51ad573cca"},
		{InterruptStartupStall, InterruptGeneration{AttemptNo: 1, Generation: 2}, "18630f7c14d7526246fab89c1c99c6a47e80e38cca3efe9e54c1e54d149badae"},
	}
	for _, tc := range cases {
		got, err := interruptGenerationKey("run-01", tc.reason, tc.g)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s key = %s, want %s", tc.reason, got, tc.want)
		}
	}
}

func TestEmitInterruptWritesFiveThingsAndDeduplicates(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, Reason: InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/1", "head_sha": "abc", "review_requirement": "required", "recommended_action": "approve", "diff_ref": "https://forge.example/change/1/diff"}, Generation: InterruptGeneration{ChangeID: "change-01", HeadSHA: "0123456789abcdef0123456789abcdef01234567"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, MaxEscalations: 2, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	in, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if in.Severity != SeverityNormal || in.Brief != "事实：change_ref=https://forge.example/change/1；head_sha=abc；review_requirement=required；recommended_action=approve；diff_ref=https://forge.example/change/1/diff。建议：approve" {
		t.Fatalf("interrupt = %#v", in)
	}
	assertCount(t, db, "interrupts", 1)
	assertCount(t, db, "budget_entries", 1)
	assertCount(t, db, "events", 1)
	assertCount(t, db, "outbox_operations", 1)
	assertCount(t, db, "interrupt_deliveries", 1)
	var status, key string
	if err := db.db.QueryRow(`SELECT status, operation_key FROM runs JOIN outbox_operations ON outbox_operations.run_id=runs.id WHERE runs.id='run'`).Scan(&status, &key); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_human" || key != "comment:interrupt:"+in.ID+":1" {
		t.Fatalf("status/key = %q/%q", status, key)
	}
	// A replay returns the same record even though the Run version advanced.
	cmd.ExpectedRunVersion = 99
	again, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil || again.ID != in.ID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	assertCount(t, db, "budget_entries", 1)
	assertCount(t, db, "outbox_operations", 1)
}

func TestStartupStallFreezesAttempt(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "spec", "run", 1)
	insertAttempt(t, db, "run", 1, "spec")
	attempt := 1
	_, err := db.EmitInterrupt(ctx, EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall, Facts: map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree 保持隔离", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"}, Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), Source: SourceRecovery, NowMS: testNow})
	if err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := db.db.QueryRow(`SELECT isolation_state,isolation_reason FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "frozen" || reason != "termination_unconfirmed" {
		t.Fatalf("isolation = %s/%s", state, reason)
	}
}

func TestConcurrentStartupStallDiscoveryConverges(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "spec", "run", 1)
	insertAttempt(t, db, "run", 1, "spec")
	attempt := 1
	cmd := EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree 保持隔离", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), Source: SourceRecovery, NowMS: testNow}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.EmitInterrupt(ctx, cmd)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent emission = %v", err)
		}
	}
	assertCount(t, db, "interrupts", 1)
	assertCount(t, db, "budget_entries", 1)
	assertCount(t, db, "outbox_operations", 1)
}

func TestEmitInterruptT4UsesConfiguredSeamAndEscapesFragmentsOnce(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	called := false
	db.SetInterruptT4(func(_ context.Context, in InterruptT4Input) (InterruptT4Output, error) {
		called = true
		if !containsString(in.Fragments, "<b>risk</b>") {
			t.Fatalf("fragments were pre-escaped: %#v", in.Fragments)
		}
		return InterruptT4Output{Headline: in.Headline, Conclusion: "<b>risk</b>", KeyPoints: []string{"<b>risk</b>"}, Options: []string{"approve", "reject", "hold"}, RecommendedOptionID: "approve"}, nil
	})
	cmd := EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, Reason: InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/1", "head_sha": "abc", "review_requirement": "<b>risk</b>", "recommended_action": "approve", "diff_ref": "https://forge.example/change/1/diff"}, Generation: InterruptGeneration{ChangeID: "change-01", HeadSHA: "0123456789abcdef0123456789abcdef01234567"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	got, err := emitTestInterrupt(t, ctx, db, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !called || got.Brief != "结论：\\<b\\>risk\\</b\\>；要点：\\<b\\>risk\\</b\\>；建议：批准审阅（approve）" {
		t.Fatalf("called=%v interrupt=%#v", called, got)
	}
}

func TestEmitInterruptRejectsNonCanonicalRecommendedActionBeforeAnyWrite(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	cmd := EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, Reason: InterruptCodeReview, Facts: map[string]string{"change_ref": "https://forge.example/change/1", "head_sha": "abc", "review_requirement": "required", "recommended_action": "bogus", "diff_ref": "https://forge.example/change/1/diff"}, Generation: InterruptGeneration{ChangeID: "change-01", HeadSHA: "0123456789abcdef012345678901234567890123456789"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	if _, err := emitTestInterrupt(t, ctx, db, cmd); err == nil || !strings.Contains(err.Error(), "recommended_action") {
		t.Fatalf("error = %v", err)
	}
	assertCount(t, db, "interrupts", 0)
	assertCount(t, db, "budget_entries", 0)
}

func TestAcceptInterruptT4AttemptGolden(t *testing.T) {
	in := InterruptT4Input{RunID: "run-01", Reason: InterruptFailureReview, Severity: SeverityHigh, Modality: "voice", Headline: "失败需要人工决定", Fragments: []string{"/sift reject", "<!-- sift-op:x -->", "<b>风险</b>"}, Options: []InterruptOption{{"retry", "重试失败步骤", "再次执行", "相同故障可能再次发生"}, {"reject", "停止 Run", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}}
	out := InterruptT4Output{Headline: "失败需要人工决定", Conclusion: "<b>风险</b>", KeyPoints: []string{"<!-- sift-op:x -->", "/sift reject"}, Options: []string{"retry", "reject", "hold"}, RecommendedOptionID: "retry"}
	accepted, brief := acceptInterruptT4(in, out)
	if !accepted || brief != "结论：\\<b\\>风险\\</b\\>；要点：\\<\\!\\-\\- sift\\-op:x \\-\\-\\>；/sift reject；建议：重试失败步骤（retry）" {
		t.Fatalf("attempt golden accepted=%v brief=%q", accepted, brief)
	}
	out.Options = []string{"reject", "retry", "hold"}
	if accepted, _ := acceptInterruptT4(in, out); accepted {
		t.Fatal("reordered attempt options accepted")
	}
	out.Options = []string{"retry", "reject", "hold"}
	out.RecommendedOptionID = "retry_report_interrupt_quota"
	if accepted, _ := acceptInterruptT4(in, out); accepted {
		t.Fatal("unknown attempt option accepted")
	}
}

func TestEmitInterruptFailureReviewVariantsUseClosedSourceAndExactGoldens(t *testing.T) {
	ctx := context.Background()
	newDB := func(t *testing.T) *DB {
		t.Helper()
		db, _ := openTestDB(t)
		if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
			t.Fatal(err)
		}
		if err := db.SeedForgeRunForTest(ctx, "run-01", "project", "cfg", "42", testNow); err != nil {
			t.Fatal(err)
		}
		insertTaskSpec(t, db, "spec", "run-01", 1)
		insertAttempt(t, db, "run-01", 1, "spec")
		if _, err := db.db.Exec(`UPDATE attempts SET phase='finished',result_exit_code=1,result_digest='failed',result_observed_at_ms=?,finished_at_ms=? WHERE run_id='run-01' AND attempt_no=1`, testNow, testNow); err != nil {
			t.Fatal(err)
		}
		// Report-quota failure_review is bound to this durable security fact.
		if _, err := db.db.Exec(`INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES('0123456789abcdef0123456789abcdef','run-01','project','security.report_quota_exhausted','system',1,'{}',1754000000000,1754000000000)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`INSERT INTO report_quota_exhaustions(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id,failure_digest,generation_key,created_at_ms) VALUES('run-01',1754000000000,1754086400000,'0123456789abcdef0123456789abcdef','59da82e35758283e3501a202eb49c719527e5e4ecf9ddb73c6bde79547046509','cf9ab8808bcf7660c789a0417555b0a9c9ad1216ddabf462a7ccf6bab6aaa083',1754000000000)`); err != nil {
			t.Fatal(err)
		}
		return db
	}
	attemptCmd := func() EmitInterruptCmd {
		attempt := 1
		return EmitInterruptCmd{RunID: "run-01", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewAttempt, Facts: map[string]string{"failure_class": "CI", "failure_evidence_ref": "/r/ci", "recommended_action": "retry"}, Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	}
	quotaCmd := func() EmitInterruptCmd {
		return EmitInterruptCmd{RunID: "run-01", ExpectedRunVersion: 1, Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewReportQuota, Facts: map[string]string{"failure_class": "report_interrupt_quota_exhausted", "failure_evidence_ref": "sift://event/0123456789abcdef0123456789abcdef", "recommended_action": "hold"}, Generation: InterruptGeneration{ReportDailyBucketStartMS: 1754000000000, ReportDailyBucketEndMS: 1754086400000, SecurityEventID: "0123456789abcdef0123456789abcdef", FailureDigest: "59da82e35758283e3501a202eb49c719527e5e4ecf9ddb73c6bde79547046509"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow}
	}

	db := newDB(t)
	db.SetInterruptT4(func(_ context.Context, in InterruptT4Input) (InterruptT4Output, error) {
		if got := []string{in.Options[0].ID, in.Options[1].ID, in.Options[2].ID}; strings.Join(got, ",") != "retry,reject,hold" {
			t.Fatalf("attempt options = %v", got)
		}
		return InterruptT4Output{Headline: in.Headline, Conclusion: "CI", KeyPoints: []string{"CI"}, Options: []string{"reject", "retry", "hold"}, RecommendedOptionID: "retry"}, nil
	})
	got, err := db.EmitInterrupt(ctx, attemptCmd())
	if err != nil {
		t.Fatal(err)
	}
	if got.Brief != "事实：failure_class=CI；failure_evidence_ref=/r/ci；recommended_action=retry。建议：retry" || got.Headline != "失败需要人工决定" {
		t.Fatalf("attempt fallback = %#v", got)
	}
	options, _ := json.Marshal(got.Options)
	if string(options) != `[{"effect":"再次执行","id":"retry","label":"重试失败步骤","risk":"相同故障可能再次发生"},{"effect":"Run 停止","id":"reject","label":"停止 Run","risk":"需人工重新发起"},{"effect":"保持等待","id":"hold","label":"暂缓决定","risk":"Run 继续占用待处理项"}]` {
		t.Fatalf("attempt options JSON = %s", options)
	}

	db = newDB(t)
	if _, err := db.db.Exec(`UPDATE runs SET status='running' WHERE id='run-01'`); err != nil {
		t.Fatal(err)
	}
	db.SetInterruptT4(func(_ context.Context, in InterruptT4Input) (InterruptT4Output, error) {
		if got := []string{in.Options[0].ID, in.Options[1].ID}; strings.Join(got, ",") != "reject,hold" {
			t.Fatalf("quota options = %v", got)
		}
		return InterruptT4Output{Headline: in.Headline, Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"reject", "hold"}, RecommendedOptionID: "hold"}, nil
	})
	got, err = db.EmitInterrupt(ctx, quotaCmd())
	if err != nil {
		t.Fatal(err)
	}
	if got.Brief != "结论：额度已耗尽；要点：请人工处理；建议：暂缓决定（hold）" || got.GenerationKey != "cf9ab8808bcf7660c789a0417555b0a9c9ad1216ddabf462a7ccf6bab6aaa083" {
		t.Fatalf("quota interrupt = %#v", got)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM runs WHERE id='run-01'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("quota changed run status=%q err=%v", status, err)
	}
	for _, out := range []InterruptT4Output{
		{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"hold", "reject"}, RecommendedOptionID: "hold"},
		{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"reject", "hold", "retry"}, RecommendedOptionID: "hold"},
		{Headline: "报告打扰额度已耗尽", Conclusion: "额度已耗尽", KeyPoints: []string{"请人工处理"}, Options: []string{"reject", "hold"}, RecommendedOptionID: "retry"},
	} {
		db := newDB(t)
		if _, err := db.db.Exec(`UPDATE runs SET status='running' WHERE id='run-01'`); err != nil {
			t.Fatal(err)
		}
		db.SetInterruptT4(func(context.Context, InterruptT4Input) (InterruptT4Output, error) { return out, nil })
		fallback, err := db.EmitInterrupt(ctx, quotaCmd())
		if err != nil || fallback.Brief != "事实：failure_class=report\\_interrupt\\_quota\\_exhausted；failure_evidence_ref=sift://event/0123456789abcdef0123456789abcdef；recommended_action=hold。建议：hold" || fallback.Severity != SeverityHigh || fallback.GenerationKey != "cf9ab8808bcf7660c789a0417555b0a9c9ad1216ddabf462a7ccf6bab6aaa083" || strings.Join([]string{fallback.Options[0].ID, fallback.Options[1].ID}, ",") != "reject,hold" {
			t.Fatalf("quota invalid T4 fallback=%#v err=%v", fallback, err)
		}
	}

	for _, tc := range []struct {
		name string
		cmd  EmitInterruptCmd
	}{
		{"quota with attempt", func() EmitInterruptCmd { c := quotaCmd(); n := 1; c.AttemptNo = &n; return c }()},
		{"attempt without identity", func() EmitInterruptCmd { c := attemptCmd(); c.AttemptNo = nil; return c }()},
		{"attempt with quota fields", func() EmitInterruptCmd { c := attemptCmd(); c.Generation.ReportDailyBucketStartMS = 1; return c }()},
		{"quota with attempt generation", func() EmitInterruptCmd { c := quotaCmd(); c.Generation.AttemptNo = 1; return c }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateFailureReviewVariant(tc.cmd); err == nil {
				t.Fatal("cross-contaminated variant accepted")
			}
		})
	}
}

func TestEmitInterruptFailureReviewRequiresFailedBoundGeneration(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "spec", "run", 1)
	insertAttempt(t, db, "run", 1, "spec")
	attempt := 1
	_, err := db.EmitInterrupt(ctx, EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewAttempt, Facts: map[string]string{"failure_class": "CI", "failure_evidence_ref": "/r/ci", "recommended_action": "retry"}, Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), Source: SourceSystem, NowMS: testNow})
	if err == nil || !strings.Contains(err.Error(), "requires failed attempt generation") {
		t.Fatalf("error = %v", err)
	}
	assertCount(t, db, "interrupts", 0)
	assertCount(t, db, "interrupt_command_effect_bindings", 0)
}

func TestRecordReportQuotaExhaustionUsesSystemEventIdentity(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE runs SET status='running' WHERE id='run'`); err != nil {
		t.Fatal(err)
	}
	got, err := db.RecordReportQuotaExhaustion(ctx, ReportQuotaExhaustionCmd{RunID: "run", ExpectedRunVersion: 1, DailyBucketStartMS: 1754000000000, DailyBucketEndMS: 1754086400000, AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", DailySummaryAt: "09:00", Channels: []InterruptChannel{{ID: "ops", Type: "webhook", TargetRef: "secret_ref:OPS", Renderer: "plain-v1", Capabilities: []string{"voice"}}}, NowMS: testNow})
	if err != nil {
		t.Fatal(err)
	}
	var eventID, source, ref string
	if err := db.db.QueryRow(`SELECT q.security_event_id,e.source,i.brief_markdown FROM report_quota_exhaustions q JOIN events e ON e.id=q.security_event_id JOIN interrupts i ON i.generation_key=q.generation_key`).Scan(&eventID, &source, &ref); err != nil {
		t.Fatal(err)
	}
	if got.ID == "" || len(eventID) != 32 || source != "system" || !strings.Contains(ref, "sift://event/"+eventID) {
		t.Fatalf("interrupt=%#v event=%q source=%q brief=%q", got, eventID, source, ref)
	}
	var channelID, operationKey string
	if err := db.db.QueryRow(`SELECT channel_id,operation_key FROM interrupt_deliveries WHERE interrupt_id=? AND surface='channel'`, got.ID).Scan(&channelID, &operationKey); err != nil {
		t.Fatal(err)
	}
	if channelID != "ops" || operationKey != ChannelPublishOperationKey(got.ID, 0) {
		t.Fatalf("channel delivery = %q/%q", channelID, operationKey)
	}
}

func TestEmitInterruptRejectsBeforeAnyWrite(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	attempt := 1
	_, err := db.EmitInterrupt(ctx, EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewAttempt, Facts: map[string]string{"failure_class": "CI", "failure_evidence_ref": "/tmp/evidence", "recommended_action": "retry\nnow"}, Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), Source: SourceSystem, NowMS: testNow})
	if err == nil || !strings.Contains(err.Error(), "interrupt_brief_lf_rejected") {
		t.Fatalf("error = %v", err)
	}
	assertCount(t, db, "interrupts", 0)
	assertCount(t, db, "budget_entries", 0)
	assertCount(t, db, "outbox_operations", 0)
}

func TestValidEventKey(t *testing.T) {
	cases := []struct {
		k    string
		want bool
	}{
		{"", false},
		{"gate:abc:failed", true},
		{"run_01:checks:1", true},
		{"event:", true},
		{"UPPER", false},
		{"has space", false},
		{"has/slash", false},
		{"a", true},
		{":", true},
		{"_", true},
	}
	for _, tc := range cases {
		if got := validEventKey(tc.k); got != tc.want {
			t.Fatalf("validEventKey(%q)=%v want %v", tc.k, got, tc.want)
		}
	}
}

func TestValidLink(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"/path", true},
		{"https://example.com/x", true},
		{"sift://event/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"sift://event/event:gate:op:failed", true},
		{"sift://event/event:", false},
		{"sift://event/event:BAD", false},
		{"sift://event/event:has space", false},
		{"sift://event/short", false},
		{"sift://other/x", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := validLink(tc.v); got != tc.want {
			t.Fatalf("validLink(%q)=%v want %v", tc.v, got, tc.want)
		}
	}
}
