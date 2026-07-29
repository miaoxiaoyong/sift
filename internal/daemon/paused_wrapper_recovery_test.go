package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/miaoxiaoyong/sift/internal/launchworker"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func TestPausedExecutionWrapperRecoveryDoesNotOverlapOwner(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process identity inspection")
	}
	wrapper := buildPausedRecoveryWrapper(t)
	for _, tc := range []struct {
		name, point, script, phase string
		agentLive                  bool
	}{
		{"control-initial", "before-permit-rpc", "while :; do sleep 1; done", "starting", false},
		{"control-rewrite", "before-started-rpc", "while :; do sleep 1; done", "spawning", true},
		{"result-before-rename", "before-result-rename", "exit 0", "running", false},
		{"result-after-rename", "after-result-rename", "exit 0", "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, now := context.Background(), time.Now().Truncate(time.Millisecond)
			root := t.TempDir()
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
			initial := pausedRecoveryCoordinator(db, root, now)
			if err := initial.RecoverStartup(ctx, boot); err != nil {
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

			ready, spawns := filepath.Join(root, "ready"), filepath.Join(root, "spawns")
			backend := &pausedRecoveryBackend{path: wrapper, ready: ready, point: tc.point, spawns: spawns}
			worker := &launchworker.Worker{DB: db, BootID: boot, WorkerID: "old", Root: root, Lease: time.Minute, Backend: backend, Agents: []config.Agent{{ID: "agent", Executable: "/bin/sh", Args: []string{"-c", tc.script}, TaskTransport: config.TaskTransportStdin}}}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			waitPausedFile(t, ready)

			check, err := sql.Open("sqlite", db.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			execution, agent := pausedIdentity(t, check)
			if tc.agentLive && agent.PID == 0 {
				agent.PID = agentPIDFromControl(t, root)
			}
			outer := pausedSpawnIdentity(t, spawns)
			assertDistinctPausedProcesses(t, outer, execution)
			assertPausedLive(t, outer.PID, outer.PGID)
			assertPausedStopped(t, execution.PID, execution.PGID)
			if got := pausedParentPID(t, execution.PID); got != outer.PID {
				t.Fatalf("execution wrapper parent=%d want outer supervisor=%d", got, outer.PID)
			}
			assertPausedProjection(t, check, execution, agent, tc.point)
			assertPausedAgentInterval(t, agent, execution.PGID, tc.agentLive)
			var phase string
			if err := check.QueryRow(`SELECT phase FROM attempts WHERE run_id='run-1'`).Scan(&phase); err != nil || phase != tc.phase {
				t.Fatalf("pause phase=%q want=%q: %v", phase, tc.phase, err)
			}

			restartedAt := now.Add(time.Second)
			restarted, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), restartedAt.UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			coordinator := pausedRecoveryCoordinator(db, root, restartedAt)
			if err := coordinator.RecoverStartup(ctx, restarted); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteStartupRecovery(ctx, restarted, restartedAt.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			candidate := &pausedRecoveryBackend{path: wrapper, spawns: spawns}
			if err := (&launchworker.Worker{DB: db, BootID: restarted, WorkerID: "new", Root: root, Lease: time.Minute, Backend: candidate, Agents: worker.Agents}).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			replacement := pausedReplacementIdentity(t, spawns)
			if candidate.count != 0 || replacement.PID != 0 {
				t.Fatalf("replacement outer=%d backend spawns=%d, want explicit empty interval", replacement.PID, candidate.count)
			}
			assertPausedLive(t, outer.PID, outer.PGID)
			assertPausedStopped(t, execution.PID, execution.PGID)
			assertPausedAgentInterval(t, agent, execution.PGID, tc.agentLive)
			assertPausedProjection(t, check, execution, agent, tc.point)

			if err := syscall.Kill(-execution.PGID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				t.Fatal(err)
			}
			if err := syscall.Kill(outer.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				t.Fatal(err)
			}
			assertPausedAbsent(t, execution.PID)
			assertPausedAbsent(t, outer.PID)
			if agent.PID != 0 {
				assertPausedAbsent(t, agent.PID)
			}
			if replacement.PID != 0 {
				t.Fatalf("replacement outer=%d must remain absent after cleanup", replacement.PID)
			}
		})
	}
}

func pausedRecoveryCoordinator(db *storage.DB, root string, now time.Time) *TerminationCoordinator {
	rt := config.DefaultConfig().Runtime
	rt.HeartbeatStaleAfter = time.Hour
	return &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: runtimepkg.PlatformProcessInspector{}, Signaler: runtimepkg.UnixProcessSignaler{}}, Runtime: rt, ProcessGroupVerified: func(string) bool { return true }, AttentionDailyQuota: recoveryQuota(), ControlRoot: root, Now: func() time.Time { return now }}
}

type pausedRecoveryBackend struct {
	path, ready, point, spawns string
	count                      int
}

func (b *pausedRecoveryBackend) Spawn(ctx context.Context, bootstrap string) (*os.Process, error) {
	cmd := osexec.CommandContext(ctx, b.path, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "SIFT_WRAPPER_TEST_PAUSE="+b.point, "SIFT_WRAPPER_TEST_READY="+b.ready)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b.count++
	if b.spawns != "" {
		f, err := os.OpenFile(b.spawns, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, err = fmt.Fprintf(f, "%d\n", cmd.Process.Pid)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	return cmd.Process, nil
}

func agentPIDFromControl(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "runs", "run-1", "attempts", "1", "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	var control struct {
		AgentIdentity *struct {
			PID int `json:"pid"`
		} `json:"agent_identity"`
	}
	if err := json.Unmarshal(data, &control); err != nil || control.AgentIdentity == nil || control.AgentIdentity.PID <= 0 {
		t.Fatalf("control agent_identity: %v", err)
	}
	return control.AgentIdentity.PID
}

func buildPausedRecoveryWrapper(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root, path := filepath.Clean(filepath.Join(filepath.Dir(file), "../..")), filepath.Join(t.TempDir(), "sift-agent-wrapper")
	cmd := osexec.Command("go", "build", "-tags", "sift_test", "-o", path, "./cmd/sift-agent-wrapper")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wrapper: %v\n%s", err, output)
	}
	return path
}

