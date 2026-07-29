package daemon

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
	}{
		{"control-initial", "before-permit-rpc", "while :; do sleep 1; done", "starting"},
		{"control-rewrite", "before-started-rpc", "while :; do sleep 1; done", "spawning"},
		{"result-before-rename", "before-result-rename", "exit 0", "running"},
		{"result-after-rename", "after-result-rename", "exit 0", "running"},
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
			boot, err := db.StartDaemonBoot(ctx, "cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
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
			pid, pgid, agent := pausedIdentity(t, check)
			assertPausedLive(t, pid, pgid)
			var phase string
			if err := check.QueryRow(`SELECT phase FROM attempts WHERE run_id='run-1'`).Scan(&phase); err != nil || phase != tc.phase {
				t.Fatalf("pause phase=%q want=%q: %v", phase, tc.phase, err)
			}
			if agent != 0 {
				assertPausedLive(t, agent, pgid)
			}

			restartedAt := now.Add(time.Second)
			restarted, err := db.StartDaemonBoot(ctx, "cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), restartedAt.UnixMilli())
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
			if candidate.count != 0 || lineCount(spawns) != 1 {
				t.Fatalf("new/global spawns=%d/%d, want 0/1", candidate.count, lineCount(spawns))
			}
			assertPausedLive(t, pid, pgid)
			if agent != 0 {
				assertPausedLive(t, agent, pgid)
			}
			var owners int
			if err := check.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run-1' AND wrapper_instance_id IS NOT NULL`).Scan(&owners); err != nil || owners != 1 {
				t.Fatalf("persisted owners=%d: %v", owners, err)
			}
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func pausedRecoveryCoordinator(db *storage.DB, root string, now time.Time) *TerminationCoordinator {
	return &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: runtimepkg.PlatformProcessInspector{}, Signaler: runtimepkg.UnixProcessSignaler{}}, Runtime: config.Runtime{HeartbeatStaleAfter: time.Hour}, ProcessGroupVerified: func(string) bool { return true }, AttentionDailyQuota: recoveryQuota(), ControlRoot: root, Now: func() time.Time { return now }}
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
		if err := os.WriteFile(b.spawns, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0600); err != nil {
			return nil, err
		}
	}
	return cmd.Process, nil
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
func pausedIdentity(t *testing.T, db *sql.DB) (int, int, int) {
	t.Helper()
	var pid, pgid int
	var agent sql.NullInt64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT wrapper_pid,wrapper_pgid,agent_pid FROM attempts WHERE run_id='run-1'`).Scan(&pid, &pgid, &agent); err == nil && pid > 0 && pgid > 0 {
			return pid, pgid, int(agent.Int64)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("missing persisted wrapper identity")
	return 0, 0, 0
}
func assertPausedLive(t *testing.T, pid, pgid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("pid %d is not live: %v", pid, err)
	}
	if got, err := syscall.Getpgid(pid); err != nil || got != pgid {
		t.Fatalf("pid %d pgid=%d want=%d: %v", pid, got, pgid, err)
	}
}
func lineCount(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
