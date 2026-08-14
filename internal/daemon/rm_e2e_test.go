package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/controlplane"
	"github.com/xsift/sift/internal/storage"
)

// startServerWithDB opens a real control-plane server bound to a unix socket
// over a fresh DB, serving in the background. It mirrors how daemon.go wires
// the operator action so ops.rm exercises the real handler.
func startServerWithDB(t *testing.T, operations func(context.Context, string, string, int64) error) (*controlplane.Server, *storage.DB, *sql.DB, config.Home) {
	t.Helper()
	ctx := context.Background()
	now := time.UnixMilli(60_000)
	homeDir, err := os.MkdirTemp("", "sift-rm")
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
	server, err := controlplane.Start(home, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if operations != nil {
		server.SetOperatorAction(operations)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = server.Serve(serveCtx) }()
	return server, db, raw, home
}

func rmSeedRun(t *testing.T, db *storage.DB, raw *sql.DB, runID, status string) {
	t.Helper()
	ctx := context.Background()
	now := int64(60_000)
	if err := db.SeedProjectForTest(ctx, "cfg-"+runID, "proj-"+runID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, runID, "proj-"+runID, "cfg-"+runID, "issue-"+runID, now); err != nil {
		t.Fatal(err)
	}
	switch status {
	case "failed":
		if _, err := raw.Exec(`UPDATE runs SET status='failed', failure_reason='agent_exit', completed_at_ms=?, version=2 WHERE id=?`, now, runID); err != nil {
			t.Fatal(err)
		}
	case "running":
		if _, err := raw.Exec(`UPDATE runs SET status='running', version=2 WHERE id=?`, runID); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOpsRmArchivesTerminalRun: rm on a failed run archives it (hides from ps)
// while keeping the run row and its data.
func TestOpsRmArchivesTerminalRun(t *testing.T) {
	_, db, raw, home := startServerWithDB(t, nil)
	rmSeedRun(t, db, raw, "runFail", "failed")

	resp, err := controlplane.OperatorRequest(home, "ops.rm", map[string]any{"run_id": "runFail", "expected_version": float64(2), "force": false})
	if err != nil {
		t.Fatalf("ops.rm: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ops.rm terminal not ok: %+v", resp.Error)
	}
	var archived int64
	if err := raw.QueryRow(`SELECT archived_at_ms FROM runs WHERE id='runFail'`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived == 0 {
		t.Fatalf("terminal run was not archived")
	}
	// RunPS hides it.
	report, err := db.RunPS(context.Background(), storage.PSQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report.Runs {
		if r.RunID == "runFail" {
			t.Fatalf("archived run still shown by RunPS")
		}
	}
}

// TestOpsRmActiveRequiresForce: rm on a running run without --force is rejected
// as conflict and does not archive.
func TestOpsRmActiveRequiresForce(t *testing.T) {
	_, db, raw, home := startServerWithDB(t, nil)
	rmSeedRun(t, db, raw, "runAct", "running")

	resp, err := controlplane.OperatorRequest(home, "ops.rm", map[string]any{"run_id": "runAct", "expected_version": float64(2), "force": false})
	if err != nil {
		t.Fatalf("ops.rm: %v", err)
	}
	if resp.OK || resp.Error.Code != "conflict" {
		t.Fatalf("ops.rm active no-force = ok=%v code=%s, want conflict", resp.OK, codeOr(resp))
	}
	var archived any
	if err := raw.QueryRow(`SELECT archived_at_ms FROM runs WHERE id='runAct'`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != nil {
		t.Fatalf("active run was archived without --force")
	}
}

// TestOpsRmForceTerminatesThenArchives: rm --force on a running run terminates
// (via the wired operator action) then archives.
func TestOpsRmForceTerminatesThenArchives(t *testing.T) {
	terminated := false
	var rawRef *sql.DB // assigned after startServerWithDB returns; the RPC fires later
	_, db, raw, home := startServerWithDB(t, func(ctx context.Context, method, runID string, version int64) error {
		if method != "ops.kill" || runID != "runForce" {
			t.Errorf("operator action = %s/%s, want ops.kill/runForce", method, runID)
		}
		terminated = true
		// Simulate the kill transitioning the run to failed.
		if _, err := rawRef.Exec(`UPDATE runs SET status='failed', failure_reason='operator_kill', completed_at_ms=60000 WHERE id='runForce'`); err != nil {
			t.Errorf("simulate kill: %v", err)
		}
		return nil
	})
	rawRef = raw
	rmSeedRun(t, db, raw, "runForce", "running")

	resp, err := controlplane.OperatorRequest(home, "ops.rm", map[string]any{"run_id": "runForce", "expected_version": float64(2), "force": true})
	if err != nil {
		t.Fatalf("ops.rm: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ops.rm --force not ok: %+v", resp.Error)
	}
	if !terminated {
		t.Fatalf("force rm did not terminate the run")
	}
	var archived int64
	if err := raw.QueryRow(`SELECT archived_at_ms FROM runs WHERE id='runForce'`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived == 0 {
		t.Fatalf("force rm did not archive the run")
	}
}

func codeOr(resp controlplane.Response) string {
	if resp.Error != nil {
		return resp.Error.Code
	}
	return ""
}
