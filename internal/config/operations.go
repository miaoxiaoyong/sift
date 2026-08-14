package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xsift/sift/internal/runtime"
)

// DriftStatus is the outcome of a single drift check.
type DriftStatus int

const (
	// DriftNone: the on-disk file's effective hash matches the startup hash.
	DriftNone DriftStatus = iota
	// DriftDetected: the effective hash differs from the startup hash. The
	// running config is unchanged; the caller should append a
	// config_drift_detected security event (once per drift episode) and have
	// doctor report a warning (config.md §4).
	DriftDetected
	// DriftUndecodable: the file changed and can no longer be parsed or
	// validated. Treated as drift (something changed) but the new content is
	// not applied; the error is surfaced for diagnostics.
	DriftUndecodable
)

// DriftResult is returned by [DriftChecker.Check].
type DriftResult struct {
	Status      DriftStatus
	CurrentHash string
	// NewEvent is true exactly on the check that transitions into a drifted
	// state. While drift persists, subsequent checks report DriftDetected but
	// NewEvent=false ("只追加一次"). Restoring the original hash clears the
	// state, so a later drift emits the event once again.
	NewEvent bool
	// Err carries the decode/normalize error for DriftUndecodable.
	Err error
}

// DriftChecker watches config.yaml for on-disk changes after startup
// (config.md §4). It never applies new content: V0 does not hot-reload global
// config (§1.3). On a hash mismatch it flags drift for a single security event
// and a doctor warning; restoring the original hash clears the warning but
// keeps the historical event.
//
// The caller schedules Check at scheduler.config_drift_check_interval.
type DriftChecker struct {
	home        Home
	startupHash string
	startupSrc  SourceInfo

	mu     sync.Mutex
	active bool // a drift episode is currently outstanding
}

// NewDriftChecker constructs a checker anchored to the startup snapshot.
func NewDriftChecker(home Home, startupHash string, startupSrc SourceInfo) *DriftChecker {
	return &DriftChecker{home: home, startupHash: startupHash, startupSrc: startupSrc}
}

// Check compares the current on-disk file against the startup snapshot. It
// short-circuits on identical existence/mtime/size; only a candidate change
// triggers the hash recompute. now is accepted for the injected-clock
// convention even though the fingerprint is time-independent.
func (d *DriftChecker) Check(_ time.Time) DriftResult {
	d.mu.Lock()
	defer d.mu.Unlock()

	path := ConfigPath(d.home)
	info, err := os.Stat(path)
	present := err == nil

	// Fast path: same existence, mtime and size as startup ⇒ byte-identical,
	// no drift. Clears any outstanding warning.
	if present == d.startupSrc.Present &&
		present &&
		info.Size() == d.startupSrc.Size &&
		sameMillis(info.ModTime(), d.startupSrc.MTime) {
		d.active = false
		return DriftResult{Status: DriftNone, CurrentHash: d.startupHash}
	}
	if !present && !d.startupSrc.Present {
		// Still absent, still nothing.
		d.active = false
		return DriftResult{Status: DriftNone, CurrentHash: d.startupHash}
	}

	// Candidate change: recompute the effective hash. Any failure to read,
	// parse or normalize is drift (the file changed into something unusable)
	// but the running config is untouched.
	snap, err := Load(d.home, time.Time{})
	if err != nil {
		// Only the first transition into drift emits the event.
		nev := !d.active
		d.active = true
		return DriftResult{Status: DriftUndecodable, NewEvent: nev, Err: err}
	}
	if snap.Hash != d.startupHash {
		nev := !d.active
		d.active = true
		return DriftResult{Status: DriftDetected, CurrentHash: snap.Hash, NewEvent: nev}
	}
	// Reformatted but same effective config: not drift.
	d.active = false
	return DriftResult{Status: DriftNone, CurrentHash: snap.Hash}
}

// sameMillis reports whether two times are equal at millisecond resolution,
// guarding against filesystem mtime granularity differences between the load
// host and the check host.
func sameMillis(a, b time.Time) bool {
	return a.Truncate(time.Millisecond).Equal(b.Truncate(time.Millisecond))
}

// Scheduling hard-guard errors (config.md §1.4). They are returned by [Guard]
// and must cause the scheduler to reject the request, never to silently exceed
// a limit or admit an unknown agent.
var (
	ErrUnknownAgent    = errors.New("config: unknown agent")
	ErrAgentAtCapacity = errors.New("config: agent at max_concurrent")
	ErrTotalAtCapacity = errors.New("config: runtime at max_concurrent_total")
	ErrProjectBusy     = errors.New("config: project held exclusively")
)

