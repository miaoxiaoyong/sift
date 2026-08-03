package storage

import (
	"context"
	"testing"
)

func seedTerminationAttempt(t *testing.T) (*DB, context.Context) {
	t.Helper()
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
	return db, ctx
}

func TestRecoveryAttemptsIncludesNonterminalAttemptRegardlessOfRunState(t *testing.T) {
	db, ctx := seedTerminationAttempt(t)
	attempts, err := db.RecoveryAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].RunID != "run" || attempts[0].AttemptNo != 1 {
		t.Fatalf("recovery attempts = %#v", attempts)
	}
}

func TestTerminationUnconfirmedFreezesAndMakesStartupStallVisible(t *testing.T) {
	db, ctx := seedTerminationAttempt(t)
	run, err := db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1, Source: TerminationRecovery, DiagnosticCause: "termination_unconfirmed", AttentionDailyQuota: interruptQuota(), NowMS: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunWaitingHuman {
		t.Fatalf("run status = %s", run.Status)
	}
	var state string
	if err := db.db.QueryRow(`SELECT isolation_state FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "frozen" {
		t.Fatalf("isolation = %s", state)
	}
	assertCount(t, db, "interrupts", 1)
}

func TestTerminationKillAfterAbsenceFailsWithoutNewAttempt(t *testing.T) {
	db, ctx := seedTerminationAttempt(t)
	run, err := db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1, Source: TerminationKill, Absent: true, Evidence: "group gone", NowMS: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunFailed || run.FailureReason != "operator_kill" {
		t.Fatalf("run = %#v", run)
	}
	assertCount(t, db, "attempts", 1)
}

func TestRecordHandoffSecurityEventDoesNotPersistCredentials(t *testing.T) {
	db, ctx := seedTerminationAttempt(t)
	if err := db.RecordHandoffSecurityEvent(ctx, "run", 1, "claim.acquire", "stale", testNow); err != nil {
		t.Fatal(err)
	}
	var typ, payload string
	if err := db.db.QueryRow(`SELECT type,payload_json FROM events WHERE type='security.handoff_rejected'`).Scan(&typ, &payload); err != nil {
		t.Fatal(err)
	}
	if typ != "security.handoff_rejected" || payload != `{"disposition":"stale","method":"claim.acquire"}` {
		t.Fatalf("security event = %q %s", typ, payload)
	}
}

func TestTerminationRetryAfterAbsenceCreatesNewAttempt(t *testing.T) {
	db, ctx := seedTerminationAttempt(t)
	oldKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := db.ExecForTest(ctx, `UPDATE attempts SET topology_qualification_key=? WHERE run_id='run' AND attempt_no=1`, oldKey); err != nil {
		t.Fatal(err)
	}
	run, err := db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1, Source: TerminationRetry, Absent: true, Evidence: "group gone", NowMS: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunQueued {
		t.Fatalf("run status = %s", run.Status)
	}
	var phase string
	if err := db.db.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=2`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "pending" {
		t.Fatalf("new attempt phase = %s", phase)
	}
	var successorKey string
	if err := db.db.QueryRow(`SELECT COALESCE(topology_qualification_key,'') FROM attempts WHERE run_id='run' AND attempt_no=2`).Scan(&successorKey); err != nil || successorKey != "" {
		t.Fatalf("absence successor qualification key=%q err=%v, want empty", successorKey, err)
	}
	var resolution string
	if err := db.db.QueryRow(`SELECT attempt_resolution FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&resolution); err != nil {
		t.Fatal(err)
	}
	if resolution != "retry_after_absence" {
		t.Fatalf("old attempt resolution = %q", resolution)
	}
	assertCount(t, db, "outbox_operations", 1)
}
