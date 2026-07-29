package runtime

import (
	"context"
	"errors"
	"os/exec"
	"sync"
)

// ErrSpawnAlreadyEntered means a wrapper has already consumed its single
// permit. It is intentionally terminal: even a failed spawn cannot reuse a
// permit because the daemon cannot make OS spawn transactional.
var ErrSpawnAlreadyEntered = errors.New("runtime: spawn permit already consumed")

// PermitGate is the wrapper-local half of ADR-010. A response may be replayed
// by the transport, but exactly one caller can cross Enter and reach Launcher.
type PermitGate struct {
	mu      sync.Mutex
	entered bool
}

// Enter consumes the one-shot before the OS spawn adapter is called.
func (g *PermitGate) Enter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entered {
		return ErrSpawnAlreadyEntered
	}
	g.entered = true
	return nil
}

// StartOnce is the only wrapper path that invokes the Agent launcher. Keeping
// the guard immediately adjacent to the call prevents a delayed/replayed
// permit response from becoming a second process.
func (g *PermitGate) StartOnce(ctx context.Context, launcher Launcher, launch AgentLaunch) (*exec.Cmd, error) {
	if err := g.Enter(); err != nil {
		return nil, err
	}
	return launcher.Start(ctx, launch)
}

// SpawnOnce is the error-only compatibility form of StartOnce.
func (g *PermitGate) SpawnOnce(ctx context.Context, launcher Launcher, launch AgentLaunch) error {
	_, err := g.StartOnce(ctx, launcher, launch)
	return err
}
