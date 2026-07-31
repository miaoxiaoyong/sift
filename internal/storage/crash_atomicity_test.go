package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestV2CurrentWritePortsCrashAtomicity injects an abort at the last mutable
// write in every M1 write family that exists today. A failed transaction must
// expose none of its earlier projection, event, receipt, trace, charge, or
// outbox writes.
func TestV2CurrentWritePortsCrashAtomicity(t *testing.T) {
	t.Run("forge run receipt", func(t *testing.T) {
		db, _ := openTestDB(t)
		insertConfigSnapshot(t, db, "cfg")
		insertProject(t, db, "project", "cfg")
		mustExec(t, db, `CREATE TRIGGER fail_receipt BEFORE INSERT ON forge_event_receipts BEGIN SELECT RAISE(ABORT, 'injected crash'); END`)
		_, err := db.CreateForgeRun(context.Background(), CreateForgeRunCmd{
			RunID: "run", ProjectID: "project", ConfigSnapshotID: "cfg", ForgeKind: "github", ForgeHost: "github.com", ForgeProjectKey: "org/repo", IssueID: "42", TriggerLabelEventID: "label-42", TriggerActor: "alice", TriggerObservedAtMS: testNow, CreatedAtMS: testNow,
		})
		if err == nil || !strings.Contains(err.Error(), "injected crash") {
			t.Fatalf("CreateForgeRun error = %v", err)
		}
		assertCount(t, db, "runs", 0)
		assertCount(t, db, "events", 0)
		assertCount(t, db, "forge_event_receipts", 0)
	})

	t.Run("task spec assignment event", func(t *testing.T) {
		db, _ := openTestDB(t)
		insertConfigSnapshot(t, db, "cfg")
		insertProject(t, db, "project", "cfg")
		insertManualRun(t, db, "run", "project", "cfg")
		mustExec(t, db, `CREATE TRIGGER fail_assignment_event BEFORE INSERT ON events WHEN NEW.type = 'run.assigned' BEGIN SELECT RAISE(ABORT, 'injected crash'); END`)
		_, err := db.SetInitialTaskSpec(context.Background(), SetInitialTaskSpecCmd{RunID: "run", ExpectedVersion: 1, TaskSpecID: "spec", CanonicalJSON: []byte(`{"schema_version":1}`), ContentDigest: "digest", Kind: "feature", AgentID: "agent", OccurredAtMS: testNow})
		if err == nil || !strings.Contains(err.Error(), "injected crash") {
			t.Fatalf("SetInitialTaskSpec error = %v", err)
		}
		assertCount(t, db, "task_spec_snapshots", 0)
		var version int
		var spec, agent string
		if err := db.db.QueryRow(`SELECT version, COALESCE(current_task_spec_id, ''), COALESCE(agent_id, '') FROM runs WHERE id='run'`).Scan(&version, &spec, &agent); err != nil {
			t.Fatal(err)
		}
		if version != 1 || spec != "" || agent != "" {
			t.Fatalf("assignment leaked: version=%d spec=%q agent=%q", version, spec, agent)
		}
	})

	t.Run("brain attempt charge", func(t *testing.T) {
		db, _ := openTestDB(t)
		seedT2Run(t, db, "run")
		call := reserveT2Call(t, db, "run")
		mustExec(t, db, `CREATE TRIGGER fail_brain_charge BEFORE INSERT ON budget_entries WHEN NEW.kind = 'token' BEGIN SELECT RAISE(ABORT, 'injected crash'); END`)
		_, err := db.RecordBrainAttempt(context.Background(), BrainAttemptCmd{CallID: call.ID, ProviderAttempt: 1, Outcome: BrainAttemptValid, RequestDigest: testCallIDDigest, RawOutputText: strp("raw"), RawOutputDigest: strp("digest"), RawOutputBytes: int64p(3), InputTokens: int64p(2), OutputTokens: int64p(3), StartedAtMS: testBucket, FinishedAtMS: testBucket + 1, TokenLimit: 100})
		if err == nil || !strings.Contains(err.Error(), "injected crash") {
			t.Fatalf("RecordBrainAttempt error = %v", err)
		}
		assertCount(t, db, "brain_attempts", 0)
		assertCount(t, db, "budget_entries", 0)
		assertCount(t, db, "budget_counters", 0)
	})

	t.Run("outbox claim and completion", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		if _, err := db.EnqueueOperation(ctx, Operation{Key: "alert:crash:claim:1", Kind: OperationForgeAlert, Payload: []byte(`{}`)}, testNow); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `CREATE TRIGGER fail_claim_attempt BEFORE INSERT ON outbox_attempts BEGIN SELECT RAISE(ABORT, 'injected crash'); END`)
		if _, err := db.ClaimOutboxOperation(ctx, "worker", testNow, 10); err == nil {
			t.Fatal("ClaimOutboxOperation unexpectedly succeeded")
		}
		var state string
		var attempts int
		if err := db.db.QueryRow(`SELECT state, attempt_count FROM outbox_operations WHERE operation_key='alert:crash:claim:1'`).Scan(&state, &attempts); err != nil {
			t.Fatal(err)
		}
		if state != string(OperationPending) || attempts != 0 {
			t.Fatalf("claim leaked: state=%s attempts=%d", state, attempts)
		}
		mustExec(t, db, `DROP TRIGGER fail_claim_attempt`)
		claim, err := db.ClaimOutboxOperation(ctx, "worker", testNow, 10)
		if err != nil || claim == nil {
			t.Fatalf("claim = %+v, %v", claim, err)
		}
		mustExec(t, db, `CREATE TRIGGER fail_completion_result BEFORE INSERT ON outbox_attempt_results BEGIN SELECT RAISE(ABORT, 'injected crash'); END`)
		if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationSucceeded, NowMS: testNow + 1}); err == nil {
			t.Fatal("CompleteOutboxAttempt unexpectedly succeeded")
		}
		if err := db.db.QueryRow(`SELECT state FROM outbox_operations WHERE id=?`, claim.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != string(OperationExecuting) {
			t.Fatalf("completion leaked state=%s", state)
		}
		assertCount(t, db, "outbox_attempt_results", 0)
	})

	t.Run("startup stall probe success", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx := context.Background()
		seedCommandRun(t, db, ctx)
		interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)
		env := commentEnv(t, "project", "v2-retry", "/sift retry "+cmdRun+" "+nonce)
		if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
			t.Fatal(err)
		}
		var probeID string
		if err := db.db.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=?`, interruptID).Scan(&probeID); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `CREATE TRIGGER fail_retry_claim BEFORE INSERT ON attempt_claims WHEN NEW.attempt_no=2 BEGIN SELECT RAISE(ABORT, 'injected crash'); END`)
		_, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: true, AbsenceEvidenceJSON: json.RawMessage(`{"absent":true}`), NowMS: testNow + 10})
		if err == nil || !strings.Contains(err.Error(), "injected crash") {
			t.Fatalf("ApplyRetryProbeResult error=%v", err)
		}
		assertCount(t, db, "attempt_probes WHERE id='"+probeID+"' AND state='pending'", 1)
		assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"'", 1)
		assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_resolution IS NOT NULL", 0)
		assertCount(t, db, "interrupts WHERE id='"+interruptID+"' AND status='open'", 1)
		assertCount(t, db, "command_event_outcomes WHERE state='pending'", 1)
		assertCount(t, db, "outbox_operations WHERE kind IN ('launch_agent','command_ack')", 0)
		if status := runStatus(t, db); status != "waiting_human" {
			t.Fatalf("run status=%s after rollback", status)
		}
		mustExec(t, db, `DROP TRIGGER fail_retry_claim`)
		if _, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: true, AbsenceEvidenceJSON: json.RawMessage(`{"absent":true}`), NowMS: testNow + 10}); err != nil {
			t.Fatal(err)
		}
		assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2", 1)
		assertCount(t, db, "attempt_claims WHERE run_id='"+cmdRun+"' AND attempt_no=2", 1)
		assertCount(t, db, "outbox_operations WHERE kind='launch_agent' AND attempt_no=2", 1)
	})
}

func assertCount(t *testing.T, db *DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
