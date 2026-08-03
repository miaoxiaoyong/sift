// Package launchworker owns the sole production consumer of launch_agent.
package launchworker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// Backend is the wrapper-host seam. It owns only where the wrapper is
// started; completion, ownership, and Agent observation remain in the
// existing handoff/result paths.
type Backend interface {
	WrapperPath() string
	Spawn(context.Context, runtime.HostLaunch) (*os.Process, error)
}

// BackendRouter selects a wrapper host using the backend frozen on the
// attempt. It deliberately has no observer or lifecycle methods.
type BackendRouter map[config.Backend]Backend

func (r BackendRouter) Spawn(ctx context.Context, launch runtime.HostLaunch) (*os.Process, error) {
	backend := config.Backend(launch.Backend)
	host, ok := r[backend]
	if !ok || host == nil {
		return nil, fmt.Errorf("launch worker: backend %q is unavailable", backend)
	}
	return host.Spawn(ctx, launch)
}

// ProcessBackend adapts runtime.ProcessBackend without exposing exec.Cmd.
type ProcessBackend struct{ Backend *runtime.ProcessBackend }

func (b ProcessBackend) WrapperPath() string {
	if b.Backend == nil {
		return ""
	}
	return b.Backend.WrapperPath()
}

func (b ProcessBackend) Spawn(ctx context.Context, launch runtime.HostLaunch) (*os.Process, error) {
	if b.Backend == nil {
		return nil, errors.New("launch worker: invalid frozen process launch")
	}
	if err := launch.ValidateFor(string(config.BackendProcess), b.Backend.WrapperPath()); err != nil {
		return nil, fmt.Errorf("launch worker: invalid frozen process launch: %w", err)
	}
	c, err := b.Backend.Spawn(ctx, launch.BootstrapPath)
	if err != nil {
		return nil, err
	}
	return c.Process, nil
}

// TmuxBackend adapts the runtime tmux host without exposing its command
// process to the launch worker.
type TmuxBackend struct{ Backend *runtime.TmuxBackend }

func (b TmuxBackend) WrapperPath() string {
	if b.Backend == nil {
		return ""
	}
	return b.Backend.WrapperPath()
}

func (b TmuxBackend) Spawn(ctx context.Context, launch runtime.HostLaunch) (*os.Process, error) {
	if b.Backend == nil {
		return nil, errors.New("launch worker: tmux backend is not initialized")
	}
	return b.Backend.Spawn(ctx, launch)
}

type Worker struct {
	DB                     *storage.DB
	BootID, WorkerID, Root string
	Lease                  time.Duration
	Now                    func() time.Time
	// Backend is retained as a test/compatibility seam for process-only
	// callers. Production wiring should use Backends so routing is explicit.
	Backend  Backend
	Backends BackendRouter
	Agents   []config.Agent
	// FrozenAgentsRequired is enabled by production wiring. Legacy fixtures may
	// omit agent definitions from synthetic config snapshots, but production
	// must never fall back to the daemon's current configuration.
	FrozenAgentsRequired      bool
	QualificationProbeTimeout time.Duration
	hooks                     workerHooks
}

type workerHooks struct {
	afterClaim           func() error
	afterPrepare         func() error
	afterBootstrapWrite  func() error
	afterBootstrapDigest func() error
	beforeSpawn          func() error
}

