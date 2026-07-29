package storage

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func interruptQuota() map[InterruptSeverity]int {
	return map[InterruptSeverity]int{SeverityLow: 3, SeverityNormal: 5, SeverityHigh: 5}
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
	in, err := db.EmitInterrupt(ctx, cmd)
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
	again, err := db.EmitInterrupt(ctx, cmd)
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

func TestEmitInterruptRejectsBeforeAnyWrite(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	_, err := db.EmitInterrupt(ctx, EmitInterruptCmd{RunID: "run", ExpectedRunVersion: 1, Reason: InterruptFailureReview, Facts: map[string]string{"failure_class": "CI", "failure_evidence_ref": "/tmp/evidence", "recommended_action": "retry\nnow"}, Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("a", 64)}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(), Source: SourceSystem, NowMS: testNow})
	if err == nil || !strings.Contains(err.Error(), "interrupt_brief_lf_rejected") {
		t.Fatalf("error = %v", err)
	}
	assertCount(t, db, "interrupts", 0)
	assertCount(t, db, "budget_entries", 0)
	assertCount(t, db, "outbox_operations", 0)
}
