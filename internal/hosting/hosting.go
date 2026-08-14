// Package hosting renders and installs the user-level process hosting units
// for `sift daemon` (WBS M8 §8.2): a launchd user agent on macOS, a systemd
// user unit on Linux, and a documented foreground fallback everywhere else.
//
// DESIGN §11 fixes two platform differences and only two: the hosting unit's
// generation/probing, and the sandbox backend. Everything else — paths, the
// two Unix sockets, file contracts, recovery — is identical across platforms.
// The daemon opens no network listener; the units below run it with no port,
// pointed at the atomically-switched `current` release symlink
// (specs/release.md §3) so an upgrade followed by `sift service restart`
// re-execs the new release without overwriting files in place.
//
// The package is deliberately free of platform syscalls: backends are selected
// by runtime.GOOS (injectable for tests) and the platform tools (launchctl /
// systemctl) are invoked by the thin CLI from Plan commands, never here.
package hosting

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/xsift/sift/internal/install"
	"github.com/xsift/sift/internal/version"
)

// These OS directory resolvers are package-level variables so tests can pin
// them to temp paths and exercise the launchd/systemd backends from any host
// OS without depending on the real $HOME / XDG_CONFIG_HOME layout. Production
// callers use the os.* defaults.
var (
	userHomeDir   = os.UserHomeDir
	userConfigDir = os.UserConfigDir
)

// launchd install teardown pacing (issue #968). These are variables, not
// constants, so tests can shrink the windows and swap the sleeper: the install
// command sequences stay fully deterministic and finish in milliseconds
// instead of real seconds.
var (
	// launchdTeardownTimeout bounds how long install waits after a successful
	// bootout for launchctl to confirm the label is absent. 5s matches the
	// observed macOS recovery window in issue #968 (waiting 5s and rerunning
	// install succeeded).
	launchdTeardownTimeout = 5 * time.Second
	// launchdTeardownPoll is the interval between absence probes while waiting.
	launchdTeardownPoll = 250 * time.Millisecond
	// launchdBootstrapRetries is how many extra bootstrap attempts follow a
	// known-transient exit-5 failure while absence stays confirmed.
	launchdBootstrapRetries = 2
	// launchdBootstrapBackoff is the pause before each bootstrap retry.
	launchdBootstrapBackoff = 500 * time.Millisecond
	// sleep is the wait primitive; injectable so tests never block on real time.
	sleep = time.Sleep
)

// Backend names the hosting mechanism a platform uses.
type Backend string

const (
	BackendLaunchd    Backend = "launchd"
	BackendSystemd    Backend = "systemd"
	BackendForeground Backend = "foreground"
)

