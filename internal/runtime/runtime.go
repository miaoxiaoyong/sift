// Package runtime implements the local execution boundary described by DESIGN §8.4.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/miaoxiaoyong/sift/internal/version"
)

const wrapperName = "sift-agent-wrapper"

// ErrWrapperVersion reports that the installed wrapper is not from the same
// Sift release as the daemon.
var ErrWrapperVersion = errors.New("runtime: wrapper version mismatch")

// WrapperVersion runs the wrapper's --version probe and returns its trimmed
// output. The wrapper prints exactly the release SemVer it was built with
// (internal/version.Release), so the output is comparable to daemonVersion.
func WrapperVersion(wrapper string) (string, error) {
	out, err := exec.Command(wrapper, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("runtime: probe wrapper %q: %w", wrapper, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveWrapper finds the wrapper next to daemonPath. It never consults PATH:
// release archives install the daemon and wrapper together, so choosing a
// wrapper elsewhere could pair incompatible binaries.
func ResolveWrapper(daemonPath, daemonVersion string) (string, error) {
	if !filepath.IsAbs(daemonPath) {
		return "", fmt.Errorf("runtime: daemon path must be absolute")
	}
	if !version.IsValidSemver(daemonVersion) {
		return "", fmt.Errorf("runtime: invalid daemon version %q", daemonVersion)
	}
	wrapper := filepath.Join(filepath.Dir(daemonPath), wrapperName)
	info, err := os.Stat(wrapper)
	if err != nil {
		return "", fmt.Errorf("runtime: installed wrapper %q: %w", wrapper, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("runtime: installed wrapper %q is not executable", wrapper)
	}

	reported, err := WrapperVersion(wrapper)
	if err != nil {
		return "", err
	}
	if reported != daemonVersion {
		return "", fmt.Errorf("%w: daemon %s, wrapper %s", ErrWrapperVersion, daemonVersion, reported)
	}
	return wrapper, nil
}

// WrapperPathNextTo returns the release-wrapper path installed next to
// daemonPath. ResolveWrapper owns the probing and version validation; this
// helper only exposes the layout for diagnostics (sift doctor).
func WrapperPathNextTo(daemonPath string) string {
	return filepath.Join(filepath.Dir(daemonPath), wrapperName)
}

// ResolveInstalledWrapper resolves the wrapper for the currently running
// daemon. It is deliberately separate from ResolveWrapper to keep launch tests
// independent of the test binary's location.
func ResolveInstalledWrapper(daemonVersion string) (string, error) {
	daemon, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("runtime: locate daemon executable: %w", err)
	}
	return ResolveWrapper(daemon, daemonVersion)
}

// ProcessBackend starts only the wrapper. The wrapper, not this backend, is
// responsible for starting the Agent in its process group.
type ProcessBackend struct {
	wrapper string
}

// NewProcessBackend verifies and records the same-version wrapper installed
// alongside daemonPath.
func NewProcessBackend(daemonPath, daemonVersion string) (*ProcessBackend, error) {
	wrapper, err := ResolveWrapper(daemonPath, daemonVersion)
	if err != nil {
		return nil, err
	}
	return &ProcessBackend{wrapper: wrapper}, nil
}

// WrapperPath returns the resolved immutable wrapper path used for launches.
func (b *ProcessBackend) WrapperPath() string { return b.wrapper }

// Spawn starts the wrapper in its own process group. bootstrapPath is the only
// wrapper argument; credentials remain in the bootstrap file and never enter
// argv or environment variables.
func (b *ProcessBackend) Spawn(ctx context.Context, bootstrapPath string) (*exec.Cmd, error) {
	if b == nil || b.wrapper == "" {
		return nil, errors.New("runtime: process backend is not initialized")
	}
	if !filepath.IsAbs(bootstrapPath) {
		return nil, fmt.Errorf("runtime: bootstrap path must be absolute")
	}
	cmd := exec.CommandContext(ctx, b.wrapper, bootstrapPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runtime: spawn wrapper: %w", err)
	}
	return cmd, nil
}

// AgentLaunch is the sole input to the Agent launcher seam. Executable must
// have been resolved during daemon startup; Args are passed directly, never to
// a shell.
type AgentLaunch struct {
	Executable string
	// ExecutableImage, when present, is an already verified image of
	// Executable. DirectLauncher executes it rather than resolving Executable
	// again by path.
	ExecutableImage *os.File
	Args            []string
	Worktree        string
	RunDir          string
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
}

// Launcher is the only interface wrapper code may use to start an Agent. V0's
// DirectLauncher is intentionally an identity implementation; a sandbox later
// wraps this one seam rather than adding alternate spawn paths.
type Launcher interface {
	Start(context.Context, AgentLaunch) (*exec.Cmd, error)
}

// DirectLauncher is the V0 launcher implementation.
type DirectLauncher struct{}

// Start creates the Agent directly in the wrapper's process group. Its
// environment intentionally contains only the non-secret run-directory hint.
func (DirectLauncher) Start(ctx context.Context, launch AgentLaunch) (*exec.Cmd, error) {
	if !filepath.IsAbs(launch.Executable) {
		return nil, fmt.Errorf("runtime: agent executable must be absolute")
	}
	if !filepath.IsAbs(launch.Worktree) || !filepath.IsAbs(launch.RunDir) {
		return nil, fmt.Errorf("runtime: worktree and run directory must be absolute")
	}
	executable := launch.Executable
	if launch.ExecutableImage != nil {
		var err error
		executable, err = executableImagePath(launch.ExecutableImage)
		if err != nil {
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, executable, launch.Args...)
	cmd.Dir = launch.Worktree
	cmd.Env = []string{"SIFT_RUN_DIR=" + launch.RunDir}
	cmd.Stdin = launch.Stdin
	cmd.Stdout = launch.Stdout
	cmd.Stderr = launch.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runtime: launch agent: %w", err)
	}
	return cmd, nil
}
