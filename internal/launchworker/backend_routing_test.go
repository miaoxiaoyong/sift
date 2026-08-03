package launchworker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TestLaunchDispatchBackendRouting proves launch routing uses the backend
// frozen on attempts, rather than the current Agent configuration, on both
// initial dispatch preparation and prepared-dispatch resumption.
func TestLaunchDispatchBackendRouting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backend  config.Backend
		prepared bool
	}{
		{name: "prepare_process", backend: config.BackendProcess},
		{name: "prepare_tmux", backend: config.BackendTmux},
		{name: "resume_process", backend: config.BackendProcess, prepared: true},
		{name: "resume_tmux", backend: config.BackendTmux, prepared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			nowMS := time.Now().Truncate(time.Millisecond).UnixMilli()
			root := t.TempDir()
			db, err := storage.Open(ctx, storage.OpenConfig{
				Path: filepath.Join(root, "sift.db"), BinaryVersion: controlplane.Version, Now: time.UnixMilli(nowMS),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.SeedProjectForTest(ctx, "cfg", "project", nowMS); err != nil {
				t.Fatal(err)
			}
			if err := db.SeedLaunchRunForTest(ctx, "run-1", "project", "cfg", nowMS, "/worktree/baseline"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecForTest(ctx, `UPDATE attempts SET backend=? WHERE run_id='run-1' AND attempt_no=1`, tc.backend); err != nil {
				t.Fatal(err)
			}
			boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), nowMS)
			if err != nil {
				t.Fatal(err)
			}
			completeLaunchRecovery(t, db, boot, nowMS+1, "supervise")

			currentBackend := config.BackendProcess
			if tc.backend == config.BackendProcess {
				currentBackend = config.BackendTmux
			}
			agent := config.Agent{
				ID: "agent", Executable: "/bin/echo", Args: []string{"baseline bootstrap"},
				TaskTransport: config.TaskTransportStdin, Backend: currentBackend,
			}
			if tc.prepared {
				prepareRoutingDispatch(t, ctx, db, boot, root, nowMS+2, agent)
			}

			processHost := &recordingBackend{}
			tmuxHost := &recordingBackend{}
			worker := &Worker{
				DB: db, BootID: boot, WorkerID: "routing-worker", Root: root, Lease: time.Minute,
				Now: func() time.Time { return time.UnixMilli(nowMS + 4) },
				Backends: BackendRouter{
					config.BackendProcess: processHost,
					config.BackendTmux:    tmuxHost,
				},
				Agents: []config.Agent{agent},
			}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}

			selected, other := processHost, tmuxHost
			if tc.backend == config.BackendTmux {
				selected, other = tmuxHost, processHost
			}
			if len(selected.calls) == 0 {
				t.Fatalf("%s host was not called", tc.backend)
			}
			if len(other.calls) != 0 {
				t.Fatalf("%s host calls = %d, want 0", currentBackend, len(other.calls))
			}
			if tc.backend == config.BackendProcess {
				assertBaselineProcessBootstrap(t, selected.calls[0], root, agent)
			}
		})
	}
}

func prepareRoutingDispatch(t *testing.T, ctx context.Context, db *storage.DB, boot, root string, nowMS int64, agent config.Agent) {
	t.Helper()
	claim, err := db.ClaimLaunchOperation(ctx, boot, "prepared-worker", nowMS, 1)
	if err != nil || claim == nil {
		t.Fatalf("claim prepared dispatch = %#v, %v", claim, err)
	}
	dispatch, err := db.PrepareLaunchDispatch(ctx, *claim, "prepared-dispatch", strings.Repeat("a", 64), strings.Repeat("b", 64), nowMS)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "runs", dispatch.RunID, "attempts", "1")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	bootstrap := runtime.Bootstrap{
		SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor,
		DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version,
		RunID: dispatch.RunID, AttemptNo: dispatch.AttemptNo, Generation: dispatch.Generation,
		DispatchID: dispatch.DispatchID, BootstrapNonce: dispatch.BootstrapNonce, RunToken: dispatch.RunToken,
		RunDir: runDir, WorktreePath: dispatch.WorktreePath,
		Agent:              runtime.BootstrapAgent{ID: agent.ID, Executable: agent.Executable, Args: agent.Args, TaskTransport: string(agent.TaskTransport)},
		TaskSpecSnapshotID: dispatch.TaskSpecID, TaskSpec: dispatch.TaskSpec,
	}
	contents, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.WriteControlFile(filepath.Join(runDir, "bootstrap.json"), contents); err != nil {
		t.Fatal(err)
	}
}

func assertBaselineProcessBootstrap(t *testing.T, call recordingBackendCall, root string, agent config.Agent) {
	t.Helper()
	wantPath := filepath.Join(root, "runs", "run-1", "attempts", "1", "bootstrap.json")
	if call.path != wantPath {
		t.Fatalf("process bootstrap path = %q, want %q", call.path, wantPath)
	}
	var bootstrap runtime.Bootstrap
	if err := json.Unmarshal(call.contents, &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.SchemaVersion != 2 || bootstrap.ProtocolMajor != controlplane.ProtocolMajor || bootstrap.ProtocolMinor != controlplane.ProtocolMinor ||
		bootstrap.DaemonVersion != controlplane.Version || bootstrap.WrapperVersion != controlplane.Version ||
		bootstrap.RunID != "run-1" || bootstrap.AttemptNo != 1 || bootstrap.Generation != 1 ||
		bootstrap.RunDir != filepath.Dir(call.path) || bootstrap.WorktreePath != "/worktree/baseline" ||
		bootstrap.Agent.ID != agent.ID || bootstrap.Agent.Executable != agent.Executable ||
		bootstrap.Agent.TaskTransport != string(agent.TaskTransport) ||
		!argsEqual(bootstrap.Agent.Args, agent.Args) || bootstrap.TaskSpecSnapshotID != "task-run-1" ||
		string(bootstrap.TaskSpec) != `{"title":"crash-suite"}` || bootstrap.DispatchID == "" || bootstrap.BootstrapNonce == "" || bootstrap.RunToken == "" {
		t.Fatalf("process bootstrap = %#v, want baseline launch contents", bootstrap)
	}
}

type recordingBackend struct {
	calls []recordingBackendCall
}

type recordingBackendCall struct {
	path     string
	contents []byte
}

func (b *recordingBackend) Spawn(_ context.Context, bootstrap string) (*os.Process, error) {
	contents, err := os.ReadFile(bootstrap)
	if err != nil {
		return nil, err
	}
	b.calls = append(b.calls, recordingBackendCall{path: bootstrap, contents: contents})
	return nil, nil
}
