package hosting

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/version"
)

// installFakeRelease provisions a temp SIFT_HOME with a release installed under
// bin/current, mirroring specs/release.md §3 so NewSpec succeeds. It returns
// the home and the absolute daemon path the unit must point at.
func installFakeRelease(t *testing.T) (home, daemonPath string) {
	t.Helper()
	home = t.TempDir()
	return home, installFakeReleaseAt(t, home)
}

// installFakeReleaseAt provisions a release under an existing home directory.
// Unlike t.TempDir, the caller's home may legally contain &, space or # — the
// escaping tests need exactly that.
func installFakeReleaseAt(t *testing.T, home string) (daemonPath string) {
	t.Helper()
	release := version.Release
	versionDir := filepath.Join(home, "bin", release)
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	daemonPath = filepath.Join(versionDir, "sift")
	// Executable shell fixture: the hosting layer only stats +x and reads the
	// current symlink target, never runs it.
	content := []byte("#!/bin/sh\necho " + release + "\n")
	if err := os.WriteFile(daemonPath, content, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(home, "bin", "current")
	if err := os.Symlink(release, current); err != nil {
		t.Fatal(err)
	}
	return daemonPath
}

// pinDirs routes both OS directory resolvers at a temp root so backend path
// resolution is deterministic on any host OS, and restores them on cleanup.
func pinDirs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cfg := filepath.Join(root, "config")
	for _, d := range []string{home, cfg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldHome, oldCfg := userHomeDir, userConfigDir
	userHomeDir = func() (string, error) { return home, nil }
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userHomeDir, userConfigDir = oldHome, oldCfg })
}

func TestDetectSelectsBackendByGOOS(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want Backend
	}{
		{"darwin", BackendLaunchd},
		{"linux", BackendSystemd},
		{"freebsd", BackendForeground},
		{"windows", BackendForeground},
		{"", BackendForeground},
	} {
		if got := Detect(tc.goos); got != tc.want {
			t.Errorf("Detect(%q) = %q, want %q", tc.goos, got, tc.want)
		}
	}
}

func TestLaunchdIdentityKeepsCurrentAndLegacyLabels(t *testing.T) {
	if Label != "cn.hexai.sift" {
		t.Fatalf("Label = %q, want installed v0.5.4 label cn.hexai.sift", Label)
	}
	if LegacyLabel != "com.miaoxiaoyong.sift" {
		t.Fatalf("LegacyLabel = %q, want original v0.1.0 label", LegacyLabel)
	}
	if Label == LegacyLabel {
		t.Fatal("current and legacy launchd labels must remain distinct")
	}
}

func TestNewSpecRequiresInstalledRelease(t *testing.T) {
	home := t.TempDir()
	if _, err := NewSpecFor(home, "linux"); err == nil {
		t.Fatal("NewSpecFor without an installed release succeeded")
	}
}

func TestNewSpecRejectsRelativeHome(t *testing.T) {
	if _, err := NewSpecFor("relative/home", "linux"); err == nil {
		t.Fatal("NewSpecFor accepted a relative home")
	}
	if _, err := NewSpecFor("", "linux"); err == nil {
		t.Fatal("NewSpecFor accepted an empty home")
	}
}

