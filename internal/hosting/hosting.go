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
// systemctl) are invoked by the thin CLI via exec.Plan.RunCmd, never here.
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

	"github.com/miaoxiaoyong/sift/internal/install"
	"github.com/miaoxiaoyong/sift/internal/version"
)

// These OS directory resolvers are package-level variables so tests can pin
// them to temp paths and exercise the launchd/systemd backends from any host
// OS without depending on the real $HOME / XDG_CONFIG_HOME layout. Production
// callers use the os.* defaults.
var (
	userHomeDir   = os.UserHomeDir
	userConfigDir = os.UserConfigDir
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
	Label = "com.miaoxiaoyong.sift"
	// ServiceName is the file stem for the systemd unit (`sift.service`).
	ServiceName = "sift"
)

// Action is the CLI verb the hosting layer acts on.
type Action string

const (
	ActionInstall   Action = "install"
	ActionUninstall Action = "uninstall"
	ActionRestart   Action = "restart"
	ActionStatus    Action = "status"
)

// ActionFromString maps a CLI verb to an Action, rejecting anything that is
// not one of the four supported verbs so the CLI surfaces a usage error rather
// than a silent default.
func ActionFromString(s string) (Action, error) {
	switch Action(s) {
	case ActionInstall, ActionUninstall, ActionRestart, ActionStatus:
		return Action(s), nil
	default:
		return "", fmt.Errorf("hosting: unknown service action %q (want install|uninstall|status|restart)", s)
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
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("hosting: resolve user home for launchd unit: %w", err)
		}
		return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
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

// Plan is what the CLI does for one action: optionally write a generated unit
// file (atomically) and optionally run a platform command. Keeping the two
// separate lets the package stay exec-free while the thin CLI performs the IO.
type Plan struct {
	Action    Action
	Summary   string
	WriteFile string   // destination of an atomic unit-file write (empty = no write)
	Content   []byte   // content for WriteFile
	RunCmd    []string // command + args to execute; nil means manual/foreground only
	Hint      string   // human step printed when RunCmd is nil or the backend is missing
}

// Plan builds the action's plan. It performs no IO beyond resolving the spec's
// current symlink (already done in NewSpec) so it is deterministic for tests.
func (s Spec) Plan(action Action) (Plan, error) {
	switch action {
	case ActionInstall:
		return s.planInstall(), nil
	case ActionUninstall:
		return s.planUninstall(), nil
	case ActionRestart:
		return s.planRestart(), nil
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
			RunCmd: []string{"launchctl", "load", s.UnitPath},
			Hint:   "launchctl load " + s.UnitPath,
		}
	case BackendSystemd:
		content, _ := s.Render()
		return Plan{
			Action: ActionInstall, Summary: "install systemd user unit",
			WriteFile: s.UnitPath, Content: content,
			RunCmd: []string{"systemctl", "--user", "daemon-reload"},
			Hint:   "systemctl --user enable --now " + ServiceName + ".service  (enable-linger for headless: loginctl enable-linger $USER)",
		}
	default:
		return Plan{
			Action: ActionInstall, Summary: "foreground daemon (no supervisor)",
			Hint:   s.foregroundHint(),
		}
	}
}

func (s Spec) planUninstall() Plan {
	switch s.Backend {
	case BackendLaunchd:
		return Plan{
			Action: ActionUninstall, Summary: "unload and remove launchd user agent",
			WriteFile: s.UnitPath, Content: nil, // nil content => remove the file
			RunCmd:    []string{"launchctl", "unload", s.UnitPath},
			Hint:      "launchctl unload " + s.UnitPath,
		}
	case BackendSystemd:
		return Plan{
			Action: ActionUninstall, Summary: "disable and remove systemd user unit",
			WriteFile: s.UnitPath, Content: nil,
			RunCmd:    []string{"systemctl", "--user", "disable", "--now", ServiceName+".service"},
			Hint:      "systemctl --user disable --now " + ServiceName + ".service",
		}
	default:
		return Plan{
			Action: ActionUninstall, Summary: "foreground daemon (no supervisor)",
			Hint:   "stop the foreground `sift daemon` process",
		}
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
			RunCmd: []string{"systemctl", "--user", "restart", ServiceName+".service"},
			Hint:   "systemctl --user restart " + ServiceName + ".service",
		}
	default:
		return Plan{
			Action: ActionRestart, Summary: "foreground daemon (no supervisor)",
			Hint:   "stop the foreground `sift daemon` and run it again to pick up the new release",
		}
	}
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
			RunCmd: []string{"systemctl", "--user", "status", ServiceName+".service"},
			Hint:   "systemctl --user status " + ServiceName + ".service",
		}
	default:
		return Plan{
			Action: ActionStatus, Summary: "foreground daemon (no supervisor)",
			Hint:   "the daemon is foreground-managed; check the operator socket at " + filepath.Join(s.HomePath, "siftd.sock"),
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

// Render renders the unit file content for the spec's backend. The foreground
// backend renders nothing (it has no unit file).
func (s Spec) Render() ([]byte, error) {
	var tmpl *template.Template
	switch s.Backend {
	case BackendLaunchd:
		tmpl = template.Must(template.New("launchd").Parse(launchdTemplate))
	case BackendSystemd:
		tmpl = template.Must(template.New("systemd").Parse(systemdTemplate))
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
        <string>{{.DaemonPath}}</string>
        <string>daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>{{.LogOut}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogErr}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>SIFT_HOME</key>
        <string>{{.HomePath}}</string>
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
Documentation=https://github.com/miaoxiaoyong/sift

[Service]
Type=simple
ExecStart={{.DaemonPath}} daemon
Restart=on-failure
RestartSec=10
Environment=SIFT_HOME={{.HomePath}}

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

// Exec runs a plan's RunCmd when the platform tool is present. It returns
// ErrNoBackend (with the foreground hint available via Plan.Hint) when the
// tool is absent, so the caller reports the foreground path instead of
// failing. systemd daemon-reload during install is followed by an enable --now
// step so the unit actually starts; that second step is folded in here rather
// than modeled as a second plan to keep the CLI a single command.
func Exec(plan Plan) ([]byte, error) {
	if len(plan.RunCmd) == 0 {
		return nil, ErrNoBackend
	}
	name := plan.RunCmd[0]
	if _, err := exec.LookPath(name); err != nil {
		return nil, ErrNoBackend
	}
	args := plan.RunCmd[1:]
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("hosting: %s: %w\n%s", strings.Join(plan.RunCmd, " "), err, out)
	}
	// install's daemon-reload does not start the unit; enable --now does.
	if plan.Action == ActionInstall && name == "systemctl" {
		enableOut, enableErr := exec.Command("systemctl", "--user", "enable", "--now", ServiceName+".service").CombinedOutput()
		if enableErr != nil {
			return append(out, enableOut...), fmt.Errorf("hosting: systemctl --user enable --now: %w\n%s", enableErr, enableOut)
		}
		out = append(out, enableOut...)
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
	b.WriteString("  homepage \"https://github.com/miaoxiaoyong/sift\"\n")
	fmt.Fprintf(&b, "  url \"https://github.com/miaoxiaoyong/sift/releases/download/v%s/sift_%s_darwin_arm64.tar.gz\"\n", release, release)
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
