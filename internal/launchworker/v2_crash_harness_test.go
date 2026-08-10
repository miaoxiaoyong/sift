package launchworker

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TestBackendV2CrashHarness is the dual-backend V2 crash suite. Every
// backend×boundary cell injects a real crash — a SIGKILLed launch-worker
// process at the lease/prepare/bootstrap boundaries, a SIGKILLed wrapper
// process group at the acquire/permit/spawn/started boundaries, and a real
// Agent crash at fast-exit — then drives a replacement boot and asserts
// durable convergence. No cell is a happy-path sentinel and none may skip.
func TestBackendV2CrashHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("V2 crash harness requires Unix process groups")
	}
	enableTestChildSubreaper(t)
	if _, err := osexec.LookPath("tmux"); err != nil {
		t.Fatalf("V2 tmux backend is required: %v", err)
	}
	wrapperPath := buildE2EWrapper(t)
	for _, backend := range []config.Backend{config.BackendProcess, config.BackendTmux} {
		for _, point := range []string{"lease", "prepare", "bootstrap"} {
			t.Run(fmt.Sprintf("V2/%s/%s", backend, point), func(t *testing.T) {
				runV2WorkerCrash(t, wrapperPath, backend, point)
			})
		}
		for _, point := range []string{"acquire", "permit", "spawn", "started"} {
			t.Run(fmt.Sprintf("V2/%s/%s", backend, point), func(t *testing.T) {
				runV2WrapperCrash(t, wrapperPath, backend, point)
			})
		}
		t.Run(fmt.Sprintf("V2/%s/fast-exit", backend), func(t *testing.T) {
			runV2FastExitCrash(t, wrapperPath, backend)
		})
	}
}

// v2Template is a process-wide migrated database image. Under -race a full
// Open+migrate costs seconds per cell, which makes the required -count=10
// harness runs infeasible; cloning a checkpointed template keeps every cell
// on an independent real database.
var v2Template struct {
	once sync.Once
	path string
	err  error
}

func v2CloneTemplateDB(t *testing.T, ctx context.Context, path string, now time.Time) *storage.DB {
	t.Helper()
	v2Template.once.Do(func() {
		dir, err := os.MkdirTemp("", "sift-v2-template-")
		if err != nil {
			v2Template.err = err
			return
		}
		template := filepath.Join(dir, "sift.db")
		db, err := storage.Open(ctx, storage.OpenConfig{Path: template, BinaryVersion: controlplane.Version, Now: now})
		if err != nil {
			v2Template.err = err
			return
		}
		if _, err := db.ExecForTest(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			_ = db.Close()
			v2Template.err = err
			return
		}
		if err := db.Close(); err != nil {
			v2Template.err = err
			return
		}
		v2Template.path = template
	})
	if v2Template.err != nil {
		t.Fatalf("V2 crash template: %v", v2Template.err)
	}
	data, err := os.ReadFile(v2Template.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, storage.OpenConfig{Path: path, BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		t.Fatalf("Open cloned V2 template: %v", err)
	}
	return db
}

// v2Cell is one isolated crash cell: real DB, boot, control-plane server.
type v2Cell struct {
	ctx      context.Context
	root     string
	db       *storage.DB
	check    *sql.DB
	boot     string
	worktree string
	runDir   string
	marker   string
}

func newV2Cell(t *testing.T, backend config.Backend) *v2Cell {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	root, err := os.MkdirTemp("/tmp", "sift-v2c-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	db := v2CloneTemplateDB(t, ctx, filepath.Join(root, "sift.db"), now)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	if err := db.SeedLaunchRunForTest(ctx, "run-1", "project", "cfg", now.UnixMilli(), worktree); err != nil {
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
	server, err := controlplane.Start(config.Home{Path: root}, db)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() { cancel(); _ = server.Close() })
	go func() { _ = server.Serve(serveCtx) }()
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = check.Close() })
	runDir := filepath.Join(root, "runs", "run-1", "attempts", "1")
	return &v2Cell{ctx: ctx, root: root, db: db, check: check, boot: boot, worktree: worktree, runDir: runDir, marker: filepath.Join(runDir, "agent-started")}
}