func TestNewSpecRejectsNonExecutableDaemon(t *testing.T) {
	home := t.TempDir()
	versionDir := filepath.Join(home, "bin", version.Release)
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "sift"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(version.Release, filepath.Join(home, "bin", "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpecFor(home, "linux"); err == nil {
		t.Fatal("NewSpecFor accepted a non-executable daemon")
	}
}

func TestNewSpecRejectsCorruptedCurrent(t *testing.T) {
	home := t.TempDir()
	versionDir := filepath.Join(home, "bin", "not-semver")
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "sift"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("not-semver", filepath.Join(home, "bin", "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpecFor(home, "linux"); err == nil {
		t.Fatal("NewSpecFor accepted a non-SemVer current symlink target")
	}
}

func TestNewSpecResolvesLaunchdUnitPath(t *testing.T) {
	home, _ := installFakeRelease(t)
	pinDirs(t)
	spec, err := NewSpecFor(home, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Backend != BackendLaunchd {
		t.Fatalf("backend = %q, want launchd", spec.Backend)
	}
	if got := spec.DaemonPath; !strings.HasSuffix(got, filepath.Join("bin", "current", "sift")) {
		t.Fatalf("daemon path %q does not follow bin/current/sift", got)
	}
	homeDir, _ := userHomeDir()
	want := filepath.Join(homeDir, "Library", "LaunchAgents", Label+".plist")
	if spec.UnitPath != want {
		t.Fatalf("unit path = %q, want %q", spec.UnitPath, want)
	}
}

func TestNewSpecResolvesSystemdUnitPath(t *testing.T) {
	home, _ := installFakeRelease(t)
	pinDirs(t)
	spec, err := NewSpecFor(home, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Backend != BackendSystemd {
		t.Fatalf("backend = %q, want systemd", spec.Backend)
	}
	cfg, _ := userConfigDir()
	want := filepath.Join(cfg, "systemd", "user", ServiceName+".service")
	if spec.UnitPath != want {
		t.Fatalf("unit path = %q, want %q", spec.UnitPath, want)
	}
}

func TestNewSpecForegroundHasNoUnitPath(t *testing.T) {
	home, _ := installFakeRelease(t)
	spec, err := NewSpecFor(home, "freebsd")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Backend != BackendForeground {
		t.Fatalf("backend = %q, want foreground", spec.Backend)
	}
	if spec.UnitPath != "" {
		t.Fatalf("foreground unit path = %q, want empty", spec.UnitPath)
	}
}

func TestRenderLaunchdTemplateContract(t *testing.T) {
	home, _ := installFakeRelease(t)
	pinDirs(t)
	spec, err := NewSpecFor(home, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	content, err := spec.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	// Crash autorestart + start-at-load.
	if !strings.Contains(s, "<key>KeepAlive</key>") || !strings.Contains(s, "<true/>") {
		t.Error("launchd template lacks KeepAlive=true crash autorestart")
	}
	if !strings.Contains(s, "<key>RunAtLoad</key>") {
		t.Error("launchd template lacks RunAtLoad")
	}
	// ExecStart-equivalent points at the current symlink daemon.
	wantArgs := "<string>" + xmlEscape(spec.DaemonPath) + "</string>"
	if !strings.Contains(s, wantArgs) {
		t.Errorf("launchd template ProgramArguments does not point at %s", spec.DaemonPath)
	}
	if !strings.Contains(s, "<string>daemon</string>") {
		t.Error("launchd template does not pass `daemon` subcommand")
	}
	if !strings.Contains(s, "<key>SIFT_HOME</key>") {
		t.Error("launchd template does not pin SIFT_HOME")
	}
	if !strings.Contains(s, "<key>PATH</key>") || !strings.Contains(s, "<string>"+LaunchdPath+"</string>") {
		t.Errorf("launchd template PATH is not the closed service PATH %q", LaunchdPath)
	}
	if !strings.Contains(s, home) {
		t.Error("launchd template does not reference the resolved home")
	}
	// No network listener anywhere: a plist socket entry would be Sockets.
	if strings.Contains(s, "Sockets") {
		t.Error("launchd template declares a socket (must be none)")
	}
	if strings.Contains(s, "Listener") {
		t.Error("launchd template mentions a listener (must open none)")
	}
	// Paths are absolute.
	if !filepath.IsAbs(spec.LogOut) || !filepath.IsAbs(spec.LogErr) {
		t.Errorf("log paths must be absolute: %q %q", spec.LogOut, spec.LogErr)
	}
}

func TestRenderSystemdTemplateContract(t *testing.T) {
	home, _ := installFakeRelease(t)
	pinDirs(t)
	spec, err := NewSpecFor(home, "linux")
	if err != nil {
		t.Fatal(err)
	}
	content, err := spec.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	// Crash autorestart on failure (not on clean exit, so it does not loop).
	if !strings.Contains(s, "Restart=on-failure") {
		t.Error("systemd template lacks Restart=on-failure")
	}
	if strings.Contains(s, "Restart=always") {
		t.Error("systemd template must not Restart=always (clean exit would loop)")
	}
	if !strings.Contains(s, "RestartSec=10") {
		t.Error("systemd template lacks RestartSec")
	}
	// ExecStart follows current/sift daemon (quoted as one token so a home
	// containing spaces or specials stays intact).
	if !strings.Contains(s, "ExecStart="+systemdQuote(spec.DaemonPath)+" daemon") {
		t.Errorf("systemd ExecStart does not point at %s daemon", spec.DaemonPath)
	}
	if !strings.Contains(s, "WantedBy=default.target") {
		t.Error("systemd template is not a user unit (WantedBy=default.target)")
	}
	if !strings.Contains(s, "Environment=SIFT_HOME="+systemdQuote(home)) {
		t.Error("systemd template does not pin SIFT_HOME")
	}
	// Foreground fallback is documented in-unit.
	if !strings.Contains(s, "Foreground fallback") {
		t.Error("systemd template does not document the foreground fallback")
	}
	// enable-linger guidance present.
	if !strings.Contains(s, "enable-linger") {
		t.Error("systemd template does not mention loginctl enable-linger")
	}
	// No port / socket activation.
	if strings.Contains(s, "ListenStream") || strings.Contains(s, "Socket") {
		t.Error("systemd template declares a socket (must be none)")
	}
}

func TestRenderForegroundReturnsNothing(t *testing.T) {
	home, _ := installFakeRelease(t)
	spec, err := NewSpecFor(home, "freebsd")
	if err != nil {
		t.Fatal(err)
	}
	content, err := spec.Render()
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Errorf("foreground render = %q, want nil", content)
	}
}

func TestPlanInstallWritesUnitAndLoads(t *testing.T) {
	for _, tc := range []struct {
		name       string
		goos       string
		wantBefore []string
		wantCmd    []string
	}{
		{"launchd", "darwin", []string{"launchctl", "bootout"}, []string{"launchctl", "bootstrap"}},
		{"systemd", "linux", nil, []string{"systemctl", "--user", "enable", "--now", ServiceName + ".service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := installFakeRelease(t)
			pinDirs(t)
			spec, err := NewSpecFor(home, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := spec.Plan(ActionInstall)
			if err != nil {
				t.Fatal(err)
			}
			if plan.WriteFile != spec.UnitPath {
				t.Errorf("WriteFile = %q, want %q", plan.WriteFile, spec.UnitPath)
			}
			if len(plan.Content) == 0 {
				t.Fatal("install plan has no unit content to write")
			}
			if len(plan.BeforeCmd) < len(tc.wantBefore) || strings.Join(plan.BeforeCmd[:len(tc.wantBefore)], "\x00") != strings.Join(tc.wantBefore, "\x00") {
				t.Errorf("BeforeCmd = %v, want prefix %v", plan.BeforeCmd, tc.wantBefore)
			}
			if len(plan.RunCmd) < len(tc.wantCmd) {
				t.Fatalf("RunCmd = %v, want prefix %v", plan.RunCmd, tc.wantCmd)
			}
			for i, w := range tc.wantCmd {
				if plan.RunCmd[i] != w {
					t.Errorf("RunCmd[%d] = %q, want %q; full=%v", i, plan.RunCmd[i], w, plan.RunCmd)
				}
			}
		})
	}
}

func TestLaunchdInstallPlanReplacesCurrentUnit(t *testing.T) {
	home, _ := installFakeRelease(t)
	pinDirs(t)
	spec, err := NewSpecFor(home, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := spec.Plan(ActionInstall)
	if err != nil {
		t.Fatal(err)
	}
	wantTarget := "gui/" + osUserUID() + "/" + Label
	if got := strings.Join(plan.BeforeCmd, " "); got != "launchctl bootout "+wantTarget {
		t.Errorf("BeforeCmd = %q, want bootout current label", got)
	}
	if got := strings.Join(plan.RunCmd, " "); got != "launchctl bootstrap gui/"+osUserUID()+" "+spec.UnitPath {
		t.Errorf("RunCmd = %q, want bootstrap rendered plist", got)
	}
}

func TestLegacyLaunchdMigrationPlans(t *testing.T) {
	pinDirs(t)
	path, err := LegacyLaunchdUnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("Library", "LaunchAgents", LegacyLabel+".plist")) {
		t.Errorf("legacy unit path = %q", path)
	}
	if got := LegacyLaunchdStatusPlan().RunCmd; strings.Join(got, " ") != "launchctl list "+LegacyLabel {
		t.Errorf("legacy status command = %v", got)
	}
	if got := LegacyLaunchdBootoutPlan().RunCmd; len(got) != 3 || got[0] != "launchctl" || got[1] != "bootout" || !strings.HasSuffix(got[2], "/"+LegacyLabel) {
		t.Errorf("legacy bootout command = %v", got)
	}
}

// TestIsLaunchdUnloaded pins the single launchctl-absent classifier that
// replaced isLaunchdUnloaded + IsAlreadyUnloaded (issue #967). It must map the
// real macOS exit codes: bootout absent = 3 ("No such process"), print-probe
// missing service = 113 ("Could not find service"); missing domain = 112 and
// permission/malformed failures must never read as absent, and a coincidental
// text match under the wrong exit code is not proof of absence.
func TestIsLaunchdUnloaded(t *testing.T) {
	bin := t.TempDir()
	launchctl := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\necho \"$LAUNCHCTL_MESSAGE\" >&2\nexit \"$LAUNCHCTL_EXIT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, message, exit string
		want                bool
	}{
		{"bootout no such process", "Boot-out failed: 3: No such process", "3", true},
		{"print could not find service", "Could not find service \"cn.hexai.sift\" in domain for user gui/1", "113", true},
		{"missing domain exit 112", "Could not find domain for user", "112", false},
		{"permission denied", "Bootstrap failed: 5: Operation not permitted", "5", false},
		{"malformed plist", "invalid property list", "6", false},
		{"coincidental text wrong exit", "No such process", "1", false},
		{"bootout message with hard marker", "Boot-out failed: 3: permission denied", "3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LAUNCHCTL_MESSAGE", tc.message)
			t.Setenv("LAUNCHCTL_EXIT", tc.exit)
			_, err := Exec(Plan{RunCmd: []string{"launchctl", "bootout"}})
			if got := IsLaunchdUnloaded(err); got != tc.want {
				t.Fatalf("IsLaunchdUnloaded(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
	if IsLaunchdUnloaded(nil) {
		t.Fatal("IsLaunchdUnloaded(nil) = true, want false")
	}
}

func TestLaunchdStartIsIdempotentAndBootstrapsOnlyWhenUnloaded(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "launchctl.log")
	launchctl := filepath.Join(bin, "launchctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + log + "\"\n" +
		"if [ \"$1\" = print ] && [ \"${LAUNCHD_LOADED:-}\" != 1 ]; then echo 'Could not find service' >&2; exit 113; fi\n" +
		"if [ \"$1\" = kickstart ] && [ \"${LAUNCHD_FAIL:-}\" = 1 ]; then echo 'permission denied' >&2; exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	unit := filepath.Join(t.TempDir(), "sift.plist")
	if err := os.WriteFile(unit, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := Plan{Action: ActionStart, ProbeCmd: []string{"launchctl", "print", "gui/1/" + Label}, RunCmd: []string{"launchctl", "kickstart", "gui/1/" + Label}, FallbackCmd: []string{"launchctl", "bootstrap", "gui/1", unit}, StartUnit: unit}

	t.Setenv("LAUNCHD_LOADED", "1")
	if _, err := Exec(plan); err != nil {
		t.Fatalf("loaded start = %v", err)
	}
	t.Setenv("LAUNCHD_LOADED", "")
	if _, err := Exec(plan); err != nil {
		t.Fatalf("unloaded start = %v", err)
	}
	calls, _ := os.ReadFile(log)
	got := string(calls)
	if !strings.Contains(got, "print gui/1/"+Label+"\nkickstart gui/1/"+Label) || !strings.Contains(got, "bootstrap gui/1 "+unit) {
		t.Fatalf("launchctl calls = %q", got)
	}
	t.Setenv("LAUNCHD_LOADED", "1")
	t.Setenv("LAUNCHD_FAIL", "1")
	if _, err := Exec(plan); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("kickstart failure = %v, want permission error", err)
	}
}

func TestLaunchdStartReportsMissingGUIActionably(t *testing.T) {
	bin := t.TempDir()
	launchctl := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\necho 'Could not find domain for user' >&2\nexit 112\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, err := Exec(Plan{Action: ActionStart, ProbeCmd: []string{"launchctl", "print", "gui/1/" + Label}, RunCmd: []string{"launchctl", "kickstart", "gui/1/" + Label}, FallbackCmd: []string{"launchctl", "bootstrap", "gui/1", "/tmp/sift.plist"}, StartUnit: "/tmp/sift.plist"})
	if err == nil || !strings.Contains(err.Error(), "SSH") || !strings.Contains(err.Error(), "foreground") {
		t.Fatalf("GUI-domain error = %v, want actionable SSH/foreground guidance", err)
	}
}

func TestPlanInstallForegroundHasNoWrite(t *testing.T) {
	home, _ := installFakeRelease(t)
	spec, err := NewSpecFor(home, "freebsd")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := spec.Plan(ActionInstall)
	if err != nil {
		t.Fatal(err)
	}
	if plan.WriteFile != "" || plan.Content != nil || plan.RunCmd != nil {
		t.Fatalf("foreground install plan should only hint, got %+v", plan)
	}
	if !strings.Contains(plan.Hint, "daemon") {
		t.Errorf("foreground hint = %q, want a daemon invocation", plan.Hint)
	}
}

func TestPlanRestartCommands(t *testing.T) {
	cases := []struct {
		goos    string
		wantCmd string
	}{
		{"darwin", "kickstart"},
		{"linux", "restart"},
	}
	for _, tc := range cases {
		home, _ := installFakeRelease(t)
		pinDirs(t)
		spec, err := NewSpecFor(home, tc.goos)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := spec.Plan(ActionRestart)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(plan.RunCmd, " "), tc.wantCmd) {
			t.Errorf("%s restart RunCmd = %v, want to contain %q", tc.goos, plan.RunCmd, tc.wantCmd)
		}
		if plan.WriteFile != "" {
			t.Errorf("%s restart should not write a file, WriteFile=%q", tc.goos, plan.WriteFile)
		}
	}
}

func TestPlanLifecycleCommands(t *testing.T) {
	for _, tc := range []struct {
		name   string
		goos   string
		action Action
		want   []string
	}{
		{"launchd start", "darwin", ActionStart, []string{"launchctl", "kickstart"}},
		{"launchd stop", "darwin", ActionStop, []string{"launchctl", "bootout"}},
		{"launchd uninstall", "darwin", ActionUninstall, []string{"launchctl", "bootout"}},
		{"launchd reload", "darwin", ActionReload, []string{"launchctl", "kickstart", "-k"}},
		{"systemd start", "linux", ActionStart, []string{"systemctl", "--user", "start", ServiceName + ".service"}},
		{"systemd stop", "linux", ActionStop, []string{"systemctl", "--user", "stop", ServiceName + ".service"}},
		{"systemd reload", "linux", ActionReload, []string{"systemctl", "--user", "restart", ServiceName + ".service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := installFakeRelease(t)
			pinDirs(t)
			spec, err := NewSpecFor(home, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := spec.Plan(tc.action)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.RunCmd) < len(tc.want) || strings.Join(plan.RunCmd[:len(tc.want)], "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("RunCmd = %v, want prefix %v", plan.RunCmd, tc.want)
			}
			if tc.action == ActionReload && !strings.Contains(plan.Summary, "currently restarts") {
				t.Errorf("reload summary = %q, want restart disclosure", plan.Summary)
			}
		})
	}
}

func TestPlanLifecycleForegroundHints(t *testing.T) {
	home, _ := installFakeRelease(t)
	spec, err := NewSpecFor(home, "freebsd")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []Action{ActionStart, ActionStop, ActionReload} {
		plan, err := spec.Plan(action)
		if err != nil {
			t.Fatal(err)
		}
		if plan.RunCmd != nil || !strings.Contains(plan.Hint, "foreground") {
			t.Errorf("%s foreground plan = %+v, want foreground-only hint", action, plan)
		}
	}
}

func TestPlanUninstallRemovesFile(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		home, _ := installFakeRelease(t)
		pinDirs(t)
		spec, err := NewSpecFor(home, goos)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := spec.Plan(ActionUninstall)
		if err != nil {
			t.Fatal(err)
		}
		if plan.WriteFile != spec.UnitPath {
			t.Errorf("%s uninstall WriteFile = %q, want %q", goos, plan.WriteFile, spec.UnitPath)
		}
		if plan.Content != nil {
			t.Errorf("%s uninstall Content must be nil (removal), got %q", goos, plan.Content)
		}
	}
}

func TestPlanRejectsUnknownAction(t *testing.T) {
	home, _ := installFakeRelease(t)
	spec, err := NewSpecFor(home, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Plan(Action("bogus")); err == nil {
		t.Fatal("unknown action succeeded")
	}
}

// TestPlanStatusForegroundProbesSocket pins the hosting §5 status contract for
// the foreground backend: the plan must carry a machine-checkable present|absent
// verdict for the operator socket, not a static hint. Both states are covered;
// "present" uses a real Unix socket so the verdict matches `[ -S ... ]`.
func TestPlanStatusForegroundProbesSocket(t *testing.T) {
	// The home is created directly under the OS temp root (like the cmd tests)
	// so the probed socket path stays within the Unix domain socket length
	// limit.
	for _, want := range []string{"absent", "present"} {
		t.Run(want, func(t *testing.T) {
			home, err := os.MkdirTemp("", "sift-status")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			installFakeReleaseAt(t, home)
			spec, err := NewSpecFor(home, "freebsd")
			if err != nil {
				t.Fatal(err)
			}
			sock := filepath.Join(home, "siftd.sock")
			if want == "present" {
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })
			}
			plan, err := spec.Plan(ActionStatus)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != want {
				t.Errorf("foreground status verdict = %q, want %q", plan.Status, want)
			}
			if plan.SocketPath != sock {
				t.Errorf("SocketPath = %q, want %q", plan.SocketPath, sock)
			}
			if plan.RunCmd != nil {
				t.Errorf("foreground status must not run a platform command, got %v", plan.RunCmd)
			}
		})
	}
}

// TestRenderEscapesUserPathsInUnits is the R1 P1-2 closing gate: SIFT_HOME is
// user-configurable and may legally contain &, spaces and #. Both unit formats
// must stay loadable (plutil -lint / systemd-analyze verify when the tool is
// on this host) and must decode back to exactly the original paths.
func TestRenderEscapesUserPathsInUnits(t *testing.T) {
	home := filepath.Join(t.TempDir(), "sift home & hash#")
	installFakeReleaseAt(t, home)
	pinDirs(t)
	for _, tc := range []struct{ name, goos, unitName string }{
		{"launchd", "darwin", Label + ".plist"},
		{"systemd", "linux", ServiceName + ".service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := NewSpecFor(home, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			content, err := spec.Render()
			if err != nil {
				t.Fatal(err)
			}
			unit := filepath.Join(t.TempDir(), tc.unitName)
			if err := os.WriteFile(unit, content, 0o644); err != nil {
				t.Fatal(err)
			}
			switch tc.goos {
			case "darwin":
				assertPlistPathSemantics(t, content, spec)
				verifyWithPlutil(t, unit, spec)
			case "linux":
				assertSystemdPathSemantics(t, content, spec)
				verifyWithSystemdAnalyze(t, unit)
			}
		})
	}
}

// assertPlistPathSemantics decodes the rendered plist as XML: entity decoding
// must recover the original paths exactly, proving the escaping kept the path
// semantics (the same decoding the real plist parser performs).
func assertPlistPathSemantics(t *testing.T, content []byte, spec Spec) {
	t.Helper()
	strs := plistStringValues(t, content)
	for _, want := range []string{spec.DaemonPath, spec.HomePath, spec.LogOut, spec.LogErr} {
		if !containsString(strs, want) {
			t.Errorf("decoded plist strings %v do not contain %q", strs, want)
		}
	}
}

// plistStringValues collects every non-whitespace XML character-data node of a
// plist (all unit values live in <string>/<integer> elements; comments and the
// doctype are separate token kinds).
func plistStringValues(t *testing.T, content []byte) []string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(content))
	var vals []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return vals
		}
		if err != nil {
			t.Fatalf("decode plist xml: %v", err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := strings.TrimSpace(string(cd)); s != "" {
				vals = append(vals, s)
			}
		}
	}
}

