package hosting

import (
	"os"
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
	return home, daemonPath
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
	wantArgs := "<string>" + spec.DaemonPath + "</string>"
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
	// ExecStart follows current/sift daemon.
	if !strings.Contains(s, "ExecStart="+spec.DaemonPath+" daemon") {
		t.Errorf("systemd ExecStart does not point at %s daemon", spec.DaemonPath)
	}
	if !strings.Contains(s, "WantedBy=default.target") {
		t.Error("systemd template is not a user unit (WantedBy=default.target)")
	}
	if !strings.Contains(s, "SIFT_HOME="+home) {
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
		{"systemd", "linux", []string{"systemctl", "--user", "daemon-reload"}},
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