func (w *Worker) RunOnce(ctx context.Context) error {
	if w.DB == nil || w.Backend == nil && w.Backends == nil || w.BootID == "" || w.Root == "" {
		return errors.New("launch worker: incomplete configuration")
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	lease := w.Lease
	if lease <= 0 {
		return errors.New("launch worker: lease must be positive")
	}
	claim, err := w.DB.ClaimLaunchOperation(ctx, w.BootID, w.WorkerID, now().UnixMilli(), lease.Milliseconds())
	if err != nil || claim == nil {
		return err
	}
	if w.hooks.afterClaim != nil {
		if err := w.hooks.afterClaim(); err != nil {
			return err
		}
	}
	agent, err := w.DB.FrozenLaunchAgent(ctx, *claim, now().UnixMilli())
	if err != nil {
		if w.FrozenAgentsRequired {
			return err
		}
		// Compatibility for old test-only database seeds which predate frozen
		// agent definitions. Production sets FrozenAgentsRequired and cannot
		// take this branch.
		agentID, idErr := w.DB.LaunchAgentID(ctx, *claim, now().UnixMilli())
		if idErr != nil {
			return err
		}
		var ok bool
		agent, ok = w.agent(agentID)
		if !ok {
			return err
		}
	}
	qualification, err := runtime.BuildQualification(runtime.QualificationInput{AgentID: agent.ID, Args: agent.Args, TaskTransport: string(agent.TaskTransport), VersionArgs: agent.VersionArgs, Executable: agent.Executable, Context: ctx, ProbeTimeout: w.QualificationProbeTimeout})
	if err != nil {
		return fmt.Errorf("launch worker: build topology qualification: %w", err)
	}
	dispatch, err := w.DB.PrepareLaunchDispatchWithQualification(ctx, *claim, randomID(), randomSecret(), randomSecret(), qualification.Key, now().UnixMilli())
	resumed := errors.Is(err, storage.ErrLaunchDispatchPrepared)
	if err != nil && !resumed {
		return err
	}
	if !resumed && w.hooks.afterPrepare != nil {
		if err := w.hooks.afterPrepare(); err != nil {
			return err
		}
	}
	var path string
	var b []byte
	if resumed {
		path = filepath.Join(w.Root, "runs", claim.RunID, "attempts", fmt.Sprintf("%d", *claim.AttemptNo), "bootstrap.json")
		b, err = readBootstrap(path)
		if err != nil {
			return err
		}
		var bootstrap runtime.Bootstrap
		if err := json.Unmarshal(b, &bootstrap); err != nil {
			return fmt.Errorf("launch worker: decode prepared bootstrap: %w", err)
		}
		sum := sha256.Sum256(b)
		dispatch, err = w.DB.ResumeLaunchDispatch(ctx, *claim, bootstrap.DispatchID, bootstrap.BootstrapNonce, bootstrap.RunToken, hex.EncodeToString(sum[:]), now().UnixMilli())
		if err != nil {
			return err
		}
		if dispatch.AgentID != agent.ID || !matchesDispatch(bootstrap, dispatch, agent, qualification, filepath.Dir(path)) {
			// A resumed prepared dispatch may carry an old key after the executable
			// was replaced while its bootstrap survived. Never leave that key for
			// recovery to treat as verified absence authority.
			if clearErr := w.DB.ClearLaunchQualification(ctx, *claim, dispatch.TopologyQualificationKey, now().UnixMilli()); clearErr != nil {
				return fmt.Errorf("launch worker: invalidate prepared qualification: %w", clearErr)
			}
			return errors.New("launch worker: prepared bootstrap does not match dispatch")
		}
	} else {
		if dispatch.AgentID != agent.ID {
			return errors.New("launch worker: prepared dispatch agent changed")
		}
		runDir := filepath.Join(w.Root, "runs", dispatch.RunID, "attempts", fmt.Sprintf("%d", dispatch.AttemptNo))
		if err := os.MkdirAll(runDir, 0700); err != nil {
			return err
		}
		bootstrap := runtime.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: dispatch.RunID, AttemptNo: dispatch.AttemptNo, Generation: dispatch.Generation, DispatchID: dispatch.DispatchID, BootstrapNonce: dispatch.BootstrapNonce, RunToken: dispatch.RunToken, RunDir: runDir, WorktreePath: dispatch.WorktreePath, Agent: runtime.BootstrapAgent{ID: agent.ID, Executable: qualification.ExecutablePath, ExecutableSHA256: qualification.ExecutableSHA256, Args: agent.Args, TaskTransport: string(agent.TaskTransport)}, TaskSpecSnapshotID: dispatch.TaskSpecID, TaskSpec: json.RawMessage(dispatch.TaskSpec)}
		b, err = json.Marshal(bootstrap)
		if err != nil {
			return err
		}
		path = filepath.Join(runDir, "bootstrap.json")
		if err := w.DB.RevalidateLaunchLease(ctx, *claim, now().UnixMilli()); err != nil {
			return err
		}
		if err := runtime.WriteControlFile(path, b); err != nil {
			return err
		}
		if w.hooks.afterBootstrapWrite != nil {
			if err := w.hooks.afterBootstrapWrite(); err != nil {
				return err
			}
		}
	}
	sum := sha256.Sum256(b)
	if err := w.DB.RecordBootstrapDigest(ctx, *claim, dispatch.DispatchID, hex.EncodeToString(sum[:]), now().UnixMilli()); err != nil {
		return err
	}
	if w.hooks.afterBootstrapDigest != nil {
		if err := w.hooks.afterBootstrapDigest(); err != nil {
			return err
		}
	}
	if err := w.DB.RevalidateLaunchLease(ctx, *claim, now().UnixMilli()); err != nil {
		return err
	}
	if w.hooks.beforeSpawn != nil {
		if err := w.hooks.beforeSpawn(); err != nil {
			return err
		}
	}
	// Recheck after all durable writes and immediately before the wrapper host
	// accepts the launch. The wrapper performs the same check at its Launcher
	// boundary, so replacement cannot run new bytes under this key.
	if err := runtime.ValidateQualificationExecutable(qualification); err != nil {
		if clearErr := w.DB.ClearLaunchQualification(ctx, *claim, qualification.Key, now().UnixMilli()); clearErr != nil {
			return fmt.Errorf("launch worker: invalidate changed qualification: %w", clearErr)
		}
		return fmt.Errorf("launch worker: qualification executable changed: %w", err)
	}
	backend := config.Backend(dispatch.Backend)
	if backend == "" {
		// Older fixtures did not project backend from attempts. Keep their
		// process-only behavior while production always uses the frozen value.
		backend = config.BackendProcess
		if agent, ok := w.agent(dispatch.AgentID); ok {
			backend = agent.Backend
		}
	}
	launch := runtime.HostLaunch{
		Backend: string(backend), RunID: dispatch.RunID, AttemptNo: dispatch.AttemptNo,
		Generation: dispatch.Generation, DispatchID: dispatch.DispatchID, BootstrapPath: path,
		OperationID: claim.ID, LeaseOwner: claim.LeaseOwner, LeaseExpiresAtMS: claim.LeaseExpiresAtMS,
	}
	if w.Backends != nil {
		host, ok := w.Backends[backend]
		if !ok || host == nil {
			return fmt.Errorf("launch worker: backend %q is unavailable", backend)
		}
		launch.WrapperPath = host.WrapperPath()
		if _, err := host.Spawn(ctx, launch); err != nil {
			return fmt.Errorf("launch worker: spawn wrapper: %w", err)
		}
	} else if w.Backend != nil {
		// The legacy single-host seam remains process-only for pre-router tests.
		launch.Backend = string(config.BackendProcess)
		launch.WrapperPath = w.Backend.WrapperPath()
		if _, err := w.Backend.Spawn(ctx, launch); err != nil {
			return fmt.Errorf("launch worker: spawn wrapper: %w", err)
		}
	} else {
		return errors.New("launch worker: no backend host configured")
	}
	return nil
}

