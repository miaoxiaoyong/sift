package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/hooks"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func TestHookCompletionRecheckRunsForNormalResultAndReplay(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
	root := t.TempDir()
	path := filepath.Join(root, "runs", "run", "attempts", "1", "result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"schema_version": 1, "run_id": "run", "attempt_no": 1, "generation": 1, "wrapper_instance_id": "instance", "agent_identity": map[string]any{"pid": 11, "started_at_ms": 1001, "executable": "/agent"}, "exit_code": 0, "signal": nil, "finished_at_ms": now.UnixMilli()})
	if err := os.WriteFile(path, result, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	coordinator := &TerminationCoordinator{DB: db, ControlRoot: root, Now: func() time.Time { return now }, HookRecheck: func(ctx context.Context, runID string, attemptNo int) error {
		calls++
		return db.CompleteHookRecheck(ctx, runID, attemptNo, now.UnixMilli())
	}}
	if disposition, err := coordinator.resolveLateFact(context.Background(), attempt); err != nil || disposition != storage.AttemptRaceDuplicate {
		t.Fatalf("completion = %q, %v", disposition, err)
	}
	if err := coordinator.RecheckHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("completion/replay hook calls = %d, want exactly one", calls)
	}
	var phase, state string
	if err := raw.QueryRow(`SELECT a.phase,h.state FROM attempts a JOIN hook_recheck_receipts h ON h.run_id=a.run_id AND h.attempt_no=a.attempt_no WHERE a.run_id='run'`).Scan(&phase, &state); err != nil || phase != "finished" || state != "completed" {
		t.Fatalf("terminal receipt = %q/%q, %v", phase, state, err)
	}
}

func hookGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestHookCrashReplayRecordsOneStableDrift(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
	repo := t.TempDir()
	hookGit(t, repo, "init")
	hookGit(t, repo, "config", "core.hooksPath", filepath.Join(repo, ".git", "hooks"))
	if _, err := db.ExecForTest(context.Background(), `UPDATE projects SET repo_path=? WHERE id='project'`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO project_hook_baselines(project_id,git_config_digest,effective_hooks_path,hooks_directory_digest,baseline_digest,captured_at_ms,updated_at_ms) VALUES('project','trusted-config',?,'trusted-directory','trusted',?,?)`, filepath.Join(repo, ".git", "hooks"), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	const baseline = "trusted"
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "pre-commit"), []byte("changed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if _, err := db.ResolveAttemptRace(context.Background(), storage.AttemptRaceCommand{RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, FactKey: "crash-terminal", NowMS: now.UnixMilli(), Agent: &storage.AgentIdentity{PID: 11, StartedAtMS: 1001, Executable: "/agent"}, Result: &storage.AttemptResult{Agent: storage.AgentIdentity{PID: 11, StartedAtMS: 1001, Executable: "/agent"}, ExitCode: &exit, Digest: "crash-terminal", FinishedAtMS: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, HookRecheck: HookRechecker(db, func() time.Time { return now })}
	if err := coordinator.RecheckHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	var first, after string
	if err := raw.QueryRow(`SELECT payload_json FROM events WHERE type='hooks_drift_detected'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RecheckHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT payload_json FROM events WHERE type='hooks_drift_detected'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var trusted string
	if err := raw.QueryRow(`SELECT baseline_digest FROM project_hook_baselines WHERE project_id='project'`).Scan(&trusted); err != nil || trusted != baseline || first != after {
		t.Fatalf("baseline/event stable = %q/%q %q/%q err=%v", baseline, trusted, first, after, err)
	}
}

func TestHookActivationMissingAfterStartupCompletionDoesNotAdoptDrift(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
	exit := 0
	if _, err := db.ResolveAttemptRace(context.Background(), storage.AttemptRaceCommand{RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, FactKey: "startup-result", NowMS: now.UnixMilli(), Agent: &storage.AgentIdentity{PID: 11, StartedAtMS: 1001, Executable: "/agent"}, Result: &storage.AttemptResult{Agent: storage.AgentIdentity{PID: 11, StartedAtMS: 1001, Executable: "/agent"}, ExitCode: &exit, Digest: "startup-result", FinishedAtMS: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHookBaseline(context.Background(), storage.RecordHookBaselineCmd{ProjectID: "project", Snapshot: storage.HookBaselineSnapshot{GitConfigDigest: "changed", EffectiveHooksPath: "/changed-hooks", HooksDirectoryDigest: "changed-dir", Digest: "changed"}, CapturedAtMS: now.Add(time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var baselines, diagnostics int
	if err := raw.QueryRow(`SELECT count(*) FROM project_hook_baselines`).Scan(&baselines); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM events WHERE type='hooks_baseline_activation_missing'`).Scan(&diagnostics); err != nil {
		t.Fatal(err)
	}
	if baselines != 0 || diagnostics != 1 {
		t.Fatalf("baseline/diagnostic = %d/%d, want absent/stable", baselines, diagnostics)
	}
}

func TestHookUpgradeBootstrapRequiresExplicitOperatorPath(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(10000)
	db, raw, _, _ := seedRecoveryCoordinator(t, "running", 0)
	repo := t.TempDir()
	hookGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "pre-commit"), []byte("operator-confirmed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE projects SET repo_path=? WHERE id='project'`, repo); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if _, err := db.ResolveAttemptRace(ctx, storage.AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, FactKey: "legacy-result", NowMS: now.UnixMilli(), Agent: &storage.AgentIdentity{PID: 11, StartedAtMS: 1001, Executable: "/agent"}, Result: &storage.AttemptResult{Agent: storage.AgentIdentity{PID: 11, StartedAtMS: 1001, Executable: "/agent"}, ExitCode: &exit, Digest: "legacy", FinishedAtMS: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Projects: []config.Project{{ID: "project", Enabled: true, Repo: repo}}}
	CaptureHookBaselines(ctx, db, cfg, func() time.Time { return now })
	var absent int
	if err := raw.QueryRow(`SELECT count(*) FROM project_hook_baselines WHERE project_id='project'`).Scan(&absent); err != nil || absent != 0 {
		t.Fatalf("automatic legacy baseline = %d, %v; want absent", absent, err)
	}
	if err := BootstrapHookBaseline(ctx, db, cfg, "project", func() time.Time { return now.Add(time.Millisecond) }); err != nil {
		t.Fatal(err)
	}
	observed, err := hooks.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := raw.QueryRow(`SELECT baseline_digest FROM project_hook_baselines WHERE project_id='project'`).Scan(&persisted); err != nil || persisted != observed.Digest {
		t.Fatalf("bootstrapped baseline = %q, %v; want trusted %q", persisted, err, observed.Digest)
	}
}

func TestHookAuditOnlyCaptureErrorLeavesLifecycleAndBaselineIntact(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(10000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	hookGit(t, repo, "init")
	invalid := filepath.Join(repo, "not-a-directory")
	if err := os.WriteFile(invalid, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	hookGit(t, repo, "config", "core.hooksPath", invalid)
	if _, err := db.ExecForTest(ctx, `UPDATE projects SET repo_path=? WHERE id='project'`, repo); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Projects: []config.Project{{ID: "project", Enabled: true, Repo: repo}}}
	CaptureHookBaselines(ctx, db, cfg, func() time.Time { return now })
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES('task','run',1,1,'{}','digest',?)`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,result_exit_code,result_digest,result_observed_at_ms,finished_at_ms,created_at_ms,updated_at_ms) VALUES('run',1,'finished',1,'process','agent','task','/work','branch','main','sha','none',0,'result',?,?,?,?)`, now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO hook_recheck_receipts(run_id,attempt_no,project_id,state,created_at_ms) VALUES('run',1,'project','pending',?)`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, HookRecheck: HookRechecker(db, func() time.Time { return now })}
	if err := coordinator.RecheckHooks(ctx); err != nil {
		t.Fatalf("audit-only recheck stopped lifecycle: %v", err)
	}
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var baseline, diagnostics, completed int
	if err := raw.QueryRow(`SELECT count(*) FROM project_hook_baselines`).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM events WHERE type='hooks_capture_failed'`).Scan(&diagnostics); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM hook_recheck_receipts WHERE state='completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if baseline != 0 || diagnostics != 2 || completed != 1 {
		t.Fatalf("baseline/diagnostics/completed = %d/%d/%d, want 0/2/1", baseline, diagnostics, completed)
	}
}
