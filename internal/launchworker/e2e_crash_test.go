package launchworker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
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

	// The agent writes its marker asynchronously after the worker has spawned
	// the wrapper; wait for the durable side effect rather than sampling it.
	waitForLines(t, marker, 1)
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

// TestLaunchWorkerKilledAtHandoffBoundaries kills a real launch-worker test
// process at each durable handoff boundary, then starts a new daemon boot and
// worker. It intentionally does not turn a hook error into a simulated crash.
func TestLaunchWorkerKilledAtHandoffBoundaries(t *testing.T) {
	wrapperPath := buildE2EWrapper(t)
	for _, point := range []struct {
		name, recovery string
	}{
		{"prepare", "redispatch"},
		{"rename", "reuse_dispatch"},
		{"digest", "reuse_dispatch"},
		{"spawn", "reuse_dispatch"},
	} {
		t.Run(point.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().Truncate(time.Millisecond)
			root, err := os.MkdirTemp("/tmp", "sift-kill-")
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

			ready := filepath.Join(root, "worker-ready")
			worker := osexec.Command(os.Args[0], "-test.run=^TestLaunchWorkerProcessHelper$")
			worker.Env = append(os.Environ(), "SIFT_LAUNCH_HELPER=1", "SIFT_LAUNCH_ROOT="+root, "SIFT_LAUNCH_BOOT="+boot, "SIFT_LAUNCH_WRAPPER="+wrapperPath, "SIFT_LAUNCH_POINT="+point.name, "SIFT_LAUNCH_READY="+ready, fmt.Sprintf("SIFT_LAUNCH_NOW=%d", now.UnixMilli()))
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, ready)
			if err := worker.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := worker.Wait(); err == nil {
				t.Fatal("killed launch worker unexpectedly succeeded")
			}

			restartedAt := now.Add(time.Second)
			restartedBoot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), restartedAt.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			completeLaunchRecovery(t, db, restartedBoot, restartedAt.UnixMilli(), point.recovery)
			server, err := controlplane.Start(config.Home{Path: filepath.Join(root, "runs")}, db)
			if err != nil {
				t.Fatal(err)
			}
			serveCtx, cancel := context.WithCancel(ctx)
			defer func() { cancel(); _ = server.Close() }()
			go func() { _ = server.Serve(serveCtx) }()

			backend := &countingBackend{backend: &execWrapperBackend{path: wrapperPath, pgid: true}}
			defer backend.cleanup()
			restarted := &Worker{DB: db, BootID: restartedBoot, WorkerID: "restarted-worker", Root: root, Lease: time.Minute, Now: func() time.Time { return restartedAt.Add(time.Millisecond) }, Backend: backend, Agents: launchTestAgents()}
			if err := restarted.RunOnce(ctx); err != nil {
				t.Fatalf("restarted worker: %v", err)
			}
			marker := filepath.Join(root, "runs", "run-1", "attempts", "1", "agent-started")
			waitForLines(t, marker, 1)
			if backend.spawns != 1 {
				t.Fatalf("restarted wrapper spawns = %d, want 1", backend.spawns)
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

// TestLaunchWorkerKilledAfterRealWrapperSpawn covers crash windows which begin
// only after Backend.Spawn has returned a real wrapper process. Each replacement
// boot sees both the durable owner and the OS process-group interval before it
// is allowed to consume launch work.
func TestLaunchWorkerKilledAfterRealWrapperSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	wrapperPath := buildE2EWrapper(t)
	for _, tc := range []struct {
		name, executable, script string
		wait                     func(t *testing.T, root string, db *sql.DB)
		killOuter                bool
	}{
		{"control-initial-write", "/bin/sh", "while :; do sleep 1; done", waitForInitialControl, false},
		{"control-agent-rewrite", "/bin/sh", "while :; do sleep 1; done", waitForAgentControl, false},
		{"claim-started", "/bin/sh", "while :; do sleep 1; done", waitForStartedClaim, false},
		{"result-json", "/bin/sh", "sleep 1; exit 0", waitForResult, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().Truncate(time.Millisecond)
			root, err := os.MkdirTemp("/tmp", "sift-post-spawn-")
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
			server, err := controlplane.Start(config.Home{Path: filepath.Join(root, "runs")}, db)
			if err != nil {
				t.Fatal(err)
			}
			serveCtx, cancel := context.WithCancel(ctx)
			defer func() { cancel(); _ = server.Close() }()
			go func() { _ = server.Serve(serveCtx) }()

			ready, spawns := filepath.Join(root, "post-spawn-ready"), filepath.Join(root, "wrapper-spawns")
			worker := osexec.Command(os.Args[0], "-test.run=^TestLaunchWorkerProcessHelper$")
			worker.Env = append(os.Environ(), "SIFT_LAUNCH_HELPER=1", "SIFT_LAUNCH_POST_SPAWN=1", "SIFT_LAUNCH_ROOT="+root, "SIFT_LAUNCH_BOOT="+boot, "SIFT_LAUNCH_WRAPPER="+wrapperPath, "SIFT_LAUNCH_READY="+ready, "SIFT_LAUNCH_SPAWN_LOG="+spawns, "SIFT_LAUNCH_AGENT="+tc.executable, "SIFT_LAUNCH_AGENT_SCRIPT="+tc.script, fmt.Sprintf("SIFT_LAUNCH_NOW=%d", now.UnixMilli()))
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = worker.Process.Kill(); _, _ = worker.Process.Wait() }()
			waitForFile(t, ready)
			outerPID := 0

			check, err := sql.Open("sqlite", db.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			if tc.killOuter {
				// Let the execution wrapper pass its control rewrite before pausing
				// its outer supervisor; the child can then publish result.json.
				waitForAgentControl(t, root, check)
				outerPID = spawnedWrapperPID(t, spawns)
				if err := syscall.Kill(outerPID, syscall.SIGSTOP); err != nil {
					t.Fatalf("pause outer wrapper before result: %v", err)
				}
			}
			tc.wait(t, root, check)
			pid, pgid := persistedWrapperIdentity(t, check)
			if !tc.killOuter {
				if got, err := syscall.Getpgid(pid); err != nil || got != pgid {
					t.Fatalf("persisted wrapper identity pid=%d pgid=%d, observed pgid=%d err=%v", pid, pgid, got, err)
				}
			}
			if tc.name == "control-agent-rewrite" {
				// The old worker is paused in Backend.Spawn while its wrapper and
				// process group remain live. A new boot and worker must not overlap it.
				pausedBoot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.Add(500*time.Millisecond).UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				completeLaunchRecovery(t, db, pausedBoot, now.Add(500*time.Millisecond).UnixMilli(), "supervise")
				candidate := &countingBackend{backend: &execWrapperBackend{path: wrapperPath, pgid: true}}
				if err := (&Worker{DB: db, BootID: pausedBoot, WorkerID: "paused-owner-candidate", Root: root, Lease: time.Minute, Backend: candidate, Agents: launchTestAgents()}).RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if candidate.spawns != 0 || countFileLines(spawns) != 1 {
					t.Fatalf("new owner overlapped paused owner: spawns=%d/%d", countFileLines(spawns), candidate.spawns)
				}
			}
			if tc.killOuter {
				if err := syscall.Kill(outerPID, syscall.SIGKILL); err != nil {
					t.Fatalf("kill paused outer wrapper after result: %v", err)
				}
			}
			if !tc.killOuter {
				if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.killOuter {
				waitForGroupAbsent(t, pgid)
			}
			if tc.killOuter {
				// The recorded execution group completed before result.json; the
				// paused supervisor is the real post-result process killed above.
				waitForGroupAbsent(t, pgid)
			}
			if err := worker.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_, _ = worker.Process.Wait()

			// A restarted worker is an attempted new owner. There is no spawn
			// permit/claim left to consume after the old wrapper interval ended.
			restartedAt := now.Add(time.Second)
			restartedBoot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), restartedAt.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			completeLaunchRecovery(t, db, restartedBoot, restartedAt.UnixMilli(), "supervise")
			backend := &countingBackend{backend: &execWrapperBackend{path: wrapperPath, pgid: true}}
			restarted := &Worker{DB: db, BootID: restartedBoot, WorkerID: "replacement", Root: root, Lease: time.Minute, Backend: backend, Agents: launchTestAgents()}
			if err := restarted.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if backend.spawns != 0 || countFileLines(spawns) != 1 {
				t.Fatalf("spawn interval count = persisted %d, replacement %d; want one old owner only", countFileLines(spawns), backend.spawns)
			}
			assertSingleLaunchOwner(t, check)
		})
	}
}

func spawnedWrapperPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("wrapper spawn PID %q: %v", data, err)
	}
	return pid
}

func persistedWrapperIdentity(t *testing.T, db *sql.DB) (int, int) {
	t.Helper()
	var pid, pgid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT wrapper_pid,wrapper_pgid FROM attempts WHERE run_id='run-1'`).Scan(&pid, &pgid); err == nil && pid > 0 && pgid > 0 {
			return pid, pgid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wrapper identity was not durably projected")
	return 0, 0
}

func waitForInitialControl(t *testing.T, root string, _ *sql.DB) {
	t.Helper()
	waitForFile(t, filepath.Join(root, "runs", "run-1", "attempts", "1", "control.json"))
}

func waitForAgentControl(t *testing.T, root string, _ *sql.DB) {
	t.Helper()
	path := filepath.Join(root, "runs", "run-1", "attempts", "1", "control.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var control struct {
			Agent json.RawMessage `json:"agent_identity"`
		}
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &control) == nil && string(control.Agent) != "null" && len(control.Agent) != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("control.json was not rewritten with agent identity")
}

func waitForStartedClaim(t *testing.T, _ string, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var phase string
		if err := db.QueryRow(`SELECT phase FROM attempts WHERE run_id='run-1'`).Scan(&phase); err == nil && phase == "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("claim.started was not durably projected")
}

func waitForResult(t *testing.T, root string, _ *sql.DB) {
	t.Helper()
	waitForFile(t, filepath.Join(root, "runs", "run-1", "attempts", "1", "result.json"))
}

func waitForGroupAbsent(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("old wrapper process group %d remains live", pgid)
}

// TestLaunchWorkerProcessHelper is only invoked in a child test binary. The
// callback publishes a synchronous boundary and waits to be SIGKILLed.
func TestLaunchWorkerProcessHelper(t *testing.T) {
	if os.Getenv("SIFT_LAUNCH_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(os.Getenv("SIFT_LAUNCH_ROOT"), "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pause := func() error {
		if err := os.WriteFile(os.Getenv("SIFT_LAUNCH_READY"), []byte("ready"), 0600); err != nil {
			return err
		}
		select {}
	}
	hooks := workerHooks{}
	if os.Getenv("SIFT_LAUNCH_POST_SPAWN") != "1" {
		switch os.Getenv("SIFT_LAUNCH_POINT") {
		case "prepare":
			hooks.afterPrepare = pause
		case "rename":
			hooks.afterBootstrapWrite = pause
		case "digest":
			hooks.afterBootstrapDigest = pause
		case "spawn":
			hooks.beforeSpawn = pause
		default:
			t.Fatal("unknown launch helper boundary")
		}
	}
	nowMS, err := strconv.ParseInt(os.Getenv("SIFT_LAUNCH_NOW"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	backend := &execWrapperBackend{path: os.Getenv("SIFT_LAUNCH_WRAPPER"), pgid: true}
	agents := launchTestAgents()
	if os.Getenv("SIFT_LAUNCH_POST_SPAWN") == "1" {
		backend.ready = os.Getenv("SIFT_LAUNCH_READY")
		backend.spawnLog = os.Getenv("SIFT_LAUNCH_SPAWN_LOG")
		agents = []config.Agent{{ID: "agent", Executable: os.Getenv("SIFT_LAUNCH_AGENT"), Args: []string{"-c", os.Getenv("SIFT_LAUNCH_AGENT_SCRIPT")}, TaskTransport: config.TaskTransportStdin}}
	}
	worker := &Worker{DB: db, BootID: os.Getenv("SIFT_LAUNCH_BOOT"), WorkerID: "killed-worker", Root: os.Getenv("SIFT_LAUNCH_ROOT"), Lease: 10 * time.Millisecond, Now: func() time.Time { return time.UnixMilli(nowMS) }, Backend: backend, Agents: agents, hooks: hooks}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	t.Fatal("launch worker passed kill boundary")
}

func completeLaunchRecovery(t *testing.T, db *storage.DB, boot string, nowMS int64, attemptAction string) {
	t.Helper()
	// redispatch creates a replacement operation, which needs its own receipt
	// before the boot barrier can open.
	for i := 0; i < 3; i++ {
		attempts, operations, err := db.StartupRecoveryPending(context.Background(), boot)
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) == 0 && len(operations) == 0 {
			break
		}
		for _, attempt := range attempts {
			if err := db.ApplyStartupRecoveryAction(context.Background(), storage.StartupRecoveryAction{BootID: boot, RunID: attempt.RunID, AttemptNo: attempt.AttemptNo, ExpectedGeneration: attempt.Generation, ObservationDigest: attemptAction, Action: attemptAction, NowMS: nowMS}); err != nil {
				t.Fatal(err)
			}
		}
		for _, operation := range operations {
			if err := db.ApplyStartupRecoveryAction(context.Background(), storage.StartupRecoveryAction{BootID: boot, OperationID: operation.ID, ExpectedOperationVersion: operation.Version, ObservationDigest: "converge", Action: "converge_operation", NowMS: nowMS}); err != nil && !errors.Is(err, storage.ErrRejectedStale) {
				t.Fatal(err)
			}
		}
	}
	if err := db.CompleteStartupRecovery(context.Background(), boot, nowMS); err != nil {
		t.Fatal(err)
	}
}

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

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
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

func (b *countingBackend) WrapperPath() string { return b.backend.WrapperPath() }

func (b *countingBackend) Spawn(ctx context.Context, launch runtimepkg.HostLaunch) (*os.Process, error) {
	b.spawns++
	return b.backend.Spawn(ctx, launch)
}

func (b *countingBackend) cleanup() { b.backend.cleanup() }

type execWrapperBackend struct {
	path     string
	pgid     bool
	process  *os.Process
	ready    string
	spawnLog string
}

func (b *execWrapperBackend) WrapperPath() string { return b.path }

func (b *execWrapperBackend) Spawn(ctx context.Context, launch runtimepkg.HostLaunch) (*os.Process, error) {
	cmd := osexec.CommandContext(ctx, b.path, launch.BootstrapPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if b.pgid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start wrapper: %w", err)
	}
	b.process = cmd.Process
	if b.spawnLog != "" {
		if err := appendLine(b.spawnLog, strconv.Itoa(cmd.Process.Pid)); err != nil {
			return nil, err
		}
	}
	if b.ready != "" {
		if err := os.WriteFile(b.ready, []byte("ready"), 0600); err != nil {
			return nil, err
		}
	}
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
	cmd := osexec.Command("go", "build", "-tags", "sift_test", "-o", path, "./cmd/sift-agent-wrapper")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wrapper: %v\n%s", err, output)
	}
	return path
}

func appendLine(path, value string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, value)
	return err
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
