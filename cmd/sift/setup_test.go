package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xsift/sift/internal/config"
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

func TestWriteSetupDocumentRejectsInvalidEditWithoutReplacingConfig(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	valid := []byte("version: 1\noperators:\n  github: [alice]\n")
	if err := os.WriteFile(config.ConfigPath(home), valid, config.ConfigFileMode); err != nil {
		t.Fatal(err)
	}
	invalid := map[string]any{
		"version": 1,
		"projects": []any{map[string]any{
			"id": "demo", "repo": "/tmp/demo", "forge": map[string]any{"kind": "github", "project": "owner/repo"}, "agents": []any{"missing"},
		}},
	}
	if err := writeSetupDocument(home, invalid, true); err == nil {
		t.Fatal("invalid edit was written")
	}
	got, err := os.ReadFile(config.ConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, valid) {
		t.Fatalf("config changed after invalid edit: %q", got)
	}
}

// TestNormalizeNumbersKeepsIntegralDecimals guards the issue #927 config byte
// drift: setupDocument decodes existing config via JSON (all numbers become
// float64), and yaml.v3 would serialize large integral floats as scientific
// notation (1000000 → 1e+06) on every init rerun. normalizeNumbers converts
// integral in-range floats to int; fractions and out-of-range values stay
// float64 so no numeric meaning changes. The assertion is the absence of
// scientific notation plus the exact decimal form (map quoting style is a
// yaml.v3 output detail and intentionally not pinned).
func TestNormalizeNumbersKeepsIntegralDecimals(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"large-int", map[string]any{"n": float64(1000000)}, "1000000"},
		{"int64-range", map[string]any{"n": float64(2147483648)}, "2147483648"},
		{"small-int", map[string]any{"n": float64(60)}, "60"},
		{"fraction", map[string]any{"n": 1.5}, "1.5"},
		{"nested", map[string]any{"sub": map[string]any{"big": float64(3000000000)}}, "3000000000"},
		{"list", map[string]any{"l": []any{float64(1000000), float64(2.5)}}, "1000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := yaml.Marshal(normalizeNumbers(c.in))
			if err != nil {
				t.Fatal(err)
			}
			s := string(data)
			if strings.Contains(s, "e+") || strings.Contains(s, "E+") {
				t.Fatalf("yaml still uses scientific notation: %q", s)
			}
			if !strings.Contains(s, c.want) {
				t.Fatalf("yaml = %q, want it to contain %q", s, c.want)
			}
		})
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