// verifyWithPlutil runs the platform loader's own validation on darwin hosts:
// plutil -lint proves the plist loads, and plutil -extract decodes the exact
// values launchd would use. Skipped where plutil is absent (non-darwin).
func verifyWithPlutil(t *testing.T, unit string, spec Spec) {
	t.Helper()
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not on PATH (non-darwin host)")
	}
	if out, err := exec.Command(plutil, "-lint", unit).CombinedOutput(); err != nil {
		t.Errorf("plutil -lint: %v\n%s", err, out)
	}
	for _, want := range []struct {
		keypath, expect string
	}{
		{"ProgramArguments.0", spec.DaemonPath},
		{"EnvironmentVariables.SIFT_HOME", spec.HomePath},
	} {
		out, err := exec.Command(plutil, "-extract", want.keypath, "raw", "-o", "-", unit).Output()
		if err != nil {
			t.Errorf("plutil -extract %s: %v", want.keypath, err)
			continue
		}
		if got := strings.TrimRight(string(out), "\n"); got != want.expect {
			t.Errorf("plutil -extract %s = %q, want %q", want.keypath, got, want.expect)
		}
	}
}

// assertSystemdPathSemantics checks the emitted quoting is decodable back to the
// original paths and that ExecStart keeps the daemon subcommand.
func assertSystemdPathSemantics(t *testing.T, content []byte, spec Spec) {
	t.Helper()
	s := string(content)
	if !strings.Contains(s, "ExecStart="+systemdQuote(spec.DaemonPath)+" daemon") {
		t.Errorf("ExecStart is not the quoted daemon path + daemon subcommand:\n%s", s)
	}
	if got := systemdValueOf(t, s, "ExecStart="); got != spec.DaemonPath {
		t.Errorf("ExecStart decodes to %q, want %q", got, spec.DaemonPath)
	}
	if !strings.Contains(s, "Environment=SIFT_HOME="+systemdQuote(spec.HomePath)) {
		t.Errorf("Environment does not carry the quoted SIFT_HOME:\n%s", s)
	}
	if got := systemdValueOf(t, s, "Environment=SIFT_HOME="); got != spec.HomePath {
		t.Errorf("SIFT_HOME decodes to %q, want %q", got, spec.HomePath)
	}
	if !strings.Contains(s, "Documentation=https://github.com/xsift/sift") {
		t.Error("systemd unit uses a non-canonical documentation URL")
	}
	if strings.Contains(s, "github.com/miaoxiaoyong/sift") || strings.Contains(s, "github.com/hexai-cn/sift") {
		t.Error("systemd unit retains a legacy repository URL")
	}
}