func (c *v2Cell) restartBoot(t *testing.T, attemptAction string) string {
	t.Helper()
	boot, err := c.db.StartDaemonBoot(c.ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	completeLaunchRecovery(t, c.db, boot, time.Now().UnixMilli(), attemptAction)
	return boot
}

// runV2WorkerCrash kills a real launch-worker helper process while it is
// parked on the named durable boundary, then proves a replacement boot and
// worker converge the launch exactly once.
func runV2WorkerCrash(t *testing.T, wrapperPath string, backend config.Backend, point string) {
	cell := newV2Cell(t, backend)
	ready := filepath.Join(cell.root, "worker-ready")
	workerNow := time.Now().UnixMilli()
	helper := osexec.Command(os.Args[0], "-test.run=^TestV2CrashWorkerHelper$")
	helper.Env = append(os.Environ(),
		"SIFT_V2_HELPER=1", "SIFT_V2_ROOT="+cell.root, "SIFT_V2_BOOT="+cell.boot,
		"SIFT_V2_POINT="+point, "SIFT_V2_WRAPPER="+wrapperPath, "SIFT_V2_READY="+ready,
		"SIFT_V2_NOW="+strconv.FormatInt(workerNow, 10))
	var output strings.Builder
	helper.Stdout, helper.Stderr = &output, &output
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatalf("killed %s worker unexpectedly succeeded: %s", point, output.String())
	}

	action := "redispatch" // lease/prepare: nothing reusable survived the kill.
	if point == "bootstrap" {
		action = "reuse_dispatch" // validated bootstrap + digest survived.
	}
	boot := cell.restartBoot(t, action)
	router, cleanup := v2BackendFactory(t, cell.root, wrapperPath, cell.db)
	defer cleanup()
	spawns := 0
	rec := &recordingHost{inner: router[backend], spawns: &spawns}
	router[backend] = rec
	worker := &Worker{DB: cell.db, BootID: boot, WorkerID: "v2-replacement", Root: cell.root, Lease: time.Minute, Backends: router, Agents: launchTestAgents()}
	if point == "bootstrap" {
		// The replacement reclaims the dead worker's executing operation only
		// after its short helper lease expires.
		time.Sleep(200 * time.Millisecond)
	}
	if err := worker.RunOnce(cell.ctx); err != nil {
		t.Fatalf("%s replacement launch: %v", point, err)
	}
	waitForLines(t, cell.marker, 1)
	waitForV2HandoffEvents(t, cell.check)
	assertSingleLaunchOwner(t, cell.check)
	if spawns != 1 {
		t.Fatalf("%s replacement spawns=%d, want 1", point, spawns)
	}
	var digests int
	if err := cell.check.QueryRow(`SELECT count(*) FILTER (WHERE bootstrap_digest IS NOT NULL) FROM attempt_claims WHERE run_id='run-1'`).Scan(&digests); err != nil || digests != 1 {
		t.Fatalf("%s bootstrap digests=%d, want 1: %v", point, digests, err)
	}
	var generation int
	if err := cell.check.QueryRow(`SELECT generation FROM attempts WHERE run_id='run-1' AND attempt_no=1`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	wantGeneration := 2 // redispatch fenced the dead generation.
	if point == "bootstrap" {
		wantGeneration = 1
	}
	if generation != wantGeneration {
		t.Fatalf("%s generation=%d, want %d", point, generation, wantGeneration)
	}
	// Replay: a further worker tick must not consume a second permit.
	if err := worker.RunOnce(cell.ctx); err != nil {
		t.Fatalf("%s replay worker: %v", point, err)
	}
	if spawns != 1 || countFileLines(cell.marker) != 1 {
		t.Fatalf("%s replay spawns=%d marker=%d, want 1/1", point, spawns, countFileLines(cell.marker))
	}
	if point != "bootstrap" {
		// A truly old generation (the killed worker's) is rejected at every
		// handoff verb through the real RPC boundary.
		assertV2StaleVerbs(t, cell.root, 1)
	}
}

// TestV2CrashWorkerHelper runs only inside a child test binary. It parks on
// the requested durable worker boundary and waits to be SIGKILLed.
func TestV2CrashWorkerHelper(t *testing.T) {
	if os.Getenv("SIFT_V2_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	root := os.Getenv("SIFT_V2_ROOT")
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(root, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	router, cleanup, err := v2MakeRouter(root, os.Getenv("SIFT_V2_WRAPPER"), db)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pause := func() error {
		if err := os.WriteFile(os.Getenv("SIFT_V2_READY"), []byte(os.Getenv("SIFT_V2_POINT")), 0600); err != nil {
			return err
		}
		select {}
	}
	nowMS, err := strconv.ParseInt(os.Getenv("SIFT_V2_NOW"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	hooks := workerHooks{}
	switch os.Getenv("SIFT_V2_POINT") {
	case "lease":
		hooks.afterClaim = pause
	case "prepare":
		hooks.afterPrepare = pause
	case "bootstrap":
		hooks.afterBootstrapDigest = pause
	default:
		t.Fatal("unknown V2 crash boundary")
	}
	worker := &Worker{DB: db, BootID: os.Getenv("SIFT_V2_BOOT"), WorkerID: "v2-killed", Root: root, Lease: 50 * time.Millisecond, Now: func() time.Time { return time.UnixMilli(nowMS) }, Backends: router, Agents: launchTestAgents(), hooks: hooks}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	t.Fatal("V2 crash worker passed kill boundary")
}

// runV2WrapperCrash parks the real wrapper on the named handoff boundary,
// SIGKILLs its component (process group / tmux session), and asserts durable
// replay convergence through the real RPC boundary.
func runV2WrapperCrash(t *testing.T, wrapperPath string, backend config.Backend, point string) {
	cell := newV2Cell(t, backend)
	pausePoint := map[string]string{"acquire": "before-permit-rpc", "permit": "after-permit-rpc", "spawn": "before-started-rpc", "started": "after-started-rpc"}[point]
	ready, dumpPath := filepath.Join(cell.root, "wrapper-ready"), filepath.Join(cell.root, "wrapper-dump.jsonl")
	shim := v2PauseShim(t, cell.root, wrapperPath, pausePoint, ready, dumpPath)
	router, cleanup := v2BackendFactory(t, cell.root, shim, cell.db)
	defer cleanup()
	spawns := 0
	rec := &recordingHost{inner: router[backend], spawns: &spawns}
	router[backend] = rec
	script := "while :; do sleep 1; done"
	if point == "spawn" || point == "started" {
		script = "echo started >> $SIFT_RUN_DIR/agent-started; while :; do sleep 1; done"
	}
	worker := &Worker{DB: cell.db, BootID: cell.boot, WorkerID: "v2-" + point, Root: cell.root, Lease: time.Minute, Backends: router, Agents: []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", script}, TaskTransport: config.TaskTransportStdin}}}
	if err := worker.RunOnce(cell.ctx); err != nil {
		t.Fatalf("%s launch: %v", point, err)
	}
	if spawns != 1 {
		t.Fatalf("%s initial spawns=%d, want 1", point, spawns)
	}
	waitForFile(t, ready)
	dump := v2WaitDump(t, dumpPath, pausePoint)
	if point == "spawn" || point == "started" {
		// The wrapper pauses after Agent.Start, not after the shell fixture has
		// scheduled its marker write. Wait for that real child side effect before
		// killing the process group so Linux scheduling cannot turn this crash
		// boundary into an assertion race.
		waitForLines(t, cell.marker, 1)
	}
	instance := v2DumpString(dump, "instance")

	if point == "acquire" {
		// Barrier 1 (paused owner, missing disappearance evidence): the
		// replacement owner count is zero while the paused owner is alive.
		if got := v2ReplacementOwners(t, cell.check, instance); got != 0 {
			t.Fatalf("replacement owners before disappearance=%d, want 0", got)
		}
		// A same-generation second wrapper process is rejected at acquire.
		runV2SecondWrapper(t, cell, wrapperPath, dump)
		if backend == config.BackendTmux {
			assertV2TmuxRespawnRejected(t, cell, router, shim, dump)
		}
	}

	v2KillCellWrapper(t, cell, backend, rec.proc, dump)
	if point == "acquire" {
		// The disappearance barrier holds: the old process group is provably
		// gone, and killing it did not admit a replacement owner by itself.
		if got := v2ReplacementOwners(t, cell.check, instance); got != 0 {
			t.Fatalf("replacement owners after disappearance=%d, want 0", got)
		}
	}

	// A replacement boot and worker replay find no claimable work and spawn
	// nothing: the durable owner interval is closed without a successor.
	boot := cell.restartBoot(t, "supervise")
	replaySpawns := 0
	replayRec := &recordingHost{inner: router[backend], spawns: &replaySpawns}
	router[backend] = replayRec
	if err := (&Worker{DB: cell.db, BootID: boot, WorkerID: "v2-replay", Root: cell.root, Lease: time.Minute, Backends: router, Agents: worker.Agents}).RunOnce(cell.ctx); err != nil {
		t.Fatalf("%s replay worker: %v", point, err)
	}
	if replaySpawns != 0 {
		t.Fatalf("%s replay spawns=%d, want 0", point, replaySpawns)
	}
	assertSingleLaunchOwner(t, cell.check)
	wantMarker := 0
	if point == "spawn" || point == "started" {
		wantMarker = 1
	}
	if got := countFileLines(cell.marker); got != wantMarker {
		t.Fatalf("%s agent marker=%d, want %d", point, got, wantMarker)
	}
	assertV2BoundaryPhase(t, cell.check, point)
	assertV2HandoffReplay(t, cell.root, point, dump)
}

// v2PauseShim is the wrapper host entrypoint for crash cells. It exports the
// pause/dump contract and execs the compiled wrapper, so both backends run
// the same bytes (tmux panes do not inherit the test process environment).
func v2PauseShim(t *testing.T, root, realWrapper, point, ready, dump string) string {
	t.Helper()
	shim := filepath.Join(root, "wrapper-shim")
	script := fmt.Sprintf("#!/bin/sh\nexport SIFT_WRAPPER_TEST_PAUSE='%s'\nexport SIFT_WRAPPER_TEST_READY='%s'\nexport SIFT_WRAPPER_TEST_DUMP='%s'\nexec '%s' \"$@\"\n", point, ready, dump, realWrapper)
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return shim
}

// v2KillCellWrapper performs the crash injection: SIGKILL the execution
// wrapper's process group and the outer supervisor (directly for process,
// via the tmux session for tmux), then prove group absence.
func v2KillCellWrapper(t *testing.T, cell *v2Cell, backend config.Backend, outer *os.Process, dump map[string]any) {
	t.Helper()
	pgid := v2ControlPGID(t, cell.runDir)
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatal(err)
	}
	if backend == config.BackendProcess {
		if outer != nil {
			if err := outer.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
				t.Fatal(err)
			}
			_, _ = outer.Wait()
		}
	} else {
		name, err := runtimepkg.TmuxSessionName("run-1", 1, v2DumpInt(dump, "generation"), v2DumpString(dump, "dispatch_id"))
		if err != nil {
			t.Fatal(err)
		}
		v2Tmux(t, cell.root, "kill-session", "-t", "="+name)
	}
	waitForGroupAbsent(t, pgid)
}

// runV2SecondWrapper starts a real second wrapper process carrying the paused
// owner's dispatch credentials (same generation, its own fresh instance). The
// durable handoff must reject it without touching the recorded owner.
func runV2SecondWrapper(t *testing.T, cell *v2Cell, wrapperPath string, dump map[string]any) {
	t.Helper()
	bootstrap := runtimepkg.Bootstrap{
		SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor,
		DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version,
		RunID: "run-1", AttemptNo: 1, Generation: v2DumpInt(dump, "generation"),
		DispatchID: v2DumpString(dump, "dispatch_id"), BootstrapNonce: v2DumpString(dump, "nonce"), RunToken: v2DumpString(dump, "run_token"),
		RunDir: cell.runDir, WorktreePath: cell.worktree,
		Agent:              runtimepkg.BootstrapAgent{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "true"}, TaskTransport: "stdin"},
		TaskSpecSnapshotID: "task-run-1", TaskSpec: json.RawMessage(`{"title":"crash-suite"}`),
	}
	data, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cell.root, "second-bootstrap.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := osexec.Command(wrapperPath, path).CombinedOutput()
	if err == nil {
		t.Fatalf("same-generation second wrapper unexpectedly succeeded: %s", out)
	}
	var instance string
	if err := cell.check.QueryRow(`SELECT wrapper_instance_id FROM attempts WHERE run_id='run-1' AND attempt_no=1`).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if instance != v2DumpString(dump, "instance") {
		t.Fatalf("second wrapper replaced owner instance %q with %q", v2DumpString(dump, "instance"), instance)
	}
	var acquired int
	if err := cell.check.QueryRow(`SELECT count(*) FROM events WHERE run_id='run-1' AND type='attempt.acquired'`).Scan(&acquired); err != nil || acquired != 1 {
		t.Fatalf("second wrapper emitted acquire events=%d, want 1: %v", acquired, err)
	}
}

// assertV2TmuxRespawnRejected proves the tmux host refuses a second spawn for
// the same frozen launch binding while the original session is alive.
func assertV2TmuxRespawnRejected(t *testing.T, cell *v2Cell, router BackendRouter, shim string, dump map[string]any) {
	t.Helper()
	var operationID string
	if err := cell.check.QueryRow(`SELECT o.id FROM outbox_operations o WHERE o.run_id='run-1' AND o.kind='launch_agent'`).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	launch := runtimepkg.HostLaunch{
		Backend: string(config.BackendTmux), RunID: "run-1", AttemptNo: 1, Generation: v2DumpInt(dump, "generation"),
		DispatchID: v2DumpString(dump, "dispatch_id"), WrapperPath: shim, BootstrapPath: filepath.Join(cell.runDir, "bootstrap.json"),
		OperationID: operationID, LeaseOwner: "intruder", LeaseExpiresAtMS: time.Now().UnixMilli() + 60000,
	}
	if _, err := router[config.BackendTmux].Spawn(cell.ctx, launch); err == nil {
		t.Fatal("tmux respawn of a live frozen binding unexpectedly succeeded")
	}
}

// assertV2HandoffReplay reissues the killed wrapper's exact handoff tuple
// through the real RPC boundary and asserts idempotent convergence.
func assertV2HandoffReplay(t *testing.T, root, point string, dump map[string]any) {
	t.Helper()
	session := v2DumpString(dump, "session")
	switch point {
	case "acquire":
		wi := dump["wrapper_identity"].(map[string]any)
		params := map[string]any{
			"run_id": "run-1", "attempt_no": 1, "generation": v2DumpInt(dump, "generation"),
			"dispatch_id": v2DumpString(dump, "dispatch_id"), "wrapper_instance_id": v2DumpString(dump, "instance"),
			"session_candidate": session, "wrapper_identity": wi,
		}
		auth := map[string]any{"kind": "bootstrap", "nonce": v2DumpString(dump, "nonce")}
		if ok, code := v2RPC(t, root, "claim.acquire", auth, params); !ok {
			t.Fatalf("acquire replay = %s, want idempotent success", code)
		}
		intruder := map[string]any{}
		for k, v := range params {
			intruder[k] = v
		}
		intruder["wrapper_instance_id"] = "intruder-instance"
		if ok, code := v2RPC(t, root, "claim.acquire", auth, intruder); ok || code != "conflict" {
			t.Fatalf("second-owner acquire = ok=%v code=%s, want conflict", ok, code)
		}
	case "permit":
		wi := dump["wrapper_identity"].(map[string]any)
		params := map[string]any{
			"run_id": "run-1", "attempt_no": 1, "generation": v2DumpInt(dump, "generation"),
			"wrapper_instance_id": v2DumpString(dump, "instance"), "wrapper_identity": wi,
			"control_digest": randomSecret(), "control_nonce_hash": randomSecret(), "permit_candidate": v2DumpString(dump, "permit"),
		}
		auth := map[string]any{"kind": "wrapper_session", "session": session}
		if ok, code := v2RPC(t, root, "claim.permit_spawn", auth, params); !ok {
			t.Fatalf("permit replay = %s, want idempotent success", code)
		}
	case "spawn", "started":
		params := map[string]any{
			"run_id": "run-1", "attempt_no": 1, "generation": v2DumpInt(dump, "generation"),
			"wrapper_instance_id": v2DumpString(dump, "instance"), "agent_identity": dump["agent_identity"],
			"control_digest": v2DumpString(dump, "control_digest"), "result_digest": nil,
		}
		auth := map[string]any{"kind": "wrapper_started", "session": session, "permit": v2DumpString(dump, "permit")}
		ok, code := v2RPC(t, root, "claim.started", auth, params)
		if point == "spawn" {
			// The killed wrapper never committed started; its lost-response
			// replay converges to running exactly once, then duplicates.
			if !ok {
				t.Fatalf("spawn-boundary started replay = %s, want success", code)
			}
			if ok, code := v2RPC(t, root, "claim.started", auth, params); !ok {
				t.Fatalf("started re-replay = %s, want idempotent duplicate", code)
			}
			assertV2BoundaryPhase(t, mustSQLDB(t, root), "started")
			return
		}
		if !ok {
			t.Fatalf("started replay = %s, want idempotent duplicate", code)
		}
	}
}

// runV2FastExitCrash crashes the Agent immediately after start (SIGKILL) so
// the result publication races the started handshake. The wrapper must still
// converge and a replayed worker must not respawn.
func runV2FastExitCrash(t *testing.T, wrapperPath string, backend config.Backend) {
	cell := newV2Cell(t, backend)
	router, cleanup := v2BackendFactory(t, cell.root, wrapperPath, cell.db)
	defer cleanup()
	spawns := 0
	rec := &recordingHost{inner: router[backend], spawns: &spawns}
	router[backend] = rec
	worker := &Worker{DB: cell.db, BootID: cell.boot, WorkerID: "v2-fast-exit", Root: cell.root, Lease: time.Minute, Backends: router, Agents: []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "echo started >> $SIFT_RUN_DIR/agent-started; kill -KILL $$"}, TaskTransport: config.TaskTransportStdin}}}
	if err := worker.RunOnce(cell.ctx); err != nil {
		t.Fatalf("fast-exit launch: %v", err)
	}
	waitForLines(t, cell.marker, 1)
	waitForFile(t, filepath.Join(cell.runDir, "result.json"))
	waitForV2HandoffEvents(t, cell.check)
	assertSingleLaunchOwner(t, cell.check)
	if spawns != 1 {
		t.Fatalf("fast-exit spawns=%d, want 1", spawns)
	}
	if backend == config.BackendTmux {
		var dispatch string
		var generation int
		if err := cell.check.QueryRow(`SELECT c.dispatch_id,a.generation FROM attempt_claims c JOIN attempts a ON a.run_id=c.run_id AND a.attempt_no=c.attempt_no WHERE c.run_id='run-1'`).Scan(&dispatch, &generation); err != nil {
			t.Fatal(err)
		}
		name, err := runtimepkg.TmuxSessionName("run-1", 1, generation, dispatch)
		if err != nil {
			t.Fatal(err)
		}
		v2WaitTmuxSessionGone(t, cell.root, name)
	}
	if err := worker.RunOnce(cell.ctx); err != nil {
		t.Fatalf("fast-exit replay worker: %v", err)
	}
	if spawns != 1 || countFileLines(cell.marker) != 1 {
		t.Fatalf("fast-exit replay spawns=%d marker=%d, want 1/1", spawns, countFileLines(cell.marker))
	}
}

