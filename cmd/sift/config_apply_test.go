package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/hosting"
)

func managedConfigApplyFixture(t *testing.T) (string, hosting.Spec, net.Listener) {
	t.Helper()
	home := freshHome(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("managed hosting is not available on this platform")
	}
	installReleaseLayout(t, home)
	spec, err := hosting.NewSpec(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(spec.UnitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.UnitPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(home, "siftd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return home, spec, listener
}

func installFakeHostingTool(t *testing.T, exitCode int) {
	t.Helper()
	bin := t.TempDir()
	tool := "systemctl"
	if runtime.GOOS == "darwin" {
		tool = "launchctl"
	}
	if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit "+string(rune('0'+exitCode))+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestAnnounceConfigAppliedManagedRestartSucceeds(t *testing.T) {
	home, _, _ := managedConfigApplyFixture(t)
	installFakeHostingTool(t, 0)
	var stdout, stderr bytes.Buffer
	announceConfigApplied(mustHome(t, home), &stdout, &stderr)
	if !strings.Contains(stdout.String(), "已自动重启 daemon 并生效") {
		t.Fatalf("stdout = %q, want successful restart", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestAnnounceConfigAppliedManagedRestartFailureWarns(t *testing.T) {
	home, _, _ := managedConfigApplyFixture(t)
	installFakeHostingTool(t, 1)
	var stdout, stderr bytes.Buffer
	announceConfigApplied(mustHome(t, home), &stdout, &stderr)
	if strings.Contains(stdout.String(), "已自动重启 daemon 并生效") {
		t.Fatalf("stdout falsely reported success: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "自动重启后台服务失败") {
		t.Fatalf("stderr = %q, want restart warning", stderr.String())
	}
}

func TestAnnounceConfigAppliedNoBackendGivesActionableHint(t *testing.T) {
	home, _, _ := managedConfigApplyFixture(t)
	// Keep PATH empty so hosting.Exec returns ErrNoBackend instead of running a
	// real supervisor. Restart must then be observable as a failure to callers.
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runService([]string{"restart"}, mustHome(t, home), &stdout, &stderr)
	if code == 0 {
		t.Fatal("restart without a hosting backend returned success")
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "已自动重启 daemon 并生效") {
		t.Fatalf("no-backend restart falsely reported success: %q", combined)
	}
	if !strings.Contains(combined, "sift daemon") || !strings.Contains(combined, "前台") {
		t.Fatalf("no-backend output lacks actionable foreground hint: %q", combined)
	}
}

func mustHome(t *testing.T, path string) (home config.Home) {
	t.Helper()
	resolved, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path != path {
		t.Fatalf("resolved home = %q, want %q", resolved.Path, path)
	}
	return resolved
}