// systemdValueOf mirrors systemd's token rule for the quoting we emit: a
// double-quoted value with backslash escapes, split at the first whitespace
// (ExecStart also carries the daemon subcommand).
func systemdValueOf(t *testing.T, unit, directive string) string {
	t.Helper()
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, directive) {
			continue
		}
		v := strings.TrimPrefix(line, directive)
		// The values we emit are always one double-quoted token (with \" and
		// \\ escapes) and may legally contain spaces, so scan to the closing
		// quote rather than splitting at the first space.
		if len(v) >= 2 && v[0] == '"' {
			for i := 1; i < len(v); i++ {
				if v[i] == '\\' {
					i++
					continue
				}
				if v[i] == '"' {
					v = v[1:i]
					break
				}
			}
		}
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if v[i] == '\\' && i+1 < len(v) {
				i++
			}
			b.WriteByte(v[i])
		}
		return b.String()
	}
	t.Fatalf("directive %q not found in unit:\n%s", directive, unit)
	return ""
}

// verifyWithSystemdAnalyze runs the static unit parser on linux hosts: the
// --user variant first, the plain variant as the same parser when no user
// manager/bus is reachable. Skipped where systemd-analyze is absent.
func verifyWithSystemdAnalyze(t *testing.T, unit string) {
	t.Helper()
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not on PATH (non-linux host)")
	}
	var lastErr error
	for _, args := range [][]string{{"--user", "verify", unit}, {"verify", unit}} {
		if out, err := exec.Command(analyze, args...).CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("systemd-analyze %s: %w\n%s", strings.Join(args, " "), err, out)
		} else {
			return
		}
	}
	t.Error(lastErr)
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func TestWriteCreatesUnitFileAtomically(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "nested", "sift.service")
	plan := Plan{WriteFile: dest, Content: []byte("[Service]\nExecStart=/x daemon\n")}
	if err := Write(plan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plan.Content) {
		t.Fatalf("written content = %q, want %q", got, plan.Content)
	}
	// No staging temp files left behind.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sift-unit-") {
			t.Errorf("staging temp left behind: %s", e.Name())
		}
	}
}

