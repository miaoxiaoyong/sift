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
