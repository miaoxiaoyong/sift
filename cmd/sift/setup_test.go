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

func TestDetectForgeKind(t *testing.T) {
	for _, tt := range []struct {
		host, want string
	}{
		{"github.com", "github"},
		{"gitlab.com", "gitlab"},
		{"gitlab.hexinfo.cn", "gitlab"},
		{"gitlab.example.com", "gitlab"},
		{"github.enterprise.com", "github"},
		{"bitbucket.org", ""},
		{"", ""},
	} {
		if got := detectForgeKind(tt.host); got != tt.want {
			t.Fatalf("detectForgeKind(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestRemoteHostProject(t *testing.T) {
	for _, tt := range []struct {
		url, host, project string
	}{
		{"git@github.com:owner/repo.git", "github.com", "owner/repo"},
		{"https://github.com/owner/repo", "github.com", "owner/repo"},
		{"ssh://git@gitlab.hexinfo.cn/group/proj.git", "gitlab.hexinfo.cn", "group/proj"},
		{"git://github.com/a/b.git", "github.com", "a/b"},
		{"https://github.com/owner/repo.git/", "github.com", "owner/repo"},
		{"not a url", "", ""},
		{"", "", ""},
	} {
		host, project := remoteHostProject(tt.url)
		if host != tt.host || project != tt.project {
			t.Fatalf("remoteHostProject(%q) = (%q,%q), want (%q,%q)", tt.url, host, project, tt.host, tt.project)
		}
	}
}

func TestSelectAgents(t *testing.T) {
	found := []string{"claude", "codex", "pi"}
	for _, tt := range []struct {
		picked, want string
	}{
		{"", "claude,codex,pi"},    // Enter = all selected
		{"all", "claude,codex,pi"}, // explicit all
		{"ALL", "claude,codex,pi"},
		{"1,3", "claude,pi"},
		{"3,1", "pi,claude"},
		{"2", "codex"},
		{" 1, 3 ", "claude,pi"},
		{"0", ""},
		{"none", ""},
		{"1,9", "claude"},          // out-of-range entries are dropped
		{"claude,pi", "claude,pi"}, // legacy names pass through
	} {
		if got := selectAgents(tt.picked, found); got != tt.want {
			t.Fatalf("selectAgents(%q) = %q, want %q", tt.picked, got, tt.want)
		}
	}
}

func TestParseOperatorSpec(t *testing.T) {
	github, gitlab := "github:alice", "gitlab:bob"
	specs, err := parseOperatorSpec(github+","+gitlab, "github")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(specs["github"], []string{"alice"}) || !equalStringSlice(specs["gitlab"], []string{"bob"}) {
		t.Fatalf("parseOperatorSpec = %#v", specs)
	}
	// Plain names attach to the project kind default.
	specs, err = parseOperatorSpec("carol, github:dan", "gitlab")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(specs["gitlab"], []string{"carol"}) || !equalStringSlice(specs["github"], []string{"dan"}) {
		t.Fatalf("parseOperatorSpec plain = %#v", specs)
	}
	if _, err := parseOperatorSpec("bogus:user", "github"); err == nil {
		t.Fatal("unrecognized kind prefix was accepted")
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProjectAddSelfHostedGitlabHostPersisted(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "ssh://git@gitlab.hexinfo.cn/group/proj.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "project", "add", "--offline", "--project", repo}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("project add = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
	p := snap.Config.Projects[0]
	if p.Forge.Kind != config.ForgeKindGitLab || p.Forge.Project != "group/proj" || p.Forge.Host != "gitlab.hexinfo.cn" {
		t.Fatalf("forge ref = %#v, want gitlab/group/proj@gitlab.hexinfo.cn", p.Forge)
	}
}

func TestInteractiveProjectAddCwdAutoDetect(t *testing.T) {
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
	t.Chdir(repo)
	repo, err = filepath.EvalSymlinks(repo) // git canonicalizes /var→/private/var on macOS
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// No --project, no --forge: cwd project, forge auto-detected from origin.
	if code := runWithInput([]string{"sift", "project", "add"}, strings.NewReader("\n"), &out, io.Discard); code != 0 {
		t.Fatalf("project add = %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "Forge 类型") {
		t.Fatalf("project add asked a forge question: %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 || snap.Config.Projects[0].Repo != repo {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
	if p := snap.Config.Projects[0].Forge; p.Kind != config.ForgeKindGitHub || p.Project != "owner/repo" {
		t.Fatalf("forge ref = %#v, want github/owner/repo", p)
	}
}

func TestInteractiveInitNumberedAgentSelection(t *testing.T) {
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
	t.Chdir(repo)
	// Deterministic agent discovery: only fake claude/codex/pi plus git are on
	// PATH, so gh/glab probes find no login and selection is stable.
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex", "pi"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(gitPath))
	var out bytes.Buffer
	// Answers: agents=1,3 ; project=Enter (cwd) ; operator=Enter (skip).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("1,3\n\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "Forge 类型") {
		t.Fatalf("init asked a forge question: %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 2 {
		t.Fatalf("agents = %#v, want the 1,3 subset", snap.Config.Agents)
	}
	if got := snap.Config.Agents[0]; got.ID != "claude" || got.Executable != "claude" {
		t.Fatalf("agent[0] = %#v", got)
	}
	if got := snap.Config.Agents[1]; got.ID != "pi" || got.Executable != "pi" {
		t.Fatalf("agent[1] = %#v", got)
	}
	if len(snap.Config.Projects) != 1 || snap.Config.Projects[0].Forge.Kind != config.ForgeKindGitHub {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
}

func TestProjectAddNonRepoErrors(t *testing.T) {
	_ = freshHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if repo := detectedRepo(); repo != "" {
		t.Skipf("test temp dir is inside a git worktree: %s", repo)
	}
	var out, errb bytes.Buffer
	if code := runWithInput([]string{"sift", "project", "add", "--offline"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("project add offline outside a repo = %d, want 1: stdout=%q", code, out.String())
	}
	if !strings.Contains(errb.String(), "cd 到项目目录") {
		t.Fatalf("error is not actionable: %q", errb.String())
	}
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
