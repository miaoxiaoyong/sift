// Command sift is the operator CLI and local control-plane daemon.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/cli/render"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/hosting"
	"github.com/miaoxiaoyong/sift/internal/install"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/version"
)

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run executes one operator command and returns the process exit status. It is
// split from main so cmd-level tests can assert exit codes without spawning a
// subprocess. config.md §7 mandates that `sift doctor` exits 0/1/2 per the
// doctor result's exit_code; the offline path computes that result locally,
// the online path receives it from the daemon in response.Result.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		return overview(stdout, stderr)
	}
	if args[1] == "help" || args[1] == "--help" || args[1] == "-h" {
		if len(args) > 3 {
			report(stderr, fmt.Errorf("usage: sift help [command]"))
			return 2
		}
		if len(args) == 3 {
			return commandHelp(args[2], stdout, stderr)
		}
		return commandHelp("", stdout, stderr)
	}
	// `sift --version` is the operator-facing release version surface; the
	// wrapper exposes the same value via `sift-agent-wrapper --version` and the
	// daemon via the RPC envelope and `sift doctor` (WBS M8 §8.1).
	if args[1] == "--version" || args[1] == "-version" {
		if len(args) != 2 {
			report(stderr, fmt.Errorf("usage: sift --version"))
			return 2
		}
		fmt.Fprintln(stdout, version.Release)
		return 0
	}
	home, err := config.ResolveHome()
	if err != nil {
		report(stderr, err)
		return 1
	}
	command := args[1]
	if command == "daemon" {
		if len(args) != 2 {
			report(stderr, fmt.Errorf("usage: sift daemon"))
			return 2
		}
		return runDaemonCommand(home, stderr)
	}
	if command == "install" {
		return runInstall(args[2:], home, stdout, stderr)
	}
	if command == "service" {
		return runService(args[2:], home, stdout, stderr)
	}
	if command == "doctor" {
		jsonOutput, offline, ok := doctorFlags(args[2:])
		if !ok {
			report(stderr, fmt.Errorf("usage: sift doctor [--offline] [--json]"))
			return 2
		}
		if offline {
			result := controlplane.OfflineDoctor(home)
			return emitDoctor(stdout, stderr, result, jsonOutput)
		}
		// Keep the protocol envelope untouched when explicitly requested.
		_ = jsonOutput
	}
	if command == "report" {
		return runReport(args[2:], home, stdout, stderr)
	}
	requestArgs := args[2:]
	if command == "doctor" {
		requestArgs = nil
	} // --json/--offline are CLI-only flags
	// --json / SIFT_JSON=1 select the raw RPC envelope; the default is the
	// humanized Chinese rendering. The flag is stripped before RPC param
	// building so it never reaches the daemon.
	jsonOutput := os.Getenv("SIFT_JSON") == "1"
	requestArgs, jsonFlag := splitJSONFlag(requestArgs)
	jsonOutput = jsonOutput || jsonFlag
	method, params, err := request(command, requestArgs)
	if err != nil {
		report(stderr, err)
		return 1
	}
	response, err := controlplane.OperatorRequest(home, method, params)
	if err != nil {
		report(stderr, fmt.Errorf("daemon unavailable: %w", err))
		return 1
	}
	if command == "attach" {
		return runAttach(response, home, stdout, stderr, jsonOutput)
	}
	if command == "doctor" {
		jsonOutput, _, _ := doctorFlags(args[2:])
		return runDoctor(response, stdout, stderr, jsonOutput)
	}
	if jsonOutput {
		// The JSON envelope is the RPC surface: keep it byte-identical with
		// the raw dump for scripting and protocol regression.
		if err := printJSON(stdout, response); err != nil {
			report(stderr, err)
			return 1
		}
		if !response.OK {
			return 1
		}
		return 0
	}
	if !response.OK {
		// Humanized failure: an actionable reason instead of the raw envelope.
		renderError(stdout, response, failureContext(command, requestArgs))
		return 1
	}
	switch command {
	case "ps":
		renderPS(stdout, response.Result)
	case "timeline":
		renderTimeline(stdout, response.Result)
	case "logs":
		renderLogs(stdout, requestArgs[0], response.Result)
	case "metrics":
		renderMetrics(stdout, response.Result)
	case "worktree":
		renderWorktree(stdout, requestArgs[0], response.Result)
	case "kill", "retry":
		renderKillRetry(stdout, command, requestArgs[0], response.Result)
	default:
		if err := printJSON(stdout, response); err != nil {
			report(stderr, err)
			return 1
		}
	}
	return 0
}

// splitJSONFlag removes the CLI-only --json flag from args so command flag
// parsers never forward it to the daemon. The environment equivalent is
// SIFT_JSON=1; callers OR both.
func splitJSONFlag(args []string) (rest []string, jsonOutput bool) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			jsonOutput = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, jsonOutput
}

// runDoctor renders the online doctor response and maps it to the process
// exit status (config.md §7). The daemon handshake is fail-closed
// (control-plane.md §3.4, release.md §4): an incompatible CLI receives
// unsupported_protocol/unsupported_binary instead of a result. OperatorRequest
// validates the response envelope, request id, protocol, and server binary
// major before this function receives it; an unvalidated response is never
// allowed to influence doctor output. A validated handshake rejection surfaces
// as the version:daemon error the mismatch implies and exits 2.
func runDoctor(response controlplane.Response, stdout, stderr io.Writer, jsonOutput bool) int {
	if !response.EnvelopeValidated() {
		report(stderr, fmt.Errorf("invalid daemon response for doctor"))
		return 1
	}
	if !response.OK {
		if response.Error != nil && (response.Error.Code == "unsupported_protocol" || response.Error.Code == "unsupported_binary") {
			return emitDoctor(stdout, stderr, doctorMismatchResult(response), jsonOutput)
		}
		if jsonOutput {
			if err := printJSON(stdout, response); err != nil {
				report(stderr, err)
			}
			return 1
		}
		fmt.Fprintln(stdout, "✗ 守护进程返回错误")
		return 1
	}
	if jsonOutput {
		if err := printJSON(stdout, response); err != nil {
			report(stderr, err)
			return 1
		}
	} else {
		renderDoctor(stdout, response.Result)
	}
	return doctorExitCode(response.Result)
}

// doctorMismatchResult synthesizes the doctor result for a handshake-rejected
// or envelope-incompatible online doctor: a single version:daemon error check
// pairing the CLI's own values with the daemon values observed on the wire.
func doctorMismatchResult(response controlplane.Response) map[string]any {
	message := "CLI and daemon binary major versions differ"
	if response.Error != nil && response.Error.Code == "unsupported_protocol" {
		message = "CLI and daemon wire protocol versions differ"
	}
	return map[string]any{
		"offline":          false,
		"exit_code":        2,
		"security_posture": "unsafe-local",
		"checks": []any{map[string]any{
			"id":      "version:daemon",
			"level":   "error",
			"message": message,
			"details": map[string]any{
				"cli_version":           version.Release,
				"cli_protocol_major":    controlplane.ProtocolMajor,
				"daemon_version":        response.ServerVersion,
				"daemon_protocol_major": response.ProtocolMajor,
			},
		}},
	}
}

