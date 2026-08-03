package launchworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
			if tc.backend == config.BackendTmux {
				worker.hooks.afterBootstrapDigest = func() error {
					return runtime.WriteControlFile(filepath.Join(root, "runs", "run-1", "attempts", "1", "bootstrap.json"), []byte(`{"run_id":"replaced","attempt_no":9,"generation":9,"dispatch_id":"replaced"}`))
				}
			}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}

			selected, other := processHost, tmuxHost
			if tc.backend == config.BackendTmux {
				selected, other = tmuxHost, processHost
			}
			if len(selected.calls) != 1 {
				t.Fatalf("%s host calls = %d, want exactly 1", tc.backend, len(selected.calls))
			}
			if len(other.calls) != 0 {
				t.Fatalf("%s host calls = %d, want 0", currentBackend, len(other.calls))
			}
			if tc.backend == config.BackendProcess {
				assertBaselineProcessBootstrap(t, selected.calls[0], root, agent)
			} else {
				call := selected.calls[0]
				if call.launch.Backend != string(config.BackendTmux) || call.launch.RunID != "run-1" || call.launch.AttemptNo != 1 || call.launch.Generation != 1 || call.launch.DispatchID == "" || call.launch.WrapperPath != tmuxHost.WrapperPath() {
					t.Fatalf("tmux frozen launch = %#v, want durable dispatch identity", call.launch)
				}
				if !strings.Contains(string(call.contents), `"replaced"`) {
					t.Fatalf("test did not replace bootstrap after digest: %q", call.contents)
				}
			}
		})
	}
}

func TestLaunchWorkerTmuxReclaimFencesDurableLeaseDrift(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift bool
	}{
		{name: "lease_switch_before_convergence", drift: true},
		{name: "unchanged_lease_converges", drift: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			nowMS := time.Now().Truncate(time.Millisecond).UnixMilli()
			root := t.TempDir()
			db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(root, "sift.db"), BinaryVersion: controlplane.Version, Now: time.UnixMilli(nowMS)})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.SeedProjectForTest(ctx, "cfg", "project", nowMS); err != nil {
				t.Fatal(err)
			}
			if err := db.SeedLaunchRunForTest(ctx, "run-1", "project", "cfg", nowMS, "/worktree/baseline"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecForTest(ctx, `UPDATE attempts SET backend='tmux' WHERE run_id='run-1' AND attempt_no=1`); err != nil {
				t.Fatal(err)
			}
			boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), nowMS)
			if err != nil {
				t.Fatal(err)
			}
			completeLaunchRecovery(t, db, boot, nowMS+1, "supervise")

			tmux := filepath.Join(root, "tmux")
			wrapper := filepath.Join(root, "wrapper")
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			stateScript := "#!/bin/sh\ncase \"$5\" in\nnew-session) printf '%s\\n' \"${10}\" > \"$0.binding\"; exit 1 ;;\nhas-session) exit 0 ;;\nshow-environment) binding=$(sed 's/^SIFT_TMUX_BINDING=//' \"$0.binding\"); printf 'SIFT_TMUX_BINDING=%s\\n' \"$binding\" ;;\nlist-panes) printf '0\\n' ;;\nshow-options) printf 'off\\n' ;;\n*) exit 99 ;;\nesac\n"
			if err := os.WriteFile(tmux, []byte(stateScript), 0755); err != nil {
				t.Fatal(err)
			}
			verify := func(ctx context.Context, launch runtime.HostLaunch) error {
				if tc.drift {
					if _, err := db.ExecForTest(ctx, `UPDATE outbox_operations SET lease_owner='replacement' WHERE id=?`, launch.OperationID); err != nil {
						return err
					}
				}
				return db.VerifyLaunchBinding(ctx, launch.OperationID, launch.LeaseOwner, launch.LeaseExpiresAtMS, launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID, launch.Backend, nowMS+5)
			}
			tmuxBackend, err := runtime.NewTmuxBackend(tmux, wrapper, filepath.Join(root, "tmux.sock"), verify)
			if err != nil {
				t.Fatal(err)
			}
			worker := &Worker{
				DB: db, BootID: boot, WorkerID: "lease-worker", Root: root, Lease: time.Minute,
				Now:      func() time.Time { return time.UnixMilli(nowMS + 4) },
				Backends: BackendRouter{config.BackendTmux: TmuxBackend{Backend: tmuxBackend}},
				Agents:   []config.Agent{{ID: "agent", Executable: "/bin/echo", Args: []string{"baseline"}, TaskTransport: config.TaskTransportStdin, Backend: config.BackendTmux}},
			}
			err = worker.RunOnce(ctx)
			if tc.drift {
				var conflict *runtime.TmuxSessionConflictError
				if !errors.As(err, &conflict) || !errors.Is(err, storage.ErrRejectedStaleWorker) {
					t.Fatalf("drifted launch error = %v, want tmux conflict wrapping stale lease", err)
				}
			} else if err != nil {
				t.Fatalf("unchanged launch error = %v, want reclaim convergence", err)
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
	if err := compareProcessBootstrapGolden(call.contents, root, agent); err != nil {
		t.Fatal(err)
	}
}

