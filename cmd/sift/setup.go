package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xsift/sift/internal/agents"
	"github.com/xsift/sift/internal/cli/render"
	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/controlplane"
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
	interactive := !opt.offline && opt.agents == "" && opt.project == "" && opt.operator == "" && opt.forge == "" && !agentArgsSet
	in := bufio.NewReader(stdin)

	// Probe gh and glab logins independently so each operators allowlist
	// prefills from its own CLI (issue #929); offline skips the probes.
	// Interactive init walks both CLIs through the three-state diagnosis —
	// missing → offer install, installed-not-logged → offer the official auth
	// login, logged → silent — with confirm-first installs (issue #960); all
	// other paths keep the graded report only.
	var logins forgeLogins
	if !opt.offline {
		if interactive && scope == setupAll {
			logins = guideForgeLogins(in, stdout)
		} else {
			logins = probeForgeLogins()
			reportForgeLogins(stdout, logins)
		}
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
				// pi ranks first when nothing is detected (issue #960 §3): it is
				// open source and needs no vendor account. Commercial agents are
				// never installed by sift — they stay detected/registered only.
				fmt.Fprintln(stdout, "⚠ 未在 PATH 中发现已收录的 coding agent（claude/codex/cursor/pi/gemini/aider/qwen/cody 等）")
				if spec := guidePiBootstrap(in, stdout); spec != "" {
					agentSpecs = spec
				} else {
					fmt.Fprintln(stdout, "  可输入可执行文件名，或直接回车跳过。")
					agentSpecs = prompt(in, stdout, "选择 Agent（逗号分隔，直接回车跳过）", "")
				}
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
						for _, a := range strings.Split(opt.agentArgs, ",") {
							if a = strings.TrimSpace(a); a != "" {
								agentArgs = append(agentArgs, a)
							}
						}
					}
					addAgent(doc, spec, &agentArgs)
				} else {
					addAgent(doc, spec, nil)
				}
			}
		}
	}

	// A detected forge login is already the operator identity. Init records it
	// directly instead of asking the user to confirm a value the CLI supplied.
	// Probe both sides even for a single-forge project: a user logged into gh and
	// glab expects both allowlists to be ready (issue #945).
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
		} else if !opt.offline {
			for _, entry := range []struct {
				kind, label string
				probe       forgeProbe
			}{
				{"github", "GitHub", logins.github},
				{"gitlab", "GitLab", logins.gitlab},
			} {
				if entry.probe.login != "" {
					addOperator(doc, entry.kind, entry.probe.login)
					fmt.Fprintf(stdout, "✓ operator: %s\n", entry.probe.login)
					continue
				}
				// A failed probe remains the only interactive question. For a
				// known project forge, do not ask about an unrelated failed CLI.
				if !interactive || (projectKind != "" && projectKind != entry.kind) {
					continue
				}
				answer := prompt(in, stdout, entry.label+" 操作员用户名（逗号分隔，直接回车跳过）", "")
				for _, name := range strings.Split(answer, ",") {
					if name = strings.TrimSpace(name); name != "" {
						addOperator(doc, entry.kind, name)
					}
				}
			}
		}
	}
	if err := writeSetupDocument(home, doc, existed); err != nil {
		report(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s 已写入 %s\n", render.Status("ok"), config.ConfigPath(home))
	announceConfigApplied(home, stdout, stderr)
	// Interactive init only: the closing three-in-one (issue #961). Offline or
	// flags-given runs skip it entirely so CI/scripted output stays unchanged;
	// the wizard exit code keeps reflecting the config write itself (红线).
	if interactive && scope == setupAll {
		setupCloseout(home, projectKind, projectKey, in, stdout, stderr)
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

// normalizeNumbers converts integral float64 values to int before yaml
// serialization. setupDocument decodes the existing config via JSON into a
// map[string]any, which turns every number into float64; yaml.v3 then emits
// large integral floats in scientific notation (1000000 → 1e+06), a byte
// drift on every init rerun over an existing config (issue #927). Only
// integral in-range values are converted; fractions and out-of-range values
// stay float64 so no numeric meaning changes.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, item := range t {
			t[k] = normalizeNumbers(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = normalizeNumbers(item)
		}
		return t
	case float64:
		if t == math.Trunc(t) && t >= math.MinInt64 && t <= math.MaxInt64 {
			return int(t)
		}
	}
	return v
}

func writeSetupDocument(home config.Home, doc map[string]any, backup bool) error {
	data, err := yaml.Marshal(normalizeNumbers(doc))
	if err != nil {
		return err
	}
	if _, err := config.ParseYAML(data); err != nil {
		return fmt.Errorf("配置无效，未写入: %w", err)
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
		// EOF is not an answer: never substitute the default (issue #960 P1),
		// otherwise `sift init </dev/null` would silently confirm an install
		// or login whose fallback is y. Callers treat the empty result as
		// skip/decline and continue the wizard.
		return ""
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

// forgeProbe is the structured result of probing one forge CLI (issue #960):
// installed reports whether the CLI is on PATH, login the parsed auth identity
// (empty when not logged in or when auth status fails). The three states —
// missing / installed-not-logged / logged — drive the init guidance.
// forgeLoginFromStatus is only a prefill, never an authorization decision.
type forgeProbe struct {
	installed bool
	login     string
}

func probeForgeLogin(kind string) forgeProbe {
	cli := forgeCLI(kind)
	if !setupCmd.lookup(cli) {
		return forgeProbe{}
	}
	out, err := setupCmd.output(cli, "auth", "status")
	if err != nil {
		return forgeProbe{installed: true}
	}
	return forgeProbe{installed: true, login: forgeLoginFromStatus(out)}
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

// setupCmd abstracts the exec calls the wizard makes so tests inject fakes
// (issue #960): CI never really installs packages or runs auth flows. The
// production implementation attaches installs and the official auth login to
// the user's stdio — Sift passes through, never wraps or buffers them.
var setupCmd setupCommand = realCommand{}

// setupDoctorRun is the seam for the closing offline self-check (issue #961
// 步骤 1): production reuses controlplane.OfflineDoctor — the exact logic
// `sift doctor --offline` executes — instead of shelling out a child process.
// Tests swap it for a fixed result so no host probes run.
var setupDoctorRun = controlplane.OfflineDoctor

// setupServiceRun reuses the `sift service` entry point for the closing
// service step (issue #961 步骤 2): install and status run through the exact
// code path the CLI command uses, never a shelled-out sift child. Tests swap
// it for a fake so no launchctl/systemctl invocation touches the host.
var setupServiceRun = func(action string, home config.Home, stdout, stderr io.Writer) int {
	return runService([]string{action}, home, stdout, stderr)
}

// setupCommand is the exec surface of the setup wizard: PATH lookup and
// captured-output probes, plus stdio passthrough runs for the official auth
// login and package-manager installs.
type setupCommand interface {
	lookup(name string) bool
	output(name string, args ...string) (string, error)
	run(name string, args ...string) error
}

// realCommand executes through os/exec; installs and auth login attach to
// os.Stdin/os.Stdout/os.Stderr per the passthrough requirement (issue #960 §1).
type realCommand struct{}

func (realCommand) lookup(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (realCommand) output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func (realCommand) run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// guidePiBootstrap offers to install the pi coding agent when interactive init
// finds no known agent on PATH (issue #960 §3). pi ranks first because it is
// open source, multi-model and needs no vendor account; commercial agents are
// never installed by sift. Returns the agent spec to register ("pi"), or ""
// when the user declines or the install cannot verify — the caller then falls
// back to the plain agent prompt. Every failure degrades to printed guidance.
func guidePiBootstrap(in *bufio.Reader, out io.Writer) string {
	if !askYes(in, out, "推荐安装 pi（开源，多模型，支持订阅/API Key）") {
		fmt.Fprintln(out, piInstallManual())
		return ""
	}
	if !setupCmd.lookup("npm") {
		fmt.Fprintf(out, "%s 未检测到 npm，请用官方脚本安装 pi：\n", render.Status("warning"))
		fmt.Fprintln(out, piInstallManual())
		return ""
	}
	if err := setupCmd.run("npm", "install", "-g", "--ignore-scripts", "@earendil-works/pi-coding-agent"); err != nil {
		fmt.Fprintf(out, "%s npm 安装 pi 失败：%v\n", render.Status("warning"), err)
		fmt.Fprintln(out, piInstallManual())
		return ""
	}
	if _, err := setupCmd.output("pi", "--version"); err != nil {
		fmt.Fprintf(out, "%s 安装后未能验证 `pi --version`；请手动安装并确认可运行。\n", render.Status("warning"))
		fmt.Fprintln(out, piInstallManual())
		return ""
	}
	if !piAuthLikely() {
		fmt.Fprintln(out, piLoginGuidance())
	}
	return "pi"
}

// piInstallManual is the non-blocking degradation text when pi auto-install is
// declined or impossible. The script URL is the single source; docs link it
// instead of copying the command (issue #960 引用不复制).
func piInstallManual() string {
	return "  手动安装 pi：\n" +
		"    curl -fsSL https://pi.dev/install.sh | sh\n" +
		"    或 npm install -g --ignore-scripts @earendil-works/pi-coding-agent\n" +
		"    装完确认 `pi --version` 可运行。"
}

// piLoginGuidance prints the two weak-signal login paths after a successful
// install (issue #960 §3.4). Strong verification stays with sift doctor; init
// never spends model calls to verify login, and v1 does not launch the pi TUI.
func piLoginGuidance() string {
	return "  登录 pi（任选一条路径；v1 不自动拉起 pi 界面）：\n" +
		"    - 订阅：运行 `pi` 后在界面输入 /login，选择 provider\n" +
		"    - API Key：export ANTHROPIC_API_KEY=...（或 OPENAI_API_KEY 等）后运行 `pi`\n" +
		"  强验证请用 `sift doctor`。"
}

// piAuthLikely returns a weak signal that pi is already configured with a
// provider: the agent auth file exists or a common API key env var is set.
// It is deliberately not a strong check — sift doctor owns verification and
// init does not burn model calls here (issue #960 §3.5).
func piAuthLikely() bool {
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".pi", "agent", "auth.json")); err == nil {
			return true
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
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

// forgeLogins holds the independent gh/glab probe results (issue #929, #960).
type forgeLogins struct {
	github forgeProbe
	gitlab forgeProbe
}

// probeForgeLogins probes gh and glab independently: github operators prefill
// from gh auth, gitlab operators from glab auth.
func probeForgeLogins() forgeLogins {
	return forgeLogins{
		github: probeForgeLogin("github"),
		gitlab: probeForgeLogin("gitlab"),
	}
}

// reportForgeLogins prints the graded three-state report (issue #960): the
// user can tell “not installed” apart from “installed but not logged in” and
// knows which CLI needs attention. Interactive init replaces this report with
// the actionable guidance (guideForgeLogins); other scopes keep it as a hint.
func reportForgeLogins(stdout io.Writer, logins forgeLogins) {
	entries := []struct {
		kind, cli, label string
		probe            forgeProbe
	}{
		{"github", "gh", "GitHub", logins.github},
		{"gitlab", "glab", "GitLab", logins.gitlab},
	}
	for _, e := range entries {
		switch {
		case !e.probe.installed:
			fmt.Fprintf(stdout, "%s 未检测到 %s CLI（%s）\n", render.Status("warning"), e.label, e.cli)
		case e.probe.login == "":
			fmt.Fprintf(stdout, "%s 检测到 %s 未登录；请运行 %s auth login。\n", render.Status("warning"), e.cli, e.cli)
		default:
			fmt.Fprintf(stdout, "%s 已检测到 %s 登录：%s\n", render.Status("ok"), e.label, e.probe.login)
		}
	}
}

// guideForgeLogins walks gh and glab through the interactive three-state
// diagnosis (issue #960 §1): missing → offer install, installed-not-logged →
// offer the official auth login, logged → silent. Every step is confirm-first
// and degrades on failure so the wizard always continues. Returns the
// best-known probes for the operator recording below.
func guideForgeLogins(in *bufio.Reader, out io.Writer) forgeLogins {
	return forgeLogins{
		github: guideForgeLogin(in, out, "github"),
		gitlab: guideForgeLogin(in, out, "gitlab"),
	}
}

// guideForgeLogin runs the three-state guidance for one forge CLI. Install and
// login failures degrade to manual instructions instead of aborting: project
// binding, agent probing and operator recording below still run (issue #960
// §2 红线). Sift never takes over the CLI's credential lifecycle — auth login
// is the official command passed through with stdio attached.
func guideForgeLogin(in *bufio.Reader, out io.Writer, kind string) forgeProbe {
	cli := forgeCLI(kind)
	label := forgeLabel(kind)
	probe := probeForgeLogin(kind)
	if !probe.installed {
		fmt.Fprintf(out, "%s 未检测到 %s CLI（%s）\n", render.Status("warning"), label, cli)
		if !askYes(in, out, "是否现在安装 "+cli) {
			return probe
		}
		if err := installForgeCLI(kind, out); err != nil {
			fmt.Fprintf(out, "%s 自动安装 %s 失败：%v\n  官方安装指引（含各平台安装命令）：%s\n", render.Status("warning"), cli, err, forgeInstallURL(kind))
			return probe
		}
		probe = probeForgeLogin(kind)
		if !probe.installed {
			fmt.Fprintf(out, "%s 安装后仍未在 PATH 中找到 %s，请按官方指引手动安装：%s\n", render.Status("warning"), cli, forgeInstallURL(kind))
			return probe
		}
	}
	if probe.login == "" {
		fmt.Fprintf(out, "%s 检测到 %s 未登录\n", render.Status("warning"), cli)
		if !askYes(in, out, "是否现在运行官方 "+cli+" auth login") {
			return probe
		}
		if err := setupCmd.run(cli, "auth", "login"); err != nil {
			fmt.Fprintf(out, "%s %s auth login 未完成：%v；登录态仍归官方 CLI，可稍后手动运行 %s auth login。\n", render.Status("warning"), cli, err, cli)
			return probe
		}
		probe = probeForgeLogin(kind)
		if probe.login == "" {
			fmt.Fprintf(out, "%s 仍未能确认 %s 登录；可稍后手动运行 %s auth login 后重跑 sift init。\n", render.Status("warning"), cli, cli)
			return probe
		}
	}
	fmt.Fprintf(out, "%s 已检测到 %s 登录：%s\n", render.Status("ok"), label, probe.login)
	return probe
}

// installForgeCLI installs the forge CLI through the degradation matrix
// (issue #960 §2): brew on macOS, apt/dnf/yum on Linux. The caller already
// confirmed; every failure returns an error so the caller degrades to the
// official manual path — installs never block the wizard.
func installForgeCLI(kind string, out io.Writer) error {
	cli := forgeCLI(kind)
	switch runtime.GOOS {
	case "darwin":
		if !setupCmd.lookup("brew") {
			return errors.New("未检测到 Homebrew")
		}
		return setupCmd.run("brew", "install", cli)
	case "linux":
		if setupCmd.lookup("apt-get") {
			// Debian/Ubuntu: gh is not in the default source; point at the
			// official repo guidance, then run the install for the user.
			fmt.Fprintf(out, "  %s Debian/Ubuntu：%s 不在默认源，先按官方指引添加仓库：%s\n", render.Status("info"), cli, forgeInstallURL(kind))
			return setupCmd.run("sudo", "apt-get", "install", "-y", cli)
		}
		for _, pm := range []string{"dnf", "yum"} {
			if setupCmd.lookup(pm) {
				return setupCmd.run("sudo", pm, "install", "-y", cli)
			}
		}
		return errors.New("未检测到 apt/dnf/yum 包管理器")
	}
	return fmt.Errorf("暂不支持在 %s 平台自动安装", runtime.GOOS)
}

// forgeLabel returns the display name for a forge kind.
func forgeLabel(kind string) string {
	if kind == "gitlab" {
		return "GitLab"
	}
	return "GitHub"
}

// forgeInstallURL returns the official installation page of one forge CLI.
// The full install command matrix lives once in docs/guides/installation.md;
// the wizard prints only this pointer (issue #960 引用不复制).
func forgeInstallURL(kind string) string {
	if kind == "gitlab" {
		return "https://gitlab.com/gitlab-org/cli"
	}
	return "https://cli.github.com/"
}

// askYes renders a confirm question with default yes. Every install/login
// step of the wizard is confirm-first, never silent (issue #960 §2 红线);
// Enter or any y/yes/是 confirms, everything else declines. stdin EOF is a
// decline, never the default: `sift init </dev/null` must not install or
// login without an explicit answer (issue #960 P1). prompt returns "" only
// on EOF — an explicit empty line (Enter) still resolves to the "y" default.
func askYes(in *bufio.Reader, out io.Writer, question string) bool {
	ans := strings.ToLower(strings.TrimSpace(prompt(in, out, question+"（y/n）", "y")))
	switch ans {
	case "y", "yes", "是":
		return true
	}
	return false
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

// setupCloseout runs the interactive three-step closing sequence after the
// config is written (issue #961): embedded offline doctor self-check, user
// service install/status, and confirm-first trigger label creation. Each step
// defaults to yes and can be skipped; every failure degrades without aborting.
// The trigger label name comes from the just-written config (labels.trigger,
// default sift:run, config.md §3.14).
func setupCloseout(home config.Home, kind, projectKey string, in *bufio.Reader, stdout, stderr io.Writer) {
	fmt.Fprintln(stdout, "\n收尾三合一（直接回车默认执行，可输入 n 跳过）：")
	setupCloseoutDoctor(in, stdout, home)
	setupCloseoutService(in, stdout, stderr, home)
	label := "sift:run"
	if snap, err := config.Load(home, time.Now()); err == nil {
		label = snap.Config.Labels.Trigger
	}
	setupCloseoutLabel(in, stdout, kind, projectKey, label)
	printSetupReady(stdout, kind, label)
}

// setupCloseoutDoctor runs the embedded offline doctor (step 1). A non-zero
// result lists each failing check with a pointer to the troubleshooting
// runbook; it never blocks the following steps. The runbook owns the repair
// steps (引用不复制), so this only points per check.
func setupCloseoutDoctor(in *bufio.Reader, out io.Writer, home config.Home) {
	if !askYes(in, out, "运行离线自检（sift doctor --offline）") {
		return
	}
	value := setupDoctorRun(home)
	renderDoctor(out, value)
	result := normalizeDoctorResult(value)
	if doctorExitCode(result) == 0 {
		return
	}
	fmt.Fprintln(out, "修复指引（逐条处理，warning 不应忽略，error 必须先修复）：")
	checks, _ := result["checks"].([]any)
	for _, raw := range checks {
		check, _ := raw.(map[string]any)
		if level, _ := check["level"].(string); level == "ok" || level == "info" {
			continue
		}
		id, _ := check["id"].(string)
		fmt.Fprintf(out, "  - %s → docs/runbooks/troubleshooting.md §3 Doctor 报告\n", id)
	}
}

// normalizeDoctorResult projects a doctor result (typed checks from the
// controlplane package or any JSON shape) onto the render map so guidance can
// iterate the checks with the same tolerance renderDoctor has.
func normalizeDoctorResult(value any) map[string]any {
	body, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		return map[string]any{}
	}
	return result
}

// setupCloseoutService installs and starts the user service (step 2) through
// the same entry point `sift service install`/`status` use. It is idempotent
// (issue #986): it checks status first and, when the service is already
// running, prints the running state and skips the install prompt entirely; only
// a not-running service asks to install and start. A host without a supervisor
// gets the foreground `sift daemon` hint from the service layer itself, exactly
// like the standalone command; a non-zero install exit prints the
// troubleshooting pointer and the wizard continues with step 3.
func setupCloseoutService(in *bufio.Reader, out, errOut io.Writer, home config.Home) {
	var statusOut bytes.Buffer
	setupServiceRun("status", home, &statusOut, errOut)
	status := statusOut.String()
	io.Copy(out, &statusOut)
	if serviceStatusRunning(status) {
		fmt.Fprintln(out, "  ✓ 服务已运行，跳过安装")
		return
	}
	if !askYes(in, out, "安装用户级服务并启动（sift service install）") {
		return
	}
	if code := setupServiceRun("install", home, out, errOut); code != 0 {
		fmt.Fprintln(out, "  ✗ service install 失败；排查步骤见 docs/runbooks/troubleshooting.md §2（无 supervisor 时按提示前台运行 `sift daemon`）")
		return
	}
	setupServiceRun("status", home, out, errOut)
}

// serviceStatusRunning reports whether a `sift service status` render already
// shows the service running, so the closeout skips install on an idempotent
// rerun (issue #986). It keys on the running-state marker both renderServiceStatus
// and the foreground report print.
func serviceStatusRunning(output string) bool {
	return strings.Contains(output, "运行中")
}

// setupCloseoutLabel creates the trigger label (step 3), the only forge write
// of the wizard, with double caution (issue #961 红线): it dedupes first via
// the forge CLI's label list, shows the exact command and asks for an explicit
// confirmation before creating, and every failure degrades to the printed
// manual command. The command form is forked by forge kind (issue #986): gh
// takes the label as a positional argument, glab requires the -n/-c/-R flags.
// An existing label is skipped without re-creating or showing a command.
// Without a bound project there is no repo context, so the step degrades to
// the manual command too.
func setupCloseoutLabel(in *bufio.Reader, out io.Writer, kind, projectKey, label string) {
	if kind == "" || projectKey == "" {
		fmt.Fprintf(out, "  %s 未绑定项目，跳过触发 label 创建；手动命令：%s\n", render.Status("warning"), labelCreateCommand(forgeCLI(kind), label, projectKey))
		return
	}
	if !askYes(in, out, "创建触发 label "+label+"（Forge 仓库写操作）") {
		return
	}
	cli := forgeCLI(kind)
	listed, err := setupCmd.output(cli, labelListArgs(cli, projectKey)...)
	if err != nil {
		fmt.Fprintf(out, "  %s 无法查询 %s 的 label 列表（%v），跳过创建；手动命令：%s\n", render.Status("warning"), projectKey, err, labelCreateCommand(cli, label, projectKey))
		return
	}
	if triggerLabelListed(listed, label) {
		fmt.Fprintf(out, "  ✓ label 已存在：%s，跳过创建\n", label)
		return
	}
	command := labelCreateCommand(cli, label, projectKey)
	fmt.Fprintf(out, "  将执行：%s\n", command)
	if !askYes(in, out, "确认执行？") {
		return
	}
	if err := setupCmd.run(cli, labelCreateArgs(cli, label, projectKey)...); err != nil {
		fmt.Fprintf(out, "  %s 创建 label 失败：%v；手动执行：%s\n", render.Status("warning"), err, command)
	}
}

// labelListArgs returns the forge CLI args that list labels for a project.
// gh and glab both accept -R/--repo, but glab uses the short form in its help
// (issue #986); the longer --repo is unambiguous and works for both, so a
// single --repo form is shared.
func labelListArgs(cli, projectKey string) []string {
	return []string{"label", "list", "--repo", projectKey}
}

// labelCreateArgs returns the forge CLI args that create a label. The command
// form is forked by forge (issue #986): gh takes the label name positionally
// plus --color/--repo, while glab requires the -n/-c/-R flags — a positional
// name is rejected by glab label create.
func labelCreateArgs(cli, label, projectKey string) []string {
	if cli == "glab" {
		args := []string{"label", "create", "-n", label, "-c", "5319e7"}
		if projectKey != "" {
			args = append(args, "-R", projectKey)
		}
		return args
	}
	args := []string{"label", "create", label, "--color", "5319e7"}
	if projectKey != "" {
		args = append(args, "--repo", projectKey)
	}
	return args
}

// labelCreateCommand renders the exact forge CLI create command for display
// and manual-fallback hints, matching labelCreateArgs.
func labelCreateCommand(cli, label, projectKey string) string {
	return cli + " " + strings.Join(labelCreateArgs(cli, label, projectKey), " ")
}

// triggerLabelListed reports whether the forge CLI's label list output already
// contains the label. The label name is the first column of every row; the
// dedupe must never create a duplicate (issue #961 红线).
func triggerLabelListed(output, label string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == label || strings.HasPrefix(line, label+" ") {
			return true
		}
	}
	return false
}

// printSetupReady closes the wizard with the trigger example and the polling
// expectation. The numeric intervals (60s idle / 15s active) are expected
// semantics only; the authoritative defaults live once in config.md §3.5 and
// are linked, never copied (引用不复制, issue #961).
func printSetupReady(out io.Writer, kind, label string) {
	fmt.Fprintf(out, "全部就绪。给一个 Issue 打上 %s 后，约 60 秒内出现在 sift ps：\n", label)
	if kind == "gitlab" {
		fmt.Fprintf(out, "  glab issue update <N> --label %q\n", label)
	} else {
		fmt.Fprintf(out, "  gh issue edit <N> --add-label %q\n", label)
	}
	fmt.Fprintln(out, "轮询预期 60s（idle）/15s（active）；权威默认值见 docs/specs/config.md §3.5 scheduler.intake_idle_interval")
}