// Guard enforces the scheduling hard guards of config.md §1.4:
//
//   - an unknown agent id is rejected outright,
//   - each agent's in-flight attempts are capped at its effective
//     max_concurrent (an omitted value inherited Runtime.DefaultAgentMaxConcurrent),
//   - the global in-flight count is capped at Runtime.MaxConcurrentTotal,
//   - a project may be held exclusively ("when needed") so two attempts do not
//     contend for the same run worktree.
//
// Acquire is atomic: either every guard passes and the slot is held, or no
// counter changes. The returned release func decrements exactly what Acquire
// reserved and is safe to call exactly once.
type Guard struct {
	cfg *Config

	mu                sync.Mutex
	agentInUse        map[string]int
	totalInUse        int
	exclusiveProjects map[string]bool
}

// NewGuard builds a Guard for an effective config. The config must already be
// normalized (every agent has a resolved MaxConcurrent).
func NewGuard(cfg *Config) *Guard {
	return &Guard{
		cfg:               cfg,
		agentInUse:        map[string]int{},
		exclusiveProjects: map[string]bool{},
	}
}

// Agent returns the effective agent definition for id, or [ErrUnknownAgent].
func (g *Guard) Agent(id string) (Agent, error) {
	for _, a := range g.cfg.Agents {
		if a.ID == id {
			return a, nil
		}
	}
	return Agent{}, fmt.Errorf("%w: %q", ErrUnknownAgent, id)
}

// Acquire reserves one scheduling slot for agentID within projectID. When
// exclusiveProject is true the project is held exclusively until release: no
// other in-flight attempt for that project may be admitted. It returns a
// release function the caller MUST invoke when the slot is freed.
func (g *Guard) Acquire(agentID, projectID string, exclusiveProject bool) (Release, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	agent, err := g.Agent(agentID)
	if err != nil {
		return nil, err
	}
	if g.agentInUse[agentID] >= agent.MaxConcurrent {
		return nil, fmt.Errorf("%w: %q at %d/%d", ErrAgentAtCapacity, agentID, g.agentInUse[agentID], agent.MaxConcurrent)
	}
	if g.totalInUse >= g.cfg.Runtime.MaxConcurrentTotal {
		return nil, fmt.Errorf("%w: %d/%d", ErrTotalAtCapacity, g.totalInUse, g.cfg.Runtime.MaxConcurrentTotal)
	}
	if exclusiveProject && g.exclusiveProjects[projectID] {
		return nil, fmt.Errorf("%w: %q", ErrProjectBusy, projectID)
	}

	g.agentInUse[agentID]++
	g.totalInUse++
	heldExclusive := false
	if exclusiveProject {
		g.exclusiveProjects[projectID] = true
		heldExclusive = true
	}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if v := g.agentInUse[agentID]; v > 0 {
			g.agentInUse[agentID] = v - 1
		}
		if g.totalInUse > 0 {
			g.totalInUse--
		}
		if heldExclusive {
			delete(g.exclusiveProjects, projectID)
		}
	}, nil
}

// Release decrements the counters reserved by a successful Acquire.
type Release func()

// Snapshot is a point-in-time view of in-flight usage, for doctor and metrics.
type UsageSnapshot struct {
	TotalInUse        int
	AgentInUse        map[string]int
	ExclusiveProjects []string
}

// Usage returns a point-in-time snapshot of current in-flight reservations.
func (g *Guard) Usage() UsageSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	agent := make(map[string]int, len(g.agentInUse))
	for k, v := range g.agentInUse {
		agent[k] = v
	}
	var excl []string
	for p := range g.exclusiveProjects {
		excl = append(excl, p)
	}
	return UsageSnapshot{TotalInUse: g.totalInUse, AgentInUse: agent, ExclusiveProjects: excl}
}

// This file implements the two-level startup probe framework of config.md §5:
//
//   - Process-level probes (§5.1): any failure refuses daemon startup. They
//     cover SIFT_HOME/permissions, config decode/normalize/fingerprint, agent
//     executables, the brain executable and tmux when used. (SQLite, sockets,
//     the single-instance lock and forge CLIs are wired by later slices.)
//   - Project-level probes (§5.2): a failure isolates only that project,
//     writes its health projection, warns once and lets healthy projects keep
//     scheduling.
//
// Probes are values the daemon composes into its ordered startup list; this
// package provides the runner and the config-owned probes.