func waitPausedFile(t *testing.T, path string) {
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

type pausedProcessIdentity struct {
	PID, PGID int
}

func pausedIdentity(t *testing.T, db *sql.DB) (pausedProcessIdentity, pausedProcessIdentity) {
	t.Helper()
	var execution pausedProcessIdentity
	var agent sql.NullInt64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT wrapper_pid,wrapper_pgid,agent_pid FROM attempts WHERE run_id='run-1'`).Scan(&execution.PID, &execution.PGID, &agent); err == nil && execution.PID > 0 && execution.PGID > 0 {
			return execution, pausedProcessIdentity{PID: int(agent.Int64), PGID: execution.PGID}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("missing persisted wrapper identity")
	return pausedProcessIdentity{}, pausedProcessIdentity{}
}

func pausedSpawnIdentity(t *testing.T, path string) pausedProcessIdentity {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(bytes.TrimSpace(data)), "%d", &pid); err != nil || pid <= 0 {
		t.Fatalf("outer supervisor spawn record %q: %v", data, err)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("outer supervisor %d pgid: %v", pid, err)
	}
	return pausedProcessIdentity{PID: pid, PGID: pgid}
}

func pausedReplacementIdentity(t *testing.T, path string) pausedProcessIdentity {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("spawn records=%q, want exactly one outer supervisor", data)
	}
	return pausedProcessIdentity{}
}

func assertDistinctPausedProcesses(t *testing.T, outer, execution pausedProcessIdentity) {
	t.Helper()
	if outer.PID == execution.PID || outer.PGID == execution.PGID {
		t.Fatalf("outer=%+v execution=%+v must have distinct PID/PGID", outer, execution)
	}
}

func pausedParentPID(t *testing.T, pid int) int {
	t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := bytes.Fields(stat[bytes.LastIndexByte(stat, ')')+2:])
	if len(fields) < 2 {
		t.Fatalf("malformed proc stat for pid %d: %q", pid, stat)
	}
	var ppid int
	if _, err := fmt.Sscanf(string(fields[1]), "%d", &ppid); err != nil {
		t.Fatal(err)
	}
	return ppid
}

func assertPausedAgentInterval(t *testing.T, agent pausedProcessIdentity, pgid int, live bool) {
	t.Helper()
	if !live {
		if agent.PID != 0 {
			assertPausedAbsent(t, agent.PID)
		}
		return
	}
	if agent.PID == 0 {
		t.Fatal("agent interval is empty after it should have started")
	}
	assertPausedLive(t, agent.PID, pgid)
}

func assertPausedProjection(t *testing.T, db *sql.DB, execution, agent pausedProcessIdentity, point string) {
	t.Helper()
	var owners, pid, pgid int
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run-1' AND wrapper_instance_id IS NOT NULL`).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("persisted owners=%d: %v", owners, err)
	}
	var persistedAgent sql.NullInt64
	var instance, session, permit sql.NullString
	err := db.QueryRow(`SELECT a.wrapper_pid,a.wrapper_pgid,a.agent_pid,c.wrapper_instance_id,c.wrapper_session_hash,c.spawn_permit_hash FROM attempts a JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id='run-1'`).Scan(&pid, &pgid, &persistedAgent, &instance, &session, &permit)
	if err != nil || pid != execution.PID || pgid != execution.PGID || !instance.Valid || !session.Valid {
		t.Fatalf("owner/claim projection pid/pgid=%d/%d instance=%q session=%q: %v", pid, pgid, instance.String, session.String, err)
	}
	if point == "before-permit-rpc" {
		if permit.Valid || persistedAgent.Valid || agent.PID != 0 {
			t.Fatalf("pre-permit projection permit=%q persisted agent=%d observed agent=%d", permit.String, persistedAgent.Int64, agent.PID)
		}
		return
	}
	if !permit.Valid {
		t.Fatalf("post-permit projection has no permit at %s", point)
	}
	if point == "before-started-rpc" {
		if persistedAgent.Valid && int(persistedAgent.Int64) != agent.PID {
			t.Fatalf("started-pending projection persisted agent=%d observed agent=%d", persistedAgent.Int64, agent.PID)
		}
		return
	}
	if agent.PID != 0 && (!persistedAgent.Valid || int(persistedAgent.Int64) != agent.PID) {
		t.Fatalf("post-permit projection persisted agent=%d observed agent=%d", persistedAgent.Int64, agent.PID)
	}
}

func assertPausedLive(t *testing.T, pid, pgid int) {
	t.Helper()
	if !pausedProcessLive(pid) {
		t.Fatalf("pid %d is not live", pid)
	}
	if got, err := syscall.Getpgid(pid); err != nil || got != pgid {
		t.Fatalf("pid %d pgid=%d want=%d: %v", pid, got, pgid, err)
	}
}

func assertPausedAbsent(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pausedProcessLive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d remained live after cleanup", pid)
}

func pausedProcessLive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && stat[i+2] != 'Z'
}

func assertPausedStopped(t *testing.T, pid, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		assertPausedLive(t, pid, pgid)
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			// /proc/<pid>/stat: "pid (comm) state ..."
			if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'T' {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d did not reach stopped (T) state", pid)
}
