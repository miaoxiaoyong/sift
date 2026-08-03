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

	// The lease fields are only used by a durable binding verifier. They fence
	// the external tmux observation without becoming part of the session name.
	OperationID      string
	LeaseOwner       string
	LeaseExpiresAtMS int64
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

// TmuxSessionConflictError reports that an existing exact target cannot be
// proven to be the live wrapper for the current frozen launch binding.
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
type DurableBindingVerifier func(context.Context, HostLaunch) error

type TmuxBackend struct {
	tmuxPath      string
	wrapper       string
	socketPath    string
	verifyBinding DurableBindingVerifier
}

// NewTmuxBackend records startup-probed, absolute paths. socketPath is a
// stable SIFT_HOME-scoped identity; its digest keeps the actual UNIX socket
// below the platform pathname limit while preserving daemon-restart affinity.
func NewTmuxBackend(tmuxPath, wrapperPath, socketPath string, verifier ...DurableBindingVerifier) (*TmuxBackend, error) {
	if !filepath.IsAbs(tmuxPath) || !filepath.IsAbs(wrapperPath) || !filepath.IsAbs(socketPath) {
		return nil, errors.New("runtime: tmux, wrapper, and tmux socket paths must be absolute")
	}
	var verify DurableBindingVerifier
	if len(verifier) > 0 {
		verify = verifier[0]
	}
	return &TmuxBackend{tmuxPath: tmuxPath, wrapper: wrapperPath, socketPath: TmuxSocketPath(socketPath), verifyBinding: verify}, nil
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
	} else if !b.sessionExists(ctx, name) {
		return nil, fmt.Errorf("runtime: create tmux session %q: %w: %s", name, err, string(out))
	} else if b.verifyBinding == nil {
		return nil, &TmuxSessionConflictError{Session: name, Cause: errors.New("tmux durable binding verifier is not configured")}
	} else if err := b.validateExistingSession(ctx, name, digest); err != nil {
		return nil, &TmuxSessionConflictError{Session: name, Cause: err}
	}
	if b.verifyBinding != nil {
		if err := b.verifyBinding(ctx, launch); err != nil {
			return nil, &TmuxSessionConflictError{Session: name, Cause: err}
		}
	}
	// new-session may have succeeded even though its client lost the response,
	// or another reclaim may have won the same deterministic binding. Once the
	// exact session proves it is the current live binding, both cases have
	// already accepted exactly one wrapper and must converge without respawn.
	return nil, nil
}

// validateExistingSession proves that name is the single live pane created for
// digest. Every target begins with the exact deterministic session name; no
// prefix lookup, attach, or lifecycle observation is used to adopt a session.
func (b *TmuxBackend) validateExistingSession(ctx context.Context, name, digest string) error {
	binding, err := b.command(ctx, "show-environment", "-t", "="+name, "SIFT_TMUX_BINDING").Output()
	if err != nil || string(binding) != "SIFT_TMUX_BINDING="+digest+"\n" {
		return errors.New("tmux session binding does not match frozen launch")
	}
	panes, err := b.command(ctx, "list-panes", "-t", "="+name, "-s", "-F", "#{pane_dead}").Output()
	if err != nil || string(panes) != "0\n" {
		return errors.New("tmux session does not have exactly one live pane")
	}
	// A fresh session has window 0/pane 0. Requiring it here is deliberately
	// fail-closed: a mutated target is never reclaimed, even if it retained the
	// right environment value.
	remain, err := b.command(ctx, "show-options", "-A", "-v", "-t", "="+name+":0.0", "remain-on-exit").Output()
	if err != nil || string(remain) != "off\n" {
		return errors.New("tmux session has remain-on-exit enabled or unknown")
	}
	return nil
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

// TmuxSocketPath derives the private server socket used by a configured home.
func TmuxSocketPath(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(os.TempDir(), "sift-tmux-"+hex.EncodeToString(sum[:12])+".sock")
}

type BackendSessionState string

const (
	SessionNotApplicable BackendSessionState = "not_applicable"
	SessionPresent       BackendSessionState = "present"
	SessionAbsent        BackendSessionState = "absent"
	SessionUnknown       BackendSessionState = "unknown"
)

type BackendSessionObservation struct {
	Backend        string
	State          BackendSessionState
	BindingDigest  string
	DiagnosticCode string
}

// ObserveBackendSession returns a diagnostic observation. It never changes
// durable execution state and never treats a session as ownership evidence.
func ObserveBackendSession(ctx context.Context, tmuxPath, socketPath, name, digest string) BackendSessionObservation {
	if tmuxPath == "" {
		return BackendSessionObservation{Backend: "tmux", State: SessionUnknown, DiagnosticCode: "tmux_unavailable"}
	}
	cmd := exec.CommandContext(ctx, tmuxPath, "-f", "/dev/null", "-S", socketPath, "has-session", "-t", "="+name)
	cmd.Env = TmuxClientEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if tmuxHasSessionAbsent(err, out) {
			return BackendSessionObservation{Backend: "tmux", State: SessionAbsent, BindingDigest: digest, DiagnosticCode: "session_absent"}
		}
		return BackendSessionObservation{Backend: "tmux", State: SessionUnknown, BindingDigest: digest, DiagnosticCode: "session_unavailable"}
	}
	if err := ObserveTmuxSession(ctx, tmuxPath, socketPath, name, digest); err != nil {
		return BackendSessionObservation{Backend: "tmux", State: SessionUnknown, BindingDigest: digest, DiagnosticCode: "session_binding_mismatch"}
	}
	return BackendSessionObservation{Backend: "tmux", State: SessionPresent, BindingDigest: digest, DiagnosticCode: "session_present"}
}

// tmux documents has-session's clean status-1 result as the session-absent
// answer. A server, protocol, permission, or timeout error is not absence and
// must remain unknown. Real tmux prints no diagnostic for this one expected
// condition; test doubles may use its explicit "can't find session" wording.
func tmuxHasSessionAbsent(err error, output []byte) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	text := strings.TrimSpace(string(output))
	return text == "" || strings.Contains(text, "can't find session")
}

// ObserveTmuxSession proves that an exact, durably-derived session is live and
// bound to the expected launch. It never attaches, adopts, or changes tmux.
func ObserveTmuxSession(ctx context.Context, tmuxPath, socketPath, name, digest string) error {
	command := func(args ...string) *exec.Cmd {
		all := append([]string{"-f", "/dev/null", "-S", socketPath}, args...)
		cmd := exec.CommandContext(ctx, tmuxPath, all...)
		cmd.Env = TmuxClientEnvironment()
		return cmd
	}
	if err := command("has-session", "-t", "="+name).Run(); err != nil {
		return errors.New("tmux session is absent or unavailable")
	}
	binding, err := command("show-environment", "-t", "="+name, "SIFT_TMUX_BINDING").Output()
	if err != nil || string(binding) != "SIFT_TMUX_BINDING="+digest+"\n" {
		return errors.New("tmux session binding does not match current launch")
	}
	panes, err := command("list-panes", "-t", "="+name, "-s", "-F", "#{pane_dead}").Output()
	if err != nil || string(panes) != "0\n" {
		return errors.New("tmux session does not have exactly one live pane")
	}
	remain, err := command("show-options", "-A", "-v", "-t", "="+name+":0.0", "remain-on-exit").Output()
	if err != nil || string(remain) != "off\n" {
		return errors.New("tmux session has remain-on-exit enabled or unknown")
	}
	return nil
}
