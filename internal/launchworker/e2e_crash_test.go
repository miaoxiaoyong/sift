package launchworker

import (
	"context"
	"database/sql"
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
	backend := execWrapperBackend{path: wrapperPath, pgid: true}
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
	if err := check.QueryRow(`SELECT a.phase,o.state FROM attempts a JOIN outbox_operations o ON o.run_id=a.run_id WHERE a.run_id='run-1'`).Scan(&phase, &operationState); err != nil {
		t.Fatal(err)
	}
	if phase != "running" || operationState != string(storage.OperationSucceeded) {
		t.Fatalf("durable crash handoff = phase %q, operation %q", phase, operationState)
	}
	// A replayed worker tick must not find a claim or create a second wrapper.
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("replayed launch worker: %v", err)
	}
	if lines := countFileLines(marker); lines != 1 {
		t.Fatalf("replayed controlled agent starts = %d, want 1", lines)
	}
}

type execWrapperBackend struct {
	path string
	pgid bool
}

func (b execWrapperBackend) Spawn(ctx context.Context, bootstrap string) (*os.Process, error) {
	cmd := osexec.CommandContext(ctx, b.path, bootstrap)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if b.pgid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start wrapper: %w", err)
	}
	return cmd.Process, nil
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
