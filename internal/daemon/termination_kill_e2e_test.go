package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/controlplane"
	"github.com/xsift/sift/internal/storage"
)

// TestOperatorKillFinishedAttemptEndToEnd drives the REAL control-plane server
// (bound to a unix socket) wired with the REAL TerminationCoordinator — exactly
// how cmd/sift/daemon.go wires it — and sends the real ops.ps + ops.kill RPCs
// that the `sift kill` CLI sends. It proves the fix works through the full
// client → socket → server → coordinator → DB path for a run stuck "running"
// with a finished attempt (the reported bug, "✓ 运行中 已完成").
func TestOperatorKillFinishedAttemptEndToEnd(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(50_000)
	homeDir, err := os.MkdirTemp("", "sft-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	home := config.Home{Path: homeDir, Resolved: homeDir}

	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "1", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE runs SET status='running', version=2 WHERE id='run'`,
		`INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','digest',50000)`,
		`INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,result_exit_code,result_digest,result_observed_at_ms,finished_at_ms,created_at_ms,updated_at_ms) VALUES ('run',1,'finished',1,'process','agent','task','/work','branch','main','abc',0,'result-digest',50001,50001,50000,50000)`,
		`INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,wrapper_instance_id,wrapper_session_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','instance','session',50000,50000)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	server, err := controlplane.Start(home, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	coordinator := &TerminationCoordinator{DB: db, Now: func() time.Time { return now }}
	server.SetOperatorAction(func(ctx context.Context, method, runID string, version int64) error {
		return coordinator.Operator(ctx, runID, version, method == "ops.retry")
	})

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = server.Serve(serveCtx) }()

	// Mirror the CLI exactly: ops.ps to resolve the version, then ops.kill.
	ps, err := controlplane.OperatorRequest(home, "ops.ps", map[string]any{"run_id": "run", "project_id": nil, "status": nil, "limit": 1, "after_run_id": nil})
	if err != nil {
		t.Fatalf("ops.ps: %v", err)
	}
	if !ps.OK {
		t.Fatalf("ops.ps not ok: %+v", ps.Error)
	}
	var psResult struct {
		Runs []struct {
			RunID   string `json:"run_id"`
			Version int64  `json:"version"`
		} `json:"runs"`
	}
	psBody, err := json.Marshal(ps.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(psBody, &psResult); err != nil || len(psResult.Runs) != 1 || psResult.Runs[0].Version < 1 {
		t.Fatalf("ops.ps result = %+v", ps.Result)
	}
	version := psResult.Runs[0].Version

	key := hex.EncodeToString([]byte("e2e-request-key-1"))
	kill, err := controlplane.OperatorRequest(home, "ops.kill", map[string]any{"run_id": "run", "expected_version": version, "request_key": key})
	if err != nil {
		t.Fatalf("ops.kill: %v", err)
	}
	if !kill.OK {
		t.Fatalf("ops.kill FAILED through the real server: code=%s message=%s (this is the reported bug)", kill.Error.Code, kill.Error.Message)
	}

	var status, reason string
	if err := raw.QueryRow(`SELECT status,failure_reason FROM runs WHERE id='run'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "operator_kill" {
		t.Fatalf("run = %s/%q, want failed/operator_kill", status, reason)
	}
}
