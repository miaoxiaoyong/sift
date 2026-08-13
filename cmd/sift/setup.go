package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/miaoxiaoyong/sift/internal/agents"
	"github.com/miaoxiaoyong/sift/internal/cli/render"
	"github.com/miaoxiaoyong/sift/internal/config"
)

type setupScope int

const (
	setupAll setupScope = iota
	setupProject
	setupAgent
)

type setupOptions struct {
	offline   bool
	agents    string
	project   string
	operator  string
	forge     string
	agentArgs string
}

// runSetup is deliberately local-only: it probes local executables and writes
// config.yaml, but never contacts the daemon.
//
// forge.kind is per-project, never global (issue #929): init no longer asks a
// global "Forge 类型"; both init and project add derive the kind from the git
// remote host (github.com→github, host containing gitlab→gitlab, host
// containing github→github), asking once only for a project whose host maps to
// nothing. Operators prefill from gh and glab logins independently.
func runSetup(args []string, stdin io.Reader, home config.Home, stdout, stderr io.Writer, scope setupScope) int {
	var opt setupOptions
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opt.offline, "offline", false, "non-interactive mode: skip all prompts and forge login probes")
	fs.StringVar(&opt.agents, "agent", "", "agent executable, or id=executable")
	fs.StringVar(&opt.agentArgs, "agent-args", "", "comma-separated agent arguments (overrides defaults)")
	fs.StringVar(&opt.project, "project", "", "repository path (default: current git worktree)")
	fs.StringVar(&opt.operator, "operator", "", "forge operator login (github:user,gitlab:user)")
	fs.StringVar(&opt.forge, "forge", "", "github or gitlab (overrides auto-detection)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if err == nil {
			err = errors.New("unexpected positional argument")
		}
		report(stderr, err)
		return 2
	}
	if opt.forge != "" && opt.forge != "github" && opt.forge != "gitlab" {
		report(stderr, errors.New("--forge must be github or gitlab"))
		return 2
	}
	if err := config.EnsureHomeLayout(home); err != nil {
		report(stderr, err)
		return 1
	}
	doc, existed, err := setupDocument(home)
	if err != nil {
		report(stderr, err)
		return 1
	}
	agentArgsSet := false
	fs.Visit(func(f *flag.Flag) {
		agentArgsSet = f.Name == "agent-args" || agentArgsSet
	})
	if scope != setupAll && opt.operator != "" {
		report(stderr, errors.New("--operator is only supported by sift init"))
		return 2
	}
	if scope == setupProject && (opt.agents != "" || agentArgsSet) {
		report(stderr, errors.New("--agent and --agent-args are only supported by sift init or sift agent add"))
		return 2
	}
	if scope == setupAgent && (opt.project != "" || opt.forge != "") {
		report(stderr, errors.New("--project and --forge are only supported by sift init or sift project add"))
		return 2
	}
	interactive := !opt.offline && opt.agents == "" && opt.project == "" && opt.operator == "" && opt.forge == ""
	in := bufio.NewReader(stdin)

	// Probe gh and glab logins independently so each operators allowlist
	// prefills from its own CLI (issue #929); offline skips the probes.
	var logins forgeLogins
	if !opt.offline {
		logins = probeForgeLogins()
		reportForgeLogins(stdout, logins)
	}

	// Project binding. forge.kind is per-project and derived from the git
	// remote host; only an undetectable host triggers one prompt for that
	// project (issue #929).
	projectKind, projectHost, projectKey := "", "", ""
	if scope != setupAgent {
		projectPath := opt.project
		if projectPath == "" {
			projectPath = detectedRepo()
		}
		if projectPath == "" && interactive {
			projectPath = prompt(in, stdout, "项目仓库路径", "")
		}
		if projectPath == "" {
			if scope == setupProject {
				report(stderr, errors.New("未检测到 git 仓库；请 cd 到项目目录运行 `sift project add`，或使用 --project PATH"))
				return 1
			}
			if interactive {
				fmt.Fprintln(stdout, "ℹ 未检测到 git 仓库，跳过项目绑定")
			}
		} else {
			abs, err := filepath.Abs(projectPath)
			if err != nil {
				report(stderr, err)
				return 1
			}
			projectHost, projectKey = originProbe(abs)
			projectKind = opt.forge
			if projectKind == "" {
				projectKind = detectForgeKind(projectHost)
			}
			if projectKind == "" && interactive {
				projectKind = promptForgeKind(in, stdout, projectHost)
				if projectKind != "github" && projectKind != "gitlab" {
					report(stderr, errors.New("Forge 类型必须是 github 或 gitlab"))
					return 2
				}
			}
			if projectKind == "" {
				projectKind = "github"
			}
			if projectKey == "" && interactive {
				projectKey = prompt(in, stdout, "Forge 项目（owner/repo）", "")
			}
			if projectKey == "" {
				report(stderr, errors.New("无法从 origin 解析 Forge 项目；请在仓库中设置 origin 后重试"))
				return 1
			}
			// Persist the probed origin host even when the kind came from the
			// one-time prompt or a --forge override: an undetectable host (e.g.
			// git.corp.example answered gitlab) must not silently fall back to
			// the platform default in forge.host. addProject omits the host
			// only when it equals the platform default (issue #929 review F1).
			addProject(doc, abs, projectKind, projectKey, projectHost)
		}
	}

	// Agent selection: numbered list with every detected agent preselected;
	// a numeric subset (1,3), all, or Enter keeps the selection (issue #929).
	// Each row shows the probed version and the built-in characteristic
	// profile (issue #930); non-interactive runs still report probed versions.
	if scope != setupProject {
		agentSpecs := strings.TrimSpace(opt.agents)
		if agentSpecs == "" && interactive {
			found := detectAgents()
			if len(found) == 0 {
				fmt.Fprintln(stdout, "⚠ 未在 PATH 中发现已收录的 coding agent（claude/codex/cursor/pi/gemini/aider/qwen/cody 等）；可输入可执行文件名，或直接回车跳过。")
				agentSpecs = prompt(in, stdout, "选择 Agent（逗号分隔，直接回车跳过）", "")
			} else {
				fmt.Fprintf(stdout, "%s 检测到 Agent：\n", render.Status("ok"))
				for i, d := range found {
					fmt.Fprintf(stdout, "  %d. %s\n", i+1, formatDetectedAgent(d))
				}
				picked := prompt(in, stdout, "选择 Agent（序号逗号分隔，如 1,3；直接回车或 all=全选；0/none=跳过）", "")
				agentSpecs = selectAgents(picked, detectedAgentNames(found))
			}
		} else if agentSpecs != "" {
			// 非交互路径：把探测到的 version 写进输出（不要求入 config；issue #930）。
			for _, spec := range strings.Split(agentSpecs, ",") {
				if spec = strings.TrimSpace(spec); spec == "" {
					continue
				}
				exe := spec
				if _, after, ok := strings.Cut(spec, "="); ok {
					exe = after
				}
				if v := agents.ProbeVersion(exe); v != "" {
					fmt.Fprintf(stdout, "%s Agent %s（%s %s）\n", render.Status("ok"), spec, exe, v)
				}
			}
		}
		for _, spec := range strings.Split(agentSpecs, ",") {
			if spec = strings.TrimSpace(spec); spec != "" {
				if agentArgsSet {
					agentArgs := []string{}
					if opt.agentArgs != "" {
						agentArgs = strings.Split(opt.agentArgs, ",")
					}
					addAgent(doc, spec, &agentArgs)
				} else {
					addAgent(doc, spec, nil)
				}
			}
		}
	}

	// Operators prefill from the CLI login of the project's forge kind; a
	// project-less init asks both sides (issue #929).
	if scope == setupAll {
		if operator := opt.operator; operator != "" {
			specs, err := parseOperatorSpec(operator, projectKind)
			if err != nil {
				report(stderr, err)
				return 2
			}
			for kind, names := range specs {
				for _, name := range names {
					addOperator(doc, kind, name)
				}
			}
		} else if interactive {
			kinds := []string{"github", "gitlab"}
			if projectKind != "" {
				kinds = []string{projectKind}
			}
			for _, kind := range kinds {
				login, label := logins.github, "GitHub"
				if kind == "gitlab" {
					login, label = logins.gitlab, "GitLab"
				}
				answer := prompt(in, stdout, label+" 操作员用户名（逗号分隔，直接回车跳过）", login)
				for _, name := range strings.Split(answer, ",") {
					if name = strings.TrimSpace(name); name != "" {
						addOperator(doc, kind, name)
					}
				}
			}
		} else if !opt.offline {
			// Non-interactive flags path without --operator: fall back to the
			// probed login of the relevant side, mirroring the pre-#929
			// `if operator == "" { operator = login }` default so the
			// documented `sift init --agent X --project .` still writes a
			// trusted operator (issue #929 review F2).
			kinds := []string{"github", "gitlab"}
			if projectKind != "" {
				kinds = []string{projectKind}
			}
			for _, kind := range kinds {
				login := logins.github
				if kind == "gitlab" {
					login = logins.gitlab
				}
				if login != "" {
					addOperator(doc, kind, login)
				}
			}
		}
	}
	if err := writeSetupDocument(home, doc, existed); err != nil {
		report(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s 已写入 %s\n", render.Status("ok"), config.ConfigPath(home))
	if isSocket(filepath.Join(home.Path, "siftd.sock")) {
		fmt.Fprintln(stdout, "⚠ daemon 运行中，运行 sift service reload 使新配置生效")
	} else {
		fmt.Fprintln(stdout, "✓ 下一步：运行 sift daemon 或 sift service install 启动")
	}
	return 0
}

func setupDocument(home config.Home) (map[string]any, bool, error) {
	path := config.ConfigPath(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{"version": 1}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	// Validate first so this editor never launders an invalid closed config.
	if _, err := config.Load(home, time.Now()); err != nil {
		return nil, false, err
	}
	jsonData, err := config.YAMLToJSON(data)
	if err != nil {
		return nil, false, err
	}
	var doc map[string]any
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return nil, false, err
	}
	return doc, true, nil
}

func writeSetupDocument(home config.Home, doc map[string]any, backup bool) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	path := config.ConfigPath(home)
	if backup {
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".bak", old, config.ConfigFileMode); err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
		if err := os.Chmod(path+".bak", config.ConfigFileMode); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(home.Path, ".config.yaml-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(config.ConfigFileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, config.ConfigFileMode)
}

func prompt(in *bufio.Reader, out io.Writer, label, fallback string) string {
	if fallback == "" {
		fmt.Fprintf(out, "%s: ", label)
	} else {
		fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	}
	line, err := in.ReadString('\n')
	if err != nil && len(line) == 0 {
		return fallback
	}
	if line = strings.TrimSpace(line); line == "" {
		return fallback
	}
	return line
}

// detectedAgent is a coding agent found on PATH together with its probed
// version and built-in characteristic profile (issue #930).
type detectedAgent struct {
	name    string
	version string
	char    agents.Characteristic
}

// detectAgents scans PATH for known coding agents (registry order), probing
// each with --version for display. Agents outside the registry are not
// auto-detected; users can still add them by executable name via --agent or
// the fallback prompt.
func detectAgents() []detectedAgent {
	var found []detectedAgent
	for _, name := range agents.Known() {
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		found = append(found, detectedAgent{
			name:    name,
			version: agents.ProbeVersion(name),
			char:    agents.For(name),
		})
	}
	return found
}

// detectedAgentNames extracts the ordered executable names for selectAgents.
func detectedAgentNames(found []detectedAgent) []string {
	names := make([]string, len(found))
	for i, d := range found {
		names[i] = d.name
	}
	return names
}

// formatDetectedAgent renders one wizard row:
// "claude (2.1.218) — 编码·推理·长上下文 · 200K · 中 · 中 · Anthropic Claude Code：…".
func formatDetectedAgent(d detectedAgent) string {
	name := d.name
	if d.version != "" {
		name = fmt.Sprintf("%s (%s)", name, d.version)
	}
	return fmt.Sprintf("%s — %s · %s", name, d.char.Summary(), d.char.Notes)
}

func probeForgeLogin(kind string) string {
	cli := forgeCLI(kind)
	if _, err := exec.LookPath(cli); err != nil {
		return ""
	}
	out, err := exec.Command(cli, "auth", "status").CombinedOutput()
	if err != nil {
		return ""
	}
	return forgeLoginFromStatus(string(out))
}

// forgeLoginFromStatus extracts gh's "account <login>" and glab's
// "as <user>" status formats. It is only a prefill, never an authorization
// decision.
func forgeLoginFromStatus(status string) string {
	re := regexp.MustCompile(`(?i)\b(?:account|as)\s+([^\s]+)`)
	if m := re.FindStringSubmatch(status); len(m) == 2 {
		return strings.Trim(m[1], "'\"")
	}
	return ""
}

func forgeCLI(kind string) string {
	if kind == "gitlab" {
		return "glab"
	}
	return "gh"
}
func detectedRepo() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// remoteHostProject splits a git remote URL into (host, owner/repo key). It
// covers scp-like (git@host:owner/repo), https, ssh and git schemes; a URL it
// cannot parse yields ("", "").
func remoteHostProject(url string) (host, key string) {
	u := strings.TrimSpace(url)
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if _, rest, ok := strings.Cut(u, "://"); ok {
		path := rest
		if i := strings.Index(path, "/"); i >= 0 {
			host, path = path[:i], path[i+1:]
		} else {
			return "", ""
		}
		if i := strings.Index(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		return host, strings.TrimSuffix(path, "/")
	}
	hostPart, path, ok := strings.Cut(u, ":")
	if !ok {
		return "", ""
	}
	if i := strings.LastIndex(hostPart, "@"); i >= 0 {
		hostPart = hostPart[i+1:]
	}
	return hostPart, strings.TrimPrefix(path, "/")
}

// detectForgeKind maps a remote host to a forge kind (issue #929):
// github.com→github, host containing gitlab→gitlab, host containing
// github→github (enterprise); otherwise "" (ask once for that project).
func detectForgeKind(host string) string {
	switch {
	case host == "github.com":
		return "github"
	case strings.Contains(host, "gitlab"):
		return "gitlab"
	case strings.Contains(host, "github"):
		return "github"
	}
	return ""
}

// originProbe reads the origin remote of repo and returns (host, project key).
func originProbe(repo string) (host, project string) {
	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", ""
	}
	return remoteHostProject(string(out))
}

// promptForgeKind asks once for a project whose remote host maps to no known
// forge, showing the detected host as context (issue #929).
func promptForgeKind(in *bufio.Reader, out io.Writer, host string) string {
	label := "Forge 类型（github/gitlab）"
	if host != "" {
		label = fmt.Sprintf("Forge 类型（检测到 host: %s）", host)
	}
	return strings.ToLower(prompt(in, out, label, "github"))
}

// selectAgents resolves an interactive agent pick against the detected list:
// empty or "all" keeps every detected agent, "0"/"none" keeps none, and a
// comma-separated numeric pick (1,3) maps to the numbered entries. Any other
// input passes through as legacy comma-separated executable specs.
func selectAgents(picked string, found []string) string {
	picked = strings.ToLower(strings.TrimSpace(picked))
	if picked == "" || picked == "all" {
		return strings.Join(found, ",")
	}
	if picked == "0" || picked == "none" {
		return ""
	}
	tokens := strings.Split(picked, ",")
	numeric := true
	for _, tok := range tokens {
		if _, err := strconv.Atoi(strings.TrimSpace(tok)); err != nil {
			numeric = false
			break
		}
	}
	if !numeric {
		return picked
	}
	var selected []string
	for _, tok := range tokens {
		n, _ := strconv.Atoi(strings.TrimSpace(tok))
		if n >= 1 && n <= len(found) {
			selected = append(selected, found[n-1])
		}
	}
	return strings.Join(selected, ",")
}

// forgeLogins holds the independent gh/glab login prefills (issue #929).
type forgeLogins struct {
	github string
	gitlab string
}

// probeForgeLogins probes gh and glab independently: github operators prefill
// from gh auth, gitlab operators from glab auth.
func probeForgeLogins() forgeLogins {
	return forgeLogins{
		github: probeForgeLogin("github"),
		gitlab: probeForgeLogin("gitlab"),
	}
}

func reportForgeLogins(stdout io.Writer, logins forgeLogins) {
	entries := []struct{ kind, cli, label, login string }{
		{"github", "gh", "GitHub", logins.github},
		{"gitlab", "glab", "GitLab", logins.gitlab},
	}
	for _, e := range entries {
		if e.login == "" {
			fmt.Fprintf(stdout, "%s 未检测到 %s 登录；请先运行 %s auth login。\n", render.Status("warning"), e.label, e.cli)
		} else {
			fmt.Fprintf(stdout, "%s 已检测到 %s 登录：%s\n", render.Status("ok"), e.label, e.login)
		}
	}
}

// parseOperatorSpec splits an --operator value into per-forge names. Plain
// names attach to defaultKind; github:user / gitlab:user select the forge.
func parseOperatorSpec(spec, defaultKind string) (map[string][]string, error) {
	if defaultKind == "" {
		defaultKind = "github"
	}
	out := map[string][]string{}
	for _, token := range strings.Split(spec, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		kind, name := defaultKind, token
		if before, after, ok := strings.Cut(token, ":"); ok {
			kind, name = before, after
		}
		if kind != "github" && kind != "gitlab" {
			return nil, fmt.Errorf("--operator 值无效：%q（支持 github:user,gitlab:user 或纯用户名）", token)
		}
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		out[kind] = append(out[kind], name)
	}
	return out, nil
}

func addAgent(doc map[string]any, spec string, args *[]string) {
	id, executable := filepath.Base(spec), spec
	if before, after, ok := strings.Cut(spec, "="); ok {
		id, executable = before, after
	}
	id = setupID(id)
	items := list(doc, "agents")
	for _, item := range items {
		if m, ok := item.(map[string]any); ok && m["id"] == id {
			return
		}
	}
	if args == nil {
		defaults := defaultAgentArgs(executable)
		args = &defaults
	}
	argv := make([]any, len(*args))
	for i, arg := range *args {
		argv[i] = arg
	}
	doc["agents"] = append(items, map[string]any{"id": id, "executable": executable, "args": argv, "task_transport": "stdin", "backend": "process"})
}

func defaultAgentArgs(executable string) []string {
	switch filepath.Base(executable) {
	case "claude":
		return []string{"-p"}
	case "codex":
		return []string{"exec", "-"}
	case "cursor", "pi":
		return []string{"-p"}
	default:
		return []string{}
	}
}

func addProject(doc map[string]any, repo, kind, project, host string) {
	items := list(doc, "projects")
	for _, item := range items {
		if m, ok := item.(map[string]any); ok && m["repo"] == repo {
			return
		}
	}
	base := setupID(filepath.Base(repo))
	used := make(map[string]bool, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			if existing, ok := m["id"].(string); ok {
				used[existing] = true
			}
		}
	}
	id := base
	for n := 2; used[id]; n++ {
		suffix := fmt.Sprintf("-%d", n)
		id = base
		if len(id)+len(suffix) > 63 {
			id = id[:63-len(suffix)]
		}
		id += suffix
	}
	ref := map[string]any{"kind": kind, "project": project}
	if host != "" && host != forgeDefaultHost(kind) {
		ref["host"] = host
	}
	doc["projects"] = append(items, map[string]any{"id": id, "repo": repo, "forge": ref, "enabled": true})
}

// forgeDefaultHost mirrors config.md §3.3: the public host used when a
// project omits forge.host.
func forgeDefaultHost(kind string) string {
	if kind == "gitlab" {
		return "gitlab.com"
	}
	return "github.com"
}
func addOperator(doc map[string]any, kind, name string) {
	op, _ := doc["operators"].(map[string]any)
	if op == nil {
		op = map[string]any{}
		doc["operators"] = op
	}
	key := kind
	items := list(op, key)
	for _, item := range items {
		if item == name {
			return
		}
	}
	op[key] = append(items, name)
}
func list(doc map[string]any, key string) []any {
	if v, ok := doc[key].([]any); ok {
		return v
	}
	return []any{}
}
func setupID(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "agent-" + s
	}
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}
func isSocket(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}