func TestWriteRemovesWhenContentNil(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "sift.service")
	if err := os.WriteFile(dest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(Plan{WriteFile: dest, Content: nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("unit file still present after nil-content write: %v", err)
	}
}

func TestWriteRemovalIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "missing.service")
	if err := Write(Plan{WriteFile: dest, Content: nil}); err != nil {
		t.Fatalf("removing a never-present unit should succeed, got %v", err)
	}
}

func TestExecForegroundPlanReturnsErrNoBackend(t *testing.T) {
	out, err := Exec(Plan{Action: ActionStatus})
	if err != ErrNoBackend {
		t.Fatalf("Exec foreground err = %v, want ErrNoBackend", err)
	}
	if out != nil {
		t.Errorf("foreground Exec output = %q, want nil", out)
	}
}

func TestExecLaunchdInstallHandlesFreshAndCurrentUnits(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "launchctl.log")
	launchctl := filepath.Join(bin, "launchctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + log + "\"\n" +
		"if [ \"$1\" = bootout ] && [ \"${LAUNCHD_LOADED:-}\" != 1 ]; then echo 'Boot-out failed: 3: No such process' >&2; exit 3; fi\n" +
		"if [ \"$1\" = bootstrap ] && [ \"${LAUNCHD_BOOTSTRAP_FAIL:-}\" = 1 ]; then exit 1; fi\n"
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, tc := range []struct {
		name, loaded string
	}{
		{"fresh", ""},
		{"current-loaded", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LAUNCHD_LOADED", tc.loaded)
			if _, err := Exec(Plan{Action: ActionInstall, BeforeCmd: []string{"launchctl", "bootout", "gui/1/" + Label}, RunCmd: []string{"launchctl", "bootstrap", "gui/1", "/tmp/sift.plist"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bootout gui/1/" + Label + "\nbootstrap gui/1 /tmp/sift.plist\n"; strings.Count(string(got), want) != 2 {
		t.Fatalf("launchctl calls = %q, want replacement sequence twice", got)
	}
}

func TestExecLaunchdInstallDoesNotClaimBootstrapFailureSucceeded(t *testing.T) {
	bin := t.TempDir()
	launchctl := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\nif [ \"$1\" = bootout ]; then echo 'Boot-out failed: 3: No such process' >&2; exit 3; fi\necho bootstrap failed >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, err := Exec(Plan{Action: ActionInstall, BeforeCmd: []string{"launchctl", "bootout", "gui/1/" + Label}, RunCmd: []string{"launchctl", "bootstrap", "gui/1", "/tmp/sift.plist"}})
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("bootstrap failure = %v, want surfaced failure", err)
	}
}

// launchdScript describes the scripted behavior of a fake launchctl binary
// used by the issue #968 install-sequence tests. Behavior is driven by
// per-command invocation counters written under the fake's state dir, so a
// single test can script a full bootout→probe→bootstrap sequence
// deterministically.
//
//   - printAbsentAfter: the print probe reports the label absent from this
//     invocation on (1 = always absent, 2 = present once, then absent, ...).
//   - printPresentAt: the single print invocation that reports the label
//     present (overrides absent for that probe; 0 = never).
//   - bootoutAbsent: bootout reports the label already gone (fresh install).
//   - bootstrapFailAt: the bootstrap invocation that fails (0 = never).
//   - bootstrapAlwaysFail: every bootstrap invocation fails (for exhaustion).
//   - bootstrapFailMsg / bootstrapFailExit: the failure the bootstrap emits.
type launchdScript struct {
	printAbsentAfter    int
	printPresentAt      int
	bootoutAbsent       bool
	bootstrapFailAt     int
	bootstrapAlwaysFail bool
	bootstrapFailMsg    string
	bootstrapFailExit   int
}

// scriptedLaunchctl writes a fake launchctl into bin whose behavior follows
// script, points PATH at bin, and returns the state dir holding the per-command
// invocation counters (readable afterwards with scriptCount). The script exits
// 0 for every command it does not explicitly script, so bootout "succeeds" for
// a loaded label by default.
func scriptedLaunchctl(t *testing.T, bin string, script launchdScript) (stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	launchctl := filepath.Join(bin, "launchctl")
	absentAfter := 1
	if script.printAbsentAfter > 0 {
		absentAfter = script.printAbsentAfter
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("state=\"$FAKE_LAUNCHD_STATE\"\n")
	b.WriteString("cnt() { c=0; [ -f \"$state/$1.cnt\" ] && c=$(cat \"$state/$1.cnt\"); c=$((c+1)); printf '%s' \"$c\" > \"$state/$1.cnt\"; echo \"$c\"; }\n")
	b.WriteString("case \"$1\" in\n")
	b.WriteString("bootout)")
	if script.bootoutAbsent {
		b.WriteString(" echo 'Boot-out failed: 3: No such process' >&2; exit 3;")
	}
	b.WriteString(" exit 0;;\n")
	b.WriteString("print)\n")
	b.WriteString("  n=$(cnt print)\n")
	if script.printPresentAt > 0 {
		fmt.Fprintf(&b, "  if [ \"$n\" -eq %d ]; then echo active; else\n", script.printPresentAt)
	}
	fmt.Fprintf(&b, "    if [ \"$n\" -lt %d ]; then echo active; else echo 'Could not find service \"cn.hexai.sift\" in domain for user gui/1' >&2; exit 113; fi\n", absentAfter)
	if script.printPresentAt > 0 {
		b.WriteString("  fi\n")
	}
	b.WriteString("  ;;\n")
	b.WriteString("bootstrap)\n")
	b.WriteString("  n=$(cnt bootstrap)\n")
	if script.bootstrapAlwaysFail {
		fmt.Fprintf(&b, "  echo '%s' >&2; exit %d\n", script.bootstrapFailMsg, script.bootstrapFailExit)
	} else if script.bootstrapFailAt > 0 {
		fmt.Fprintf(&b, "  if [ \"$n\" -eq %d ]; then echo '%s' >&2; exit %d; fi\n", script.bootstrapFailAt, script.bootstrapFailMsg, script.bootstrapFailExit)
	}
	b.WriteString("  ;;\n")
	b.WriteString("esac\n")
	b.WriteString("exit 0\n")
	if err := os.WriteFile(launchctl, []byte(b.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LAUNCHD_STATE", stateDir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stateDir
}

// scriptCount reads how many times the fake launchctl ran cmd.
func scriptCount(t *testing.T, stateDir, cmd string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, cmd+".cnt"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// sleepRecorder replaces the package sleeper so install-sequence tests never
// block on real time; it records the sleeps the sequence actually performed.
type sleepRecorder struct {
	sleeps []time.Duration
}

func (r *sleepRecorder) sleep(d time.Duration) { r.sleeps = append(r.sleeps, d) }

// pinLaunchdPacing shrinks the issue #968 pacing windows and swaps in a
// no-op sleeper, making the sequence tests deterministic and millisecond-fast;
// production pacing is restored on cleanup. Tests that need no teardown wait
// at all can set the returned recorder's patience to zero by shrinking
// launchdTeardownTimeout further via the package vars.
func pinLaunchdPacing(t *testing.T) *sleepRecorder {
	t.Helper()
	oldTimeout, oldPoll := launchdTeardownTimeout, launchdTeardownPoll
	oldRetries, oldBackoff := launchdBootstrapRetries, launchdBootstrapBackoff
	oldSleep := sleep
	launchdTeardownTimeout = 60 * time.Millisecond
	launchdTeardownPoll = 5 * time.Millisecond
	launchdBootstrapRetries = 2
	launchdBootstrapBackoff = 5 * time.Millisecond
	r := &sleepRecorder{}
	sleep = r.sleep
	t.Cleanup(func() {
		launchdTeardownTimeout, launchdTeardownPoll = oldTimeout, oldPoll
		launchdBootstrapRetries, launchdBootstrapBackoff = oldRetries, oldBackoff
		sleep = oldSleep
	})
	return r
}

// launchdInstallPlan is the install plan the issue #968 tests drive; the
// domain (gui/1) and plist path are arbitrary fakes resolved by the scripted
// launchctl.
func launchdInstallPlan() Plan {
	return Plan{
		Action:       ActionInstall,
		BeforeCmd:    []string{"launchctl", "bootout", "gui/1/" + Label},
		AbsenceProbe: []string{"launchctl", "print", "gui/1/" + Label},
		RunCmd:       []string{"launchctl", "bootstrap", "gui/1", "/tmp/sift.plist"},
	}
}

// TestLaunchdInstallImmediateSuccess pins the happy path (issue #968): a
// loaded current label is booted out, launchctl confirms absence, and the
// freshly rendered plist bootstraps on the first try. The fresh-install
// variant (bootout exit 3 "No such process") must reach the same success.
func TestLaunchdInstallImmediateSuccess(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bootoutAbsent bool
	}{
		{"current-loaded", false},
		{"fresh", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pinLaunchdPacing(t)
			state := scriptedLaunchctl(t, t.TempDir(), launchdScript{bootoutAbsent: tc.bootoutAbsent})
			if _, err := Exec(launchdInstallPlan()); err != nil {
				t.Fatalf("install = %v, want success", err)
			}
			if got := scriptCount(t, state, "print"); got != 1 {
				t.Errorf("print probes = %d, want 1", got)
			}
			if got := scriptCount(t, state, "bootstrap"); got != 1 {
				t.Errorf("bootstrap calls = %d, want 1", got)
			}
			if len(r.sleeps) != 0 {
				t.Errorf("immediate success must not sleep, slept %v", r.sleeps)
			}
		})
	}
}

// TestLaunchdInstallDelayedDisappearance pins the teardown quiescence wait
// (issue #968): the label is still present on the first probe, then disappears
// before the bounded deadline; install must keep probing rather than racing
// the teardown with a blind bootstrap.
func TestLaunchdInstallDelayedDisappearance(t *testing.T) {
	r := pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{printAbsentAfter: 2})
	if _, err := Exec(launchdInstallPlan()); err != nil {
		t.Fatalf("install = %v, want success after waiting for teardown", err)
	}
	if got := scriptCount(t, state, "print"); got != 2 {
		t.Errorf("print probes = %d, want 2 (present then absent)", got)
	}
	if got := scriptCount(t, state, "bootstrap"); got != 1 {
		t.Errorf("bootstrap calls = %d, want 1", got)
	}
	if len(r.sleeps) == 0 {
		t.Error("delayed disappearance must poll between probes, no sleep recorded")
	}
}

// TestLaunchdInstallTransientBootstrapThenSuccess pins the bounded exit-5
// retry (issue #968): the first bootstrap hits the known transient
// Input/output error while the label stays confirmed absent; the retry then
// succeeds. The absence probe must be re-run before each retry.
func TestLaunchdInstallTransientBootstrapThenSuccess(t *testing.T) {
	r := pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{
		bootstrapFailAt:   1,
		bootstrapFailMsg:  "Input/output error",
		bootstrapFailExit: 5,
	})
	if _, err := Exec(launchdInstallPlan()); err != nil {
		t.Fatalf("install = %v, want success after one transient retry", err)
	}
	if got := scriptCount(t, state, "bootstrap"); got != 2 {
		t.Errorf("bootstrap calls = %d, want 2 (transient then success)", got)
	}
	if got := scriptCount(t, state, "print"); got != 2 {
		t.Errorf("print probes = %d, want 2 (pre-install + pre-retry absence check)", got)
	}
	if len(r.sleeps) == 0 {
		t.Error("transient retry must back off between attempts, no sleep recorded")
	}
}

// TestLaunchdInstallRetryExhaustion pins the bounded failure surface: a
// bootstrap that keeps failing with transient exit 5 while absence stays
// confirmed must exhaust the retry budget and return a non-zero actionable
// error — never claim success.
func TestLaunchdInstallRetryExhaustion(t *testing.T) {
	pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{
		bootstrapAlwaysFail: true,
		bootstrapFailMsg:    "Input/output error",
		bootstrapFailExit:   5,
	})
	_, err := Exec(launchdInstallPlan())
	if err == nil {
		t.Fatal("install succeeded after retry exhaustion")
	}
	if !strings.Contains(err.Error(), "retry `sift service install`") && !strings.Contains(err.Error(), "rerun `sift service install`") {
		t.Errorf("exhaustion error = %v, want actionable rerun hint", err)
	}
	if got := scriptCount(t, state, "bootstrap"); got != launchdBootstrapRetries+1 {
		t.Errorf("bootstrap calls = %d, want %d", got, launchdBootstrapRetries+1)
	}
}

// TestLaunchdInstallTeardownTimeoutNeverBootstraps pins the bounded teardown
// wait: when launchctl never confirms absence, install must abort before any
// bootstrap — bootstrapping over a half-torn job is exactly the issue #968
// race.
func TestLaunchdInstallTeardownTimeoutNeverBootstraps(t *testing.T) {
	pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{printAbsentAfter: 1 << 20})
	_, err := Exec(launchdInstallPlan())
	if err == nil {
		t.Fatal("install succeeded while the label never disappeared")
	}
	if !strings.Contains(err.Error(), "did not quiesce") {
		t.Errorf("teardown-timeout error = %v, want quiescence disclosure", err)
	}
	if got := scriptCount(t, state, "bootstrap"); got != 0 {
		t.Errorf("bootstrap calls = %d, want 0 (aborted before bootstrap)", got)
	}
}

// TestLaunchdInstallPermissionErrorDoesNotRetry pins the no-retry rule for
// permanent failures: a permission-denied bootstrap must surface immediately
// with exactly one bootstrap attempt.
func TestLaunchdInstallPermissionErrorDoesNotRetry(t *testing.T) {
	pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{
		bootstrapFailAt:   1,
		bootstrapFailMsg:  "Bootstrap failed: 5: Input/output error: Operation not permitted",
		bootstrapFailExit: 5,
	})
	_, err := Exec(launchdInstallPlan())
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("install = %v, want surfaced permission error", err)
	}
	if got := scriptCount(t, state, "bootstrap"); got != 1 {
		t.Errorf("bootstrap calls = %d, want exactly 1 (no retry)", got)
	}
}