// TestAgentArgsTrimmedAndNonInteractive asserts issue #976: --agent-args items
// are trimmed per element (leading/trailing spaces dropped, empties skipped)
// and passing --agent-args without --agent keeps init non-interactive (no
// agent-selection prompt, no dependency guidance).
func TestAgentArgsTrimmedAndNonInteractive(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	replaceSetupCmd(t, &fakeCommand{}) // nothing on PATH: deterministic

	fake := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// --agent-args with spaces around commas and values: every element must be
	// trimmed and empty elements dropped before registration.
	code := runWithInput([]string{
		"sift", "init", "--offline",
		"--agent", fake,
		"--agent-args", " --foo , , bar , ",
	}, strings.NewReader(""), &out, io.Discard)
	if code != 0 {
		t.Fatalf("init --agent-args = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 1 {
		t.Fatalf("agents = %#v", snap.Config.Agents)
	}
	if got := snap.Config.Agents[0].Args; len(got) != 2 || got[0] != "--foo" || got[1] != "bar" {
		t.Fatalf("trimmed args = %#v", got)
	}

	// --agent-args alone (no --agent): still non-interactive. The probe would
	// otherwise ask to select an agent; with nothing on PATH it must not block.
	out.Reset()
	code = runWithInput([]string{
		"sift", "init", "--offline",
		"--agent-args", "--x",
	}, strings.NewReader(""), &out, io.Discard)
	if code != 0 {
		t.Fatalf("init --agent-args alone = %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "选择 Agent") || strings.Contains(out.String(), "推荐安装 pi") {
		t.Fatalf("--agent-args alone still entered interactive guidance: %q", out.String())
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

// TestDetectAgentsVersionsAndCharacteristics pins issue #930: auto-detect
// probes versions via --version and every detected row carries the built-in
// characteristic profile (Chinese tags).
func TestDetectAgentsVersionsAndCharacteristics(t *testing.T) {
	bin := t.TempDir()
	for name, body := range map[string]string{
		"claude": "#!/bin/sh\nprintf 'Claude Code version 2.1.0\\n'\n",
		"codex":  "#!/bin/sh\nprintf 'codex-cli 0.145.0\\n'\n",
		"pi":     "#!/bin/sh\nprintf '0.84.1\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(gitPath))

	found := detectAgents()
	if len(found) != 3 {
		t.Fatalf("detectAgents = %#v, want claude/codex/pi", found)
	}
	if got := found[0]; got.name != "claude" || got.version != "2.1.0" {
		t.Fatalf("found[0] = %#v, want claude 2.1.0", got)
	}
	if got := found[1]; got.name != "codex" || got.version != "0.145.0" {
		t.Fatalf("found[1] = %#v, want codex 0.145.0", got)
	}
	row := formatDetectedAgent(found[0])
	for _, want := range []string{"claude (2.1.0)", "编码·推理·长上下文", "200K", "中", "Anthropic"} {
		if !strings.Contains(row, want) {
			t.Fatalf("formatDetectedAgent(claude) = %q, missing %q", row, want)
		}
	}
}

// TestInteractiveInitCharacteristicsDisplay is the wizard integration test for
// issue #930: the numbered rows show executable (version) plus the built-in
// characteristic labels in Chinese, and the default all-selection still writes
// every detected agent to config.
func TestInteractiveInitCharacteristicsDisplay(t *testing.T) {
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
	bin := t.TempDir()
	for name, body := range map[string]string{
		"claude": "#!/bin/sh\nprintf 'Claude Code version 2.0.0\\n'\n",
		"codex":  "#!/bin/sh\nprintf 'codex-cli 0.5.0\\n'\n",
		"pi":     "#!/bin/sh\nprintf '0.9.9\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(gitPath))

	var out bytes.Buffer
	// Answers: gh install=n ; glab install=n ; agents=all ; operator=Enter (skip).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\nall\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	for _, want := range []string{
		"1. claude (2.0.0) — 编码·推理·长上下文 · 200K · 中 · 中 · Anthropic",
		"2. codex (0.5.0) — 编码·审查 · 200K · 中 · 快 · OpenAI",
		"3. pi (0.9.9) — 编码·规划·审查 · 200K · 高 · 中 · pi 编码代理",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("wizard output missing %q:\n%s", want, out.String())
		}
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 3 {
		t.Fatalf("agents = %#v, want all 3 detected agents", snap.Config.Agents)
	}
}

// TestNonInteractiveAgentAddReportsVersion pins issue #930: the non-interactive
// path writes the probed version into the output (without putting it in config).
func TestNonInteractiveAgentAddReportsVersion(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	agent := filepath.Join(bin, "claude")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nprintf 'Claude Code version 3.1.4\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(gitPath))

	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "agent", "add", "--offline", "--agent", "claude"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("agent add = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "Agent claude（claude 3.1.4）") {
		t.Fatalf("output does not report the probed version: %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 1 {
		t.Fatalf("agents = %#v", snap.Config.Agents)
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

func TestProjectAddForgeOverridePersistsDetectedHost(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// Host maps to no known forge: --forge overrides the kind, but the
	// probed host must still be persisted instead of being dropped.
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@git.corp.example:group/proj.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "project", "add", "--offline", "--project", repo, "--forge", "gitlab"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("project add = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
	p := snap.Config.Projects[0].Forge
	if p.Kind != config.ForgeKindGitLab || p.Project != "group/proj" || p.Host != "git.corp.example" {
		t.Fatalf("forge ref = %#v, want gitlab/group/proj@git.corp.example", p)
	}
}

func TestInteractiveProjectAddAskOncePersistsHost(t *testing.T) {
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
	// Undetectable host: the one-time prompt answers gitlab, and the host
	// must survive into forge.host (issue #929 review F1).
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@git.corp.example:group/proj.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	t.Chdir(repo)
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "project", "add"}, strings.NewReader("gitlab\n"), &out, io.Discard); code != 0 {
		t.Fatalf("project add = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
	p := snap.Config.Projects[0].Forge
	if p.Kind != config.ForgeKindGitLab || p.Project != "group/proj" || p.Host != "git.corp.example" {
		t.Fatalf("forge ref = %#v, want gitlab/group/proj@git.corp.example", p)
	}
}

func TestInitFlagsOperatorLoginFallback(t *testing.T) {
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
	// Both detected identities are used directly, even though the project is
	// GitHub-bound. Init must not ask the user to confirm either login.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf 'github.com\\n  ✓ Logged in to github.com account probe-user (keyring)\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte("#!/bin/sh\nprintf 'Logged in to gitlab.com as gitlab-user\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(gitPath))
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "init", "--agent", agent, "--project", repo}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Config.Operators.GitHub; len(got) != 1 || got[0] != "probe-user" {
		t.Fatalf("operators.github = %#v, want [probe-user]", got)
	}
	if got := snap.Config.Operators.GitLab; len(got) != 1 || got[0] != "gitlab-user" {
		t.Fatalf("operators.gitlab = %#v, want [gitlab-user]", got)
	}
	if strings.Contains(out.String(), "操作员用户名") || !strings.Contains(out.String(), "✓ operator: probe-user") || !strings.Contains(out.String(), "✓ operator: gitlab-user") {
		t.Fatalf("detected operators were not used directly: %q", out.String())
	}
	if len(snap.Config.Projects) != 1 || snap.Config.Projects[0].Forge.Host != "github.com" {
		t.Fatalf("projects = %#v (default github.com host stays omitted)", snap.Config.Projects)
	}
}

func TestInteractiveInitProbeLoginUsesOperatorWithoutPrompt(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@github.com:owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	t.Chdir(repo)
	agentBin := t.TempDir()
	for _, name := range []string{"claude", "gh"} {
		body := "#!/bin/sh\n"
		if name == "claude" {
			body += "printf 'Claude Code version 2.0.0\\n'\n"
		} else {
			body += "printf 'github.com\\n  ✓ Logged in to github.com account probe-user (keyring)\\n'\n"
		}
		if err := os.WriteFile(filepath.Join(agentBin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", agentBin+string(os.PathListSeparator)+filepath.Dir(gitPath))

	var out bytes.Buffer
	// The only answers are the glab install decline (gh is already logged in)
	// and agent selection. A successful gh probe must consume no operator
	// answer and must be written directly to the allowlist.
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nall\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "操作员用户名") || !strings.Contains(out.String(), "✓ operator: probe-user") {
		t.Fatalf("probe login was not used without prompting: %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Config.Operators.GitHub; len(got) != 1 || got[0] != "probe-user" {
		t.Fatalf("operators.github = %#v, want [probe-user]", got)
	}
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
	// Answers: gh install=n ; glab install=n ; agents=1,3 ; project=Enter (cwd) ;
	// operator=Enter (skip).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\n1,3\n\n"), &out, io.Discard); code != 0 {
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
	if strings.Contains(out.String(), "sift service reload") || !strings.Contains(out.String(), "前台运行") {
		t.Fatalf("daemon-aware output = %q", out.String())
	}
}

// ---- issue #960: init dependency guidance ----------------------------------

// fakeCommand is a test double for setupCmd: CI never really installs packages
// or runs official auth flows (issue #960 acceptance 5). lookup defaults to
// false, output to an error, run records invocations and returns nil.
type fakeCommand struct {
	found    map[string]bool
	outputFn func(name string, args ...string) (string, error)
	runFn    func(name string, args ...string) error
	runs     [][]string
}

func (f *fakeCommand) lookup(name string) bool { return f.found[name] }
func (f *fakeCommand) output(name string, args ...string) (string, error) {
	if f.outputFn != nil {
		return f.outputFn(name, args...)
	}
	return "", errors.New("fake command: not found")
}
func (f *fakeCommand) run(name string, args ...string) error {
	f.runs = append(f.runs, append([]string{name}, args...))
	if f.runFn != nil {
		return f.runFn(name, args...)
	}
	return nil
}

func replaceSetupCmd(t *testing.T, f *fakeCommand) {
	t.Helper()
	prev := setupCmd
	setupCmd = f
	t.Cleanup(func() { setupCmd = prev })
}

func replaceSetupDoctor(t *testing.T, f func(config.Home) map[string]any) {
	t.Helper()
	prev := setupDoctorRun
	setupDoctorRun = f
	t.Cleanup(func() { setupDoctorRun = prev })
}

func replaceSetupService(t *testing.T, f func(action string, home config.Home, stdout, stderr io.Writer) int) {
	t.Helper()
	prev := setupServiceRun
	setupServiceRun = f
	t.Cleanup(func() { setupServiceRun = prev })
}

// initTestRepo creates a git repo with a GitHub origin and chdirs into it.
func initTestRepo(t *testing.T) config.Home {
	t.Helper()
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
	return home
}

// gitOnlyPATH narrows PATH to the directory holding git so no forge CLI, npm or
// coding agent is visible (deterministic probes regardless of the host).
func gitOnlyPATH(t *testing.T) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(gitPath))
}

// TestInitForgeMissingGuidesInstall pins acceptance 1 (missing → install
// prompt) and acceptance 2 (declining install still completes init with exit
// 0 and the config written).
func TestInitForgeMissingGuidesInstall(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	// Explicitly inject the "missing" command+auth state: host PATH/HOME and a
	// real gh on the runner (ubuntu puts gh in /usr/bin next to git) must not
	// flip this into an installed-not-logged probe (issue #960 hermeticity).
	replaceSetupCmd(t, &fakeCommand{}) // nothing on PATH

	var out bytes.Buffer
	// Answers: gh install=n ; glab install=n ; pi=n ; agent fallback=Enter ;
	// operator=Enter (EOF).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\nn\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	for _, want := range []string{
		"未检测到 GitHub CLI（gh）", "是否现在安装 gh",
		"未检测到 GitLab CLI（glab）", "是否现在安装 glab",
		"手动安装 pi",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("guidance output missing %q:\n%s", want, out.String())
		}
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 || len(snap.Config.Agents) != 0 {
		t.Fatalf("decline must still complete project binding without agents: %#v", snap.Config)
	}
}

// TestInitForgeNotLoggedGuidesLogin pins acceptance 1 (installed-not-logged →
// login question): declining falls back to the manual operator question, and a
// successful official auth login records the operator without asking.
func TestInitForgeNotLoggedGuidesLogin(t *testing.T) {
	t.Run("login success records operator silently", func(t *testing.T) {
		home := initTestRepo(t)
		gitOnlyPATH(t)
		calls := 0
		fake := &fakeCommand{found: map[string]bool{"gh": true, "glab": true}}
		fake.outputFn = func(name string, args ...string) (string, error) {
			if name == "gh" {
				calls++
				if calls == 1 {
					return "", errors.New("gh not logged in")
				}
				return "github.com\n  ✓ Logged in to github.com account gh-user (keyring)\n", nil
			}
			return "", errors.New("fake command: not found")
		}
		replaceSetupCmd(t, fake)

		var out bytes.Buffer
		// Answers: gh login=y ; glab login=n ; pi=n ; agent fallback=Enter ;
		// operator=Enter (EOF).
		if code := runWithInput([]string{"sift", "init"}, strings.NewReader("y\nn\nn\n\n"), &out, io.Discard); code != 0 {
			t.Fatalf("init = %d: %s", code, out.String())
		}
		for _, want := range []string{"检测到 gh 未登录", "是否现在运行官方 gh auth login", "✓ 已检测到 GitHub 登录：gh-user", "✓ operator: gh-user"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("output missing %q:\n%s", want, out.String())
			}
		}
		if got := fake.runs; len(got) != 1 || strings.Join(got[0], " ") != "gh auth login" {
			t.Fatalf("official auth login was not passed through: %#v", got)
		}
		snap, err := config.Load(home, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Config.Operators.GitHub; len(got) != 1 || got[0] != "gh-user" {
			t.Fatalf("operators.github = %#v, want [gh-user]", got)
		}
		if strings.Contains(out.String(), "操作员用户名") {
			t.Fatalf("logged operator must not be asked to confirm: %q", out.String())
		}
	})

	t.Run("decline falls back to manual operator question", func(t *testing.T) {
		home := initTestRepo(t)
		gitOnlyPATH(t)
		fake := &fakeCommand{found: map[string]bool{"gh": true}}
		fake.outputFn = func(name string, args ...string) (string, error) {
			if name == "gh" {
				return "", errors.New("gh not logged in")
			}
			return "", errors.New("fake command: not found")
		}
		replaceSetupCmd(t, fake)

		var out bytes.Buffer
		// Answers: gh login=n ; glab install=n ; pi=n ; agent fallback=Enter ;
		// operator=alice.
		if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\nn\n\nalice\n"), &out, io.Discard); code != 0 {
			t.Fatalf("init = %d: %s", code, out.String())
		}
		if !strings.Contains(out.String(), "是否现在运行官方 gh auth login") {
			t.Fatalf("decline should keep the login question: %q", out.String())
		}
		snap, err := config.Load(home, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Config.Operators.GitHub; len(got) != 1 || got[0] != "alice" {
			t.Fatalf("operators.github = %#v, want [alice]", got)
		}
	})
}

// TestInitForgeInstallFailureDegrades pins acceptance 2: a failed install
// command degrades to the official manual path and init still completes with
// exit code 0 and the config written.
func TestInitForgeInstallFailureDegrades(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	fake := &fakeCommand{found: map[string]bool{"brew": true, "npm": true}}
	fake.runFn = func(name string, args ...string) error {
		if name == "brew" {
			return errors.New("permission denied")
		}
		return nil
	}
	replaceSetupCmd(t, fake)

	var out bytes.Buffer
	// Answers: gh install=y ; glab install=n ; pi=n ; agent fallback=Enter ;
	// operator=Enter (EOF).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("y\nn\nn\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	for _, want := range []string{"自动安装 gh 失败", "cli.github.com"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("degradation output missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(config.ConfigPath(home)); err != nil {
		t.Fatalf("config was not written after failed install: %v", err)
	}
}

// TestInitPiBootstrapInstallsAndRegisters pins acceptance 3: an empty agent
// scan offers pi first; confirming installs via npm, verifies pi and writes
// config agents[pi] (default -p args, reusing addAgent).
func TestInitPiBootstrapInstallsAndRegisters(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	fake := &fakeCommand{found: map[string]bool{"npm": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		if name == "pi" {
			return "pi 0.9.9\n", nil
		}
		return "", errors.New("fake command: not found")
	}
	replaceSetupCmd(t, fake)

	var out bytes.Buffer
	// Answers: gh install=n ; glab install=n ; pi=Enter (yes) ; operator=Enter (EOF).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "推荐安装 pi（开源，多模型，支持订阅/API Key）") {
		t.Fatalf("pi guidance missing: %q", out.String())
	}
	wantRun := []string{"npm", "install", "-g", "--ignore-scripts", "@earendil-works/pi-coding-agent"}
	if len(fake.runs) != 1 || strings.Join(fake.runs[0], " ") != strings.Join(wantRun, " ") {
		t.Fatalf("npm install = %#v, want %v", fake.runs, wantRun)
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 1 || snap.Config.Agents[0].ID != "pi" || snap.Config.Agents[0].Executable != "pi" {
		t.Fatalf("agents = %#v, want pi registered", snap.Config.Agents)
	}
	if got := snap.Config.Agents[0].Args; strings.Join(got, ",") != "-p" {
		t.Fatalf("pi default args = %#v, want [-p]", got)
	}
}

// TestInitPiBootstrapDeclinedPrintsGuidance pins acceptance 3: declining the pi
// offer prints the manual path and does not block the wizard.
func TestInitPiBootstrapDeclinedPrintsGuidance(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	// Inject the explicit missing state so a host-installed gh/glab cannot
	// turn the missing-install prompt into a login prompt (issue #960).
	replaceSetupCmd(t, &fakeCommand{}) // nothing on PATH

	var out bytes.Buffer
	// Answers: gh install=n ; glab install=n ; pi=n ; agent fallback=Enter ;
	// operator=Enter (EOF).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\nn\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "curl -fsSL https://pi.dev/install.sh | sh") {
		t.Fatalf("declined pi install must print the manual path: %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 0 {
		t.Fatalf("agents = %#v, want none", snap.Config.Agents)
	}
}

// TestInitPiBootstrapNoNpmDegrades pins acceptance 3: without npm the pi offer
// degrades to the official script guidance and does not block.
func TestInitPiBootstrapNoNpmDegrades(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	fake := &fakeCommand{} // npm not on PATH
	replaceSetupCmd(t, fake)

	var out bytes.Buffer
	// Answers: gh install=n ; glab install=n ; pi=y ; agent fallback=Enter ;
	// operator=Enter (EOF).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\ny\n\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "未检测到 npm") || !strings.Contains(out.String(), "https://pi.dev/install.sh") {
		t.Fatalf("no-npm degradation missing: %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 0 {
		t.Fatalf("agents = %#v, want none", snap.Config.Agents)
	}
}

// TestInitNonInteractivePathsSkipGuidance pins acceptance 4: --offline probes
// nothing, and the flags-all-given path keeps only the graded report — no
// install/login/pi prompts either way.
func TestInitNonInteractivePathsSkipGuidance(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		home := initTestRepo(t)
		gitOnlyPATH(t)
		var out bytes.Buffer
		if code := runWithInput([]string{"sift", "init", "--offline"}, strings.NewReader(""), &out, io.Discard); code != 0 {
			t.Fatalf("init offline = %d: %s", code, out.String())
		}
		for _, forbidden := range []string{"是否现在安装", "是否现在运行官方", "推荐安装 pi", "未检测到 GitHub CLI", "已检测到 GitHub 登录", "收尾三合一", "全部就绪"} {
			if strings.Contains(out.String(), forbidden) {
				t.Fatalf("offline init must skip all guidance/report, got %q in %q", forbidden, out.String())
			}
		}
		if _, err := os.Stat(config.ConfigPath(home)); err != nil {
			t.Fatalf("config was not written: %v", err)
		}
	})

	t.Run("flags given", func(t *testing.T) {
		home := initTestRepo(t)
		gitOnlyPATH(t)
		// The graded report must show the missing CLI regardless of any real gh
		// on the runner: inject the explicit missing state (issue #960).
		replaceSetupCmd(t, &fakeCommand{}) // nothing on PATH
		agent := filepath.Join(t.TempDir(), "fake-agent")
		if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		// --agent/--project given: non-interactive. The graded report still
		// shows the missing CLI, but no guidance prompt may appear.
		if code := runWithInput([]string{"sift", "init", "--agent", agent, "--project", "."}, strings.NewReader(""), &out, io.Discard); code != 0 {
			t.Fatalf("init flags = %d: %s", code, out.String())
		}
		if !strings.Contains(out.String(), "未检测到 GitHub CLI（gh）") {
			t.Fatalf("graded report missing for flags path: %q", out.String())
		}
		for _, forbidden := range []string{"是否现在安装", "是否现在运行官方", "推荐安装 pi"} {
			if strings.Contains(out.String(), forbidden) {
				t.Fatalf("flags path must not prompt, got %q in %q", forbidden, out.String())
			}
		}
		snap, err := config.Load(home, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Config.Agents) != 1 {
			t.Fatalf("agents = %#v", snap.Config.Agents)
		}
	})
}

// TestProbeForgeLoginThreeStates pins acceptance 1 at the unit level: the probe
// distinguishes missing, installed-not-logged and logged via the injected
// runner.
func TestProbeForgeLoginThreeStates(t *testing.T) {
	fake := &fakeCommand{found: map[string]bool{"gh": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		if name == "gh" && len(args) == 2 && args[0] == "auth" && args[1] == "status" {
			return "github.com\n  ✓ Logged in to github.com account alice (keyring)\n", nil
		}
		return "", errors.New("fake command: not found")
	}
	replaceSetupCmd(t, fake)

	if got := probeForgeLogin("github"); !got.installed || got.login != "alice" {
		t.Fatalf("logged probe = %#v", got)
	}
	fake.found["gh"] = false
	if got := probeForgeLogin("github"); got.installed || got.login != "" {
		t.Fatalf("missing probe = %#v", got)
	}
	fake.found["gh"] = true
	fake.outputFn = func(string, ...string) (string, error) { return "", errors.New("not logged in") }
	if got := probeForgeLogin("github"); !got.installed || got.login != "" {
		t.Fatalf("installed-not-logged probe = %#v", got)
	}
}

// TestReportForgeLoginsGraded pins the three-state report wording: missing and
// installed-not-logged are distinguishable, logged shows the identity.
func TestReportForgeLoginsGraded(t *testing.T) {
	var out bytes.Buffer
	reportForgeLogins(&out, forgeLogins{
		github: forgeProbe{installed: true, login: "alice"},
		gitlab: forgeProbe{installed: true},
	})
	for _, want := range []string{"已检测到 GitHub 登录：alice", "检测到 glab 未登录；请运行 glab auth login。"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("graded report missing %q: %q", want, out.String())
		}
	}
	out.Reset()
	reportForgeLogins(&out, forgeLogins{})
	for _, want := range []string{"未检测到 GitHub CLI（gh）", "未检测到 GitLab CLI（glab）"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("graded report missing %q: %q", want, out.String())
		}
	}
}

// TestAskYes pins the confirm-first semantics: Enter and y/yes/是 confirm,
// anything else (n/no, unexpected text) declines — installs are never silent
// (issue #960 §2 红线). stdin EOF is a decline too, never the default y
// (issue #960 P1): `sift init </dev/null` must not confirm anything.
func TestAskYes(t *testing.T) {
	for _, tt := range []struct {
		answer, name string
		want         bool
	}{
		{"\n", "enter", true},
		{"y\n", "y", true},
		{"yes\n", "yes", true},
		{"是\n", "是", true},
		{"n\n", "n", false},
		{"no\n", "no", false},
		{"garbage\n", "unexpected", false},
		{"", "eof", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if got := askYes(bufio.NewReader(strings.NewReader(tt.answer)), &out, "测试问题"); got != tt.want {
				t.Fatalf("askYes(%q) = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}

// TestInitEOFDeclinesConfirmations pins the issue #960 P1 regression at the
// wizard level with the fake runner: stdin EOF must never confirm an
// install/login default, so `sift init </dev/null` degrades to printed
// guidance with zero executed commands while the config flow still completes;
// an explicit newline line, by contrast, still confirms the default yes.
func TestInitEOFDeclinesConfirmations(t *testing.T) {
	t.Run("empty stdin skips installs and logins", func(t *testing.T) {
		home := initTestRepo(t)
		gitOnlyPATH(t)
		// gh missing (install offer) and glab installed-but-not-logged
		// (login offer): every confirm answer is stdin EOF.
		fake := &fakeCommand{found: map[string]bool{"glab": true}}
		fake.outputFn = func(name string, args ...string) (string, error) {
			if name == "glab" {
				return "", errors.New("glab not logged in")
			}
			return "", errors.New("fake command: not found")
		}
		replaceSetupCmd(t, fake)

		var out bytes.Buffer
		if code := runWithInput([]string{"sift", "init"}, strings.NewReader(""), &out, io.Discard); code != 0 {
			t.Fatalf("init = %d: %s", code, out.String())
		}
		if len(fake.runs) != 0 {
			t.Fatalf("EOF must decline every install/login: fake runs = %#v", fake.runs)
		}
		for _, want := range []string{
			"未检测到 GitHub CLI（gh）", "是否现在安装 gh",
			"检测到 glab 未登录", "是否现在运行官方 glab auth login",
			"手动安装 pi",
		} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("declined guidance missing %q:\n%s", want, out.String())
			}
		}
		snap, err := config.Load(home, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Config.Projects) != 1 || len(snap.Config.Agents) != 0 ||
			len(snap.Config.Operators.GitHub)+len(snap.Config.Operators.GitLab) != 0 {
			t.Fatalf("EOF must keep the flow going with project binding only: %#v", snap.Config)
		}
	})

	t.Run("explicit newline still confirms default yes", func(t *testing.T) {
		home := initTestRepo(t)
		gitOnlyPATH(t)
		// Platform package manager plus npm visible so Enter-confirmed
		// installs actually run and are recorded by the fake runner.
		found := map[string]bool{"npm": true}
		if runtime.GOOS == "linux" {
			found["apt-get"] = true
		} else {
			found["brew"] = true
		}
		fake := &fakeCommand{found: found}
		fake.outputFn = func(name string, args ...string) (string, error) {
			if name == "pi" {
				return "pi 0.9.9\n", nil
			}
			return "", errors.New("fake command: not found")
		}
		replaceSetupCmd(t, fake)

		var out bytes.Buffer
		// Answers: gh install=Enter (yes) ; glab install=n ; pi=Enter (yes) ;
		// operator=Enter. Explicit newlines, never EOF: defaults confirm.
		if code := runWithInput([]string{"sift", "init"}, strings.NewReader("\nn\n\n\n"), &out, io.Discard); code != 0 {
			t.Fatalf("init = %d: %s", code, out.String())
		}
		var wantGh []string
		if runtime.GOOS == "linux" {
			wantGh = []string{"sudo", "apt-get", "install", "-y", "gh"}
		} else {
			wantGh = []string{"brew", "install", "gh"}
		}
		wantPi := []string{"npm", "install", "-g", "--ignore-scripts", "@earendil-works/pi-coding-agent"}
		if len(fake.runs) != 2 ||
			strings.Join(fake.runs[0], " ") != strings.Join(wantGh, " ") ||
			strings.Join(fake.runs[1], " ") != strings.Join(wantPi, " ") {
			t.Fatalf("Enter-confirmed installs = %#v, want %v then %v", fake.runs, wantGh, wantPi)
		}
		snap, err := config.Load(home, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Config.Agents) != 1 || snap.Config.Agents[0].ID != "pi" {
			t.Fatalf("agents = %#v, want pi registered", snap.Config.Agents)
		}
	})
}

// ---- issue #961: init 收尾三合一 -------------------------------------------

// fakeDoctorCheck mirrors the controlplane doctorCheck JSON shape: production
// OfflineDoctor returns a typed slice inside the result map, which must not
// break the guidance normalization (regression: `[]any` assertion only).
type fakeDoctorCheck struct {
	ID      string `json:"id"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// fakeDoctorResult builds a deterministic offline doctor result with the given
// exit code and per-check levels, so closeout tests never depend on the host.
func fakeDoctorResult(exitCode int, levels ...string) map[string]any {
	checks := make([]fakeDoctorCheck, 0, len(levels))
	for _, level := range levels {
		message := level
		if level != "ok" {
			message = "fake " + level + " message"
		}
		checks = append(checks, fakeDoctorCheck{ID: "check-" + level, Level: level, Message: message})
	}
	return map[string]any{"offline": true, "exit_code": exitCode, "security_posture": "unsafe-local", "checks": checks}
}

// TestSetupCloseoutDoctorRendersPerCheckGuidance pins acceptance 1 (step 1): a
// non-zero offline doctor result renders and then lists each non-ok check with
// a pointer to the troubleshooting runbook, while ok checks get none.
func TestSetupCloseoutDoctorRendersPerCheckGuidance(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	replaceSetupDoctor(t, func(config.Home) map[string]any {
		return fakeDoctorResult(2, "ok", "warning", "error")
	})
	var out bytes.Buffer
	setupCloseoutDoctor(bufio.NewReader(strings.NewReader("\n")), &out, home)
	for _, want := range []string{"Sift 诊断", "结论：有错误（退出码 2）", "修复指引", "check-warning", "check-error", "troubleshooting.md"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor guidance missing %q:\n%s", want, out.String())
		}
	}
	if strings.Count(out.String(), "troubleshooting.md §3") != 2 {
		t.Fatalf("exactly the failing checks must receive guidance: %q", out.String())
	}
}

// TestSetupCloseoutDoctorDeclineSkips pins that declining the offline check
// runs nothing (acceptance 1: each step can be skipped).
func TestSetupCloseoutDoctorDeclineSkips(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	replaceSetupDoctor(t, func(config.Home) map[string]any {
		ran = true
		return fakeDoctorResult(0)
	})
	var out bytes.Buffer
	setupCloseoutDoctor(bufio.NewReader(strings.NewReader("n\n")), &out, home)
	if ran || strings.Contains(out.String(), "Sift 诊断") {
		t.Fatalf("declined doctor step must not run: ran=%v %q", ran, out.String())
	}
}

// TestSetupCloseoutServiceFailurePrintsTroubleshooting pins acceptance 2: a
// failed install prints the troubleshooting pointer, skips the confirming
// status, and does not stop the wizard (the label step and closing output
// still run below). The status probe still runs first (issue #986).
func TestSetupCloseoutServiceFailurePrintsTroubleshooting(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	replaceSetupService(t, func(action string, home config.Home, stdout, stderr io.Writer) int {
		actions = append(actions, action)
		fmt.Fprintf(stdout, "fake service %s output\n", action)
		if action == "install" {
			return 1
		}
		return 0
	})
	var out bytes.Buffer
	setupCloseoutService(bufio.NewReader(strings.NewReader("\n")), &out, io.Discard, home)
	if !strings.Contains(out.String(), "troubleshooting.md") || !strings.Contains(out.String(), "sift daemon") {
		t.Fatalf("install failure must print the troubleshooting pointer: %q", out.String())
	}
	// Status probes first; a failed install must not run status again.
	if len(actions) != 2 || actions[0] != "status" || actions[1] != "install" {
		t.Fatalf("service actions = %v, want [status install]", actions)
	}
}

// TestSetupCloseoutServiceForegroundRunsStatus pins acceptance 2: a no-
// supervisor install succeeds (service layer prints the foreground hint) and
// status still runs afterwards, mirroring the standalone `sift service` flow.
// The status probe runs first (issue #986), then install and a confirming
// status.
func TestSetupCloseoutServiceForegroundRunsStatus(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	replaceSetupService(t, func(action string, home config.Home, stdout, stderr io.Writer) int {
		actions = append(actions, action)
		if action == "install" {
			fmt.Fprintln(stdout, "foreground daemon (no supervisor)：前台运行 `sift daemon`")
		} else {
			fmt.Fprintln(stdout, "fake status output")
		}
		return 0
	})
	var out bytes.Buffer
	setupCloseoutService(bufio.NewReader(strings.NewReader("\n")), &out, io.Discard, home)
	for _, want := range []string{"sift daemon", "fake status output"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("foreground service flow missing %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "troubleshooting.md") {
		t.Fatalf("foreground success must not print a failure pointer: %q", out.String())
	}
	// Status probes first, then install and a confirming status.
	if len(actions) != 3 || actions[0] != "status" || actions[1] != "install" || actions[2] != "status" {
		t.Fatalf("service actions = %v, want [status install status]", actions)
	}
}

// TestSetupCloseoutServiceRunningSkipsInstall pins issue #986 idempotency: the
// status step runs first and, when the service is already running, prints the
// skip note without asking the install prompt at all.
func TestSetupCloseoutServiceRunningSkipsInstall(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	replaceSetupService(t, func(action string, home config.Home, stdout, stderr io.Writer) int {
		actions = append(actions, action)
		fmt.Fprintln(stdout, "运行中（launchd，PID 123，socket ...）")
		return 0
	})
	var out bytes.Buffer
	// Empty (EOF) input: no install prompt is asked, so nothing is consumed.
	setupCloseoutService(bufio.NewReader(strings.NewReader("")), &out, io.Discard, home)
	if len(actions) != 1 || actions[0] != "status" {
		t.Fatalf("service actions = %v, want [status]", actions)
	}
	if !strings.Contains(out.String(), "服务已运行，跳过安装") {
		t.Fatalf("running service must print the skip note: %q", out.String())
	}
}

// TestSetupCloseoutServiceDeclineSkipsInstall pins that a not-running service
// asks to install, and declining runs only the initial status probe.
func TestSetupCloseoutServiceDeclineSkipsInstall(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	replaceSetupService(t, func(action string, home config.Home, stdout, stderr io.Writer) int {
		actions = append(actions, action)
		fmt.Fprintf(stdout, "fake service %s output\n", action)
		return 0
	})
	var out bytes.Buffer
	setupCloseoutService(bufio.NewReader(strings.NewReader("n\n")), &out, io.Discard, home)
	if len(actions) != 1 || actions[0] != "status" {
		t.Fatalf("declined install actions = %v, want [status]", actions)
	}
}

// TestSetupCloseoutLabelCreatesAfterDedupeAndConfirm pins acceptance 1 (step 3)
// and the double-caution red line: the label is deduped first, the exact
// command is shown, and only an explicit confirmation executes the create.
func TestSetupCloseoutLabelCreatesAfterDedupeAndConfirm(t *testing.T) {
	fake := &fakeCommand{found: map[string]bool{"gh": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "label" && args[1] == "list" {
			return "NAME  DESCRIPTION  COLOR\n", nil
		}
		return "", errors.New("fake command: not found")
	}
	replaceSetupCmd(t, fake)
	var out bytes.Buffer
	// Step ask = y, then the shown-command confirmation = y.
	setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\ny\n")), &out, "github", "owner/repo", "sift:run")
	want := []string{"gh", "label", "create", "sift:run", "--color", "5319e7", "--repo", "owner/repo"}
	if len(fake.runs) != 1 || strings.Join(fake.runs[0], " ") != strings.Join(want, " ") {
		t.Fatalf("label create = %#v, want %v", fake.runs, want)
	}
	if !strings.Contains(out.String(), "将执行：gh label create sift:run --color 5319e7 --repo owner/repo") {
		t.Fatalf("must show the exact command before confirming: %q", out.String())
	}
}

// TestSetupCloseoutLabelGlabUsesNamedFlags pins issue #986: glab label create
// takes -n/-c/-R flags (a positional name is rejected), while gh keeps the
// positional form. The dedupe list and the shown command follow the same fork.
func TestSetupCloseoutLabelGlabUsesNamedFlags(t *testing.T) {
	fake := &fakeCommand{found: map[string]bool{"glab": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		if name == "glab" && len(args) >= 2 && args[0] == "label" && args[1] == "list" {
			return "NAME  DESCRIPTION  COLOR\n", nil
		}
		return "", errors.New("fake command: not found")
	}
	replaceSetupCmd(t, fake)
	var out bytes.Buffer
	setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\ny\n")), &out, "gitlab", "group/proj", "sift:run")
	want := []string{"glab", "label", "create", "-n", "sift:run", "-c", "5319e7", "-R", "group/proj"}
	if len(fake.runs) != 1 || strings.Join(fake.runs[0], " ") != strings.Join(want, " ") {
		t.Fatalf("glab label create = %#v, want %v", fake.runs, want)
	}
	if !strings.Contains(out.String(), "将执行：glab label create -n sift:run -c 5319e7 -R group/proj") {
		t.Fatalf("must show the exact glab command before confirming: %q", out.String())
	}
}

// TestSetupCloseoutLabelGlabDegradesManualCommand pins issue #986: a failed
// glab create degrades to the flag-based manual command.
func TestSetupCloseoutLabelGlabDegradesManualCommand(t *testing.T) {
	fake := &fakeCommand{found: map[string]bool{"glab": true}}
	fake.outputFn = func(string, ...string) (string, error) {
		return "NAME\n", nil
	}
	fake.runFn = func(string, ...string) error {
		return errors.New("permission denied")
	}
	replaceSetupCmd(t, fake)
	var out bytes.Buffer
	setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\ny\n")), &out, "gitlab", "group/proj", "sift:run")
	if !strings.Contains(out.String(), "手动执行：glab label create -n sift:run -c 5319e7 -R group/proj") {
		t.Fatalf("failed glab create must degrade to the flag-based manual command: %q", out.String())
	}
}

// TestSetupCloseoutLabelExistsSkipsCreate pins acceptance 1: an already
// existing label is skipped without re-creating and without showing a create
// command (issue #961 红线: never silently create — nor duplicate).
func TestSetupCloseoutLabelExistsSkipsCreate(t *testing.T) {
	fake := &fakeCommand{found: map[string]bool{"gh": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		return "NAME  DESCRIPTION  COLOR\nsift:run  5319e7  \n", nil
	}
	replaceSetupCmd(t, fake)
	var out bytes.Buffer
	setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\n")), &out, "github", "owner/repo", "sift:run")
	if len(fake.runs) != 0 {
		t.Fatalf("existing label must not be re-created: %#v", fake.runs)
	}
	if strings.Contains(out.String(), "gh label create") {
		t.Fatalf("existing label must not show a create command: %q", out.String())
	}
}

// TestSetupCloseoutLabelDegrades pins the remaining degradation paths: a
// declined confirmation and a failed create both print no run / the manual
// command, a failed dedupe probe never creates, and a missing project prints
// the manual command.
func TestSetupCloseoutLabelDegrades(t *testing.T) {
	t.Run("declined confirmation does not create", func(t *testing.T) {
		fake := &fakeCommand{found: map[string]bool{"gh": true}}
		fake.outputFn = func(string, ...string) (string, error) {
			return "NAME\n", nil
		}
		replaceSetupCmd(t, fake)
		var out bytes.Buffer
		setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\nn\n")), &out, "github", "owner/repo", "sift:run")
		if len(fake.runs) != 0 {
			t.Fatalf("declined create ran: %#v", fake.runs)
		}
	})
	t.Run("failed create degrades to manual command", func(t *testing.T) {
		fake := &fakeCommand{found: map[string]bool{"gh": true}}
		fake.outputFn = func(string, ...string) (string, error) {
			return "NAME\n", nil
		}
		fake.runFn = func(string, ...string) error {
			return errors.New("permission denied")
		}
		replaceSetupCmd(t, fake)
		var out bytes.Buffer
		setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\ny\n")), &out, "github", "owner/repo", "sift:run")
		if !strings.Contains(out.String(), "手动执行：gh label create sift:run --color 5319e7 --repo owner/repo") {
			t.Fatalf("failed create must degrade to the manual command: %q", out.String())
		}
	})
	t.Run("failed dedupe probe never creates", func(t *testing.T) {
		fake := &fakeCommand{found: map[string]bool{"gh": true}}
		replaceSetupCmd(t, fake)
		var out bytes.Buffer
		setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\n")), &out, "github", "owner/repo", "sift:run")
		if len(fake.runs) != 0 {
			t.Fatalf("unverified create ran: %#v", fake.runs)
		}
		if !strings.Contains(out.String(), "手动命令：gh label create") {
			t.Fatalf("unverifiable dedupe must degrade to the manual command: %q", out.String())
		}
	})
	t.Run("missing project degrades to manual command", func(t *testing.T) {
		var out bytes.Buffer
		setupCloseoutLabel(bufio.NewReader(strings.NewReader("y\n")), &out, "", "", "sift:run")
		if !strings.Contains(out.String(), "手动命令：gh label create sift:run --color 5319e7") {
			t.Fatalf("no-project step must degrade to the manual command: %q", out.String())
		}
	})
}

// TestTriggerLabelListed pins the dedupe parser against the plain table output
// of both forge CLIs (label name is the first column).
func TestTriggerLabelListed(t *testing.T) {
	table := "NAME  DESCRIPTION  COLOR\nsift:run  5319e7  \nbug  something  d73a4a\n"
	for _, tt := range []struct {
		output, label string
		want          bool
	}{
		{table, "sift:run", true},
		{table, "bug", true},
		{"sift:run\n", "sift:run", true},
		{table, "sift:approved", false},
		{"NAME\n", "sift:run", false},
		{"", "sift:run", false},
	} {
		if got := triggerLabelListed(tt.output, tt.label); got != tt.want {
			t.Fatalf("triggerLabelListed(%q, %q) = %v, want %v", tt.output, tt.label, got, tt.want)
		}
	}
}

// TestInteractiveInitCloseoutThreeSteps is the wizard integration test for
// issue #961: after the config write the three closing steps appear in order,
// each defaults to yes, the label is deduped then created through the injected
// forge CLI, and the closing output carries the trigger example plus the
// polling expectation. Non-interactive init below still runs zero closing
// actions (acceptance 3).
func TestInteractiveInitCloseoutThreeSteps(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	fake := &fakeCommand{found: map[string]bool{"gh": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		switch {
		case name == "gh" && len(args) == 2 && args[0] == "auth" && args[1] == "status":
			return "github.com\n  ✓ Logged in to github.com account alice (keyring)\n", nil
		case name == "gh" && len(args) >= 2 && args[0] == "label" && args[1] == "list":
			return "NAME  DESCRIPTION  COLOR\n", nil
		}
		return "", errors.New("fake command: not found")
	}
	replaceSetupCmd(t, fake)
	replaceSetupDoctor(t, func(config.Home) map[string]any {
		return fakeDoctorResult(0, "ok")
	})
	replaceSetupService(t, func(action string, home config.Home, stdout, stderr io.Writer) int {
		fmt.Fprintf(stdout, "fake service %s\n", action)
		return 0
	})

	var out bytes.Buffer
	// Answers: glab install=n ; pi=n ; agent fallback=Enter ; closeout:
	// doctor=y, service=y, label=y, create-confirm=y.
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\n\ny\ny\ny\ny\n"), &out, io.Discard); code != 0 {
		t.Fatalf("init = %d: %s", code, out.String())
	}
	for _, want := range []string{
		"收尾三合一",
		"运行离线自检（sift doctor --offline）",
		"Sift 诊断",
		"安装用户级服务并启动（sift service install）",
		"fake service install",
		"fake service status",
		"创建触发 label sift:run（Forge 仓库写操作）",
		"将执行：gh label create sift:run --color 5319e7 --repo owner/repo",
		"全部就绪",
		"gh issue edit <N> --add-label \"sift:run\"",
		"60 秒",
		"docs/specs/config.md §3.5",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("closeout output missing %q:\n%s", want, out.String())
		}
	}
	create := []string{"gh", "label", "create", "sift:run", "--color", "5319e7", "--repo", "owner/repo"}
	if len(fake.runs) != 1 || strings.Join(fake.runs[0], " ") != strings.Join(create, " ") {
		t.Fatalf("label create = %#v, want %v", fake.runs, create)
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 || snap.Config.Projects[0].Forge.Project != "owner/repo" {
		t.Fatalf("projects = %#v", snap.Config.Projects)
	}
}

// TestInteractiveInitCloseoutIdempotentRerun pins issue #986 idempotency at the
// wizard level: a repeated interactive init over an already-provisioned setup
// must not re-create an existing label and must skip the install prompt when
// the service reports running. The first run provisions (label create + service
// install), the second run only probes status and the label list.
func TestInteractiveInitCloseoutIdempotentRerun(t *testing.T) {
	home := initTestRepo(t)
	gitOnlyPATH(t)
	listCount := 0
	fake := &fakeCommand{found: map[string]bool{"gh": true}}
	fake.outputFn = func(name string, args ...string) (string, error) {
		switch {
		case name == "gh" && len(args) == 2 && args[0] == "auth" && args[1] == "status":
			return "github.com\n  ✓ Logged in to github.com account alice (keyring)\n", nil
		case name == "gh" && len(args) >= 2 && args[0] == "label" && args[1] == "list":
			listCount++
			if listCount > 1 {
				return "NAME  DESCRIPTION  COLOR\nsift:run  5319e7  \n", nil
			}
			return "NAME  DESCRIPTION  COLOR\n", nil
		}
		return "", errors.New("fake command: not found")
	}
	replaceSetupCmd(t, fake)
	replaceSetupDoctor(t, func(config.Home) map[string]any {
		return fakeDoctorResult(0, "ok")
	})
	installDone := false
	replaceSetupService(t, func(action string, home config.Home, stdout, stderr io.Writer) int {
		if action == "install" {
			installDone = true
			return 0
		}
		if installDone {
			fmt.Fprintln(stdout, "运行中（launchd，PID 123，socket ...）")
		} else {
			fmt.Fprintln(stdout, "未运行")
		}
		return 0
	})

	runInit := func(answers string) string {
		t.Helper()
		var out bytes.Buffer
		if code := runWithInput([]string{"sift", "init"}, strings.NewReader(answers), &out, io.Discard); code != 0 {
			t.Fatalf("init = %d: %s", code, out.String())
		}
		return out.String()
	}

	// First run answers: gh install=n ; glab install=n ; pi=n ; agent=Enter ;
	// closeout: doctor=y, service=y, label=y, create-confirm=y.
	first := runInit("n\nn\nn\n\ny\ny\ny\ny\n")
	if !strings.Contains(first, "将执行：gh label create sift:run --color 5319e7 --repo owner/repo") {
		t.Fatalf("first init must create the label: %q", first)
	}
	if len(fake.runs) != 1 {
		t.Fatalf("first init label create = %#v", fake.runs)
	}

	// Second run: the label already exists and the service reports running, so
	// the closeout must skip both the create and the install prompt.
	fake.runs = nil
	var out bytes.Buffer
	// Answers: gh install=n ; glab install=n ; pi=n ; agent=Enter ; closeout:
	// doctor=y, label=y (service runs already and asks nothing).
	if code := runWithInput([]string{"sift", "init"}, strings.NewReader("n\nn\nn\n\ny\ny\n"), &out, io.Discard); code != 0 {
		t.Fatalf("second init = %d: %s", code, out.String())
	}
	if len(fake.runs) != 0 {
		t.Fatalf("second init must not create an existing label: %#v", fake.runs)
	}
	for _, want := range []string{"label 已存在：sift:run，跳过创建", "服务已运行，跳过安装"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("second init closeout must be idempotent, missing %q:\n%s", want, out.String())
		}
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 {
		t.Fatalf("projects = %#v, want still one project", snap.Config.Projects)
	}
}

// TestPiAuthLikely pins the weak login signal: the auth file or a common API
// key env var counts as possibly logged in, otherwise guidance is shown.
func TestPiAuthLikely(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	if piAuthLikely() {
		t.Fatal("empty env + no auth file must not count as logged in")
	}
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".pi", "agent", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !piAuthLikely() {
		t.Fatal("auth.json must count as possibly logged in")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if !piAuthLikely() {
		t.Fatal("ANTHROPIC_API_KEY must count as possibly logged in")
	}
}