// TestProcessBootstrapGoldenRejectsByteDrift keeps the regression assertion
// honest: semantic JSON decoding must not make either kind of byte drift pass.
func TestProcessBootstrapGoldenRejectsByteDrift(t *testing.T) {
	root := "/worktree/root"
	agent := config.Agent{ID: "agent", Executable: "/bin/echo", Args: []string{"baseline bootstrap"}, TaskTransport: config.TaskTransportStdin}
	golden := processBootstrapGolden(root, agent)
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "unknown_json_field", mutate: func(b []byte) []byte {
			return append([]byte(strings.TrimSuffix(string(b), "}")), []byte(`,"unknown":true}`)...)
		}},
		{name: "trailing_byte", mutate: func(b []byte) []byte { return append(b, '\n') }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := compareProcessBootstrapGolden(tc.mutate(golden), root, agent); err == nil {
				t.Fatalf("negative sample unexpectedly matched byte-for-byte golden")
			}
		})
	}
}

var dynamicBootstrapSlot = regexp.MustCompile(`"(dispatch_id|bootstrap_nonce|run_token)":"[^"]*"`)

func compareProcessBootstrapGolden(contents []byte, root string, agent config.Agent) error {
	want := processBootstrapGolden(root, agent)
	got := dynamicBootstrapSlot.ReplaceAll(contents, []byte(`"$1":"<dynamic>"`))
	want = dynamicBootstrapSlot.ReplaceAll(want, []byte(`"$1":"<dynamic>"`))
	if string(got) != string(want) {
		return fmt.Errorf("process bootstrap bytes differ from pre-router golden: got %q, want %q", got, want)
	}
	return nil
}

// processBootstrapGolden is the explicit pre-router wire baseline. Only the
// three dispatch-generated slots are normalized by compareProcessBootstrapGolden.
func processBootstrapGolden(root string, agent config.Agent) []byte {
	quote := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	args, _ := json.Marshal(agent.Args)
	return []byte(fmt.Sprintf(`{"schema_version":2,"protocol_major":%d,"protocol_minor":%d,"daemon_version":%s,"wrapper_version":%s,"run_id":"run-1","attempt_no":1,"generation":1,"dispatch_id":"<dynamic>","bootstrap_nonce":"<dynamic>","run_token":"<dynamic>","run_dir":%s,"worktree_path":"/worktree/baseline","agent":{"id":%s,"executable":%s,"args":%s,"task_transport":%s},"task_spec_snapshot_id":"task-run-1","task_spec":{"title":"crash-suite"}}`, controlplane.ProtocolMajor, controlplane.ProtocolMinor, quote(controlplane.Version), quote(controlplane.Version), quote(filepath.Join(root, "runs", "run-1", "attempts", "1")), quote(agent.ID), quote(agent.Executable), args, quote(string(agent.TaskTransport))))
}

type recordingBackend struct {
	calls []recordingBackendCall
}

type recordingBackendCall struct {
	path     string
	contents []byte
	launch   runtime.HostLaunch
}

func (b *recordingBackend) WrapperPath() string { return "/test/sift-agent-wrapper" }

func (b *recordingBackend) Spawn(_ context.Context, launch runtime.HostLaunch) (*os.Process, error) {
	contents, err := os.ReadFile(launch.BootstrapPath)
	if err != nil {
		return nil, err
	}
	b.calls = append(b.calls, recordingBackendCall{path: launch.BootstrapPath, contents: contents, launch: launch})
	return nil, nil
}