func (w *Worker) agent(id string) (config.Agent, bool) {
	for _, a := range w.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return config.Agent{}, false
}
func readBootstrap(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("launch worker: unsafe prepared bootstrap")
	}
	return os.ReadFile(path)
}

func matchesDispatch(b runtime.Bootstrap, d storage.LaunchDispatch, agent config.Agent, qualification runtime.Qualification, runDir string) bool {
	return b.SchemaVersion == 2 && b.ProtocolMajor == controlplane.ProtocolMajor && b.ProtocolMinor == controlplane.ProtocolMinor &&
		b.DaemonVersion == controlplane.Version && b.WrapperVersion == controlplane.Version &&
		b.RunID == d.RunID && b.AttemptNo == d.AttemptNo && b.Generation == d.Generation &&
		b.DispatchID == d.DispatchID && b.BootstrapNonce == d.BootstrapNonce && b.RunToken == d.RunToken &&
		b.RunDir == runDir && b.WorktreePath == d.WorktreePath && b.TaskSpecSnapshotID == d.TaskSpecID &&
		b.Agent.ID == agent.ID && b.Agent.Executable == qualification.ExecutablePath && b.Agent.ExecutableSHA256 == qualification.ExecutableSHA256 && b.Agent.TaskTransport == string(agent.TaskTransport) &&
		bytes.Equal(b.TaskSpec, d.TaskSpec) && argsEqual(b.Agent.Args, agent.Args)
}

func argsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func randomSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func randomID() string { return randomSecret()[:32] }
