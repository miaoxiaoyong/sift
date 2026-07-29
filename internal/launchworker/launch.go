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

type Backend interface {
	Spawn(context.Context, string) (*os.Process, error)
}

// ProcessBackend adapts runtime.ProcessBackend without exposing exec.Cmd.
type ProcessBackend struct{ Backend *runtime.ProcessBackend }

func (b ProcessBackend) Spawn(ctx context.Context, path string) (*os.Process, error) {
	c, err := b.Backend.Spawn(ctx, path)
	if err != nil {
		return nil, err
	}
	return c.Process, nil
}

type Worker struct {
	DB                     *storage.DB
	BootID, WorkerID, Root string
	Lease                  time.Duration
	Now                    func() time.Time
	Backend                Backend
	Agents                 []config.Agent
	hooks                  workerHooks
}

type workerHooks struct {
	afterBootstrapWrite  func() error
	afterBootstrapDigest func() error
	beforeSpawn          func() error
}

func (w *Worker) RunOnce(ctx context.Context) error {
	if w.DB == nil || w.Backend == nil || w.BootID == "" || w.Root == "" {
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
	dispatch, err := w.DB.PrepareLaunchDispatch(ctx, *claim, randomID(), randomSecret(), randomSecret(), now().UnixMilli())
	resumed := errors.Is(err, storage.ErrLaunchDispatchPrepared)
	if err != nil && !resumed {
		return err
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
		agent, ok := w.agent(dispatch.AgentID)
		if !ok || !matchesDispatch(bootstrap, dispatch, agent, filepath.Dir(path)) {
			return errors.New("launch worker: prepared bootstrap does not match dispatch")
		}
	} else {
		agent, ok := w.agent(dispatch.AgentID)
		if !ok {
			return fmt.Errorf("launch worker: configured agent %q not found", dispatch.AgentID)
		}
		runDir := filepath.Join(w.Root, "runs", dispatch.RunID, "attempts", fmt.Sprintf("%d", dispatch.AttemptNo))
		if err := os.MkdirAll(runDir, 0700); err != nil {
			return err
		}
		bootstrap := runtime.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: dispatch.RunID, AttemptNo: dispatch.AttemptNo, Generation: dispatch.Generation, DispatchID: dispatch.DispatchID, BootstrapNonce: dispatch.BootstrapNonce, RunToken: dispatch.RunToken, RunDir: runDir, WorktreePath: dispatch.WorktreePath, Agent: runtime.BootstrapAgent{ID: agent.ID, Executable: agent.Executable, Args: agent.Args, TaskTransport: string(agent.TaskTransport)}, TaskSpecSnapshotID: dispatch.TaskSpecID, TaskSpec: json.RawMessage(dispatch.TaskSpec)}
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
	if _, err := w.Backend.Spawn(ctx, path); err != nil {
		return fmt.Errorf("launch worker: spawn wrapper: %w", err)
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

func matchesDispatch(b runtime.Bootstrap, d storage.LaunchDispatch, agent config.Agent, runDir string) bool {
	return b.SchemaVersion == 2 && b.ProtocolMajor == controlplane.ProtocolMajor && b.ProtocolMinor == controlplane.ProtocolMinor &&
		b.DaemonVersion == controlplane.Version && b.WrapperVersion == controlplane.Version &&
		b.RunID == d.RunID && b.AttemptNo == d.AttemptNo && b.Generation == d.Generation &&
		b.DispatchID == d.DispatchID && b.BootstrapNonce == d.BootstrapNonce && b.RunToken == d.RunToken &&
		b.RunDir == runDir && b.WorktreePath == d.WorktreePath && b.TaskSpecSnapshotID == d.TaskSpecID &&
		b.Agent.ID == agent.ID && b.Agent.Executable == agent.Executable && b.Agent.TaskTransport == string(agent.TaskTransport) &&
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