// majorVersion extracts the release major for the client-side envelope check;
// the daemon applies the same rule in its handshake.
func majorVersion(release string) string {
	if i := strings.IndexByte(release, '.'); i >= 0 {
		return release[:i]
	}
	return release
}

// emitDoctor prints the offline doctor result and maps its exit_code to the
// process exit status (config.md §7).
func emitDoctor(stdout, stderr io.Writer, result map[string]any, jsonOutput bool) int {
	if jsonOutput {
		if err := printJSON(stdout, result); err != nil {
			report(stderr, err)
			return 1
		}
	} else {
		renderDoctor(stdout, result)
	}
	return doctorExitCode(result)
}

// doctorExitCode extracts the process exit status from a doctor result. The
// doctor computes exit_code as 0 (clean), 1 (warning) or 2 (error); this only
// projects it. The offline result carries a Go int, the online result arrives
// from JSON as a float64. Any absent, malformed, fractional, or out-of-range
// value is untrustworthy and therefore fails closed as an error.
func doctorExitCode(result any) int {
	m, ok := result.(map[string]any)
	if !ok {
		return 2
	}
	switch code := m["exit_code"].(type) {
	case int:
		if code >= 0 && code <= 2 {
			return code
		}
	case float64:
		if code >= 0 && code <= 2 && code == float64(int(code)) {
			return int(code)
		}
	}
	return 2
}

func doctorFlags(args []string) (jsonOutput, offline, ok bool) {
	jsonOutput = os.Getenv("SIFT_JSON") == "1"
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		case "--offline":
			offline = true
		default:
			return false, false, false
		}
	}
	return jsonOutput, offline, true
}

func renderDoctor(w io.Writer, value any) {
	// Normalize typed offline checks to the same shape used by the online RPC.
	var result map[string]any
	body, marshalErr := json.Marshal(value)
	ok := marshalErr == nil && json.Unmarshal(body, &result) == nil
	if !ok {
		fmt.Fprintln(w, "✗ 无法读取诊断结果")
		return
	}
	fmt.Fprintln(w, "Sift 诊断")
	if offline, _ := result["offline"].(bool); offline {
		fmt.Fprintln(w, "模式：离线（不连接守护进程）")
	}
	checks, _ := result["checks"].([]any)
	for _, raw := range checks {
		check, _ := raw.(map[string]any)
		level, _ := check["level"].(string)
		id, _ := check["id"].(string)
		message, _ := check["message"].(string)
		fmt.Fprintf(w, "%s %s：%s\n", render.Status(level), id, doctorMessage(message, level))
		if details, ok := check["details"].(map[string]any); ok && len(details) > 0 {
			if b, err := json.Marshal(details); err == nil {
				fmt.Fprintf(w, "  详情：%s\n", string(b))
			}
		}
	}
	code := doctorExitCode(result)
	labels := []string{"正常", "有警告", "有错误"}
	if code >= 0 && code < len(labels) {
		fmt.Fprintf(w, "\n结论：%s（退出码 %d）\n", labels[code], code)
	}
}

func doctorMessage(message, level string) string {
	if strings.Contains(strings.ToLower(message), "unsupported") {
		return "版本或协议不兼容"
	}
	if level == "ok" {
		return "检查通过"
	}
	if level == "warning" {
		return "需要注意：" + message
	}
	if level == "error" {
		return "检查失败：" + message
	}
	return message
}

func request(command string, args []string) (string, map[string]any, error) {
	switch command {
	case "ps":
		return "ops.ps", map[string]any{"run_id": nil, "project_id": nil, "status": nil, "limit": 100, "after_run_id": nil}, nil
	case "doctor":
		if len(args) != 0 {
			return "", nil, fmt.Errorf("doctor accepts only --offline")
		}
		return "ops.doctor", map[string]any{}, nil
	case "logs":
		if len(args) != 1 {
			return "", nil, fmt.Errorf("usage: sift logs <run-id>")
		}
		return "ops.logs", map[string]any{"run_id": args[0], "attempt_no": nil, "offset": 0, "limit": 262144}, nil
	case "attach":
		if len(args) != 1 || args[0] == "" {
			return "", nil, fmt.Errorf("usage: sift attach <run-id>")
		}
		return "ops.attach", map[string]any{"run_id": args[0]}, nil
	case "worktree":
		if len(args) != 1 {
			return "", nil, fmt.Errorf("usage: sift worktree <run-id>")
		}
		return "ops.worktree", map[string]any{"run_id": args[0]}, nil
	case "hooks-bootstrap":
		if len(args) != 1 || args[0] == "" {
			return "", nil, fmt.Errorf("usage: sift hooks-bootstrap <project-id>")
		}
		return "ops.hooks-bootstrap", map[string]any{"project_id": args[0]}, nil
	case "kill", "retry":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		version := fs.Int("expected-version", 0, "expected Run version")
		key := fs.String("request-key", "", "idempotency key")
		if err := fs.Parse(args); err != nil {
			return "", nil, err
		}
		if fs.NArg() != 1 || *version < 1 || *key == "" {
			return "", nil, fmt.Errorf("usage: sift %s <run-id> --expected-version N --request-key KEY", command)
		}
		return "ops." + command, map[string]any{"run_id": fs.Arg(0), "expected_version": *version, "request_key": *key}, nil
	case "metrics":
		fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
		project := fs.String("project", "", "scope metrics to a project id")
		if err := fs.Parse(args); err != nil {
			return "", nil, err
		}
		if fs.NArg() != 0 {
			return "", nil, fmt.Errorf("usage: sift metrics [--project ID]")
		}
		return "ops.metrics", map[string]any{"project_id": nullableStringCLI(*project)}, nil
	case "timeline":
		fs := flag.NewFlagSet("timeline", flag.ContinueOnError)
		run := fs.String("run", "", "filter timeline to a run id")
		project := fs.String("project", "", "filter timeline to a project id")
		eventType := fs.String("type", "", "filter by event type")
		afterSeq := fs.Int64("after-seq", 0, "keyset pagination cursor (seq; may be passed alone)")
		afterMS := fs.Int64("after-ms", 0, "explicit occurred_at_ms cursor half (optional; the server resolves it from --after-seq when omitted)")
		limit := fs.Int("limit", 100, "max events (1..1000)")
		if err := fs.Parse(args); err != nil {
			return "", nil, err
		}
		if fs.NArg() != 0 || *limit < 1 || *limit > 1000 || *afterSeq < 0 || *afterMS < 0 {
			return "", nil, fmt.Errorf("usage: sift timeline [--run ID] [--project ID] [--type T] [--limit N] [--after-seq N [--after-ms MS]]")
		}
		// --after-ms is optional: legacy callers page with --after-seq alone, and
		// the server resolves the seq's occurred_at_ms before the keyset (B3).
		// Only non-zero cursor halves are sent, so a lone --after-seq yields the
		// legacy param set without after_occurred_at_ms.
		params := map[string]any{"run_id": nullableStringCLI(*run), "project_id": nullableStringCLI(*project), "type": nullableStringCLI(*eventType), "limit": *limit}
		if *afterSeq > 0 {
			params["after_seq"] = *afterSeq
		}
		if *afterMS > 0 {
			params["after_occurred_at_ms"] = *afterMS
		}
		return "ops.timeline", params, nil
	default:
		return "", nil, fmt.Errorf("unknown command %q", command)
	}
}

