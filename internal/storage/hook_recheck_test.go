package storage

import (
	"context"
	"database/sql"
	"testing"
)

func TestHookActivationMissingCompletedEvidenceDoesNotInstallBaseline(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES('task','run',1,1,'{}','digest',?)`, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,result_exit_code,result_digest,result_observed_at_ms,finished_at_ms,created_at_ms,updated_at_ms) VALUES('run',1,'finished',1,'process','agent','task','/work','branch','main','sha','none',0,'result',?,?,?,?)`, testNow, testNow, testNow, testNow); err != nil {
		t.Fatal(err)
	}
	snapshot := HookBaselineSnapshot{GitConfigDigest: "config", EffectiveHooksPath: "/hooks", HooksDirectoryDigest: "dir", Digest: "drifted"}
	if err := db.RecordHookBaseline(ctx, RecordHookBaselineCmd{ProjectID: "project", Snapshot: snapshot, CapturedAtMS: testNow + 1}); err != nil {
		t.Fatal(err)
	}
	var baselines, diagnostics int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM project_hook_baselines WHERE project_id='project'`).Scan(&baselines); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type='hooks_baseline_activation_missing'`).Scan(&diagnostics); err != nil {
		t.Fatal(err)
	}
	if baselines != 0 || diagnostics != 1 {
		t.Fatalf("baseline/diagnostics = %d/%d, want absent/stable diagnostic", baselines, diagnostics)
	}
}

func TestHookLegacyBootstrapGatesLaunchUntilExplicitConfirmation(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "legacy", "project", "cfg", testNow, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE attempts SET phase='finished',result_exit_code=0,result_digest='legacy',result_observed_at_ms=?,finished_at_ms=? WHERE run_id='legacy'`, testNow, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE runs SET issue_id='legacy-issue' WHERE id='legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "upgrade", "project", "cfg", testNow+1, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "hash-cfg", "test", 1, 1, testNow+2)
	if err != nil {
		t.Fatal(err)
	}
	attempts, operations, err := db.StartupRecoveryPending(ctx, boot)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if err := db.ApplyStartupRecoveryAction(ctx, StartupRecoveryAction{BootID: boot, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: "test", Action: "supervise", NowMS: testNow + 3}); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range operations {
		if err := db.ApplyStartupRecoveryAction(ctx, StartupRecoveryAction{BootID: boot, OperationID: operation.ID, ExpectedOperationVersion: operation.Version, ObservationDigest: "test", Action: "converge_operation", NowMS: testNow + 3}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteStartupRecovery(ctx, boot, testNow+3); err != nil {
		t.Fatal(err)
	}
	if claim, err := db.ClaimLaunchOperation(ctx, boot, "worker", testNow+4, 100); err != nil || claim != nil {
		t.Fatalf("unconfirmed legacy claim = %#v, %v; want blocked", claim, err)
	}
	snapshot := HookBaselineSnapshot{GitConfigDigest: "confirmed-config", EffectiveHooksPath: "/confirmed-hooks", HooksDirectoryDigest: "confirmed-directory", Digest: "confirmed"}
	if err := db.RecordHookBaseline(ctx, RecordHookBaselineCmd{ProjectID: "project", Snapshot: snapshot, TrustedBootstrap: true, CapturedAtMS: testNow + 5}); err != nil {
		t.Fatal(err)
	}
	if claim, err := db.ClaimLaunchOperation(ctx, boot, "worker", testNow+6, 100); err != nil || claim == nil {
		t.Fatalf("confirmed legacy claim = %#v, %v; want launch", claim, err)
	}
	var baseline, bootstraps int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM project_hook_baselines WHERE project_id='project'`).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type='hooks_baseline_bootstrapped'`).Scan(&bootstraps); err != nil {
		t.Fatal(err)
	}
	if baseline != 1 || bootstraps != 1 {
		t.Fatalf("baseline/bootstrap event = %d/%d, want 1/1", baseline, bootstraps)
	}
}

func TestHookRecheckCrashReplayReceiptIsAtomicWithTerminalResult(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES('task','run',1,1,'{}','digest',?)`, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,agent_pid,agent_started_at_ms,agent_executable,created_at_ms,updated_at_ms) VALUES('run',1,'running',1,'process','agent','task','/work','branch','main','sha','none',11,12,'/agent',?,?)`, testNow, testNow); err != nil {
		t.Fatal(err)
	}
	exit := 0
	cmd := AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, FactKey: "result", NowMS: testNow + 1, Agent: &AgentIdentity{PID: 11, StartedAtMS: 12, Executable: "/agent"}, Result: &AttemptResult{Agent: AgentIdentity{PID: 11, StartedAtMS: 12, Executable: "/agent"}, ExitCode: &exit, Digest: "result", FinishedAtMS: testNow + 1}}
	if _, err := db.ResolveAttemptRace(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveAttemptRace(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM hook_recheck_receipts WHERE run_id='run' AND attempt_no=1 AND state='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending receipts = %d, want one", pending)
	}
	if err := db.CompleteHookRecheck(ctx, "run", 1, testNow+2); err != nil {
		t.Fatal(err)
	}
	var completed sql.NullInt64
	if err := db.db.QueryRowContext(ctx, `SELECT completed_at_ms FROM hook_recheck_receipts WHERE run_id='run' AND attempt_no=1`).Scan(&completed); err != nil || !completed.Valid {
		t.Fatalf("completion marker = %v, %v", completed, err)
	}
}