// assertV2StaleVerbs rejects a fenced old generation at all three handoff
// verbs through the real RPC boundary.
func assertV2StaleVerbs(t *testing.T, root string, generation int) {
	t.Helper()
	wi := map[string]any{"pid": 1, "started_at_ms": 1, "executable": "/wrapper", "pgid": 1}
	acquire := map[string]any{"run_id": "run-1", "attempt_no": 1, "generation": generation, "dispatch_id": "old-dispatch", "wrapper_instance_id": "old-instance", "session_candidate": randomSecret(), "wrapper_identity": wi}
	if ok, code := v2RPC(t, root, "claim.acquire", map[string]any{"kind": "bootstrap", "nonce": randomSecret()}, acquire); ok || code != "stale" {
		t.Fatalf("old-generation acquire = ok=%v code=%s, want stale", ok, code)
	}
	permit := map[string]any{"run_id": "run-1", "attempt_no": 1, "generation": generation, "wrapper_instance_id": "old-instance", "wrapper_identity": wi, "control_digest": randomSecret(), "control_nonce_hash": randomSecret(), "permit_candidate": randomSecret()}
	if ok, code := v2RPC(t, root, "claim.permit_spawn", map[string]any{"kind": "wrapper_session", "session": randomSecret()}, permit); ok || code != "stale" {
		t.Fatalf("old-generation permit = ok=%v code=%s, want stale", ok, code)
	}
	started := map[string]any{"run_id": "run-1", "attempt_no": 1, "generation": generation, "wrapper_instance_id": "old-instance", "agent_identity": map[string]any{"pid": 2, "started_at_ms": 1, "executable": "/agent"}, "control_digest": randomSecret(), "result_digest": nil}
	if ok, code := v2RPC(t, root, "claim.started", map[string]any{"kind": "wrapper_started", "session": randomSecret(), "permit": randomSecret()}, started); ok || code != "stale" {
		t.Fatalf("old-generation started = ok=%v code=%s, want stale", ok, code)
	}
}