type attachResponse struct {
	RunID       string `json:"run_id"`
	AttemptNo   int    `json:"attempt_no"`
	Generation  int    `json:"generation"`
	Backend     string `json:"backend"`
	SessionName string `json:"session_name"`
}

// runAttach resolves the read-only tmux session and attaches. With --json the
// raw RPC envelope is printed instead (scripting surface); the default is the
// interactive attach with a one-line humanized hint before tmux takes over the
// terminal. The daemon result is still validated fail-closed before any exec.
func runAttach(response controlplane.Response, home config.Home, stdout, stderr io.Writer, jsonOutput bool) int {
	if jsonOutput {
		if err := printJSON(stdout, response); err != nil {
			report(stderr, err)
			return 1
		}
		if !response.OK {
			return 1
		}
		return 0
	}
	if !response.OK || response.ProtocolMajor != controlplane.ProtocolMajor || response.ProtocolMinor < 0 || response.ProtocolMinor > controlplane.ProtocolMinor || !version.IsValidSemver(response.ServerVersion) {
		report(stderr, fmt.Errorf("invalid daemon response for attach"))
		return 1
	}
	body, err := json.Marshal(response.Result)
	if err != nil {
		report(stderr, fmt.Errorf("invalid attach result: %w", err))
		return 1
	}
	var result attachResponse
	if err := schema.Decode(body, &result, schema.Closed); err != nil || result.RunID == "" || result.AttemptNo < 1 || result.Generation < 1 || result.Backend != "tmux" || !validAttachSessionName(result.SessionName) {
		report(stderr, fmt.Errorf("invalid daemon attach result"))
		return 1
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		report(stderr, fmt.Errorf("tmux 不可用：请先安装 tmux（brew install tmux 或 apt install tmux）"))
		return 1
	}
	fmt.Fprintf(stdout, "✓ 正在只读连接运行 %s（tmux 会话 %s；按 Ctrl-b d 分离退出）\n", result.RunID, result.SessionName)
	socket := runtime.TmuxSocketPath(filepath.Join(home.Path, "tmux.sock"))
	cmd := exec.Command(tmux, "-f", "/dev/null", "-S", socket, "attach-session", "-r", "-t", "="+result.SessionName)
	cmd.Env = runtime.TmuxClientEnvironment()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}

