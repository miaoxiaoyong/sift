package launchworker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TestLaunchWorkerWrapperCrashSuite deliberately uses the production assembly:
// a real SQLite DB, control-plane sockets, launch worker, compiled wrapper, and
// a real child process. The child is allowed to crash; the important assertion
// is that the durable handoff still has one owner and one spawn.
func TestLaunchWorkerWrapperCrashSuite(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	homePath, err := os.MkdirTemp("/tmp", "sift-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homePath) })
	home := config.Home{Path: homePath}
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	root := home.Path
	worktree := t.TempDir()
	marker := filepath.Join(root, "runs", "run-1", "attempts", "1", "agent-started")
	if err := db.SeedLaunchRunForTest(ctx, "run-1", "project", "cfg", now.UnixMilli(), worktree); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	attempts, pending, err := db.StartupRecoveryPending(ctx, boot)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if err := db.ApplyStartupRecoveryAction(ctx, storage.StartupRecoveryAction{BootID: boot, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: "fresh-attempt", Action: "supervise", NowMS: now.UnixMilli() + 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range pending {
		if err := db.ApplyStartupRecoveryAction(ctx, storage.StartupRecoveryAction{BootID: boot, OperationID: op.ID, ExpectedOperationVersion: op.Version, ObservationDigest: "fresh-launch", Action: "converge_operation", NowMS: now.UnixMilli() + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteStartupRecovery(ctx, boot, now.UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
	serverHome := config.Home{Path: filepath.Join(home.Path, "runs")}
	server, err := controlplane.Start(serverHome, db)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); _ = server.Close() }()
	go func() { _ = server.Serve(serveCtx) }()

	wrapperPath := buildE2EWrapper(t)
	backend := &execWrapperBackend{path: wrapperPath, pgid: true}
	defer backend.cleanup()
	worker := &Worker{
		DB: db, BootID: boot, WorkerID: "e2e-worker", Root: root, Lease: time.Minute,
		Backend: backend, Agents: []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "echo started >> $SIFT_RUN_DIR/agent-started; kill -KILL $$"}, TaskTransport: config.TaskTransportStdin}},
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("launch worker: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lines := countFileLines(marker); lines != 1 {
		t.Fatalf("controlled agent starts = %d, want 1", lines)
	}
	var phase, operationState string
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	waitForDurableLaunch(t, check)
	if err := check.QueryRow(`SELECT a.phase,o.state FROM attempts a JOIN outbox_operations o ON o.run_id=a.run_id WHERE a.run_id='run-1'`).Scan(&phase, &operationState); err != nil {
		t.Fatal(err)
	}
	if phase != "running" || operationState != string(storage.OperationSucceeded) {
		t.Fatalf("durable crash handoff = phase %q, operation %q", phase, operationState)
	}
	assertSingleLaunchOwner(t, check)
	// A replayed worker tick must not find a claim or create a second wrapper.
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("replayed launch worker: %v", err)
	}
	if lines := countFileLines(marker); lines != 1 {
		t.Fatalf("replayed controlled agent starts = %d, want 1", lines)
	}
}

func TestLaunchWorkerReclaimsPreparedBootstrapAfterCrashWindows(t *testing.T) {
	wrapperPath := buildE2EWrapper(t)
	for _, crash := range []struct {
		name  string
		hooks workerHooks
	}{
		{name: "after-rename", hooks: workerHooks{afterBootstrapWrite: injectedCrash}},
		{name: "after-digest", hooks: workerHooks{afterBootstrapDigest: injectedCrash}},
		{name: "before-spawn", hooks: workerHooks{beforeSpawn: injectedCrash}},
	} {
		t.Run(crash.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().Truncate(time.Millisecond)
			root, err := os.MkdirTemp("/tmp", "sift-reclaim-")
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
			boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			completeLaunchRecovery(t, db, boot, now.UnixMilli(), "supervise")

			worker := &Worker{DB: db, BootID: boot, WorkerID: "crashed-worker", Root: root, Lease: 10 * time.Millisecond, Now: func() time.Time { return now }, Backend: &execWrapperBackend{path: wrapperPath, pgid: true}, Agents: launchTestAgents(), hooks: crash.hooks}
			if err := worker.RunOnce(ctx); !errors.Is(err, errInjectedCrash) {
				t.Fatalf("crashed worker = %v, want injected crash", err)
			}

			restartedAt := now.Add(20 * time.Millisecond)
			restartedBoot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), restartedAt.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			completeLaunchRecovery(t, db, restartedBoot, restartedAt.UnixMilli(), "reuse_dispatch")
			server, err := controlplane.Start(config.Home{Path: filepath.Join(root, "runs")}, db)
			if err != nil {
				t.Fatal(err)
			}
			serveCtx, cancel := context.WithCancel(ctx)
			defer func() { cancel(); _ = server.Close() }()
			go func() { _ = server.Serve(serveCtx) }()

			backend := &countingBackend{backend: &execWrapperBackend{path: wrapperPath, pgid: true}}
			defer backend.cleanup()
			worker = &Worker{DB: db, BootID: restartedBoot, WorkerID: "reclaimed-worker", Root: root, Lease: time.Minute, Now: func() time.Time { return restartedAt.Add(time.Millisecond) }, Backend: backend, Agents: launchTestAgents()}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatalf("reclaimed worker: %v", err)
			}
			marker := filepath.Join(root, "runs", "run-1", "attempts", "1", "agent-started")
			waitForLines(t, marker, 1)
			if backend.spawns != 1 {
				t.Fatalf("wrappers spawned = %d, want 1", backend.spawns)
			}
			if lines := countFileLines(marker); lines != 1 {
				t.Fatalf("agents started = %d, want 1", lines)
			}
			check, err := sql.Open("sqlite", db.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			assertSingleLaunchOwner(t, check)
		})
	}
}

