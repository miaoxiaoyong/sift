package storage

import (
	"context"
	"testing"
)

func TestRerunChecksStartedAttemptOnlyAcceptsSuccessOrConflict(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-complete", "project-complete", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-complete", "project-complete", "cfg-complete", "42", testNow); err != nil {
		t.Fatal(err)
	}
	payload, _ := CanonicalJSON(map[string]any{
		"run_id": "run-complete", "change_id": "change-1", "head_sha": "head-1",
		"check_run_id": "check-1", "retry_no": 1,
		"triage_source_digest":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"created_from_event_id": "event:source",
	})
	op := Operation{ID: "complete-op", Key: RerunChecksOperationKey("run-complete", "head-1", "check-1", 1), Kind: OperationRerunChecks, RunID: "run-complete", Payload: payload}
	if _, err := db.EnqueueOperation(ctx, op, testNow); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimOutboxOperationKind(ctx, "worker", OperationRerunChecks, testNow+1, 1000)
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}
	if err := db.MarkOutboxAttemptRequestStarted(ctx, *claim, testNow+2); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationRetryable, ErrorClass: ErrorTransient, NowMS: testNow + 3}); err == nil {
		t.Fatal("started request accepted retryable completion")
	}
	if err := db.CompleteOutboxAttempt(ctx, *claim, CompleteOutcome{State: OperationSucceeded, Evidence: []byte(`{"requested":true}`), NowMS: testNow + 4}); err != nil {
		t.Fatalf("success: %v", err)
	}
	var state string
	if err := db.db.QueryRow(`SELECT state FROM outbox_operations WHERE id=?`, claim.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" {
		t.Fatalf("state=%s", state)
	}
}
