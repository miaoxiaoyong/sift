// Package launchworker owns the sole production consumer of launch_agent.
package launchworker

import (
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
	if err != nil {
		return err
	}
	agent, ok := w.agent(dispatch.AgentID)
	if !ok {
		return fmt.Errorf("launch worker: configured agent %q not found", dispatch.AgentID)
	}
	runDir := filepath.Join(w.Root, "runs", dispatch.RunID, "attempts", fmt.Sprintf("%d", dispatch.AttemptNo))
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	bootstrap := runtime.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: dispatch.RunID, AttemptNo: dispatch.AttemptNo, Generation: dispatch.Generation, DispatchID: dispatch.DispatchID, BootstrapNonce: dispatch.BootstrapNonce, RunToken: dispatch.RunToken, RunDir: runDir, WorktreePath: dispatch.WorktreePath, Agent: runtime.BootstrapAgent{ID: agent.ID, Executable: agent.Executable, Args: agent.Args, TaskTransport: string(agent.TaskTransport)}, TaskSpecSnapshotID: dispatch.TaskSpecID, TaskSpec: json.RawMessage(dispatch.TaskSpec)}
	b, err := json.Marshal(bootstrap)
	if err != nil {
		return err
	}
	path := filepath.Join(runDir, "bootstrap.json")
	// File creation and spawn are separate external effects; fence both.
	if err := w.DB.RevalidateLaunchLease(ctx, *claim, now().UnixMilli()); err != nil {
		return err
	}
	if err := runtime.WriteControlFile(path, b); err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if err := w.DB.RecordBootstrapDigest(ctx, *claim, dispatch.DispatchID, hex.EncodeToString(sum[:]), now().UnixMilli()); err != nil {
		return err
	}
	if err := w.DB.RevalidateLaunchLease(ctx, *claim, now().UnixMilli()); err != nil {
		return err
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
func randomSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func randomID() string { return randomSecret()[:32] }