// Probe is one process-level check. Run must be safe to call with a cancelled
// context.
type Probe struct {
	Name string
	Run  func(ctx context.Context) error
}

// Outcome is the recorded result of one Probe.
type Outcome struct {
	Name string
	Err  error
}

// RunProcessProbes runs every probe and returns all outcomes. A non-nil error
// in any outcome means startup must refuse (config.md §5.1).
func RunProcessProbes(ctx context.Context, probes []Probe) []Outcome {
	out := make([]Outcome, 0, len(probes))
	for _, p := range probes {
		err := p.Run(ctx)
		out = append(out, Outcome{Name: p.Name, Err: err})
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// AnyFailed reports whether any outcome carries an error, i.e. whether startup
// must refuse.
func AnyFailed(outcomes []Outcome) bool {
	for _, o := range outcomes {
		if o.Err != nil {
			return true
		}
	}
	return false
}

// Diagnostics collects resolved absolute executable paths from startup probes
// (config.md §5.1.7). The running launcher and process-identity records use
// these resolved paths; the original configured values still enter the config
// hash, so PATH drift after startup cannot silently change which binary runs.
type Diagnostics struct {
	// AgentPaths maps agent id → resolved absolute executable path.
	AgentPaths map[string]string
	// BrainPath is the resolved brain executable, empty in deterministic mode.
	BrainPath string
	// TmuxPath is the resolved tmux binary when a backend requires it.
	TmuxPath string
}

// NewDiagnostics returns a zeroed Diagnostics with initialized maps.
func NewDiagnostics() *Diagnostics {
	return &Diagnostics{AgentPaths: map[string]string{}}
}

// AgentExecutableProbe builds the §5.1.4 probe: every defined agent's
// executable must resolve on PATH. Even agents not yet referenced by a project
// are probed — agent definitions are startup-sensitive closed config.
func AgentExecutableProbe(cfg *Config, diag *Diagnostics) Probe {
	return Probe{
		Name: "agent-executables",
		Run: func(ctx context.Context) error {
			for _, a := range cfg.Agents {
				p, err := exec.LookPath(a.Executable)
				if err != nil {
					return fmt.Errorf("agent %q: executable %q not found on PATH: %w", a.ID, a.Executable, err)
				}
				diag.AgentPaths[a.ID] = p
			}
			return nil
		},
	}
}

// BrainExecutableProbe builds the §5.1.5 probe. A null/empty executable is
// deterministic mode and is not probed (config.md §3.4).
func BrainExecutableProbe(cfg *Config, diag *Diagnostics) Probe {
	return Probe{
		Name: "brain-executable",
		Run: func(ctx context.Context) error {
			if cfg.Brain.Executable == "" {
				return nil
			}
			p, err := exec.LookPath(cfg.Brain.Executable)
			if err != nil {
				return fmt.Errorf("brain: executable %q not found on PATH: %w", cfg.Brain.Executable, err)
			}
			diag.BrainPath = p
			return nil
		},
	}
}

// TmuxProbe builds the §5.1.6 probe: tmux is resolved only when some agent or
// the runtime backend selects the tmux backend.
func TmuxProbe(cfg *Config, diag *Diagnostics) Probe {
	return Probe{
		Name: "tmux",
		Run: func(ctx context.Context) error {
			if !usesTmux(cfg) {
				return nil
			}
			p, err := exec.LookPath("tmux")
			if err != nil {
				return fmt.Errorf("tmux backend selected but tmux not found on PATH: %w", err)
			}
			p, err = filepath.EvalSymlinks(p)
			if err != nil {
				return fmt.Errorf("resolve tmux executable: %w", err)
			}
			p, err = filepath.Abs(p)
			if err != nil {
				return fmt.Errorf("make tmux executable absolute: %w", err)
			}
			if err := probeTmuxCapabilities(ctx, p); err != nil {
				return fmt.Errorf("tmux backend capability probe: %w", err)
			}
			diag.TmuxPath = p
			return nil
		},
	}
}

// probeTmuxCapabilities verifies the exact invocation contract used by the
// runtime host. It starts a short-lived isolated server with the same scrubbed
// environment as production, rather than trusting a version string alone.
func probeTmuxCapabilities(ctx context.Context, tmuxPath string) error {
	version := exec.CommandContext(ctx, tmuxPath, "-V")
	version.Env = runtime.TmuxClientEnvironment()
	out, err := version.Output()
	if err != nil {
		return fmt.Errorf("read version: %w", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "tmux %d.%d", &major, &minor); err != nil || major < 3 || major == 3 && minor < 2 {
		return fmt.Errorf("need tmux >= 3.2 with new-session -e and multi-argv shell-command, got %q", strings.TrimSpace(string(out)))
	}
	dir, err := os.MkdirTemp("", "sift-tmux-probe-")
	if err != nil {
		return fmt.Errorf("create probe directory: %w", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(os.TempDir(), "sift-tmux-probe-"+filepath.Base(dir)+".sock")
	defer os.Remove(socket)
	const session = "sift-capability-probe"
	command := func(args ...string) *exec.Cmd {
		args = append([]string{"-f", "/dev/null", "-S", socket}, args...)
		cmd := exec.CommandContext(ctx, tmuxPath, args...)
		cmd.Env = runtime.TmuxClientEnvironment()
		return cmd
	}
	defer command("kill-server").Run()
	if out, err := command("new-session", "-d", "-s", session, "-e", "SIFT_TMUX_PROBE=1", "--", "/bin/sleep", "5").CombinedOutput(); err != nil {
		return fmt.Errorf("new-session -e multi-argv: %w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err = command("show-environment", "-t", "="+session, "SIFT_TMUX_PROBE").Output()
	if err != nil || strings.TrimSpace(string(out)) != "SIFT_TMUX_PROBE=1" {
		return fmt.Errorf("verify new-session -e: %w: %q", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func usesTmux(cfg *Config) bool {
	if cfg.Runtime.Backend == BackendTmux {
		return true
	}
	for _, a := range cfg.Agents {
		if a.Backend == BackendTmux {
			return true
		}
	}
	return false
}

// checkRepoDir is the §5.2.1 repo skeleton check: the path must exist and be a
// directory. Normalize already enforced absoluteness; git integrity and
// base-branch checks arrive with the Forge/runtime slices.
func checkRepoDir(repo string) error {
	info, err := os.Stat(repo)
	if err != nil {
		return fmt.Errorf("repo %s: %w", repo, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repo %s: not a directory", repo)
	}
	return nil
}

// ProjectProbe is one project-level check (config.md §5.2).
type ProjectProbe struct {
	Name string
	Run  func(ctx context.Context, p Project) error
}

// ProjectOutcome is the recorded result for one project.
type ProjectOutcome struct {
	ProjectID string
	Name      string
	Err       error
}

// RunProjectProbes runs each probe across each enabled project independently.
// A failure is recorded for that project only; other projects are unaffected
// (§5.2). The caller turns a failing outcome into a one-time warning and a
// project health projection entry.
func RunProjectProbes(ctx context.Context, cfg *Config, probes []ProjectProbe) []ProjectOutcome {
	out := make([]ProjectOutcome, 0)
	for _, p := range cfg.Projects {
		if !p.Enabled {
			continue
		}
		for _, pr := range probes {
			err := pr.Run(ctx, p)
			out = append(out, ProjectOutcome{ProjectID: p.ID, Name: pr.Name, Err: err})
			if ctx.Err() != nil {
				return out
			}
		}
	}
	return out
}

// FailedProjects returns the set of project ids with at least one failing
// probe, for the one-time warning deduplication.
func FailedProjects(outcomes []ProjectOutcome) []string {
	seen := map[string]bool{}
	var failed []string
	for _, o := range outcomes {
		if o.Err != nil && !seen[o.ProjectID] {
			seen[o.ProjectID] = true
			failed = append(failed, o.ProjectID)
		}
	}
	return failed
}

// ProjectRepoProbe builds the §5.2.1 project probe skeleton: the repo path must
// be absolute and exist as a directory. Git-repository integrity, base-branch
// readability, policy schema and forge auth land in later slices.
func ProjectRepoProbe() ProjectProbe {
	return ProjectProbe{
		Name: "repo",
		Run: func(_ context.Context, p Project) error {
			return checkRepoDir(p.Repo)
		},
	}
}

// ProjectAgentsProbe builds the §5.2.4 project probe: each candidate agent id
// must resolve against the effective config. Normalize already enforces this
// for configured lists; the probe is the runtime re-check.
func ProjectAgentsProbe(cfg *Config) ProjectProbe {
	known := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		known[a.ID] = true
	}
	return ProjectProbe{
		Name: "agents",
		Run: func(_ context.Context, p Project) error {
			for _, id := range p.Agents {
				if !known[id] {
					return fmt.Errorf("project %q references unknown agent %q", p.ID, id)
				}
			}
			return nil
		},
	}
}