func assertV2BoundaryPhase(t *testing.T, db *sql.DB, point string) {
	t.Helper()
	want := map[string]string{"acquire": "starting", "permit": "spawning", "spawn": "spawning", "started": "running"}[point]
	var phase string
	if err := db.QueryRow(`SELECT phase FROM attempts WHERE run_id='run-1' AND attempt_no=1`).Scan(&phase); err != nil || phase != want {
		t.Fatalf("boundary phase=%q, want %q: %v", phase, want, err)
	}
}

func v2ReplacementOwners(t *testing.T, db *sql.DB, excludeInstance string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run-1' AND wrapper_instance_id IS NOT NULL AND wrapper_instance_id != ?`, excludeInstance).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func v2ControlPGID(t *testing.T, runDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	var control struct {
		WrapperIdentity struct {
			PGID int `json:"pgid"`
		} `json:"wrapper_identity"`
	}
	if err := json.Unmarshal(data, &control); err != nil || control.WrapperIdentity.PGID <= 0 {
		t.Fatalf("control wrapper pgid: %v", err)
	}
	return control.WrapperIdentity.PGID
}

// v2RPC issues one framed control-plane request and returns ok/error-code.
func v2RPC(t *testing.T, root, method string, auth, params map[string]any) (bool, string) {
	t.Helper()
	conn, err := net.Dial("unix", filepath.Join(root, "run.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := map[string]any{"protocol_major": 1, "protocol_minor": 0, "client_version": controlplane.Version, "request_id": randomSecret()[:32], "method": method, "auth": auth, "params": params}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(body)))
	if _, err := conn.Write(append(h[:], body...)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, binary.BigEndian.Uint32(h[:]))
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	var r struct {
		OK     bool `json:"ok"`
		Result struct {
			Disposition string `json:"disposition"`
		} `json:"result"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		return false, r.Error.Code
	}
	return r.OK, ""
}

func v2WaitDump(t *testing.T, path, point string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if line == "" {
					continue
				}
				var entry struct {
					Point  string         `json:"point"`
					Fields map[string]any `json:"fields"`
				}
				if json.Unmarshal([]byte(line), &entry) == nil && entry.Point == point {
					return entry.Fields
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wrapper dump for %s was not written to %s", point, path)
	return nil
}

func v2DumpString(dump map[string]any, key string) string {
	v, _ := dump[key].(string)
	return v
}

func v2DumpInt(dump map[string]any, key string) int {
	f, _ := dump[key].(float64)
	return int(f)
}

func mustSQLDB(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "sift.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func v2Tmux(t *testing.T, root string, args ...string) {
	t.Helper()
	tmux, err := osexec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	socket := runtimepkg.TmuxSocketPath(filepath.Join(root, "tmux.sock"))
	full := append([]string{"-f", "/dev/null", "-S", socket}, args...)
	cmd := osexec.Command(tmux, full...)
	cmd.Env = runtimepkg.TmuxClientEnvironment()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func v2WaitTmuxSessionGone(t *testing.T, root, name string) {
	t.Helper()
	tmux, err := osexec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	socket := runtimepkg.TmuxSocketPath(filepath.Join(root, "tmux.sock"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd := osexec.Command(tmux, "-f", "/dev/null", "-S", socket, "has-session", "-t", "="+name)
		cmd.Env = runtimepkg.TmuxClientEnvironment()
		if err := cmd.Run(); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tmux session %q did not exit", name)
}
