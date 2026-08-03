package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/miaoxiaoyong/sift/internal/command"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/launchworker"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// v2OwnerRun is a grammar-valid (32-hex) run id so the /sift retry body parses.
const v2OwnerRun = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestBackendV2OwnerReplacementBarriers runs the paused-owner/disappearance
// replacement timing through the production assembly for both backends. Two
// controllable barriers bracket the owner interval: while the owner wrapper is
// SIGSTOP-paused (alive, missing evidence) recovery supervises and no
// replacement owner exists; after the owner process group provably disappears,
// the frozen startup_stall retry path produces exactly one replacement owner on
// the successor attempt.
func TestBackendV2OwnerReplacementBarriers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("V2 owner replacement harness requires Unix process groups")
	}
	if _, err := osexec.LookPath("tmux"); err != nil {
		t.Fatalf("V2 tmux backend is required: %v", err)
	}
	wrapperPath := buildPausedRecoveryWrapper(t)
	for _, backend := range []config.Backend{config.BackendProcess, config.BackendTmux} {
		t.Run(fmt.Sprintf("V2/%s/owner-replacement", backend), func(t *testing.T) {
			runV2OwnerReplacement(t, wrapperPath, backend)
		})
	}
}

func runV2OwnerReplacement(t *testing.T, wrapperPath string, backend config.Backend) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	root, err := os.MkdirTemp("/tmp", "sift-v2owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	db := v2OwnerTemplateDB(t, ctx, filepath.Join(root, "sift.db"), now)
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, v2OwnerRun, "project", "cfg", now.UnixMilli(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO project_hook_baselines(project_id,git_config_digest,effective_hooks_path,hooks_directory_digest,baseline_digest,captured_at_ms,updated_at_ms) VALUES ('project','git','/hooks','dir','baseline',?,?)`, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE attempts SET backend=? WHERE run_id='`+v2OwnerRun+`' AND attempt_no=1`, backend); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := pausedRecoveryCoordinator(db, root, now).RecoverStartup(ctx, boot); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteStartupRecovery(ctx, boot, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	server, err := controlplane.Start(config.Home{Path: filepath.Join(root, "runs")}, db)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); _ = server.Close() }()
	go func() { _ = server.Serve(serveCtx) }()
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	// This harness deliberately cannot verify process-group absence, so a
	// disappeared owner freezes into startup_stall (human retry) instead of the
	// verified-absence auto-successor path.
	coordinator := func(now time.Time) *TerminationCoordinator {
		rt := config.DefaultConfig().Runtime
		rt.HeartbeatStaleAfter = time.Hour
		c := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: runtimepkg.PlatformProcessInspector{}, Signaler: runtimepkg.UnixProcessSignaler{}}, Runtime: rt, ProcessGroupVerified: func(string) bool { return false }, AttentionDailyQuota: recoveryQuota(), ControlRoot: root, Now: func() time.Time { return now }}
		return c
	}

	agents := []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "echo started >> $SIFT_RUN_DIR/agent-started; while :; do sleep 1; done"}, TaskTransport: config.TaskTransportStdin}}
	ready := filepath.Join(root, "owner-ready")
	shim := v2OwnerPauseShim(t, root, wrapperPath, "after-started-rpc", ready)
	router, routerSocket, cleanup := v2OwnerRouter(t, root, shim, db)
	defer cleanup()
	if err := (&launchworker.Worker{DB: db, BootID: boot, WorkerID: "v2-owner", Root: root, Lease: time.Minute, Backends: router, Agents: agents}).RunOnce(ctx); err != nil {
		t.Fatalf("owner launch: %v", err)
	}
	waitPausedFile(t, ready)
	marker := filepath.Join(root, "runs", v2OwnerRun, "attempts", "1", "agent-started")
	waitPausedLines(t, marker, 1)

	ownerPID, ownerPGID, ownerInstance := v2OwnerIdentity(t, check)
	assertV2ProcessLive(t, ownerPID)
	assertV2ProcessStopped(t, ownerPID)
	var dispatch string
	var generation int
	if err := check.QueryRow(`SELECT c.dispatch_id,a.generation FROM attempt_claims c JOIN attempts a ON a.run_id=c.run_id AND a.attempt_no=c.attempt_no WHERE c.run_id='`+v2OwnerRun+`' AND c.attempt_no=1`).Scan(&dispatch, &generation); err != nil {
		t.Fatal(err)
	}

	// Barrier 1 — paused owner, missing disappearance evidence. Recovery sees
	// the live owner and supervises; a candidate worker consumes nothing; the
	// replacement owner count is exactly zero.
	pausedBoot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator(now.Add(500*time.Millisecond)).RecoverStartup(ctx, pausedBoot); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteStartupRecovery(ctx, pausedBoot, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var interrupts int
	if err := check.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='` + v2OwnerRun + `'`).Scan(&interrupts); err != nil || interrupts != 0 {
		t.Fatalf("paused owner triggered %d interrupts, want supervised silence: %v", interrupts, err)
	}
	if err := (&launchworker.Worker{DB: db, BootID: pausedBoot, WorkerID: "v2-candidate", Root: root, Lease: time.Minute, Backends: router, Agents: agents}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := v2ReplacementOwnerCount(t, check, ownerInstance); got != 0 {
		t.Fatalf("replacement owners before disappearance=%d, want 0", got)
	}
	assertV2ProcessLive(t, ownerPID)
	if got := countPausedLines(marker); got != 1 {
		t.Fatalf("paused candidate spawned agent markers=%d, want 1", got)
	}

	// Disappearance barrier — SIGKILL the owner component and prove absence.
	if err := syscall.Kill(-ownerPGID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatal(err)
	}
	if backend == config.BackendTmux {
		name, err := runtimepkg.TmuxSessionName(v2OwnerRun, 1, generation, dispatch)
		if err != nil {
			t.Fatal(err)
		}
		v2OwnerTmux(t, routerSocket, "kill-session", "-t", "="+name)
	}
	assertV2ProcessGone(t, ownerPID)
	assertV2GroupGone(t, ownerPGID)

	// The owner interval ended after claim.started, so the startup recovery
	// arbitration keeps supervising the recorded started fact. The production
	// silence detector is the heartbeat-stale Timeout tick; with the group
	// provably gone it freezes the attempt: startup_stall, waiting_human.
	goneNow := time.Now()
	stall := coordinator(goneNow)
	stall.Runtime.HeartbeatStaleAfter = time.Millisecond
	if err := stall.Timeout(ctx); err != nil {
		t.Fatal(err)
	}
	var interruptID, nonce, runStatus string
	if err := check.QueryRow(`SELECT i.id,i.nonce,r.status FROM interrupts i JOIN runs r ON r.id=i.run_id WHERE i.run_id='`+v2OwnerRun+`' AND i.reason='startup_stall' AND i.status='open'`).Scan(&interruptID, &nonce, &runStatus); err != nil || runStatus != "waiting_human" {
		t.Fatalf("disappearance did not freeze into startup_stall/waiting_human: %v status=%s", err, runStatus)
	}

	// The frozen retry path (durable request + probe success) creates the
	// successor attempt, claim, and launch operation.
	key, err := command.RecomputeEventKey("project", command.SourceForgeComment, "v2-owner-retry")
	if err != nil {
		t.Fatal(err)
	}
	actor := "alice"
	env := command.CommandEventEnvelopeV1{
		SchemaVersion: 1, EventKey: key, ProjectID: "project", Source: command.SourceForgeComment,
		RemoteEventID: "v2-owner-retry", Target: command.CommandTarget{Kind: command.TargetIssue, ID: "issue-1"},
		Actor: &actor, RawDigest: strings.Repeat("a", 64), OccurredAtMS: goneNow.UnixMilli() + 1,
		Comment: &command.CommandComment{ID: "v2-owner-retry", Body: "/sift retry " + v2OwnerRun + " " + nonce},
	}
	if _, err := db.ApplyCommandEvent(ctx, storage.ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: goneNow.UnixMilli() + 1}); err != nil {
		t.Fatalf("retry request: %v", err)
	}
	var probeID string
	if err := check.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=?`, interruptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyRetryProbeResult(ctx, storage.RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: true, AbsenceEvidenceJSON: json.RawMessage(`{"absent":true}`), NowMS: goneNow.UnixMilli() + 2}); err != nil {
		t.Fatalf("probe success: %v", err)
	}

	// The replacement worker owns exactly one live wrapper: the successor on
	// attempt 2, on this cell's backend.
	replaceNow := time.Now()
	replaceBoot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), replaceNow.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator(replaceNow).RecoverStartup(ctx, replaceBoot); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteStartupRecovery(ctx, replaceBoot, replaceNow.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	plainRouter, _, plainCleanup := v2OwnerRouter(t, root, wrapperPath, db)
	defer plainCleanup()
	fastAgents := []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", "echo started >> $SIFT_RUN_DIR/agent-started"}, TaskTransport: config.TaskTransportStdin}}
	if err := (&launchworker.Worker{DB: db, BootID: replaceBoot, WorkerID: "v2-replacement", Root: root, Lease: time.Minute, Backends: plainRouter, Agents: fastAgents}).RunOnce(ctx); err != nil {
		t.Fatalf("replacement launch: %v", err)
	}
	waitPausedLines(t, filepath.Join(root, "runs", v2OwnerRun, "attempts", "2", "agent-started"), 1)

	// The durable probe-success event is the absence proof that releases the
	// old isolation. The replacement wrapper may only acquire after that event;
	// compare append-only event sequence, not wall-clock timestamps.
	var absenceSeq, replacementAcquireSeq int
	if err := check.QueryRow(`SELECT e.seq FROM attempts a JOIN events e ON e.id=a.isolation_release_event_id WHERE a.run_id='` + v2OwnerRun + `' AND a.attempt_no=1`).Scan(&absenceSeq); err != nil {
		t.Fatalf("old owner absence evidence: %v", err)
	}
	if err := check.QueryRow(`SELECT seq FROM events WHERE run_id='` + v2OwnerRun + `' AND attempt_no=2 AND type='attempt.acquired'`).Scan(&replacementAcquireSeq); err != nil {
		t.Fatalf("replacement owner acquire evidence: %v", err)
	}
	if absenceSeq >= replacementAcquireSeq {
		t.Fatalf("replacement acquired before absence proof: absence seq=%d replacement acquire seq=%d", absenceSeq, replacementAcquireSeq)
	}

	var liveOwners int
	if err := check.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='` + v2OwnerRun + `' AND phase NOT IN ('finished','orphaned') AND wrapper_instance_id IS NOT NULL`).Scan(&liveOwners); err != nil || liveOwners != 1 {
		t.Fatalf("live owners after replacement=%d, want exactly 1: %v", liveOwners, err)
	}
	var newInstance, oldPhase, resolution, isolation string
	if err := check.QueryRow(`SELECT wrapper_instance_id FROM attempts WHERE run_id='` + v2OwnerRun + `' AND attempt_no=2`).Scan(&newInstance); err != nil || newInstance == "" || newInstance == ownerInstance {
		t.Fatalf("replacement owner instance=%q (old %q): %v", newInstance, ownerInstance, err)
	}
	if err := check.QueryRow(`SELECT phase,attempt_resolution,isolation_state FROM attempts WHERE run_id='`+v2OwnerRun+`' AND attempt_no=1`).Scan(&oldPhase, &resolution, &isolation); err != nil || oldPhase != "orphaned" || resolution != "retry_after_absence" || isolation != "none" {
		t.Fatalf("old attempt=%s/%s/%s, want orphaned/retry_after_absence/none: %v", oldPhase, resolution, isolation, err)
	}
	if got := countPausedLines(marker); got != 1 {
		t.Fatalf("old agent markers=%d, want 1 (no overlap)", got)
	}
}

func v2OwnerPauseShim(t *testing.T, root, realWrapper, point, ready string) string {
	t.Helper()
	shim := filepath.Join(root, "owner-wrapper-shim")
	script := fmt.Sprintf("#!/bin/sh\nexport SIFT_WRAPPER_TEST_PAUSE='%s'\nexport SIFT_WRAPPER_TEST_READY='%s'\nexec '%s' \"$@\"\n", point, ready, realWrapper)
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return shim
}

func v2OwnerRouter(t *testing.T, root, wrapperPath string, db *storage.DB) (launchworker.BackendRouter, string, func()) {
	t.Helper()
	installed := filepath.Join(root, "installed", fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(installed, 0700); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "sift-agent-wrapper"), contents, 0700); err != nil {
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
	socket := filepath.Join(root, fmt.Sprintf("tmux-%d.sock", time.Now().UnixNano()))
	tmuxRuntime, err := runtimepkg.NewTmuxBackend(tmuxPath, wrapperPath, socket, verify)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		cmd := osexec.Command(tmuxPath, "-f", "/dev/null", "-S", tmuxRuntime.SocketPath(), "kill-server")
		cmd.Env = runtimepkg.TmuxClientEnvironment()
		_ = cmd.Run()
	}
	return launchworker.BackendRouter{config.BackendProcess: launchworker.ProcessBackend{Backend: processRuntime}, config.BackendTmux: launchworker.TmuxBackend{Backend: tmuxRuntime}}, tmuxRuntime.SocketPath(), cleanup
}

func v2OwnerIdentity(t *testing.T, db *sql.DB) (int, int, string) {
	t.Helper()
	var pid, pgid int
	var instance string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := db.QueryRow(`SELECT wrapper_pid,wrapper_pgid,wrapper_instance_id FROM attempts WHERE run_id='`+v2OwnerRun+`' AND attempt_no=1`).Scan(&pid, &pgid, &instance)
		if err == nil && pid > 0 && pgid > 0 && instance != "" {
			return pid, pgid, instance
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owner identity was not durably projected")
	return 0, 0, ""
}

func v2ReplacementOwnerCount(t *testing.T, db *sql.DB, excludeInstance string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='`+v2OwnerRun+`' AND wrapper_instance_id IS NOT NULL AND wrapper_instance_id != ?`, excludeInstance).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertV2ProcessLive(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("pid %d is not live", pid)
	}
}

