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

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// v2MakeRouter assembles the production BackendRouter for a harness cell: the
// frozen ProcessBackend (installed-wrapper contract) and the real tmux host.
// wrapperPath is the binary/script both hosts will start; crash cells pass a
// pause shim which execs the compiled wrapper, happy-path cells pass the
// compiled wrapper directly.
func v2MakeRouter(root, wrapperPath string, db *storage.DB) (BackendRouter, func(), error) {
	installed := filepath.Join(root, "installed")
	if err := os.MkdirAll(installed, 0700); err != nil {
		return nil, nil, err
	}
	contents, err := os.ReadFile(wrapperPath)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(installed, "sift-agent-wrapper"), contents, 0700); err != nil {
		return nil, nil, err
	}
	daemonPath := filepath.Join(installed, "siftd")
	if err := os.WriteFile(daemonPath, nil, 0700); err != nil {
		return nil, nil, err
	}
	processRuntime, err := runtimepkg.NewProcessBackend(daemonPath, controlplane.Version, controlplane.ProtocolMajor)
	if err != nil {
		return nil, nil, err
	}
	tmuxPath, err := osexec.LookPath("tmux")
	if err != nil {
		return nil, nil, fmt.Errorf("V2 tmux backend is required: %w", err)
	}
	verify := func(ctx context.Context, launch runtimepkg.HostLaunch) error {
		return db.VerifyLaunchBinding(ctx, launch.OperationID, launch.LeaseOwner, launch.LeaseExpiresAtMS, launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID, launch.Backend, time.Now().UnixMilli())
	}
	tmuxRuntime, err := runtimepkg.NewTmuxBackend(tmuxPath, wrapperPath, filepath.Join(root, "tmux.sock"), verify)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		cmd := osexec.Command(tmuxPath, "-f", "/dev/null", "-S", tmuxRuntime.SocketPath(), "kill-server")
		cmd.Env = runtimepkg.TmuxClientEnvironment()
		_ = cmd.Run()
	}
	return BackendRouter{config.BackendProcess: ProcessBackend{Backend: processRuntime}, config.BackendTmux: TmuxBackend{Backend: tmuxRuntime}}, cleanup, nil
}

func v2BackendFactory(t *testing.T, root, wrapperPath string, db *storage.DB) (BackendRouter, func()) {
	t.Helper()
	router, cleanup, err := v2MakeRouter(root, wrapperPath, db)
	if err != nil {
		t.Fatal(err)
	}
	return router, cleanup
}

// recordingHost counts wrapper spawns and retains the last spawn handle so a
// crash cell can kill the exact component the worker started.
type recordingHost struct {
	inner  Backend
	spawns *int
	proc   *os.Process
}

func (h *recordingHost) WrapperPath() string { return h.inner.WrapperPath() }

func (h *recordingHost) Spawn(ctx context.Context, launch runtimepkg.HostLaunch) (*os.Process, error) {
	proc, err := h.inner.Spawn(ctx, launch)
	if err == nil {
		*h.spawns++
		h.proc = proc
	}
	return proc, err
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
