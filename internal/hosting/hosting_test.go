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
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/version"
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
		name    string
		goos    string
		wantCmd []string
	}{
		{"launchd", "darwin", []string{"launchctl", "load"}},
		{"systemd", "linux", []string{"systemctl", "--user", "enable", "--now", ServiceName + ".service"}},
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
		{"launchd start", "darwin", ActionStart, []string{"launchctl", "load"}},
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
}

func TestFormulaDefaultsReleaseVersion(t *testing.T) {
	f := Formula("", "")
	if !strings.Contains(f, version.Release) {
		t.Errorf("Formula with empty release does not fall back to %q", version.Release)
	}
}
