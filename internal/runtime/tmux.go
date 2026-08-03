package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LaunchIdentity is the immutable identity from which a tmux session is
// derived. It deliberately contains no user-controlled display names or
// credentials.
type LaunchIdentity struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	AttemptNo     int    `json:"attempt_no"`
	Generation    int    `json:"generation"`
	DispatchID    string `json:"dispatch_id"`
}

// TmuxBindingDigest returns the SHA-256 of the closed, canonical launch
// identity JSON. encoding/json emits struct fields in declaration order.
func TmuxBindingDigest(runID string, attemptNo, generation int, dispatchID string) (string, error) {
	if runID == "" || dispatchID == "" || attemptNo < 1 || generation < 1 {
		return "", errors.New("runtime: incomplete tmux launch identity")
	}
	identity := LaunchIdentity{SchemaVersion: 1, RunID: runID, AttemptNo: attemptNo, Generation: generation, DispatchID: dispatchID}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("runtime: encode tmux launch identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func TmuxSessionName(runID string, attemptNo, generation int, dispatchID string) (string, error) {
	digest, err := TmuxBindingDigest(runID, attemptNo, generation, dispatchID)
	if err != nil {
		return "", err
	}
	return "sift-" + digest, nil
}

// TmuxBackend starts only the wrapper through tmux's argv interface. The
// wrapper and bootstrap paths are read from the owner-only bootstrap file;
// neither is ever passed through a shell command string.
type TmuxBackend struct {
	tmuxPath string
	wrapper  string
}

func NewTmuxBackend(tmuxPath, wrapperPath string) (*TmuxBackend, error) {
	if !filepath.IsAbs(tmuxPath) || !filepath.IsAbs(wrapperPath) {
		return nil, errors.New("runtime: tmux and wrapper paths must be absolute")
	}
	return &TmuxBackend{tmuxPath: tmuxPath, wrapper: wrapperPath}, nil
}

func (b *TmuxBackend) Spawn(ctx context.Context, bootstrapPath string) (*os.Process, error) {
	if b == nil || b.tmuxPath == "" || b.wrapper == "" {
		return nil, errors.New("runtime: tmux backend is not initialized")
	}
	if !filepath.IsAbs(bootstrapPath) {
		return nil, errors.New("runtime: bootstrap path must be absolute")
	}
	contents, err := os.ReadFile(bootstrapPath)
	if err != nil {
		return nil, fmt.Errorf("runtime: read tmux bootstrap: %w", err)
	}
	var boot Bootstrap
	if err := json.Unmarshal(contents, &boot); err != nil {
		return nil, fmt.Errorf("runtime: decode tmux bootstrap: %w", err)
	}
	name, err := TmuxSessionName(boot.RunID, boot.AttemptNo, boot.Generation, boot.DispatchID)
	if err != nil {
		return nil, err
	}
	digest := strings.TrimPrefix(name, "sift-")
	args := []string{"new-session", "-d", "-s", name, "-e", "SIFT_TMUX_BINDING=" + digest, "--", b.wrapper, bootstrapPath}
	cmd := exec.CommandContext(ctx, b.tmuxPath, args...)
	if err := cmd.Run(); err == nil {
		return cmd.Process, nil
	} else if validTmuxSession(ctx, b.tmuxPath, name, digest) {
		// new-session may have succeeded while its client response was lost.
		// Only the exact binding and a single live pane permit convergence.
		return nil, nil
	} else {
		return nil, fmt.Errorf("runtime: create tmux session %q: %w", name, err)
	}
}

func validTmuxSession(ctx context.Context, tmuxPath, name, digest string) bool {
	target := "=" + name
	if err := exec.CommandContext(ctx, tmuxPath, "has-session", "-t", target).Run(); err != nil {
		return false
	}
	env, err := exec.CommandContext(ctx, tmuxPath, "show-environment", "-t", target, "SIFT_TMUX_BINDING").Output()
	if err != nil || strings.TrimSpace(string(env)) != "SIFT_TMUX_BINDING="+digest {
		return false
	}
	remain, err := exec.CommandContext(ctx, tmuxPath, "show-window-options", "-t", target, "remain-on-exit").Output()
	if err != nil || strings.TrimSpace(string(remain)) != "remain-on-exit off" {
		return false
	}
	panes, err := exec.CommandContext(ctx, tmuxPath, "list-panes", "-t", target, "-F", "#{pane_dead}").Output()
	if err != nil {
		return false
	}
	lines := strings.Fields(string(panes))
	return len(lines) == 1 && lines[0] == "0"
}
