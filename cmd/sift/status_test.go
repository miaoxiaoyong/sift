package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// TestStatusFreshHome pins the fresh-home surface (issue #935 acceptance):
// 未配置/未运行, a hint to `sift update --check`, and a structured --json
// with no daemon, no config and no projects.
func TestStatusFreshHome(t *testing.T) {
	freshHome(t)
	var out bytes.Buffer
	if code := run([]string{"sift", "status"}, &out, io.Discard); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	got := out.String()
	for _, want := range []string{"Sift 状态", "未配置", "未运行", "sift init", "sift update --check"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output lacks %q:\n%s", want, got)
		}
	}

	var jout bytes.Buffer
	if code := run([]string{"sift", "status", "--json"}, &jout, io.Discard); code != 0 {
		t.Fatalf("status --json exit = %d", code)
	}
	var st statusResult
	if err := json.Unmarshal(jout.Bytes(), &st); err != nil {
		t.Fatalf("status --json is not the closed result: %v; output=%q", err, jout.String())
	}
	if st.Daemon.Running || st.Daemon.Socket || st.Config.Present || st.Config.Valid || st.Projects.Total != 0 || st.Projects.Enabled != 0 {
		t.Fatalf("fresh status result = %+v, want everything absent/zero", st)
	}
}

// TestStatusUsageRejectsUnknownFlag keeps the status flag surface closed.
func TestStatusUsageRejectsUnknownFlag(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "status", "--wide"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "sift status") {
		t.Fatalf("stderr = %q, want usage line", stderr.String())
	}
}

// TestStatusConfiguredAndDaemonRunning writes a valid config with two projects
// (one disabled) and listens on siftd.sock, then asserts the human and JSON
// surfaces: 运行中 + PID + project counts.
func TestStatusConfiguredAndDaemonRunning(t *testing.T) {
	home := freshHome(t)
	doc := map[string]any{"version": 1}
	addProject(doc, "/repo/alpha", "github", "owner/alpha", "github.com")
	addProject(doc, "/repo/beta", "github", "owner/beta", "github.com")
	doc["projects"].([]any)[1].(map[string]any)["enabled"] = false
	if err := writeSetupDocument(config.Home{Path: home}, doc, false); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(home, "siftd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })

	var out bytes.Buffer
	if code := run([]string{"sift", "status"}, &out, io.Discard); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	got := out.String()
	for _, want := range []string{"运行中", "有效", "2 个", "1 启用", "Sift 状态"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output lacks %q:\n%s", want, got)
		}
	}

	var jout bytes.Buffer
	if code := run([]string{"sift", "status", "--json"}, &jout, io.Discard); code != 0 {
		t.Fatalf("status --json exit = %d", code)
	}
	var st statusResult
	if err := json.Unmarshal(jout.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Daemon.Running || st.Daemon.PID <= 0 {
		t.Fatalf("daemon = %+v, want running with a PID (peer credential)", st.Daemon)
	}
	if !st.Config.Valid || !st.Config.Present {
		t.Fatalf("config = %+v, want valid+present", st.Config)
	}
	if st.Projects.Total != 2 || st.Projects.Enabled != 1 {
		t.Fatalf("projects = %+v, want total 2 enabled 1", st.Projects)
	}
}

// TestStatusStaleSocketNotRunning covers a socket file left behind by a
// crashed daemon: the overview must report 未运行 (with the stale hint), not
// claim a running daemon.
func TestStatusStaleSocketNotRunning(t *testing.T) {
	home := freshHome(t)
	sock := filepath.Join(home, "siftd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener without unlinking: the socket inode stays on disk
	// but no one accepts, exactly the stale-socket crash residue.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"sift", "status"}, &out, io.Discard); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "未运行") || !strings.Contains(got, "残留") {
		t.Fatalf("stale-socket status = %q, want 未运行 with stale hint", got)
	}
}

// TestStatusRunningWithoutPeerPID pins F935-3: when the bounded connect proves
// liveness but the peer PID cannot be obtained (peerpid_other.go or a
// getsockopt failure yields PID 0), the human overview must still report
// 运行中 and must never show the misleading "PID 0".
func TestStatusRunningWithoutPeerPID(t *testing.T) {
	var out bytes.Buffer
	renderStatusHuman(&out, statusResult{
		Daemon:  statusDaemon{Running: true, Socket: true, PID: 0},
		Config:  statusConfig{Present: true, Valid: true, Path: "/tmp/sift/config.yaml"},
		Version: "0.0.0-test",
	})
	got := out.String()
	if !strings.Contains(got, "运行中") {
		t.Fatalf("status output lacks 运行中:\n%s", got)
	}
	if strings.Contains(got, "PID 0") {
		t.Fatalf("status output shows misleading PID 0 when peer PID is unavailable:\n%s", got)
	}
}

// TestStatusInvalidConfig reports 无效 and keeps the error for --json.
func TestStatusInvalidConfig(t *testing.T) {
	home := freshHome(t)
	cfgPath := config.ConfigPath(config.Home{Path: home})
	if err := os.WriteFile(cfgPath, []byte("version: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"sift", "status"}, &out, io.Discard); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(out.String(), "无效") {
		t.Fatalf("invalid-config status = %q, want 无效", out.String())
	}
	var jout bytes.Buffer
	if code := run([]string{"sift", "status", "--json"}, &jout, io.Discard); code != 0 {
		t.Fatalf("status --json exit = %d", code)
	}
	var st statusResult
	if err := json.Unmarshal(jout.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Config.Present || st.Config.Valid || st.Config.Error == "" {
		t.Fatalf("config = %+v, want present+invalid with error", st.Config)
	}
}