// TestLaunchdInstallDomainErrorDoesNotRetry pins the no-retry rule for the
// wrong-domain / no-GUI case: it must fail immediately with the actionable
// SSH / foreground hint.
func TestLaunchdInstallDomainErrorDoesNotRetry(t *testing.T) {
	pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{
		bootstrapFailAt:   1,
		bootstrapFailMsg:  "Bootstrap failed: 5: Input/output error: Could not find domain for user",
		bootstrapFailExit: 5,
	})
	_, err := Exec(launchdInstallPlan())
	if err == nil || !strings.Contains(err.Error(), "SSH") || !strings.Contains(err.Error(), "foreground") {
		t.Fatalf("install = %v, want actionable SSH/foreground domain hint", err)
	}
	if got := scriptCount(t, state, "bootstrap"); got != 1 {
		t.Errorf("bootstrap calls = %d, want exactly 1 (no retry)", got)
	}
}

// TestLaunchdInstallAmbiguousStateDoesNotRetry pins the no-retry rule for a
// bootstrap that fails with the transient exit 5 but whose label is still
// present or otherwise not confirmed absent: the state is ambiguous, so
// install must fail now rather than fight launchd.
func TestLaunchdInstallAmbiguousStateDoesNotRetry(t *testing.T) {
	pinLaunchdPacing(t)
	state := scriptedLaunchctl(t, t.TempDir(), launchdScript{
		printPresentAt:    2, // absent for the wait probe, present for the retry gate
		bootstrapFailAt:   1,
		bootstrapFailMsg:  "Input/output error",
		bootstrapFailExit: 5,
	})
	_, err := Exec(launchdInstallPlan())
	if err == nil || !strings.Contains(err.Error(), "not confirmed absent") {
		t.Fatalf("install = %v, want ambiguous-state failure", err)
	}
	if got := scriptCount(t, state, "bootstrap"); got != 1 {
		t.Errorf("bootstrap calls = %d, want exactly 1 (no retry on ambiguous state)", got)
	}
}

