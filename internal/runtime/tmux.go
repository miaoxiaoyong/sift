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

// HostLaunch is the frozen dispatch identity and wrapper invocation accepted by
// a wrapper host. BootstrapPath is only wrapper argv: hosts must never derive
// identity from its mutable contents.
type HostLaunch struct {
	Backend       string
	RunID         string
	AttemptNo     int
	Generation    int
	DispatchID    string
	WrapperPath   string
	BootstrapPath string
}

// ValidateFor verifies that a host received a complete frozen dispatch and
// the exact wrapper path it was configured to host.
func (l HostLaunch) ValidateFor(backend, wrapper string) error {
	if l.Backend != backend || l.WrapperPath != wrapper || !filepath.IsAbs(l.WrapperPath) || !filepath.IsAbs(l.BootstrapPath) {
		return errors.New("runtime: invalid frozen host launch paths or backend")
	}
	_, err := TmuxBindingDigest(l.RunID, l.AttemptNo, l.Generation, l.DispatchID)
	return err
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

// TmuxSessionConflictError reports that an exact target already exists. This
// slice never adopts it: durable reclaim convergence is a later operation.
type TmuxSessionConflictError struct {
	Session string
	Cause   error
}

func (e *TmuxSessionConflictError) Error() string {
	return fmt.Sprintf("runtime: tmux session conflict %q: %v", e.Session, e.Cause)
}

func (e *TmuxSessionConflictError) Unwrap() error { return e.Cause }

// TmuxClientEnvironment is deliberately small. In particular it never copies
// the daemon environment into either a newly created tmux server or its pane.
func TmuxClientEnvironment() []string {
	env := []string{"PATH=/usr/bin:/bin"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		env = append(env, "HOME="+home)
	}
	if tmp := os.TempDir(); filepath.IsAbs(tmp) {
		env = append(env, "TMPDIR="+tmp)
	}
	return env
}

// TmuxBackend starts only the wrapper through tmux's argv interface.
type TmuxBackend struct {
	tmuxPath   string
	wrapper    string
	socketPath string
}

// NewTmuxBackend records startup-probed, absolute paths. socketPath is a
// stable SIFT_HOME-scoped identity; its digest keeps the actual UNIX socket
// below the platform pathname limit while preserving daemon-restart affinity.
func NewTmuxBackend(tmuxPath, wrapperPath, socketPath string) (*TmuxBackend, error) {
	if !filepath.IsAbs(tmuxPath) || !filepath.IsAbs(wrapperPath) || !filepath.IsAbs(socketPath) {
		return nil, errors.New("runtime: tmux, wrapper, and tmux socket paths must be absolute")
	}
	sum := sha256.Sum256([]byte(socketPath))
	return &TmuxBackend{tmuxPath: tmuxPath, wrapper: wrapperPath, socketPath: filepath.Join(os.TempDir(), "sift-tmux-"+hex.EncodeToString(sum[:12])+".sock")}, nil
}

func (b *TmuxBackend) WrapperPath() string { return b.wrapper }

// SocketPath is the isolated server socket selected from the stable identity.
func (b *TmuxBackend) SocketPath() string { return b.socketPath }

// Spawn starts a wrapper identified solely by the frozen HostLaunch input.
func (b *TmuxBackend) Spawn(ctx context.Context, launch HostLaunch) (*os.Process, error) {
	if b == nil || b.tmuxPath == "" || b.wrapper == "" || b.socketPath == "" {
		return nil, errors.New("runtime: tmux backend is not initialized")
	}
	if err := launch.ValidateFor("tmux", b.wrapper); err != nil {
		return nil, fmt.Errorf("runtime: invalid frozen tmux launch: %w", err)
	}
	name, err := TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
	if err != nil {
		return nil, err
	}
	digest := name[len("sift-"):]
	cmd := b.command(ctx, "new-session", "-d", "-s", name, "-e", "SIFT_TMUX_BINDING="+digest, "--", launch.WrapperPath, launch.BootstrapPath)
	if out, err := cmd.CombinedOutput(); err == nil {
		return cmd.Process, nil
	} else if b.sessionExists(ctx, name) {
		return nil, &TmuxSessionConflictError{Session: name, Cause: err}
	} else {
		return nil, fmt.Errorf("runtime: create tmux session %q: %w: %s", name, err, string(out))
	}
}

func (b *TmuxBackend) command(ctx context.Context, args ...string) *exec.Cmd {
	args = append([]string{"-f", "/dev/null", "-S", b.socketPath}, args...)
	cmd := exec.CommandContext(ctx, b.tmuxPath, args...)
	cmd.Env = TmuxClientEnvironment()
	return cmd
}

func (b *TmuxBackend) sessionExists(ctx context.Context, name string) bool {
	return b.command(ctx, "has-session", "-t", "="+name).Run() == nil
}