func validAttachSessionName(name string) bool {
	if len(name) != len("sift-")+64 || name[:len("sift-")] != "sift-" {
		return false
	}
	for _, c := range name[len("sift-"):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
func report(w io.Writer, err error) { fmt.Fprintln(w, "sift:", err) }
func overview(stdout, stderr io.Writer) int {
	home, err := config.ResolveHome()
	if err != nil {
		report(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Sift %s\n\n", version.Release)
	configured := "否"
	if _, err := os.Stat(config.ConfigPath(home)); err == nil {
		configured = "是"
	}
	fmt.Fprintf(stdout, "配置文件：%s（%s）\n", config.ConfigPath(home), configured)
	if configured == "否" {
		fmt.Fprintln(stdout, "下一步：运行 sift doctor --offline 检查环境")
	} else {
		fmt.Fprintln(stdout, "下一步：运行 sift daemon 启动服务，或 sift ps 查看运行")
	}
	fmt.Fprintln(stdout, "\n运行 sift help 查看全部命令。")
	return 0
}

func commandHelp(command string, stdout, stderr io.Writer) int {
	if command == "" {
		fmt.Fprintln(stdout, "Sift 命令参考\n\n基础命令：\n  daemon               启动本地守护进程\n  doctor               检查本地环境\n\n查询命令：\n  ps                   查看运行\n  logs <run-id>        查看运行日志\n  timeline             查看事件时间线\n  metrics              查看运行指标\n\n运行控制：\n  kill <run-id>        停止运行\n  retry <run-id>       重试运行\n  report <kind>        提交报告\n\n用法：sift <命令> [选项]\n示例：sift doctor --offline；sift ps")
		return 0
	}
	entries := map[string][3]string{
		"doctor":          {"检查本地环境并报告问题", "sift doctor [--offline] [--json]", "sift doctor --offline"},
		"ps":              {"查看运行中的任务", "sift ps [--json]", "sift ps"},
		"daemon":          {"启动本地守护进程", "sift daemon", "sift daemon"},
		"logs":            {"查看指定运行的日志", "sift logs <run-id> [--json]", "sift logs run-123"},
		"timeline":        {"查看事件时间线", "sift timeline [--json]", "sift timeline --limit 20"},
		"metrics":         {"查看运行指标", "sift metrics [--project ID] [--json]", "sift metrics --project demo"},
		"worktree":        {"查看运行对应的工作树", "sift worktree <run-id> [--json]", "sift worktree run-123"},
		"attach":          {"只读连接到运行会话", "sift attach <run-id> [--json]", "sift attach run-123"},
		"kill":            {"停止指定运行", "sift kill <run-id> --expected-version N --request-key KEY [--json]", "sift kill run-123 --expected-version 2 --request-key stop-1"},
		"retry":           {"重试指定运行", "sift retry <run-id> --expected-version N --request-key KEY [--json]", "sift retry run-123 --expected-version 2 --request-key retry-1"},
		"report":          {"向运行提交报告", "sift report <kind> --key KEY --payload JSON [--json]", "sift report review --key run-123 --payload '{}'"},
		"hooks-bootstrap": {"为项目安装 Git hooks", "sift hooks-bootstrap <project-id>", "sift hooks-bootstrap project-1"},
		"install":         {"安装 Sift 发布包", "sift install <archive.tar.gz>", "sift install sift.tar.gz"},
		"service":         {"管理后台服务", "sift service <install|uninstall|start|stop|restart|reload|status>", "sift service status"},
	}
	entry, ok := entries[command]
	if !ok {
		report(stderr, fmt.Errorf("未知命令 %q；运行 sift help 查看可用命令", command))
		return 2
	}
	fmt.Fprintf(stdout, "sift %s\n\n%s\n\n用法：%s\n示例：%s\n", command, entry[0], entry[1], entry[2])
	return 0
}

// runInstall installs a release archive into the version-directory layout
// (specs/release.md §3). The archive is the per-combo tarball produced by the
// goreleaser pipeline: it must carry manifest.json plus both release binaries
// for the current platform.
func runInstall(args []string, home config.Home, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		report(stderr, fmt.Errorf("usage: sift install <release-archive.tar.gz>"))
		return 2
	}
	installed, err := install.Install(home.Path, args[0])
	if err != nil {
		report(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "installed sift %s to %s; current -> %s\n", installed, filepath.Join(home.Path, "bin", installed), installed)
	return 0
}

// nullableStringCLI emits nil for an empty string so the RPC param set stays
// exactly the closed keys the server validates.
func nullableStringCLI(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// runService drives the user-level hosting units (WBS M8 §8.2). It renders the
// platform unit (launchd user agent / systemd user unit), writes it atomically,
// and runs the matching platform command. On a host without a supervisor it
// reports the foreground fallback rather than failing (DESIGN §11).
func runService(args []string, home config.Home, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		report(stderr, fmt.Errorf("usage: sift service <install|uninstall|start|stop|restart|reload|status>"))
		return 2
	}
	action, err := hosting.ActionFromString(args[0])
	if err != nil {
		report(stderr, err)
		return 2
	}
	spec, err := hosting.NewSpec(home.Path)
	if err != nil {
		report(stderr, err)
		return 1
	}
	plan, err := spec.Plan(action)
	if err != nil {
		report(stderr, err)
		return 1
	}
	migratedLegacy := false
	if spec.Backend == hosting.BackendLaunchd && (action == hosting.ActionInstall || action == hosting.ActionRestart || action == hosting.ActionReload) {
		migratedLegacy, err = migrateLegacyLaunchd(stdout)
		if err != nil {
			report(stderr, err)
			return 1
		}
	}
	// The documented upgrade path is install-archive -> restart. If the old
	// agent was the only loaded unit, migration removed its plist; load the new
	// plist before kickstart so restart reaches the existing daemon.
	if migratedLegacy && (action == hosting.ActionRestart || action == hosting.ActionReload) {
		if _, statErr := os.Stat(spec.UnitPath); os.IsNotExist(statErr) {
			installPlan, planErr := spec.Plan(hosting.ActionInstall)
			if planErr != nil {
				report(stderr, planErr)
				return 1
			}
			if err := hosting.Write(installPlan); err != nil {
				report(stderr, err)
				return 1
			}
			if _, err := hosting.Exec(installPlan); err != nil && !errors.Is(err, hosting.ErrNoBackend) {
				report(stderr, err)
				return 1
			}
		}
	}
	installed := false
	if action == hosting.ActionStatus && spec.Backend != hosting.BackendForeground {
		_, err := os.Stat(spec.UnitPath)
		installed = err == nil
		if os.IsNotExist(err) {
			renderServiceStatus(stdout, spec, "", false)
			return 0
		}
		if err != nil {
			report(stderr, fmt.Errorf("stat service unit %s: %w", spec.UnitPath, err))
			return 1
		}
	}
	// launchctl bootout must happen before removing the plist. A stopped agent
	// returns exit 3 / "No such process", which is already the desired state.
	if action != hosting.ActionUninstall {
		if err := hosting.Write(plan); err != nil {
			report(stderr, err)
			return 1
		}
	}
	out, execErr := hosting.Exec(plan)
	if (action == hosting.ActionUninstall || action == hosting.ActionStop) && hosting.IsAlreadyUnloaded(execErr) {
		// "No such process" is the successful idempotent case; do not render
		// launchctl's diagnostic as though the CLI had failed.
		out, execErr = nil, nil
	}
	if action == hosting.ActionUninstall && (execErr == nil || errors.Is(execErr, hosting.ErrNoBackend)) {
		if err := hosting.Write(plan); err != nil {
			report(stderr, err)
			return 1
		}
	}
	if execErr == nil && plan.WriteFile != "" {
		if plan.Content != nil {
			fmt.Fprintf(stdout, "wrote %s unit: %s\n", spec.Backend, plan.WriteFile)
		} else {
			fmt.Fprintf(stdout, "removed %s unit: %s\n", spec.Backend, plan.WriteFile)
		}
	}
	switch {
	case execErr == nil:
		if len(out) > 0 && action != hosting.ActionStatus {
			_, _ = stdout.Write(out)
		}
		if action == hosting.ActionStatus {
			renderServiceStatus(stdout, spec, string(out), installed)
		} else {
			fmt.Fprintf(stdout, "%s: %s\n", spec.Backend, plan.Summary)
			if action == hosting.ActionReload {
				fmt.Fprintln(stdout, "reload 当前等价于 restart（热重载 SIGHUP 未实现，留后续）")
			}
		}
		return 0
	case errors.Is(execErr, hosting.ErrNoBackend):
		// No supervisor: the foreground hint is the supported path, not an
		// error. Print it and exit 0 so `sift service install` is portable.
		printForegroundReport(stdout, plan)
		return 0
	case action == hosting.ActionStatus:
		// Both launchctl list and systemctl status use non-zero exits for a
		// stopped unit. The retained unit file still means installed.
		renderServiceStatus(stdout, spec, string(out), installed)
		return 0
	default:
		report(stderr, execErr)
		return 1
	}
}

// migrateLegacyLaunchd removes the v0.1.0 label before installing the current
// one. Without this, both labels can supervise daemons that contend for the
// same SIFT_HOME lock after an upgrade.
func migrateLegacyLaunchd(stdout io.Writer) (bool, error) {
	oldUnit, err := hosting.LegacyLaunchdUnitPath()
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(oldUnit)
	if statErr != nil && !os.IsNotExist(statErr) {
		return false, fmt.Errorf("stat legacy launchd unit %s: %w", oldUnit, statErr)
	}
	legacyFile := statErr == nil
	_, probeErr := hosting.Exec(hosting.LegacyLaunchdStatusPlan())
	legacyLoaded := probeErr == nil
	if !legacyFile && !legacyLoaded {
		return false, nil
	}
	if !errors.Is(probeErr, hosting.ErrNoBackend) {
		_, bootoutErr := hosting.Exec(hosting.LegacyLaunchdBootoutPlan())
		if bootoutErr != nil && !hosting.IsAlreadyUnloaded(bootoutErr) {
			return false, bootoutErr
		}
	}
	if err := hosting.Write(hosting.Plan{Action: hosting.ActionUninstall, WriteFile: oldUnit}); err != nil {
		return false, err
	}
	fmt.Fprintf(stdout, "迁移：已移除旧 label %s\n", hosting.LegacyLabel)
	return true, nil
}

// renderServiceStatus keeps platform command output out of the default CLI
// surface while retaining the facts an operator needs: supervisor backend,
// running state, PID when available, and the control socket path.
func renderServiceStatus(stdout io.Writer, spec hosting.Spec, output string, installed bool) {
	state, pid := "未运行", ""
	if !installed {
		state = "未安装"
	} else if serviceRunning(spec.Backend, output) {
		state = "运行中"
		pid = servicePID(spec.Backend, output)
	}
	level := "error"
	if state == "运行中" {
		level = "ok"
	}
	fmt.Fprintf(stdout, "%s %s（%s", render.Status(level), state, spec.Backend)
	if pid != "" {
		fmt.Fprintf(stdout, "，PID %s", pid)
	}
	fmt.Fprintf(stdout, "，socket %s）\n", filepath.Join(spec.HomePath, "siftd.sock"))
}

var launchdPIDPattern = regexp.MustCompile(`(?m)^\s*"PID"\s*=\s*([0-9]+|-)\s*;`)

func serviceRunning(backend hosting.Backend, output string) bool {
	switch backend {
	case hosting.BackendLaunchd:
		return servicePID(backend, output) != ""
	case hosting.BackendSystemd:
		return strings.Contains(output, "Active: active (running)") && servicePID(backend, output) != ""
	default:
		return false
	}
}

func servicePID(backend hosting.Backend, output string) string {
	switch backend {
	case hosting.BackendLaunchd:
		match := launchdPIDPattern.FindStringSubmatch(output)
		if len(match) != 2 || match[1] == "-" {
			return ""
		}
		pid, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil || pid == 0 {
			return ""
		}
		return match[1]
	case hosting.BackendSystemd:
		fields := strings.Fields(output)
		for i, field := range fields {
			if field == "PID:" && i+1 < len(fields) {
				pid, err := strconv.ParseUint(fields[i+1], 10, 32)
				if err == nil && pid > 0 {
					return fields[i+1]
				}
			}
		}
	}
	return ""
}

// printForegroundReport writes the no-supervisor report and uses the socket
// verdict for its status action.
func printForegroundReport(stdout io.Writer, plan hosting.Plan) {
	if plan.Action == hosting.ActionStatus {
		level, state := "error", "未运行"
		if plan.Status == "present" {
			level, state = "ok", "运行中"
		}
		fmt.Fprintf(stdout, "%s %s（foreground，socket %s: %s）\n", render.Status(level), state, plan.SocketPath, plan.Status)
		return
	}
	fmt.Fprintf(stdout, "%s\n  %s\n", plan.Summary, plan.Hint)
	if plan.Action == hosting.ActionReload {
		fmt.Fprintln(stdout, "reload 当前等价于 restart（热重载 SIGHUP 未实现，留后续）")
	}
}

var reportKinds = map[string]bool{"progress": true, "goal": true, "blocker": true, "completed": true}

// runReport implements `sift report`. It reads only SIFT_RUN_DIR/control.json,
// connects only to run.sock, and retries exclusively on the closed not_ready
// policy captured from the first response; every other error fails closed with
// no offline fallback (report.md §1, §2, §4; control-plane.md §8).
func runReport(args []string, home config.Home, stdout, stderr io.Writer) int {
	if len(args) < 1 || !reportKinds[args[0]] {
		report(stderr, fmt.Errorf("usage: sift report <progress|goal|blocker|completed> --key KEY --payload JSON"))
		return 2
	}
	kind := args[0]
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	key := fs.String("key", "", "32-char lowercase-hex report key")
	payload := fs.String("payload", "", "closed JSON payload object")
	jsonFlag := fs.Bool("json", false, "emit the raw JSON envelope")
	if err := fs.Parse(args[1:]); err != nil {
		report(stderr, err)
		return 2
	}
	jsonOutput := os.Getenv("SIFT_JSON") == "1" || *jsonFlag
	if *key == "" || *payload == "" {
		report(stderr, fmt.Errorf("usage: sift report <progress|goal|blocker|completed> --key KEY --payload JSON"))
		return 2
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*payload), &p); err != nil || p == nil {
		report(stderr, fmt.Errorf("payload must be a JSON object"))
		return 2
	}
	control, err := controlplane.ReadControlFile(os.Getenv("SIFT_RUN_DIR"))
	if err != nil {
		report(stderr, err)
		return 1
	}
	params := map[string]any{"run_id": control.RunID, "attempt_no": control.AttemptNo, "generation": control.Generation, "report_key": *key, "kind": kind, "payload": p}
	auth := controlplane.Auth{Kind: "run_token", Token: control.RunToken}
	var delays []int
	var policyBytes []byte
	for attempt := 0; ; attempt++ {
		resp, err := controlplane.RunReportRequest(home, auth, params)
		if err != nil {
			report(stderr, fmt.Errorf("daemon unavailable: %w", err))
			return 1
		}
		if resp.OK {
			if jsonOutput {
				if err := printJSON(stdout, resp); err != nil {
					report(stderr, err)
					return 1
				}
			} else {
				renderReport(stdout, kind, resp.Result)
			}
			return 0
		}
		if resp.Error.Code != "not_ready" {
			if jsonOutput {
				if err := printJSON(stdout, resp); err != nil {
					report(stderr, err)
				}
			} else {
				renderReportError(stdout, resp)
			}
			return 1
		}
		raw, ok := resp.Error.Details["retry_policy"]
		if !ok {
			report(stderr, fmt.Errorf("not_ready response omitted retry_policy"))
			return 1
		}
		rawBytes, mErr := json.Marshal(raw)
		if mErr != nil {
			report(stderr, fmt.Errorf("not_ready retry_policy is malformed"))
			return 1
		}
		if attempt == 0 {
			delays, err = decodeReportDelays(rawBytes)
			if err != nil {
				report(stderr, err)
				return 1
			}
			policyBytes = rawBytes
		} else if !bytes.Equal(rawBytes, policyBytes) {
			report(stderr, fmt.Errorf("not_ready retry_policy drifted during retry"))
			return 1
		}
		if attempt >= len(delays) {
			report(stderr, fmt.Errorf("report timed out waiting for attempt to become running"))
			return 1
		}
		time.Sleep(time.Duration(delays[attempt]) * time.Millisecond)
	}
}

// decodeReportDelays validates the closed retry_policy and computes its delay
// sequence. The CLI never guesses a default, rounds a value, or reads
// config.yaml (report.md §4, control-plane.md §8).
func decodeReportDelays(raw []byte) ([]int, error) {
	var policy controlplane.RetryPolicy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policy); err != nil {
		return nil, fmt.Errorf("not_ready retry_policy is not closed")
	}
	return policy.BackoffDelays()
}

// renormalize round-trips an RPC result through JSON so every renderer reads
// the same wire shapes (numbers as float64) whether the caller passes a typed
// struct, a decoded map, or a nil/partial value.
func renormalize(value any, out any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// failureContext names the missing subject for the not_found humanization.
func failureContext(command string, args []string) string {
	runID := ""
	if len(args) > 0 {
		runID = args[0]
	}
	switch command {
	case "logs":
		return fmt.Sprintf("运行 %s 的日志不存在（可能已被清理）", runID)
	case "worktree":
		return fmt.Sprintf("运行 %s 没有可用的工作树", runID)
	case "kill", "retry":
		return fmt.Sprintf("运行 %s 不存在", runID)
	default:
		return "请求的数据不存在"
	}
}

// renderError turns a failed RPC into an actionable Chinese line. The exit
// status is unchanged; only the presentation differs from the raw envelope.
func renderError(w io.Writer, response controlplane.Response, context string) {
	code, message := "", ""
	if response.Error != nil {
		code, message = response.Error.Code, response.Error.Message
	}
	switch code {
	case "not_found":
		fmt.Fprintf(w, "✗ 未找到：%s\n", context)
	case "stale":
		fmt.Fprintln(w, "✗ 运行或尝试已变化（stale）：请先运行 sift ps 查看最新状态，再重试。")
	case "unauthorized":
		fmt.Fprintln(w, "✗ 凭据被拒绝（unauthorized）：请检查守护进程是否正常运行（sift doctor）。")
	case "unavailable":
		fmt.Fprintf(w, "✗ 暂不可用（unavailable）：%s\n", message)
	case "storage":
		fmt.Fprintf(w, "✗ 数据读取失败（storage）：%s\n", message)
	case "conflict":
		fmt.Fprintf(w, "✗ 操作被拒绝（conflict）：%s\n", message)
	case "invalid_request":
		fmt.Fprintf(w, "✗ 请求无效（invalid_request）：%s\n", message)
	default:
		if message == "" {
			message = code
		}
		fmt.Fprintf(w, "✗ 操作失败（%s）：%s\n", code, message)
	}
}

// runStatusLabel maps the Run status enum to a Chinese label with an icon.
func runStatusLabel(s string) string {
	switch s {
	case "queued":
		return "ℹ 排队"
	case "running":
		return "✓ 运行中"
	case "waiting_human":
		return "⚠ 等待人工"
	case "done":
		return "✓ 完成"
	case "failed":
		return "✗ 失败"
	}
	return s
}

// phaseLabel maps the attempt phase enum to Chinese.
func phaseLabel(p string) string {
	switch p {
	case "pending":
		return "等待"
	case "starting":
		return "启动中"
	case "spawning":
		return "派生中"
	case "running":
		return "运行中"
	case "finished":
		return "已完成"
	case "orphaned":
		return "已失联"
	}
	return p
}

// severityLabel maps the attention severity enum to Chinese.
func severityLabel(s string) string {
	switch s {
	case "low":
		return "低"
	case "normal":
		return "普通"
	case "high":
		return "高"
	}
	return s
}

// sourceLabel maps the event source enum to Chinese.
func sourceLabel(s string) string {
	switch s {
	case "system":
		return "系统"
	case "forge":
		return "Forge"
	case "operator":
		return "运维"
	case "agent":
		return "Agent"
	case "recovery":
		return "恢复"
	}
	return s
}

// eventTypeLabels covers the append-only event types emitted by the storage
// layer (storage.md §7.1). Unknown or future types fall back to the raw name.
var eventTypeLabels = map[string]string{
	"intake.trigger_observed":            "触发已观测",
	"intake.issue_observed":              "Issue 已观测",
	"intake.decision":                    "接纳决策",
	"intake.reply_accepted":              "回复已接纳",
	"intake.reply_ignored":               "回复已忽略",
	"run.assigned":                       "运行已分配",
	"run.transitioned":                   "运行状态迁移",
	"run.transition_rejected":            "状态迁移被拒",
	"attempt.completed":                  "尝试完成",
	"attempt.acquired":                   "尝试接管",
	"attempt.spawn_permitted":            "尝试派生已放行",
	"attempt.race_resolved":              "尝试竞争已解决",
	"report.progress":                    "进度报告",
	"report.goal":                        "目标报告",
	"report.blocker":                     "阻塞报告",
	"report.completed":                   "完成报告",
	"interrupt.emitted":                  "中断已发出",
	"interrupt.dispatched":               "中断已分派",
	"interrupt.escalated":                "中断已升级",
	"interrupt.expired":                  "中断已过期",
	"interrupt.expired_auto_reject":      "中断过期自动拒绝",
	"forge_change_merged":                "Forge 变更已合并",
	"change.merged_observed":             "合并已观测",
	"command.event":                      "命令事件",
	"command.ignored":                    "命令已忽略",
	"gate.reevaluation.conflict":         "门禁复审冲突",
	"gate.reevaluation.failed":           "门禁复审失败",
	"security.report_quota_exhausted":    "报告配额耗尽",
	"security.report_interrupt_rejected": "报告中断被拒",
	"security.handoff_rejected":          "交接被拒",
	"termination.absence_confirmed":      "终止已确认",
	"backend.session_diagnostic":         "后端会话诊断",
	"project.isolated":                   "项目已隔离",
	"project.capability_checked":         "能力检查",
	"hooks_baseline_missing":             "Hooks 基线缺失",
	"hooks_baseline_activation_missing":  "Hooks 基线激活缺失",
	"hooks_baseline_bootstrapped":        "Hooks 基线已引导",
	"hooks_drift_detected":               "Hooks 漂移已检测",
}

func eventTypeLabel(t string) string {
	if label, ok := eventTypeLabels[t]; ok {
		return label
	}
	if strings.HasPrefix(t, "gate.reevaluation.") {
		return "门禁复审"
	}
	return t
}

// renderPS humanizes the ops.ps result: a run table (run-id / project /
// status / phase / version / interrupt / outbox) and today's remaining
// attention quota per severity. An empty list gets a friendly hint.
func renderPS(w io.Writer, value any) {
	var result struct {
		Runs []struct {
			RunID     string  `json:"run_id"`
			ProjectID string  `json:"project_id"`
			Status    string  `json:"status"`
			Version   float64 `json:"version"`
			Attempt   *struct {
				AttemptNo float64 `json:"attempt_no"`
				AgentID   string  `json:"agent_id"`
				Phase     string  `json:"phase"`
			} `json:"attempt"`
			OpenInterruptCount float64 `json:"open_interrupt_count"`
			PendingOutboxCount float64 `json:"pending_outbox_count"`
		} `json:"runs"`
		AttentionRemaining map[string]float64 `json:"attention_remaining"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取运行列表")
		return
	}
	fmt.Fprintf(w, "运行列表（共 %d 个）\n", len(result.Runs))
	if len(result.Runs) == 0 {
		fmt.Fprintln(w, "  暂无运行：触发 Issue 后，运行会自动出现在这里。也可以运行 sift doctor --offline 检查环境。")
	} else {
		rows := make([][]string, 0, len(result.Runs))
		for _, r := range result.Runs {
			phase, attempt := "-", "-"
			if r.Attempt != nil {
				phase = phaseLabel(r.Attempt.Phase)
				attempt = fmt.Sprintf("第 %d 次", int(r.Attempt.AttemptNo))
			}
			agent := "-"
			if r.Attempt != nil && r.Attempt.AgentID != "" {
				agent = r.Attempt.AgentID
			}
			rows = append(rows, []string{
				r.RunID, r.ProjectID, agent, runStatusLabel(r.Status), phase, attempt,
				fmt.Sprintf("%d", int(r.Version)),
				fmt.Sprintf("%d", int(r.OpenInterruptCount)),
				fmt.Sprintf("%d", int(r.PendingOutboxCount)),
			})
		}
		fmt.Fprint(w, render.Table([]string{"运行 ID", "项目", "Agent", "状态", "阶段", "尝试", "版本", "中断", "待发"}, rows))
	}
	if len(result.AttentionRemaining) > 0 {
		fmt.Fprintln(w, "今日注意力剩余：")
		for _, sev := range []string{"low", "normal", "high"} {
			fmt.Fprintf(w, "  %s %d", severityLabel(sev), int(result.AttentionRemaining[sev]))
		}
		fmt.Fprintln(w)
		allZero := result.AttentionRemaining["low"] == 0 && result.AttentionRemaining["normal"] == 0 && result.AttentionRemaining["high"] == 0
		if allZero {
			fmt.Fprintln(w, "  （未配置每日注意力配额）")
		}
	}
}

// renderTimeline humanizes the ops.timeline result: newest-first events,
// sectioned by local date, with Chinese event-type labels.
func renderTimeline(w io.Writer, value any) {
	var result struct {
		Events []struct {
			Seq          float64  `json:"Seq"`
			RunID        string   `json:"RunID"`
			Type         string   `json:"Type"`
			Source       string   `json:"Source"`
			Actor        string   `json:"Actor"`
			AttemptNo    *float64 `json:"AttemptNo"`
			OccurredAtMS float64  `json:"OccurredAtMS"`
		} `json:"events"`
		HasMore          bool    `json:"has_more"`
		NextSeq          float64 `json:"next_seq"`
		NextOccurredAtMS float64 `json:"next_occurred_at_ms"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取事件时间线")
		return
	}
	if len(result.Events) == 0 {
		fmt.Fprintln(w, "事件时间线（暂无事件）")
		return
	}
	// Present newest-first by occurrence time; seq provides a stable tie-breaker.
	evs := result.Events
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].OccurredAtMS != evs[j].OccurredAtMS {
			return evs[i].OccurredAtMS > evs[j].OccurredAtMS
		}
		return evs[i].Seq > evs[j].Seq
	})
	fmt.Fprintf(w, "事件时间线（最新在前，共 %d 条）\n", len(evs))
	lastDate := ""
	for _, e := range evs {
		t := time.UnixMilli(int64(e.OccurredAtMS))
		if date := t.Format("2006-01-02"); date != lastDate {
			fmt.Fprintf(w, "── %s ──\n", date)
			lastDate = date
		}
		attempt := ""
		if e.AttemptNo != nil {
			attempt = fmt.Sprintf(" · 尝试 %d", int(*e.AttemptNo))
		}
		actor := ""
		if e.Actor != "" {
			actor = " · " + e.Actor
		}
		fmt.Fprintf(w, "%s  %s  %s%s%s（%s）\n", t.Format("15:04:05"), eventTypeLabel(e.Type), e.RunID, attempt, actor, sourceLabel(e.Source))
	}
	if result.HasMore {
		fmt.Fprintf(w, "（还有更多事件：运行 sift timeline --after-seq %d --after-ms %d 查看下一页）\n", int64(result.NextSeq), int64(result.NextOccurredAtMS))
	}
}

