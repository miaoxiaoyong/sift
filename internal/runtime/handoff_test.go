package runtime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

type countingLauncher struct{ calls int }

func (l *countingLauncher) Start(context.Context, AgentLaunch) (*exec.Cmd, error) {
	l.calls++
	return nil, nil
}

func TestPermitGateConsumesReplayedPermitBeforeSpawn(t *testing.T) {
	var gate PermitGate
	launcher := &countingLauncher{}
	launch := AgentLaunch{Executable: "/agent", Worktree: "/work", RunDir: "/run"}
	if err := gate.SpawnOnce(context.Background(), launcher, launch); err != nil {
		t.Fatal(err)
	}
	if err := gate.SpawnOnce(context.Background(), launcher, launch); !errors.Is(err, ErrSpawnAlreadyEntered) {
		t.Fatalf("replay = %v", err)
	}
	if launcher.calls != 1 {
		t.Fatalf("spawn calls = %d, want 1", launcher.calls)
	}
}