func completeLaunchRecovery(t *testing.T, db *storage.DB, boot string, nowMS int64, attemptAction string) {
	t.Helper()
	attempts, operations, err := db.StartupRecoveryPending(context.Background(), boot)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if err := db.ApplyStartupRecoveryAction(context.Background(), storage.StartupRecoveryAction{BootID: boot, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: attemptAction, Action: attemptAction, NowMS: nowMS}); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range operations {
		if err := db.ApplyStartupRecoveryAction(context.Background(), storage.StartupRecoveryAction{BootID: boot, OperationID: operation.ID, ExpectedOperationVersion: operation.Version, ObservationDigest: "converge", Action: "converge_operation", NowMS: nowMS}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteStartupRecovery(context.Background(), boot, nowMS); err != nil {
		t.Fatal(err)
	}
}

var errInjectedCrash = errors.New("injected crash")

func injectedCrash() error { return errInjectedCrash }

func launchTestAgents() []config.Agent {
	return []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "echo started >> $SIFT_RUN_DIR/agent-started"}, TaskTransport: config.TaskTransportStdin}}
}

func assertSingleLaunchOwner(t *testing.T, db *sql.DB) {
	t.Helper()
	var owners int
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run-1' AND wrapper_instance_id IS NOT NULL`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Fatalf("persisted launch owners = %d, want 1", owners)
	}
	var dispatches int
	if err := db.QueryRow(`SELECT count(*) FROM attempt_claims WHERE run_id='run-1' AND dispatch_id IS NOT NULL`).Scan(&dispatches); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("persisted dispatches = %d, want 1", dispatches)
	}
}

func waitForDurableLaunch(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var phase, state string
		if err := db.QueryRow(`SELECT a.phase,o.state FROM attempts a JOIN outbox_operations o ON o.run_id=a.run_id WHERE a.run_id='run-1'`).Scan(&phase, &state); err == nil && phase == "running" && state == string(storage.OperationSucceeded) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("launch did not reach durable running state")
}

func waitForLines(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countFileLines(path) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lines in %s = %d, want %d", path, countFileLines(path), want)
}

type countingBackend struct {
	backend *execWrapperBackend
	spawns  int
}

func (b *countingBackend) Spawn(ctx context.Context, bootstrap string) (*os.Process, error) {
	b.spawns++
	return b.backend.Spawn(ctx, bootstrap)
}

func (b *countingBackend) cleanup() { b.backend.cleanup() }

type execWrapperBackend struct {
	path    string
	pgid    bool
	process *os.Process
}

func (b *execWrapperBackend) Spawn(ctx context.Context, bootstrap string) (*os.Process, error) {
	cmd := osexec.CommandContext(ctx, b.path, bootstrap)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if b.pgid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start wrapper: %w", err)
	}
	b.process = cmd.Process
	return cmd.Process, nil
}

func (b *execWrapperBackend) cleanup() {
	if b.process == nil {
		return
	}
	_ = b.process.Kill()
	_, _ = b.process.Wait()
}

func buildE2EWrapper(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	path := filepath.Join(t.TempDir(), "sift-agent-wrapper")
	cmd := osexec.Command("go", "build", "-o", path, "./cmd/sift-agent-wrapper")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wrapper: %v\n%s", err, output)
	}
	return path
}

func countFileLines(path string) int {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		return -1
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}