// renderLogs humanizes the ops.logs result: the attempt header, the decoded
// log bytes (control characters escaped), and an honest truncation hint when
// the bounded read hit the byte limit before EOF.
func renderLogs(w io.Writer, runID string, value any) {
	var result struct {
		AttemptNo  float64 `json:"attempt_no"`
		EOF        bool    `json:"eof"`
		DataBase64 string  `json:"data_base64"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取运行日志")
		return
	}
	data, err := base64.StdEncoding.DecodeString(result.DataBase64)
	if err != nil {
		fmt.Fprintln(w, "✗ 日志数据损坏（base64 解码失败）")
		return
	}
	fmt.Fprintf(w, "运行 %s 日志（第 %d 次尝试）\n", runID, int(result.AttemptNo))
	_, _ = w.Write(escapeLogBytes(data))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(w)
	}
	if !result.EOF {
		fmt.Fprintf(w, "（日志量较大，已显示 %d 字节；后续内容未显示）\n", len(data))
	}
}

// escapeLogBytes makes raw agent.log bytes printable: line/tab/CR survive,
// other control bytes become visible \xNN escapes (control-plane.md §6.2).
func escapeLogBytes(data []byte) []byte {
	var b bytes.Buffer
	for _, c := range data {
		switch {
		case c == '\n' || c == '\t' || c == '\r' || c >= 0x20 && c != 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.Bytes()
}

// renderMetrics humanizes the ops.metrics result in sections: attention
// quota consumption, the ratio series with honest coverage notes, LLM token
// usage, and the trigger→started latency distribution.
func renderMetrics(w io.Writer, value any) {
	type ratio struct {
		Numerator   float64 `json:"numerator"`
		Denominator float64 `json:"denominator"`
		Rate        float64 `json:"rate"`
		Coverage    string  `json:"coverage"`
	}
	var result struct {
		Metrics struct {
			Scope                      string `json:"scope"`
			WeightedAttentionPerChange struct {
				WeightedMinutes float64 `json:"weighted_minutes"`
				MergedChanges   float64 `json:"merged_changes"`
				PerMergedChange float64 `json:"per_merged_change"`
				Coverage        string  `json:"coverage"`
			} `json:"weighted_attention_per_merged_change"`
			FalseReleaseRate          ratio `json:"false_release_rate"`
			GateBypassRate            ratio `json:"gate_bypass_rate"`
			GateMissRate              ratio `json:"gate_miss_rate"`
			GateFalseBlockRate        ratio `json:"gate_false_block_rate"`
			HITLRate                  ratio `json:"hitl_rate"`
			DispatchAccuracy          ratio `json:"dispatch_accuracy"`
			AttentionQuotaConsumption []struct {
				Severity string  `json:"severity"`
				Consumed float64 `json:"consumed"`
				Limit    float64 `json:"limit"`
				Rate     float64 `json:"rate"`
			} `json:"attention_quota_consumption"`
			ForgeAPIQuotaConsumption []struct {
				ProjectID string  `json:"project_id"`
				Consumed  float64 `json:"consumed"`
				Limit     float64 `json:"limit"`
				Unit      string  `json:"unit"`
			} `json:"forge_api_quota_consumption"`
			LLMCostPerMergedChange struct {
				PerMergedChangeInput  float64 `json:"per_merged_change_input_tokens"`
				PerMergedChangeOutput float64 `json:"per_merged_change_output_tokens"`
				MergedChanges         float64 `json:"merged_changes"`
				Coverage              string  `json:"coverage"`
			} `json:"llm_cost_per_merged_change"`
		} `json:"metrics"`
		Latency struct {
			Count    float64 `json:"count"`
			MinMS    float64 `json:"min_ms"`
			P50MS    float64 `json:"p50_ms"`
			P90MS    float64 `json:"p90_ms"`
			MaxMS    float64 `json:"max_ms"`
			Coverage string  `json:"coverage"`
		} `json:"trigger_started_latency"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取指标")
		return
	}
	m := &result.Metrics
	scope := "全局"
	if m.Scope != "" && m.Scope != "global" {
		scope = "项目 " + m.Scope
	}
	fmt.Fprintf(w, "指标（%s）\n", scope)

	if len(m.AttentionQuotaConsumption) == 0 {
		fmt.Fprintln(w, "注意力配额：暂无已记录的消耗")
	} else {
		fmt.Fprintln(w, "注意力配额（今日已用 / 上限）：")
		for _, q := range m.AttentionQuotaConsumption {
			fmt.Fprintf(w, "  %s：%d / %d（%.1f%%）\n", severityLabel(q.Severity), int(q.Consumed), int(q.Limit), q.Rate*100)
		}
	}

	if len(m.ForgeAPIQuotaConsumption) == 0 {
		fmt.Fprintln(w, "Forge API 用量：暂无项目")
	} else {
		fmt.Fprintln(w, "Forge API 用量（本小时已用 / 上限）：")
		for _, q := range m.ForgeAPIQuotaConsumption {
			fmt.Fprintf(w, "  项目 %s：%d / %d %s\n", q.ProjectID, int(q.Consumed), int(q.Limit), q.Unit)
		}
	}

	wMetrics := m.WeightedAttentionPerChange
	fmt.Fprintf(w, "每合并变更注意力：%.1f 分钟（%d 个合并变更）\n", wMetrics.PerMergedChange, int(wMetrics.MergedChanges))
	if wMetrics.Coverage != "" {
		fmt.Fprintf(w, "  覆盖说明：%s\n", wMetrics.Coverage)
	}

	ratioLine := func(label string, r ratio) {
		fmt.Fprintf(w, "%s：%.1f%%（%d/%d）\n", label, r.Rate*100, int(r.Numerator), int(r.Denominator))
		if r.Coverage != "" {
			fmt.Fprintf(w, "  覆盖说明：%s\n", r.Coverage)
		}
	}
	ratioLine("误放行率", m.FalseReleaseRate)
	ratioLine("门禁绕过率", m.GateBypassRate)
	ratioLine("门禁漏检率", m.GateMissRate)
	ratioLine("门禁误拦率", m.GateFalseBlockRate)
	ratioLine("人工介入率", m.HITLRate)
	ratioLine("分派准确率", m.DispatchAccuracy)

	llm := m.LLMCostPerMergedChange
	fmt.Fprintf(w, "LLM 用量（每合并变更）：输入 %d / 输出 %d tokens（%d 个合并变更）\n", int(llm.PerMergedChangeInput), int(llm.PerMergedChangeOutput), int(llm.MergedChanges))
	if llm.Coverage != "" {
		fmt.Fprintf(w, "  覆盖说明：%s\n", llm.Coverage)
	}

	l := &result.Latency
	if l.Count == 0 {
		fmt.Fprintln(w, "触发→启动延迟：暂无样本")
	} else {
		fmt.Fprintf(w, "触发→启动延迟：%d 个样本 · 最小 %s · P50 %s · P90 %s · 最大 %s\n",
			int(l.Count), render.Duration(int64(l.MinMS)), render.Duration(int64(l.P50MS)), render.Duration(int64(l.P90MS)), render.Duration(int64(l.MaxMS)))
	}
	if l.Coverage != "" {
		fmt.Fprintf(w, "  覆盖说明：%s\n", l.Coverage)
	}
}