func TestExecMissingToolReturnsErrNoBackend(t *testing.T) {
	// A command whose tool cannot exist on PATH returns ErrNoBackend so the
	// caller falls back to the foreground hint rather than hard-failing.
	_, err := Exec(Plan{Action: ActionStatus, RunCmd: []string{"definitely-not-a-real-binary-905"}})
	if err != ErrNoBackend {
		t.Fatalf("Exec missing-tool err = %v, want ErrNoBackend", err)
	}
}

func TestFormulaConsistentWithReleaseArchive(t *testing.T) {
	const rel = "0.1.0-dev"
	const sha = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	f := Formula(rel, sha)
	// Archive name matches the GoReleaser output the install path consumes.
	if !strings.Contains(f, "sift_"+rel+"_darwin_arm64.tar.gz") {
		t.Error("formula URL does not reference the release archive name")
	}
	if !strings.Contains(f, "version \""+rel+"\"") {
		t.Error("formula does not pin the release version")
	}
	if !strings.Contains(f, "sha256 \""+sha+"\"") {
		t.Error("formula does not embed the sha256")
	}
	// Installs both release binaries (DESIGN §8.4 pair).
	if !strings.Contains(f, `bin.install "sift"`) || !strings.Contains(f, `bin.install "sift-agent-wrapper"`) {
		t.Error("formula does not install both release binaries")
	}
	// Points at the hosting install, not its own service plumbing.
	if !strings.Contains(f, "sift service install") {
		t.Error("formula does not direct users to sift service install")
	}
	// States the no-port posture.
	if !strings.Contains(f, "no port") && !strings.Contains(f, "no network port") {
		t.Error("formula does not declare the no-port posture")
	}
	if !strings.Contains(f, "https://github.com/xsift/sift") || strings.Contains(f, "github.com/miaoxiaoyong/sift") || strings.Contains(f, "github.com/hexai-cn/sift") {
		t.Error("formula does not use the canonical repository URLs")
	}
}

func TestFormulaDefaultsReleaseVersion(t *testing.T) {
	f := Formula("", "")
	if !strings.Contains(f, version.Release) {
		t.Errorf("Formula with empty release does not fall back to %q", version.Release)
	}
}
