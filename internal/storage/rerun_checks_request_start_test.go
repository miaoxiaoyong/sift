package storage

import (
	"context"
	"testing"
)

func TestRerunChecksPreRequestCrashCanReclaim(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-rerun", "project-rerun", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-rerun", "project-rerun", "cfg-rerun", "42", testNow); err != nil {
		t.Fatal(err)
	}
	payload, _ := CanonicalJSON(map[string]any{
		"run_id": "run-rerun", "change_id": "change-1", "head_sha": "head-1",
		"check_run_id": "check-1", "retry_no": 1,
		"triage_source_digest":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"created_from_event_id": "event:source",
	})
	op := Operation{ID: "rerun-op", Key: RerunChecksOperationKey("run-rerun", "head-1", "check-1", 1), Kind: OperationRerunChecks, RunID: "run-rerun", Payload: payload}
	if _, err := db.EnqueueOperation(ctx, op, testNow); err != nil {
		t.Fatal(err)
	}
	first, err := db.ClaimOutboxOperationKind(ctx, "worker-1", OperationRerunChecks, testNow+1, 100)
	if err != nil || first == nil {
		t.Fatalf("first claim=%v err=%v", first, err)
	}
	second, err := db.ClaimOutboxOperationKind(ctx, "worker-2", OperationRerunChecks, first.LeaseExpiresAtMS+1, 100)
	if err != nil || second == nil {
		t.Fatalf("pre-request reclaim=%v err=%v", second, err)
	}
	if second.ClaimAttemptNo != 2 || second.AttemptID == first.AttemptID {
		t.Fatalf("replacement attempt=%+v first=%+v", second, first)
	}
	var outcome, summary string
	if err := db.db.QueryRow(`SELECT outcome,error_summary FROM outbox_attempt_results WHERE attempt_id=?`, first.AttemptID).Scan(&outcome, &summary); err != nil {
		t.Fatal(err)
	}
	if outcome != "retry" || summary != "lease_expired" {
		t.Fatalf("old result=%s/%s", outcome, summary)
	}
}

func TestRequestStartRejectsOtherOperationKinds(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	op := Operation{ID: "comment-op", Key: "comment:test:1", Kind: OperationForgeComment, Payload: []byte(`{"purpose":"summary"}`)}
	if _, err := db.EnqueueOperation(ctx, op, testNow); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimOutboxOperationKind(ctx, "worker", OperationForgeComment, testNow+1, 100)
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}
	if _, err := db.db.Exec(`INSERT INTO outbox_attempt_request_starts(attempt_id,started_at_ms) VALUES(?,?)`, claim.AttemptID, testNow+2); err == nil {
		t.Fatal("non-rerun request start accepted")
	}
}
