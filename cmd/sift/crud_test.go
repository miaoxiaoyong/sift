package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// addTestProject provisions a git repo with a github.com origin and registers
// it via the real `sift project add` offline path.
func addTestProject(t *testing.T, repo, origin string) {
	t.Helper()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", repo, err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", origin).CombinedOutput(); err != nil {
		t.Fatalf("git remote add %s: %v: %s", repo, err, out)
	}
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "project", "add", "--offline", "--project", repo, "--forge", "github"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("project add = %d: %s", code, out.String())
	}
}

// addTestAgent registers an agent whose executable base name hits the built-in
// registry (claude), via the real `sift agent add` offline path.
func addTestAgent(t *testing.T) string {
	t.Helper()
	agent := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runWithInput([]string{"sift", "agent", "add", "--offline", "--agent", "claude=" + agent}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("agent add = %d: %s", code, out.String())
	}
	return agent
}

// TestProjectListTwoRowsAndJSON pins the core acceptance: after two registered
// projects, `sift project list` renders two rows carrying the absolute repo
// path, the forge column (kind:project), enabled and the resolved agents;
// --json is structured.
func TestProjectListTwoRowsAndJSON(t *testing.T) {
	_ = freshHome(t)
	repo1 := filepath.Join(t.TempDir(), "demo-one")
	repo2 := filepath.Join(t.TempDir(), "demo-two")
	addTestProject(t, repo1, "git@github.com:owner/demo-one.git")
	addTestProject(t, repo2, "git@github.com:owner/demo-two.git")

	out := runCapture(t, []string{"sift", "project", "list"})
	for _, want := range []string{"id", "repo", "forge", "enabled", "agents"} {
		if !strings.Contains(out, want) {
			t.Fatalf("project list lacks header %q:\n%s", want, out)
		}
	}
	for _, want := range []string{repo1, repo2, "github:owner/demo-one", "github:owner/demo-two", "是"} {
		if !strings.Contains(out, want) {
			t.Fatalf("project list lacks %q:\n%s", want, out)
		}
	}

	var buf bytes.Buffer
	if code := run([]string{"sift", "project", "list", "--json"}, &buf, io.Discard); code != 0 {
		t.Fatalf("project list --json exit = %d", code)
	}
	var result struct {
		Projects []struct {
			ID    string `json:"id"`
			Repo  string `json:"repo"`
			Forge struct {
				Kind    string `json:"kind"`
				Project string `json:"project"`
				Host    string `json:"host"`
			} `json:"forge"`
			Enabled bool     `json:"enabled"`
			Agents  []string `json:"agents"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("project list --json is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(result.Projects) != 2 {
		t.Fatalf("--json projects = %d, want 2:\n%s", len(result.Projects), buf.String())
	}
	if result.Projects[0].Forge.Kind != "github" || result.Projects[0].Forge.Project != "owner/demo-one" || result.Projects[0].Forge.Host != "github.com" {
		t.Fatalf("--json forge ref = %+v", result.Projects[0].Forge)
	}
	if !result.Projects[0].Enabled {
		t.Fatalf("--json enabled = false, want true")
	}
}

// TestProjectListAgentsColumnResolved pins that the agents column shows the
// normalized candidate ids: a project with no explicit agents field lists
// every registered agent.
func TestProjectListAgentsColumnResolved(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	addTestAgent(t)
	addTestProject(t, repo, "git@github.com:owner/demo.git")

	out := runCapture(t, []string{"sift", "project", "list"})
	if !strings.Contains(out, "claude") {
		t.Fatalf("project list agents column lacks resolved agent id:\n%s", out)
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 1 || len(snap.Config.Projects[0].Agents) != 1 || snap.Config.Projects[0].Agents[0] != "claude" {
		t.Fatalf("snapshot agents = %#v", snap.Config.Projects)
	}
}

// TestProjectListEmpty pins the friendly empty surface: no config yet renders
// a hint with exit 0 and --json renders an empty array.
func TestProjectListEmpty(t *testing.T) {
	freshHome(t)
	out := runCapture(t, []string{"sift", "project", "list"})
	if !strings.Contains(out, "尚未注册项目") {
		t.Fatalf("empty project list lacks friendly hint:\n%s", out)
	}
	var buf bytes.Buffer
	if code := run([]string{"sift", "project", "list", "--json"}, &buf, io.Discard); code != 0 {
		t.Fatalf("empty --json exit = %d", code)
	}
	if !strings.Contains(buf.String(), `"projects": []`) {
		t.Fatalf("empty --json is not an empty array:\n%s", buf.String())
	}
}

// TestProjectRemoveAndIdempotentRerun pins the remove acceptance: after the
// delete the project is gone from config, a .bak backup exists, and re-running
// the same remove reports 不存在 with a non-zero exit (never a crash).
func TestProjectRemoveAndIdempotentRerun(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	addTestProject(t, repo, "git@github.com:owner/demo.git")

	var out bytes.Buffer
	if code := run([]string{"sift", "project", "remove", "demo"}, &out, io.Discard); code != 0 {
		t.Fatalf("remove = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "已移除项目 demo") {
		t.Fatalf("remove output = %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Projects) != 0 {
		t.Fatalf("project still registered after remove: %#v", snap.Config.Projects)
	}
	if info, err := os.Stat(config.ConfigPath(home) + ".bak"); err != nil || info.Mode().Perm() != config.ConfigFileMode {
		t.Fatalf("backup missing or wrong mode: %v, %v", info, err)
	}
	var errOut bytes.Buffer
	if code := run([]string{"sift", "project", "remove", "demo"}, io.Discard, &errOut); code == 0 {
		t.Fatalf("rerun remove exited 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "不存在") {
		t.Fatalf("rerun remove stderr = %q", errOut.String())
	}
}

// TestAgentListFeaturesAndJSON pins the agent acceptance: the list renders
// id/executable/args/backend and the registry feature labels (strengths·
// context·cost·speed); --json carries the structured characteristics.
func TestAgentListFeaturesAndJSON(t *testing.T) {
	_ = freshHome(t)
	agent := addTestAgent(t)

	out := runCapture(t, []string{"sift", "agent", "list"})
	for _, want := range []string{"id", "executable", "args", "backend", "特点"} {
		if !strings.Contains(out, want) {
			t.Fatalf("agent list lacks header %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"claude", agent, "-p", "process", "编码", "200K"} {
		if !strings.Contains(out, want) {
			t.Fatalf("agent list lacks %q:\n%s", want, out)
		}
	}

	var buf bytes.Buffer
	if code := run([]string{"sift", "agent", "list", "--json"}, &buf, io.Discard); code != 0 {
		t.Fatalf("agent list --json exit = %d", code)
	}
	var result struct {
		Agents []struct {
			ID         string   `json:"id"`
			Executable string   `json:"executable"`
			Args       []string `json:"args"`
			Backend    string   `json:"backend"`
			Char       struct {
				Strengths []string `json:"strengths"`
				Context   string   `json:"context"`
				Cost      string   `json:"cost"`
				Speed     string   `json:"speed"`
			} `json:"characteristics"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("agent list --json is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(result.Agents) != 1 {
		t.Fatalf("--json agents = %d, want 1:\n%s", len(result.Agents), buf.String())
	}
	a := result.Agents[0]
	if a.ID != "claude" || a.Executable != agent || a.Backend != "process" {
		t.Fatalf("--json agent = %+v", a)
	}
	if len(a.Args) != 1 || a.Args[0] != "-p" {
		t.Fatalf("--json args = %#v, want [\"-p\"]", a.Args)
	}
	if a.Char.Context != "200K" || a.Char.Cost != "medium" || a.Char.Speed != "medium" {
		t.Fatalf("--json characteristics = %+v", a.Char)
	}
	if len(a.Char.Strengths) == 0 || a.Char.Strengths[0] != "coding" {
		t.Fatalf("--json strengths = %#v", a.Char.Strengths)
	}
}

// TestAgentRemove pins `sift agent remove <id>`: same write/idempotency
// semantics as project remove (backup + not-found rerun, non-zero exit).
func TestAgentRemove(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	_ = addTestAgent(t)

	var out bytes.Buffer
	if code := run([]string{"sift", "agent", "remove", "claude"}, &out, io.Discard); code != 0 {
		t.Fatalf("agent remove = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "已移除Agent claude") {
		t.Fatalf("remove output = %q", out.String())
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Config.Agents) != 0 {
		t.Fatalf("agent still registered after remove: %#v", snap.Config.Agents)
	}
	if _, err := os.Stat(config.ConfigPath(home) + ".bak"); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	var errOut bytes.Buffer
	if code := run([]string{"sift", "agent", "remove", "claude"}, io.Discard, &errOut); code == 0 {
		t.Fatalf("rerun agent remove exited 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "不存在") {
		t.Fatalf("rerun agent remove stderr = %q", errOut.String())
	}
}

// TestAgentRemoveReferencedByProject pins the #937 round-1 P1 fix: an agent
// explicitly referenced by a project's agents field must not be removable —
// the command exits non-zero, names the referencing project, and leaves
// config.yaml untouched (removing it would write an unloadable config).
func TestAgentRemoveReferencedByProject(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "demo")
	_ = addTestAgent(t)
	addTestProject(t, repo, "git@github.com:owner/demo.git")

	// The CLI never writes an explicit project agents field; hand-edit the
	// config to reproduce the review repro (project bound to the agent).
	doc, existed, err := setupDocument(home)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("config missing after project add")
	}
	projects := list(doc, "projects")
	pm, ok := projects[0].(map[string]any)
	if !ok {
		t.Fatalf("projects[0] = %T", projects[0])
	}
	pm["agents"] = []any{"claude"}
	if err := writeSetupDocument(home, doc, existed); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(config.ConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{"sift", "agent", "remove", "claude"}, io.Discard, &stderr); code == 0 {
		t.Fatalf("agent remove of a referenced agent exited 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "demo") {
		t.Fatalf("agent remove error lacks referencing project id:\n%s", stderr.String())
	}
	after, err := os.ReadFile(config.ConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("referenced agent remove rewrote config:\nbefore %q\nafter %q", before, after)
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatalf("config unloadable after rejected remove: %v", err)
	}
	if len(snap.Config.Agents) != 1 || snap.Config.Agents[0].ID != "claude" {
		t.Fatalf("agents after rejected remove = %#v", snap.Config.Agents)
	}
}

// TestAgentListEmpty pins the friendly empty surface for `sift agent list`.
func TestAgentListEmpty(t *testing.T) {
	freshHome(t)
	out := runCapture(t, []string{"sift", "agent", "list"})
	if !strings.Contains(out, "尚未注册 Agent") {
		t.Fatalf("empty agent list lacks friendly hint:\n%s", out)
	}
}

// TestResourceCommandsUsageErrors pins the closed argument surface: unknown
// verbs, bad list flags and remove arity are usage-class exits (2).
func TestResourceCommandsUsageErrors(t *testing.T) {
	freshHome(t)
	cases := []struct {
		argv []string
		want int
	}{
		{[]string{"sift", "project"}, 2},
		{[]string{"sift", "project", "frobnicate"}, 2},
		{[]string{"sift", "project", "list", "--bogus"}, 2},
		{[]string{"sift", "project", "remove"}, 2},
		{[]string{"sift", "project", "remove", "a", "b"}, 2},
		{[]string{"sift", "agent"}, 2},
		{[]string{"sift", "agent", "frobnicate"}, 2},
		{[]string{"sift", "agent", "list", "--bogus"}, 2},
		{[]string{"sift", "agent", "remove"}, 2},
	}
	for _, tc := range cases {
		var stderr bytes.Buffer
		if code := run(tc.argv, io.Discard, &stderr); code != tc.want {
			t.Errorf("%v exit = %d, want %d; stderr=%q", tc.argv, code, tc.want, stderr.String())
		}
	}
}

// TestProjectRemoveInvalidConfigFailsClosed pins that a remove never launders
// an invalid config: config.Load's validation error surfaces as a non-zero
// exit and the on-disk file is untouched.
func TestProjectRemoveInvalidConfigFailsClosed(t *testing.T) {
	_ = freshHome(t)
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	path := config.ConfigPath(home)
	if err := os.WriteFile(path, []byte("version: 1\nprojects: [not-a-map]\n"), config.ConfigFileMode); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{"sift", "project", "remove", "demo"}, io.Discard, &stderr); code == 0 {
		t.Fatalf("remove on invalid config exited 0, want non-zero")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invalid config was rewritten by remove:\nbefore %q\nafter %q", before, after)
	}
}
