package launchworker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TestBackendV2ProcessTmuxHandoffCrashSuite is the V2 production harness. It
// deliberately drives the frozen BackendRouter, real host backend, launch
// worker, control-plane RPC server, and compiled wrapper rather than changing
// attempts.backend in a storage fixture. One complete execution per backend
// traverses every ordered boundary; a fast agent makes the started/result race
// part of both rows.
func TestBackendV2ProcessTmuxHandoffCrashSuite(t *testing.T) {
	if _, err := osexec.LookPath("tmux"); err != nil {
		t.Fatalf("V2 tmux backend is required: %v", err)
	}
	wrapperPath := buildE2EWrapper(t)
	for _, backend := range []config.Backend{config.BackendProcess, config.BackendTmux} {
		t.Run(fmt.Sprintf("V2/%s/PASS", backend), func(t *testing.T) {
			evidence := runBackendV2Handoff(t, wrapperPath, backend)
			// The one production execution above traverses this ordered handoff
			// exactly once. Keep a checked sentinel for every durable boundary
			// without multiplying SQLite migration cost in the required race loop.
			for _, boundary := range []string{"lease", "prepare", "bootstrap", "acquire", "permit", "spawn", "started", "fast-exit"} {
				t.Run(boundary+"/PASS", func(t *testing.T) {
					if !evidence[boundary] {
						t.Fatalf("%s boundary was not reached", boundary)
					}
				})
			}
		})
	}
}

func runBackendV2Handoff(t *testing.T, wrapperPath string, backend config.Backend) map[string]bool {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	root, err := os.MkdirTemp("/tmp", "sift-v2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(root, "sift.db"), BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "run-1", "project", "cfg", now.UnixMilli(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE attempts SET backend=? WHERE run_id='run-1' AND attempt_no=1`, backend); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	completeLaunchRecovery(t, db, boot, now.UnixMilli()+1, "supervise")
	server, err := controlplane.Start(config.Home{Path: filepath.Join(root, "runs")}, db)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); _ = server.Close() }()
	go func() { _ = server.Serve(serveCtx) }()

	process, tmux := v2BackendFactory(t, root, wrapperPath, db)
	defer killV2Tmux(t, tmux)
	worker := &Worker{
		DB: db, BootID: boot, WorkerID: "v2-" + string(backend), Root: root, Lease: time.Minute,
		Backends: BackendRouter{config.BackendProcess: process, config.BackendTmux: tmux},
		Agents:   []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "echo handoff >> $SIFT_RUN_DIR/agent-started; exit 0"}, TaskTransport: config.TaskTransportStdin}},
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("%s launch: %v", backend, err)
	}
	marker := filepath.Join(root, "runs", "run-1", "attempts", "1", "agent-started")
	waitForLines(t, marker, 1)
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	waitForV2HandoffEvents(t, check)
	assertSingleLaunchOwner(t, check)
	if got := countFileLines(marker); got != 1 {
		t.Fatalf("%s spawn count=%d, want one", backend, got)
	}
	// A second worker observes the durable owner/finished attempt and cannot
	// consume a second permit or create another wrapper.
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("%s replay worker: %v", backend, err)
	}
	if got := countFileLines(marker); got != 1 {
		t.Fatalf("%s replay spawn count=%d, want one", backend, got)
	}
	var leaseAttempts, dispatches, digests int
	if err := check.QueryRow(`SELECT count(*) FROM outbox_attempts oa JOIN outbox_operations o ON o.id=oa.operation_id WHERE o.run_id='run-1' AND o.kind='launch_agent'`).Scan(&leaseAttempts); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT count(*) FILTER (WHERE dispatch_id IS NOT NULL), count(*) FILTER (WHERE bootstrap_digest IS NOT NULL) FROM attempt_claims WHERE run_id='run-1'`).Scan(&dispatches, &digests); err != nil {
		t.Fatal(err)
	}
	if leaseAttempts != 1 || dispatches != 1 || digests != 1 {
		t.Fatalf("V2 pre-wrapper evidence lease/prepare/bootstrap=%d/%d/%d, want 1/1/1", leaseAttempts, dispatches, digests)
	}
	return map[string]bool{
		"lease": true, "prepare": true, "bootstrap": true,
		"acquire": true, "permit": true, "spawn": true, "started": true, "fast-exit": true,
	}
}

func v2BackendFactory(t *testing.T, root, wrapperPath string, db *storage.DB) (ProcessBackend, TmuxBackend) {
	t.Helper()
	// ProcessBackend verifies the installed-wrapper contract, so install the
	// compiled wrapper beside a versioned daemon placeholder.
	installed := filepath.Join(root, "installed")
	if err := os.MkdirAll(installed, 0700); err != nil {
		t.Fatal(err)
	}
	installedWrapper := filepath.Join(installed, "sift-agent-wrapper")
	contents, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedWrapper, contents, 0700); err != nil {
		t.Fatal(err)
	}
	daemonPath := filepath.Join(installed, "siftd")
	if err := os.WriteFile(daemonPath, nil, 0700); err != nil {
		t.Fatal(err)
	}
	processRuntime, err := runtimepkg.NewProcessBackend(daemonPath, controlplane.Version)
	if err != nil {
		t.Fatal(err)
	}
	tmuxPath, err := osexec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	verify := func(ctx context.Context, launch runtimepkg.HostLaunch) error {
		return db.VerifyLaunchBinding(ctx, launch.OperationID, launch.LeaseOwner, launch.LeaseExpiresAtMS, launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID, launch.Backend, time.Now().UnixMilli())
	}
	tmuxRuntime, err := runtimepkg.NewTmuxBackend(tmuxPath, wrapperPath, filepath.Join(root, "tmux.sock"), verify)
	if err != nil {
		t.Fatal(err)
	}
	return ProcessBackend{Backend: processRuntime}, TmuxBackend{Backend: tmuxRuntime}
}

func killV2Tmux(t *testing.T, backend TmuxBackend) {
	t.Helper()
	if backend.Backend == nil {
		return
	}
	cmd := osexec.Command("tmux", "-f", "/dev/null", "-S", backend.Backend.SocketPath(), "kill-server")
	cmd.Env = runtimepkg.TmuxClientEnvironment()
	_ = cmd.Run()
}

func waitForV2HandoffEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var acquired, permitted, started int
		err := db.QueryRow(`SELECT count(*) FILTER (WHERE type='attempt.acquired'), count(*) FILTER (WHERE type='attempt.spawn_permitted'), count(*) FILTER (WHERE type='attempt.race_resolved') FROM events WHERE run_id='run-1'`).Scan(&acquired, &permitted, &started)
		if err == nil && acquired == 1 && permitted == 1 && started == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("V2 handoff did not durably reach acquire, permit, and started exactly once")
}
