package daemon

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xsift/sift/internal/storage"
)

// seedRunWithFinishedAttempt creates a run stuck in "running" whose latest
// attempt has already reached "finished" — exactly the state the bug report
// shows ("✓ 运行中 已完成"). The run never left running, so there is no live
// process, yet no non-terminal attempt exists either.
func seedRunWithFinishedAttempt(t *testing.T) (*storage.DB, *sql.DB, int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close(); db.Close() })
	for _, statement := range []string{
		`UPDATE runs SET status='running', version=2 WHERE id='run'`,
		`INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','digest',10000)`,
		`INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,result_exit_code,result_digest,result_observed_at_ms,finished_at_ms,created_at_ms,updated_at_ms) VALUES ('run',1,'finished',1,'process','agent','task','/work','branch','main','abc',0,'result-digest',10001,10001,10000,10000)`,
		`INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,wrapper_instance_id,wrapper_session_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','instance','session',10000,10000)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var version int64
	if err := raw.QueryRow(`SELECT version FROM runs WHERE id='run'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return db, raw, version, now
}

// TestOperatorKillFinishedAttempt kills a run stuck in "running" with a
// finished attempt. Before the fix Operator returned ErrRejectedStale and the
// run was un-killable; now the absence outcome applies and the run becomes
// failed with an operator_kill reason and a termination.absence_confirmed
// audit event.
func TestOperatorKillFinishedAttempt(t *testing.T) {
	db, raw, version, now := seedRunWithFinishedAttempt(t)
	coordinator := &TerminationCoordinator{DB: db, Now: func() time.Time { return now }}

	if err := coordinator.Operator(context.Background(), "run", version, false); err != nil {
		t.Fatalf("kill of a finished-attempt run failed: %v", err)
	}

	var status, reason string
	var newVersion int64
	if err := raw.QueryRow(`SELECT status,failure_reason,version FROM runs WHERE id='run'`).Scan(&status, &reason, &newVersion); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "operator_kill" {
		t.Fatalf("run = %s/%q, want failed/operator_kill", status, reason)
	}
	if newVersion != version+1 {
		t.Fatalf("run version = %d, want %d (one transition)", newVersion, version+1)
	}
	var events int
	if err := raw.QueryRow(`SELECT count(*) FROM events WHERE run_id='run' AND type='termination.absence_confirmed'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("termination.absence_confirmed events = %d, want 1", events)
	}
}

// TestOperatorRetryFinishedAttempt retries a run stuck in "running" with a
// finished attempt. The absence outcome applies and retry enqueues a new
// attempt (mirrors the normal retry-after-absence path), moving the run back
// to queued and bumping retry_count.
func TestOperatorRetryFinishedAttempt(t *testing.T) {
	db, raw, version, now := seedRunWithFinishedAttempt(t)
	coordinator := &TerminationCoordinator{DB: db, Now: func() time.Time { return now }}

	if err := coordinator.Operator(context.Background(), "run", version, true); err != nil {
		t.Fatalf("retry of a finished-attempt run failed: %v", err)
	}

	var status string
	var attempts, retryCount int
	if err := raw.QueryRow(`SELECT status,retry_count FROM runs WHERE id='run'`).Scan(&status, &retryCount); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("run status = %q, want queued", status)
	}
	if retryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", retryCount)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (a new retry attempt)", attempts)
	}
}

// TestOperatorKillFinishedAttemptStaleOnVersionRace confirms the genuine
// version stale path still works: a kill with a stale expected_version is
// rejected as stale even though the attempt is finished.
func TestOperatorKillFinishedAttemptStaleOnVersionRace(t *testing.T) {
	db, _, version, now := seedRunWithFinishedAttempt(t)
	coordinator := &TerminationCoordinator{DB: db, Now: func() time.Time { return now }}

	if err := coordinator.Operator(context.Background(), "run", version+1, false); !errors.Is(err, storage.ErrRejectedStale) {
		t.Fatalf("stale version kill = %v, want ErrRejectedStale", err)
	}
}

// TestOperatorKillAlreadyTerminalRun kills a run that is already done/failed.
// It must surface ErrRunAlreadyTerminal (a clear, non-stale signal) rather
// than the misleading stale.
func TestOperatorKillAlreadyTerminalRun(t *testing.T) {
	db, raw, version, now := seedRunWithFinishedAttempt(t)
	if _, err := raw.Exec(`UPDATE runs SET status='failed', failure_reason='agent_exit', completed_at_ms=10002 WHERE id='run'`); err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, Now: func() time.Time { return now }}

	if err := coordinator.Operator(context.Background(), "run", version, false); !errors.Is(err, storage.ErrRunAlreadyTerminal) {
		t.Fatalf("kill of an already-terminal run = %v, want ErrRunAlreadyTerminal", err)
	}
}

// TestOperatorKillQueuedRunWithoutAttempt kills a non-terminal run that has
// no attempt body at all (queued before assignment). There is nothing to
// record a termination against, so the run is forced straight to failed.
func TestOperatorKillQueuedRunWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close(); db.Close() })
	coordinator := &TerminationCoordinator{DB: db, Now: func() time.Time { return now }}

	if err := coordinator.Operator(ctx, "run", 1, false); err != nil {
		t.Fatalf("kill of a queued run without attempts failed: %v", err)
	}
	var status string
	if err := raw.QueryRow(`SELECT status FROM runs WHERE id='run'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
}
