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
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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

func runSetupCommand(args []string, stdin io.Reader, home config.Home, stdout, stderr io.Writer, scope setupScope) int {
	if len(args) == 0 || args[0] != "add" {
		usage := map[setupScope]string{
			setupProject: "sift project add [--project PATH] [--forge github|gitlab] [--offline]",
			setupAgent:   "sift agent add [--agent NAME] [--agent-args ARG,ARG] [--offline]",
		}[scope]
		report(stderr, fmt.Errorf("usage: %s", usage))
		return 2
	}
	return runSetup(args[1:], stdin, home, stdout, stderr, scope)
}

// runSetup is deliberately local-only: it probes local executables and writes
// config.yaml, but never contacts the daemon.
func runSetup(args []string, stdin io.Reader, home config.Home, stdout, stderr io.Writer, scope setupScope) int {
	var opt setupOptions
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opt.offline, "offline", false, "non-interactive mode: skip all prompts and forge login probes")
	fs.StringVar(&opt.agents, "agent", "", "agent executable, or id=executable")
	fs.StringVar(&opt.agentArgs, "agent-args", "", "comma-separated agent arguments (overrides defaults)")
	fs.StringVar(&opt.project, "project", "", "repository path")
	fs.StringVar(&opt.operator, "operator", "", "forge operator login")
	fs.StringVar(&opt.forge, "forge", "", "github or gitlab")
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
	forgeKind := opt.forge
	login := ""
	if scope != setupAgent {
		if forgeKind == "" {
			if interactive {
				forgeKind = strings.ToLower(prompt(in, stdout, "Forge 类型（github/gitlab）", "github"))
				if forgeKind != "github" && forgeKind != "gitlab" {
					report(stderr, errors.New("Forge 类型必须是 github 或 gitlab"))
					return 2
				}
			} else {
				forgeKind = "github"
			}
		}
		if !opt.offline {
			login = probeForgeLogin(forgeKind)
			if login == "" {
				fmt.Fprintf(stdout, "%s 未检测到 %s 登录；请先运行 %s auth login。\n", render.Status("warning"), forgeKind, forgeCLI(forgeKind))
			} else {
				fmt.Fprintf(stdout, "%s 已检测到 %s 登录：%s\n", render.Status("ok"), forgeKind, login)
			}
		}
	}
	if scope != setupProject {
		agents := strings.TrimSpace(opt.agents)
		if agents == "" && interactive {
			found := detectAgents()
			if len(found) == 0 {
				fmt.Fprintln(stdout, "⚠ 未在 PATH 中发现 claude/codex/cursor/pi；可输入可执行文件名，或直接回车跳过。")
			} else {
				fmt.Fprintf(stdout, "✓ 检测到 Agent：%s\n", strings.Join(found, ", "))
			}
			agents = prompt(in, stdout, "选择 Agent（逗号分隔，直接回车跳过）", strings.Join(found, ","))
		}
		for _, spec := range strings.Split(agents, ",") {
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
	if scope != setupAgent {
		project := opt.project
		if project == "" && interactive {
			project = prompt(in, stdout, "项目仓库路径", detectedRepo())
		}
		if project != "" {
			abs, err := filepath.Abs(project)
			if err != nil {
				report(stderr, err)
				return 1
			}
			key := forgeProject(abs)
			if key == "" && interactive {
				key = prompt(in, stdout, "Forge 项目（owner/repo）", "")
			}
			if key == "" {
				report(stderr, errors.New("无法从 origin 解析 Forge 项目；请在仓库中设置 origin 后重试"))
				return 1
			}
			addProject(doc, abs, forgeKind, key)
		}
		if scope == setupAll {
			operator := opt.operator
			if operator == "" && interactive {
				operator = prompt(in, stdout, "允许操作的用户名（逗号分隔，直接回车跳过）", login)
			}
			if operator == "" {
				operator = login
			}
			for _, name := range strings.Split(operator, ",") {
				if name = strings.TrimSpace(name); name != "" {
					addOperator(doc, forgeKind, name)
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

func detectAgents() []string {
	var found []string
	for _, name := range []string{"claude", "codex", "cursor", "pi"} {
		if _, err := exec.LookPath(name); err == nil {
			found = append(found, name)
		}
	}
	return found
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
func forgeProject(repo string) string {
	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	parts := strings.Split(s, "/")
	if len(parts) == 2 {
		return s
	} // git@host:owner/repo
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[1:], "/")
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

func addProject(doc map[string]any, repo, kind, project string) {
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
	doc["projects"] = append(items, map[string]any{"id": id, "repo": repo, "forge": map[string]any{"kind": kind, "project": project}, "enabled": true})
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