const (
	// Label is the reverse-DNS identifier used for the launchd agent and as a
	// stable, platform-neutral service handle. It never contains a path
	// separator.
	Label = "cn.hexai.sift"
	// LegacyLabel was used by v0.1.0. Install removes its launchd agent before
	// creating Label so an upgrade cannot leave two competing daemons.
	LegacyLabel = "com.miaoxiaoyong.sift"
	// ServiceName is the file stem for the systemd unit (`sift.service`).
	ServiceName = "sift"
	// LaunchdPath is deliberately closed rather than inherited from the shell:
	// launchd does not read interactive shell profiles. It covers Homebrew on
	// Apple Silicon and Intel plus the macOS system tools.
	LaunchdPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// Action is the CLI verb the hosting layer acts on.
type Action string

const (
	ActionInstall   Action = "install"
	ActionUninstall Action = "uninstall"
	ActionStart     Action = "start"
	ActionStop      Action = "stop"
	ActionRestart   Action = "restart"
	ActionReload    Action = "reload"
	ActionStatus    Action = "status"
)

// ActionFromString maps a CLI verb to an Action, rejecting unknown verbs so
// the CLI surfaces a usage error rather than a silent default.
func ActionFromString(s string) (Action, error) {
	switch Action(s) {
	case ActionInstall, ActionUninstall, ActionStart, ActionStop, ActionRestart, ActionReload, ActionStatus:
		return Action(s), nil
	default:
		return "", fmt.Errorf("hosting: unknown service action %q (want install|uninstall|start|stop|restart|reload|status)", s)
	}
}

// ErrNoBackend is returned by Exec when the platform hosting tool is absent;
// the caller reports the foreground fallback hint instead of treating it as a
// hard failure (DESIGN §11 foreground mode is a supported, not autorestart,
// configuration).
var ErrNoBackend = errors.New("hosting: platform backend unavailable; run `sift daemon` in the foreground")

// Spec is a fully-resolved, installable hosting unit. Every path is absolute;
// the unit ExecStart follows the `current` release symlink so an upgrade only
// needs a restart, never a per-file overwrite.
type Spec struct {
	Backend    Backend
	HomePath   string // SIFT_HOME, absolute (~/.sift)
	DaemonPath string // <home>/bin/current/sift, absolute (follows current)
	Release    string // current release version, read from the current symlink
	UnitPath   string // absolute destination of the generated unit file
	LogOut     string // <home>/logs/siftd.log
	LogErr     string // <home>/logs/siftd.err.log
	Label      string
}

// Detect selects the hosting backend for an operating system. goos is taken
// from runtime.GOOS in production and injected by tests so a single host can
// exercise all three backends. Anything without a supported supervisor falls
// through to the foreground backend, which is a first-class (just not
// autorestart) mode per DESIGN §11.
func Detect(goos string) Backend {
	switch goos {
	case "darwin":
		return BackendLaunchd
	case "linux":
		return BackendSystemd
	default:
		return BackendForeground
	}
}

// NewSpec resolves the hosting unit for the running platform from home. It
// requires a release to already be installed under bin/current: the unit must
// point at a real binary, and an upgrade restart is meaningless without one.
func NewSpec(home string) (Spec, error) {
	return NewSpecFor(home, runtime.GOOS)
}

// NewSpecFor is NewSpec with an explicit goos, for tests that exercise every
// backend regardless of the host OS.
func NewSpecFor(home, goos string) (Spec, error) {
	if home == "" {
		return Spec{}, errors.New("hosting: home path is required")
	}
	if !filepath.IsAbs(home) {
		return Spec{}, fmt.Errorf("hosting: home path %q must be absolute", home)
	}
	current := filepath.Join(home, install.BinDirName, install.CurrentLink)
	release, err := readRelease(current)
	if err != nil {
		return Spec{}, err
	}
	daemonPath := filepath.Join(current, install.DaemonBinary)
	if info, err := os.Stat(daemonPath); err != nil {
		return Spec{}, fmt.Errorf("hosting: daemon binary not installed at %s (run `sift install <archive>` first): %w", daemonPath, err)
	} else if info.Mode().Perm()&0o111 == 0 {
		return Spec{}, fmt.Errorf("hosting: daemon binary %s is not executable", daemonPath)
	}
	logDir := filepath.Join(home, "logs")
	spec := Spec{
		Backend:    Detect(goos),
		HomePath:   filepath.Clean(home),
		DaemonPath: daemonPath,
		Release:    release,
		LogOut:     filepath.Join(logDir, "siftd.log"),
		LogErr:     filepath.Join(logDir, "siftd.err.log"),
		Label:      Label,
	}
	unitPath, err := unitDestination(spec.Backend)
	if err != nil {
		return Spec{}, err
	}
	spec.UnitPath = unitPath
	return spec, nil
}

// readRelease reads the version the `current` symlink points at. The symlink
// target is the version directory name (specs/release.md §3), which is a
// canonical SemVer; this validates it so a corrupted symlink can never produce
// a unit whose ExecStart silently targets the wrong place.
func readRelease(current string) (string, error) {
	target, err := os.Readlink(current)
	if err != nil {
		return "", fmt.Errorf("hosting: no current release installed at %s (run `sift install <archive>` first): %w", current, err)
	}
	target = filepath.Clean(target)
	if !version.IsValidSemver(target) {
		return "", fmt.Errorf("hosting: current symlink target %q is not a canonical release version", target)
	}
	return target, nil
}

// unitDestination resolves where the generated unit file is written for a
// backend: the conventional user-level location on each platform.
func unitDestination(b Backend) (string, error) {
	switch b {
	case BackendLaunchd:
		return launchdUnitPath(Label)
	case BackendSystemd:
		cfg, err := userConfigDir()
		if err != nil {
			return "", fmt.Errorf("hosting: resolve user config dir for systemd unit: %w", err)
		}
		return filepath.Join(cfg, "systemd", "user", ServiceName+".service"), nil
	default:
		// The foreground backend writes nothing; it only prints a hint.
		return "", nil
	}
}

// LegacyLaunchdUnitPath returns the v0.1.0 launchd plist location. It is
// intentionally separate from the current Spec because it is only used by the
// one-time install migration.
func LegacyLaunchdUnitPath() (string, error) {
	return launchdUnitPath(LegacyLabel)
}

func launchdUnitPath(label string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("hosting: resolve user home for launchd unit: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// Plan is what the CLI does for one action: optionally write a generated unit
// file (atomically) and run its platform command sequence. Keeping the two
// separate lets the package stay exec-free while the thin CLI performs the IO.
type Plan struct {
	Action       Action
	Summary      string
	WriteFile    string   // destination of an atomic unit-file write (empty = no write)
	Content      []byte   // content for WriteFile
	BeforeCmd    []string // command run before RunCmd; used for launchd replacement
	ProbeCmd     []string // optional state probe used by idempotent start
	AbsenceProbe []string // optional launchctl probe that must confirm the label is absent before install bootstraps (teardown quiescence, issue #968)
	RunCmd       []string // command + args to execute; nil means manual/foreground only
	FallbackCmd  []string // command used when ProbeCmd finds an unloaded service
	StartUnit    string   // retained plist used by launchd start bootstrap
	Hint         string   // human step printed when RunCmd is nil or the backend is missing
	// Status is a machine-checkable verdict for ActionStatus plans that have
	// no platform command (foreground): "present" when the operator socket
	// exists at plan time, "absent" otherwise. SocketPath names the probed
	// file so a caller can re-verify with `[ -S "$SocketPath" ]` (hosting §5).
	Status     string
	SocketPath string
}

// Plan builds the action's plan. It performs no IO beyond resolving the spec's
// current symlink (already done in NewSpec) so it is deterministic for tests.
func (s Spec) Plan(action Action) (Plan, error) {
	switch action {
	case ActionInstall:
		return s.planInstall(), nil
	case ActionUninstall:
		return s.planUninstall(), nil
	case ActionStart:
		return s.planStart(), nil
	case ActionStop:
		return s.planStop(), nil
	case ActionRestart:
		return s.planRestart(), nil
	case ActionReload:
		return s.planReload(), nil
	case ActionStatus:
		return s.planStatus(), nil
	default:
		return Plan{}, fmt.Errorf("hosting: unknown action %q", action)
	}
}

func (s Spec) planInstall() Plan {
	switch s.Backend {
	case BackendLaunchd:
		content, _ := s.Render() // Render only errors on unknown backends
		return Plan{
			Action: ActionInstall, Summary: "install launchd user agent",
			WriteFile: s.UnitPath, Content: content,
			// bootstrap rejects an already-loaded label. Boot it out first so a
			// repeated install applies the freshly rendered environment; exit 3
			// means no current job and is the successful fresh-install case.
			// After bootout, launchd needs a moment to finish tearing the job
			// down; AbsenceProbe gates bootstrap on launchctl confirming the
			// label is absent (issue #968), instead of racing the teardown.
			BeforeCmd:    []string{"launchctl", "bootout", "gui/" + osUserUID() + "/" + s.Label},
			AbsenceProbe: []string{"launchctl", "print", "gui/" + osUserUID() + "/" + s.Label},
			RunCmd:       []string{"launchctl", "bootstrap", "gui/" + osUserUID(), s.UnitPath},
			Hint:         "launchctl bootstrap gui/$(id -u) " + s.UnitPath,
		}
	case BackendSystemd:
		content, _ := s.Render()
		return Plan{
			Action: ActionInstall, Summary: "install and start systemd user unit",
			WriteFile: s.UnitPath, Content: content,
			RunCmd: []string{"systemctl", "--user", "enable", "--now", ServiceName + ".service"},
			Hint:   "systemctl --user enable --now " + ServiceName + ".service  (enable-linger for headless: loginctl enable-linger $USER)",
		}
	default:
		return Plan{
			Action: ActionInstall, Summary: "foreground daemon (no supervisor)",
			Hint: s.foregroundHint(),
		}
	}
}

func (s Spec) planUninstall() Plan {
	switch s.Backend {
	case BackendLaunchd:
		return Plan{
			Action: ActionUninstall, Summary: "unload and remove launchd user agent",
			WriteFile: s.UnitPath, Content: nil, // nil content => remove the file
			RunCmd: []string{"launchctl", "bootout", "gui/" + osUserUID() + "/" + s.Label},
			Hint:   "launchctl bootout gui/$(id -u)/" + s.Label,
		}
	case BackendSystemd:
		return Plan{
			Action: ActionUninstall, Summary: "disable and remove systemd user unit",
			WriteFile: s.UnitPath, Content: nil,
			RunCmd: []string{"systemctl", "--user", "disable", "--now", ServiceName + ".service"},
			Hint:   "systemctl --user disable --now " + ServiceName + ".service",
		}
	default:
		return Plan{
			Action: ActionUninstall, Summary: "foreground daemon (no supervisor)",
			Hint: "stop the foreground `sift daemon` process",
		}
	}
}

func (s Spec) planStart() Plan {
	switch s.Backend {
	case BackendLaunchd:
		target := "gui/" + osUserUID() + "/" + s.Label
		return Plan{
			Action:      ActionStart,
			Summary:     "start launchd user agent",
			ProbeCmd:    []string{"launchctl", "print", target},
			RunCmd:      []string{"launchctl", "kickstart", target},
			FallbackCmd: []string{"launchctl", "bootstrap", "gui/" + osUserUID(), s.UnitPath},
			StartUnit:   s.UnitPath,
			Hint:        "launchctl kickstart " + target + " (or bootstrap the retained plist when unloaded)",
		}
	case BackendSystemd:
		return Plan{Action: ActionStart, Summary: "start systemd user unit", RunCmd: []string{"systemctl", "--user", "start", ServiceName + ".service"}, Hint: "systemctl --user start " + ServiceName + ".service"}
	default:
		return Plan{Action: ActionStart, Summary: "foreground daemon (no supervisor)", Hint: "the foreground daemon is `sift daemon`; run it in a terminal, tmux, or screen"}
	}
}

func (s Spec) planStop() Plan {
	switch s.Backend {
	case BackendLaunchd:
		// bootout prevents KeepAlive from immediately respawning the daemon;
		// start loads the retained plist again.
		return Plan{Action: ActionStop, Summary: "stop launchd user agent", RunCmd: []string{"launchctl", "bootout", "gui/" + osUserUID() + "/" + s.Label}, Hint: "launchctl bootout gui/$(id -u)/" + s.Label}
	case BackendSystemd:
		return Plan{Action: ActionStop, Summary: "stop systemd user unit", RunCmd: []string{"systemctl", "--user", "stop", ServiceName + ".service"}, Hint: "systemctl --user stop " + ServiceName + ".service"}
	default:
		return Plan{Action: ActionStop, Summary: "foreground daemon (no supervisor)", Hint: "stop the foreground `sift daemon` process (Ctrl-C in its terminal)"}
	}
}

func (s Spec) planRestart() Plan {
	switch s.Backend {
	case BackendLaunchd:
		// kickstart -k atomically restarts the agent by label (load-on-demand
		// domains reload from the plist first), matching the "atomic upgrade
		// then restart" contract.
		return Plan{
			Action: ActionRestart, Summary: "restart launchd user agent",
			RunCmd: []string{"launchctl", "kickstart", "-k", "gui/" + osUserUID() + "/" + s.Label},
			Hint:   "launchctl kickstart -k gui/$(id -u)/" + s.Label,
		}
	case BackendSystemd:
		return Plan{
			Action: ActionRestart, Summary: "restart systemd user unit",
			RunCmd: []string{"systemctl", "--user", "restart", ServiceName + ".service"},
			Hint:   "systemctl --user restart " + ServiceName + ".service",
		}
	default:
		return Plan{
			Action: ActionRestart, Summary: "foreground daemon (no supervisor)",
			Hint: "stop the foreground `sift daemon` and run it again to pick up the new release",
		}
	}
}

// planReload deliberately reuses restart until the daemon implements SIGHUP
// configuration reload. Keeping it as a distinct action makes that limitation
// visible to callers instead of implying a hot reload occurred.
func (s Spec) planReload() Plan {
	plan := s.planRestart()
	plan.Action = ActionReload
	plan.Summary = "reload service (currently restarts)"
	return plan
}

// LegacyLaunchdStatusPlan probes the v0.1.0 agent during the one-time label
// migration. A non-zero result means it is not loaded.
func LegacyLaunchdStatusPlan() Plan {
	return Plan{Action: ActionStatus, RunCmd: []string{"launchctl", "list", LegacyLabel}}
}

// LegacyLaunchdBootoutPlan unloads the v0.1.0 agent during migration.
func LegacyLaunchdBootoutPlan() Plan {
	return Plan{Action: ActionUninstall, RunCmd: []string{"launchctl", "bootout", "gui/" + osUserUID() + "/" + LegacyLabel}}
}

func (s Spec) planStatus() Plan {
	switch s.Backend {
	case BackendLaunchd:
		return Plan{
			Action: ActionStatus, Summary: "launchd agent status",
			RunCmd: []string{"launchctl", "list", s.Label},
			Hint:   "launchctl list " + s.Label,
		}
	case BackendSystemd:
		return Plan{
			Action: ActionStatus, Summary: "systemd unit status",
			RunCmd: []string{"systemctl", "--user", "status", ServiceName + ".service"},
			Hint:   "systemctl --user status " + ServiceName + ".service",
		}
	default:
		// hosting §5: the foreground status contract is the operator socket's
		// existence, not a static hint. Probe it now so the CLI can report a
		// verdict verifiable with `[ -S "$SIFT_HOME/siftd.sock" ]`.
		sock := filepath.Join(s.HomePath, "siftd.sock")
		status := "absent"
		if info, err := os.Stat(sock); err == nil && info.Mode()&os.ModeSocket != 0 {
			status = "present"
		}
		return Plan{
			Action:     ActionStatus,
			Summary:    "foreground daemon (no supervisor)",
			Status:     status,
			SocketPath: sock,
			Hint:       "the daemon is foreground-managed; check the operator socket at " + sock,
		}
	}
}

func (s Spec) foregroundHint() string {
	return fmt.Sprintf("no process supervisor detected; run `%s daemon` in the foreground (a terminal, tmux or screen). V0 does not autorestart in this mode (DESIGN §11). Logs: %s", s.DaemonPath, s.LogErr)
}

// osUserUID returns the current user's numeric uid for the launchd kickstart
// domain target. It is resolved lazily so tests do not depend on it.
func osUserUID() string {
	if uid := os.Getuid(); uid >= 0 {
		return fmt.Sprintf("%d", uid)
	}
	return "$(id -u)"
}

// unitFuncs are the template functions that make a user-configurable SIFT_HOME
// safe inside the generated units. The home (and every path derived from it:
// daemon, logs) may legally contain &, space, #, and angle brackets; a unit
// must stay loadable and must decode back to exactly the original path.
// launchd consumes XML (plist) so values are entity-escaped; systemd consumes
// its own syntax so ExecStart/Environment values are quoted as one token.
var unitFuncs = template.FuncMap{
	"xmlEscape":    xmlEscape,
	"systemdQuote": systemdQuote,
}

// xmlEscape renders a value for insertion into XML element content. Only &, <
// and > are special there; every XML parser (launchd's own plutil included)
// decodes the entities back to the original characters, so escaping never
// changes path semantics. Spaces and # need no XML escaping at all.
func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// systemdQuote renders a value so systemd parses it as exactly one token in
// ExecStart / Environment: the value is double-quoted and any embedded
// backslash or double quote is escaped. SIFT_HOME may legally contain spaces
// (word-splitting), & and # (both harmless inside a quoted token; systemd only
// treats a line-leading # as a comment), so the quotes keep the path intact.
func systemdQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// Render renders the unit file content for the spec's backend. The foreground
// backend renders nothing (it has no unit file).
func (s Spec) Render() ([]byte, error) {
	var tmpl *template.Template
	switch s.Backend {
	case BackendLaunchd:
		tmpl = template.Must(template.New("launchd").Funcs(unitFuncs).Parse(launchdTemplate))
	case BackendSystemd:
		tmpl = template.Must(template.New("systemd").Funcs(unitFuncs).Parse(systemdTemplate))
	case BackendForeground:
		return nil, nil
	default:
		return nil, fmt.Errorf("hosting: unknown backend %q", s.Backend)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, s); err != nil {
		return nil, fmt.Errorf("hosting: render %s unit: %w", s.Backend, err)
	}
	return buf.Bytes(), nil
}

// launchdTemplate is a user agent (~/Library/LaunchAgents), not a system
// daemon: it must run as the user to reach the user's gh/glab/agent CLI
// login state (DESIGN §11). KeepAlive gives crash autorestart; RunAtLoad
// starts it at login; there is no port anywhere — the daemon speaks two Unix
// sockets. ProgramArguments follows the current symlink so an upgrade needs
// only a restart.
const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Sift launchd user agent (DESIGN §11 / WBS M8 §8.2). Generated for release {{.Release}}.
     Run as a user agent (not a system daemon) so it can use the user's gh/glab
     and agent CLI login state. Opens no network port; control plane is two
     owner-only Unix sockets. ProgramArguments follows the ` + "`current`" + ` release
     symlink: upgrade with ` + "`sift install`" + ` then ` + "`sift service restart`" + `. -->
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{xmlEscape .DaemonPath}}</string>
        <string>daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>{{xmlEscape .LogOut}}</string>
    <key>StandardErrorPath</key>
    <string>{{xmlEscape .LogErr}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>SIFT_HOME</key>
        <string>{{xmlEscape .HomePath}}</string>
        <key>PATH</key>
        <string>` + LaunchdPath + `</string>
    </dict>
</dict>
</plist>
`

// systemdTemplate is a user unit (~/.config/systemd/user/sift.service), not a
// system service. Restart=on-failure gives crash autorestart without looping a
// clean exit. enable-linger is required to keep the user manager alive when
// the user is not logged in (DESIGN §11). ExecStart follows `current`.
const systemdTemplate = `# Sift systemd user unit (DESIGN §11 / WBS M8 §8.2). Generated for release {{.Release}}.
#
# Install as a USER unit (not a system service) so it runs as your user and can
# reach your gh/glab and agent CLI login state. To run while you are not logged
# in, enable lingering for your user once:
#     loginctl enable-linger "$USER"
#
# The daemon opens no network port; its control plane is two owner-only Unix
# sockets. ExecStart follows the ` + "`current`" + ` release symlink, so after
# ` + "`sift install`" + ` of a new release just run ` + "`sift service restart`" + `.
#
# Foreground fallback (no systemd): on a distribution without systemd, run
#     {{.DaemonPath}} daemon
# in a terminal / tmux / screen. V0 does not autorestart in that mode.
[Unit]
Description=Sift local control-plane daemon (user)
Documentation=https://github.com/xsift/sift

[Service]
Type=simple
ExecStart={{systemdQuote .DaemonPath}} daemon
Restart=on-failure
RestartSec=10
Environment=SIFT_HOME={{systemdQuote .HomePath}}

[Install]
WantedBy=default.target
`

// Write performs the atomic unit-file write (or removal, when content is nil)
// described by a plan. Writes are temp-file + rename so a crash mid-write can
// never leave a half-written unit that launchd/systemd would happily load.
func Write(plan Plan) error {
	if plan.WriteFile == "" {
		return nil
	}
	if plan.Content == nil {
		// Removal is best-effort: an already-absent unit is a successful
		// uninstall.
		if err := os.Remove(plan.WriteFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("hosting: remove %s: %w", plan.WriteFile, err)
		}
		return nil
	}
	dir := filepath.Dir(plan.WriteFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("hosting: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".sift-unit-*")
	if err != nil {
		return fmt.Errorf("hosting: stage unit file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	mode := os.FileMode(0o644)
	if _, err := tmp.Write(plan.Content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("hosting: write unit file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("hosting: chmod unit file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hosting: close unit file: %w", err)
	}
	if err := os.Rename(tmpName, plan.WriteFile); err != nil {
		return fmt.Errorf("hosting: install unit file: %w", err)
	}
	return nil
}

// IsAlreadyUnloaded reports launchctl's documented "No such process" result
// for bootout. That result makes uninstall and label migration idempotent.
func IsAlreadyUnloaded(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 3 && strings.Contains(strings.ToLower(err.Error()), "no such process")
}

// Exec runs a plan's commands when the platform tool is present. It returns
// ErrNoBackend (with the foreground hint available via Plan.Hint) when the
// tool is absent, so the caller reports the foreground path instead of
// failing. A systemd install reloads unit definitions before executing its
// explicit enable --now plan command, so the just-written unit is visible.
func Exec(plan Plan) ([]byte, error) {
	if len(plan.RunCmd) == 0 {
		return nil, ErrNoBackend
	}
	if _, err := exec.LookPath(plan.RunCmd[0]); err != nil {
		return nil, ErrNoBackend
	}
	if len(plan.BeforeCmd) > 0 {
		if _, err := exec.LookPath(plan.BeforeCmd[0]); err != nil {
			return nil, ErrNoBackend
		}
		out, err := runCommand(plan.BeforeCmd)
		if err != nil && !(plan.Action == ActionInstall && IsAlreadyUnloaded(err)) {
			return out, err
		}
	}
	if plan.Action == ActionInstall && plan.RunCmd[0] == "systemctl" {
		reloadOut, reloadErr := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
		if reloadErr != nil {
			return reloadOut, fmt.Errorf("hosting: systemctl --user daemon-reload: %w\n%s", reloadErr, reloadOut)
		}
	}
	if plan.Action == ActionStart && plan.RunCmd[0] == "launchctl" && len(plan.ProbeCmd) > 0 {
		return execLaunchdStart(plan)
	}
	if plan.Action == ActionInstall && plan.RunCmd[0] == "launchctl" {
		return execLaunchdInstall(plan)
	}
	return runCommand(plan.RunCmd)
}

// execLaunchdInstall runs the launchd install continuation after Exec has
// already booted out the current label (tolerating "No such process" as the
// fresh-install case): wait boundedly until launchctl confirms the label is
// absent (teardown quiescence, issue #968), then bootstrap. A bootstrap that
// fails with the known transient exit 5 (Input/output error) is retried a
// bounded number of times, and only while absence stays confirmed. Everything
// else — permission, missing GUI domain, malformed plist, or a state that is
// ambiguous or still present — fails immediately without retry, and exhaustion
// returns a non-zero actionable error rather than claiming success.
func execLaunchdInstall(plan Plan) ([]byte, error) {
	if len(plan.AbsenceProbe) > 0 {
		if waitOut, waitErr := waitForLaunchdAbsent(plan.AbsenceProbe); waitErr != nil {
			return waitOut, waitErr
		}
	}
	var lastOut []byte
	var lastErr error
	for attempt := 0; attempt <= launchdBootstrapRetries; attempt++ {
		if attempt > 0 {
			sleep(launchdBootstrapBackoff)
		}
		out, err := runCommand(plan.RunCmd)
		if err == nil {
			return out, nil
		}
		lastOut, lastErr = out, err
		// Only the known transient exit 5 while the label is still confirmed
		// absent is retryable. Any other failure, or any state that is not
		// unambiguously absent, is permanent: surface it now.
		if !isTransientBootstrapFailure(err) || len(plan.AbsenceProbe) == 0 {
			return out, launchdDomainHint(err)
		}
		if _, probeErr := runCommand(plan.AbsenceProbe); probeErr == nil || !isServiceAbsent(probeErr) {
			return out, fmt.Errorf("hosting: launchctl bootstrap failed with transient exit 5 but the service label is not confirmed absent; not retrying: %w", err)
		}
	}
	return lastOut, fmt.Errorf("hosting: launchctl bootstrap failed %d consecutive times with transient exit 5 (input/output error) while the label stayed absent; launchd teardown did not quiesce; aborting install — rerun `sift service install`: %w", launchdBootstrapRetries+1, lastErr)
}

// waitForLaunchdAbsent polls the absence probe until launchctl confirms the
// label is gone, the bounded deadline passes, or the probe fails in a way that
// is not a clear "absent" verdict (permission, domain, ambiguous). A label
// that never disappears is an install abort: bootstrapping over a half-torn
// job is exactly the race issue #968 reproduces.
func waitForLaunchdAbsent(probe []string) ([]byte, error) {
	target := strings.Join(probe, " ")
	deadline := time.Now().Add(launchdTeardownTimeout)
	for {
		out, err := runCommand(probe)
		if err == nil {
			// Probe succeeded: the label is still loaded. Keep waiting, but only
			// until the bounded deadline.
			if time.Now().After(deadline) {
				return out, fmt.Errorf("hosting: launchd label %s is still loaded %s after bootout; launchd teardown did not quiesce; aborting install — rerun `sift service install`", target, launchdTeardownTimeout)
			}
			sleep(launchdTeardownPoll)
			continue
		}
		if isServiceAbsent(err) {
			return out, nil
		}
		return out, launchdDomainHint(fmt.Errorf("hosting: cannot confirm launchd label %s is absent after bootout: %w", target, err))
	}
}

// isServiceAbsent reports whether a launchctl result unambiguously means the
// label does not exist. Absence must be proven by the message text, never by a
// bare non-zero exit: permission, domain and malformed-input failures must not
// be mistaken for a clean teardown.
func isServiceAbsent(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, hard := range []string{"permission denied", "operation not permitted", "could not find domain", "domain unavailable", "invalid property list", "malformed"} {
		if strings.Contains(text, hard) {
			return false
		}
	}
	for _, absent := range []string{"no such process", "could not find service", "service not found", "does not exist"} {
		if strings.Contains(text, absent) {
			return true
		}
	}
	return false
}

// isTransientBootstrapFailure reports launchctl's known teardown-race bootstrap
// failure: exit 5 / "Input/output error" (errno EIO), which a bounded retry
// after confirmed absence can recover from. Every other failure — permission,
// missing domain, malformed plist, different exit code — is permanent and must
// never be retried. Hard markers win even when launchd bundles them into the
// same stderr as an EIO text, so a permission or domain problem is never
// masked as transient.
func isTransientBootstrapFailure(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 5 {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, hard := range []string{"permission denied", "operation not permitted", "could not find domain", "domain unavailable", "invalid property list", "malformed"} {
		if strings.Contains(text, hard) {
			return false
		}
	}
	return strings.Contains(text, "input/output error") || strings.Contains(text, "eio")
}

func execLaunchdStart(plan Plan) ([]byte, error) {
	probeOut, probeErr := runCommand(plan.ProbeCmd)
	if probeErr == nil {
		out, err := runCommand(plan.RunCmd)
		return out, launchdDomainHint(err)
	}
	if !isLaunchdUnloaded(probeErr) {
		return probeOut, launchdDomainHint(probeErr)
	}
	if plan.StartUnit == "" {
		return probeOut, fmt.Errorf("hosting: launchd label is not loaded and no plist is configured; run `sift service install` first")
	}
	if _, err := os.Stat(plan.StartUnit); err != nil {
		return probeOut, fmt.Errorf("hosting: launchd label is not loaded and plist %s is unavailable; run `sift service install` first: %w", plan.StartUnit, err)
	}
	out, err := runCommand(plan.FallbackCmd)
	return out, launchdDomainHint(err)
}

func isLaunchdUnloaded(err error) bool {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "permission denied") || strings.Contains(text, "operation not permitted") {
		return false
	}
	return strings.Contains(text, "no such process") || strings.Contains(text, "could not find service")
}

func launchdDomainHint(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "could not find domain") || strings.Contains(text, "domain unavailable") {
		return fmt.Errorf("hosting: launchd GUI user domain is unavailable (SSH sessions usually have no GUI domain); log into the macOS desktop and retry, or run `sift daemon` in the foreground: %w", err)
	}
	return err
}

func runCommand(cmd []string) ([]byte, error) {
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("hosting: %s: %w\n%s", strings.Join(cmd, " "), err, out)
	}
	return out, nil
}

// Formula renders the Homebrew tap formula draft for a release. It is a draft:
// the published tap is generated from the GitHub Release (Issue C / WBS §8.2
// non-scope). The archive URL and name match the GoReleaser output
// (specs/release.md §2) so a release install and a brew install expose the
// same two binaries; the formula installs both and points the user at
// `sift service install` for the launchd agent. No port is opened.
//
// sha256 is the darwin/arm64 archive digest; a real release computes it from
// the published archive (tools/release verify).
func Formula(release, sha256 string) string {
	if release == "" {
		release = version.Release
	}
	if sha256 == "" {
		sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	var b strings.Builder
	b.WriteString("# frozen_string_literal: true\n\n")
	b.WriteString("# Sift Homebrew formula (DRAFT). Generated from the GitHub Release; the\n")
	b.WriteString("# published tap lives in its own repo (WBS §8.2 non-scope here). The\n")
	b.WriteString("# archive name and layout match specs/release.md §2 so a brew install\n")
	b.WriteString("# and a release-archive install expose the same two binaries. The daemon\n")
	b.WriteString("# opens no network port (two owner-only Unix sockets).\n")
	b.WriteString("# Regenerate with: go run ./tools/hosting formula --version <v> --sha256 <h>\n")
	b.WriteString("class Sift < Formula\n")
	b.WriteString("  desc \"Local multi-agent task orchestration hub\"\n")
	b.WriteString("  homepage \"https://github.com/xsift/sift\"\n")
	fmt.Fprintf(&b, "  url \"https://github.com/xsift/sift/releases/download/v%s/sift_%s_darwin_arm64.tar.gz\"\n", release, release)
	fmt.Fprintf(&b, "  version %q\n", release)
	fmt.Fprintf(&b, "  sha256 %q\n", sha256)
	b.WriteString("\n")
	b.WriteString("  # The archive carries sift + sift-agent-wrapper + manifest.json for the\n")
	b.WriteString("  # darwin/arm64 combo; the formula installs both binaries (the daemon\n")
	b.WriteString("  # resolves its wrapper from its own install directory, DESIGN §8.4).\n")
	b.WriteString("  def install\n")
	b.WriteString("    bin.install \"sift\"\n")
	b.WriteString("    bin.install \"sift-agent-wrapper\"\n")
	b.WriteString("  end\n")
	b.WriteString("\n")
	b.WriteString("  def caveats\n")
	b.WriteString("    <<~EOS\n")
	b.WriteString("      Sift runs as a user-level launchd agent (no system daemon, no port).\n")
	b.WriteString("      Register it after install with:\n")
	b.WriteString("        sift service install\n")
	b.WriteString("      Logs: ~/.sift/logs/\n")
	b.WriteString("    EOS\n")
	b.WriteString("  end\n")
	b.WriteString("\n")
	b.WriteString("  test do\n")
	fmt.Fprintf(&b, "    assert_match(/^%s$/, shell_output(\"#{bin}/sift --version\").strip)\n", release)
	b.WriteString("  end\n")
	b.WriteString("end\n")
	return b.String()
}