func assertV2ProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := osexec.Command("ps", "-o", "stat=", "-p", fmt.Sprintf("%d", pid)).Output()
		if err == nil && strings.Contains(string(out), "T") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d did not reach stopped (T) state", pid)
}

func assertV2ProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d remained live after disappearance", pid)
}

func assertV2GroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owner process group %d remains live", pgid)
}

func waitPausedLines(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countPausedLines(path) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lines in %s = %d, want %d", path, countPausedLines(path), want)
}

func countPausedLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func v2OwnerTmux(t *testing.T, socket string, args ...string) {
	t.Helper()
	tmux, err := osexec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{"-f", "/dev/null", "-S", socket}, args...)
	cmd := osexec.Command(tmux, full...)
	cmd.Env = runtimepkg.TmuxClientEnvironment()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

var v2OwnerTemplate struct {
	once sync.Once
	path string
	err  error
}

func v2OwnerTemplateDB(t *testing.T, ctx context.Context, path string, now time.Time) *storage.DB {
	t.Helper()
	v2OwnerTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "sift-v2-owner-template-")
		if err != nil {
			v2OwnerTemplate.err = err
			return
		}
		template := filepath.Join(dir, "sift.db")
		db, err := storage.Open(ctx, storage.OpenConfig{Path: template, BinaryVersion: controlplane.Version, Now: now})
		if err != nil {
			v2OwnerTemplate.err = err
			return
		}
		if _, err := db.ExecForTest(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			_ = db.Close()
			v2OwnerTemplate.err = err
			return
		}
		if err := db.Close(); err != nil {
			v2OwnerTemplate.err = err
			return
		}
		v2OwnerTemplate.path = template
	})
	if v2OwnerTemplate.err != nil {
		t.Fatalf("V2 owner template: %v", v2OwnerTemplate.err)
	}
	data, err := os.ReadFile(v2OwnerTemplate.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, storage.OpenConfig{Path: path, BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		t.Fatalf("Open cloned V2 owner template: %v", err)
	}
	return db
}