// renderWorktree humanizes the ops.worktree result (spec §6.2). The current
// daemon always answers not_found, which the humanized error path explains.
func renderWorktree(w io.Writer, runID string, value any) {
	var result struct {
		RunID               string  `json:"run_id"`
		AttemptNo           float64 `json:"attempt_no"`
		Path                string  `json:"path"`
		Exists              bool    `json:"exists"`
		IsolationState      string  `json:"isolation_state"`
		ReadOnlyRecommended bool    `json:"read_only_recommended"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取工作树信息")
		return
	}
	if result.Path == "" {
		fmt.Fprintf(w, "✗ 运行 %s 没有可用的工作树\n", runID)
		return
	}
	fmt.Fprintf(w, "运行 %s 的工作树（第 %d 次尝试）\n", result.RunID, int(result.AttemptNo))
	state := "存在"
	if !result.Exists {
		state = "不存在"
	}
	fmt.Fprintf(w, "路径：%s（%s）\n", result.Path, state)
	fmt.Fprintf(w, "隔离状态：%s\n", result.IsolationState)
	if result.ReadOnlyRecommended {
		fmt.Fprintln(w, "建议只读：是（请勿在工作树内直接修改）")
	}
}

// renderKillRetry humanizes the ops.kill/ops.retry success result. Failures
// are rendered by renderError with the exit status unchanged.
func renderKillRetry(w io.Writer, verb, runID string, value any) {
	var result struct {
		Accepted    bool   `json:"accepted"`
		State       string `json:"state"`
		Disposition string `json:"disposition"`
		ProbeID     string `json:"probe_id"`
		Message     string `json:"message"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取操作结果")
		return
	}
	verbLabel := "停止"
	if verb == "retry" {
		verbLabel = "重试"
	}
	if result.Accepted || result.Disposition == "accepted" {
		fmt.Fprintf(w, "✓ 已请求%s运行 %s", verbLabel, runID)
		state := result.State
		if state == "" {
			state = result.Message
		}
		if state != "" {
			fmt.Fprintf(w, "（%s）", state)
		}
		if result.ProbeID != "" {
			fmt.Fprintf(w, "，验证标识 %s", result.ProbeID)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  已启动受控终止流程，等待执行体消失证据；用 sift ps 查看最新状态。")
		return
	}
	fmt.Fprintf(w, "✗ 未能%s运行 %s\n", verbLabel, runID)
	if result.Message != "" {
		fmt.Fprintf(w, "  原因：%s\n", result.Message)
	}
}

// reportKindLabel maps the closed report kind to Chinese.
func reportKindLabel(kind string) string {
	switch kind {
	case "progress":
		return "进度"
	case "goal":
		return "目标"
	case "blocker":
		return "阻塞"
	case "completed":
		return "完成"
	}
	return kind
}

// renderReport humanizes the report.submit success result.
func renderReport(w io.Writer, kind string, value any) {
	var result struct {
		Disposition string `json:"disposition"`
		ReceiptID   string `json:"receipt_id"`
		EventID     string `json:"event_id"`
	}
	if err := renormalize(value, &result); err != nil {
		fmt.Fprintln(w, "✗ 无法读取报告结果")
		return
	}
	fmt.Fprintf(w, "✓ 报告已提交（%s）：receipt %s\n", reportKindLabel(kind), result.ReceiptID)
	if result.EventID != "" {
		fmt.Fprintf(w, "  事件 %s 已记录\n", result.EventID)
	}
}

// renderReportError turns a permanent report.submit failure into an actionable
// Chinese line. The exit status is unchanged.
func renderReportError(w io.Writer, response controlplane.Response) {
	code, message, detailCode := "", "", ""
	if response.Error != nil {
		code, message = response.Error.Code, response.Error.Message
		if v, ok := response.Error.Details["code"].(string); ok {
			detailCode = v
		}
	}
	switch {
	case detailCode == "report_interrupt_quota_exhausted":
		fmt.Fprintln(w, "✗ 报告被拒绝：报告中断配额已用尽（report_interrupt_quota_exhausted）")
	case code == "unauthorized":
		fmt.Fprintln(w, "✗ 报告被拒绝：凭据无效（unauthorized）")
	case code == "stale":
		fmt.Fprintln(w, "✗ 报告被拒绝：运行或尝试已变化（stale），请核对运行状态后重试")
	case code == "conflict":
		fmt.Fprintln(w, "✗ 报告被拒绝：与既有报告冲突或超出频率限制（conflict）")
	case code == "invalid_request":
		fmt.Fprintln(w, "✗ 报告被拒绝：请求参数无效（invalid_request）")
	case code == "internal":
		fmt.Fprintf(w, "✗ 报告提交失败（internal）：%s\n", message)
	default:
		fmt.Fprintf(w, "✗ 报告被拒绝（%s）：%s\n", code, message)
	}
}
