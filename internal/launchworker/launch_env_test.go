package launchworker

import (
	"encoding/json"
	"testing"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/controlplane"
	"github.com/xsift/sift/internal/runtime"
	"github.com/xsift/sift/internal/storage"
)

// Issue #993: a resumed prepared bootstrap must match the frozen agent's
// launch_env; a drifted snapshot invalidates the prepared dispatch instead of
// launching under a stale environment.

func TestMatchesDispatchComparesFrozenLaunchEnv(t *testing.T) {
	agent := config.Agent{
		ID: "agent", Executable: "/bin/sh", Args: []string{"-c"}, TaskTransport: config.TaskTransportStdin,
		LaunchEnv: map[string]string{"HOME": "/frozen/home", "PATH": "/frozen/bin"},
	}
	dispatch := storage.LaunchDispatch{
		RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch",
		BootstrapNonce: "nonce", RunToken: "token", AgentID: "agent",
		TaskSpecID: "task-1", WorktreePath: "/work", TaskSpec: []byte(`{}`),
	}
	qualification := runtime.Qualification{ExecutablePath: "/bin/sh", ExecutableSHA256: "sha"}
	runDir := "/runs/run-1/attempts/1"
	mk := func(env map[string]string) runtime.Bootstrap {
		return runtime.Bootstrap{
			SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor,
			DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version,
			RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch",
			BootstrapNonce: "nonce", RunToken: "token", RunDir: runDir, WorktreePath: "/work",
			Agent: runtime.BootstrapAgent{
				ID: "agent", Executable: "/bin/sh", ExecutableSHA256: "sha",
				Args: []string{"-c"}, TaskTransport: "stdin", LaunchEnv: env,
			},
			TaskSpecSnapshotID: "task-1", TaskSpec: json.RawMessage(`{}`),
		}
	}
	if !matchesDispatch(mk(agent.LaunchEnv), dispatch, agent, qualification, runDir) {
		t.Fatal("identical frozen launch_env must match")
	}
	if matchesDispatch(mk(nil), dispatch, agent, qualification, runDir) {
		t.Fatal("bootstrap without launch_env must not match a frozen agent carrying one")
	}
	if matchesDispatch(mk(map[string]string{"HOME": "/frozen/home", "PATH": "/other"}), dispatch, agent, qualification, runDir) {
		t.Fatal("drifted PATH in prepared bootstrap must not match")
	}
}
