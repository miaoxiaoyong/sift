package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
)

func TestInitFlagsWriteMergeAndBackup(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@github.com:owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	agent := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"sift", "init", "--offline", "--agent", agent, "--project", repo, "--forge", "github", "--operator", "alice"}
	var out bytes.Buffer
	if code := runWithInput(args, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "已写入") || !strings.Contains(out.String(), "sift daemon") {
		t.Fatalf("output = %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 1 || snap.Config.Agents[0].Executable != agent || strings.Join(snap.Config.Agents[0].Args, ",") != "" {
		t.Fatalf("agents = %#v", snap.Config.Agents)
	}
	if len(snap.Config.Projects) != 1 || snap.Config.Projects[0].Repo != repo || snap.Config.Projects[0].Forge.Project != "owner/repo" {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
	if got := snap.Config.Operators.GitHub; len(got) != 1 || got[0] != "alice" {
		t.Fatalf("operators = %#v", got)
	}
	if info, err := os.Stat(config.ConfigPath(home)); err != nil || info.Mode().Perm() != config.ConfigFileMode {
		t.Fatalf("config mode = %v, %v", info, err)
	}

	out.Reset()
	if code := runWithInput(args, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("second init = %d: %s", code, out.String())
	}
	snap, err = config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 1 || len(snap.Config.Projects) != 1 {
		t.Fatalf("rerun was not idempotent: %#v", snap.Config)
	}
	if info, err := os.Stat(config.ConfigPath(home) + ".bak"); err != nil || info.Mode().Perm() != config.ConfigFileMode {
		t.Fatalf("backup mode = %v, %v", info, err)
	}
}

func TestForgeLoginFromStatus(t *testing.T) {
	for _, tt := range []struct {
		name, status, want string
	}{
		{"github", "github.com\n  ✓ Logged in to github.com account miaoxiaoyong (keyring)\n", "miaoxiaoyong"},
		{"gitlab", "Logged in to gitlab.hexinfo.cn as hex.miao\n", "hex.miao"},
		{"missing", "not logged in", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := forgeLoginFromStatus(tt.status); got != tt.want {
				t.Fatalf("forgeLoginFromStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestAgentArgsDefaultsAndOverride(t *testing.T) {
	doc := map[string]any{"version": 1}
	addAgent(doc, "claude", nil)
	addAgent(doc, "codex", nil)
	custom := []string{"--custom", "value"}
	addAgent(doc, "custom=custom-agent", &custom)

	agents := list(doc, "agents")
	if got := agents[0].(map[string]any)["args"]; !equalStrings(got, []string{"-p"}) {
		t.Fatalf("claude args = %#v", got)
	}
	if got := agents[1].(map[string]any)["args"]; !equalStrings(got, []string{"exec", "-"}) {
		t.Fatalf("codex args = %#v", got)
	}
	if got := agents[2].(map[string]any)["args"]; !equalStrings(got, custom) {
		t.Fatalf("custom args = %#v", got)
	}
}

func TestProjectAddDoesNotChangeOperators(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConfigPath(home), []byte("version: 1\noperators:\n  github: [alice]\n"), config.ConfigFileMode); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@github.com:owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "project", "add", "--offline", "--project", repo, "--forge", "github"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("project add = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Config.Operators.GitHub; len(got) != 1 || got[0] != "alice" {
		t.Fatalf("operators changed: %#v", got)
	}
	if len(snap.Config.Projects) != 1 {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
}

func equalStrings(value any, want []string) bool {
	got, ok := value.([]any)
	if !ok || len(got) != len(want) {
		return false
	}
	for i, arg := range got {
		if arg != want[i] {
			return false
		}
	}
	return true
}

func TestSetupAddAndDaemonAwareHint(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "agent", "add", "--offline", "--agent", agent}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("agent add = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Config.Agents[0].Args; strings.Join(got, ",") != "-p" {
		t.Fatalf("default args = %#v", got)
	}
	codex := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := runWithInput([]string{"sift", "agent", "add", "--offline", "--agent", codex, "--agent-args=--custom,value"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("agent add override = %d: %s", code, out.String())
	}
	snap, err = config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Config.Agents[1].Args; strings.Join(got, ",") != "--custom,value" {
		t.Fatalf("override args = %#v", got)
	}
	addr := net.UnixAddr{Name: filepath.Join(home.Path, "siftd.sock"), Net: "unix"}
	listener, err := net.ListenUnix("unix", &addr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	out.Reset()
	if code := runWithInput([]string{"sift", "agent", "add", "--offline", "--agent", agent}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("agent add = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "sift service reload") {
		t.Fatalf("daemon-aware output = %q", out.String())
	}
}
