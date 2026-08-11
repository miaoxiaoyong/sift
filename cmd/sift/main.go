// Command sift is the operator CLI and local control-plane daemon.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		return runAttach(response, home, stdout, stderr)
	}
	if command == "doctor" {
		jsonOutput, _, _ := doctorFlags(args[2:])
		return runDoctor(response, stdout, stderr, jsonOutput)
	}
	if err := printJSON(stdout, response); err != nil {
		report(stderr, err)
		return 1
	}
	if !response.OK {
		return 1
	}
	return 0
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
		afterSeq := fs.Int64("after-seq", 0, "keyset pagination cursor (seq)")
		limit := fs.Int("limit", 100, "max events (1..1000)")
		if err := fs.Parse(args); err != nil {
			return "", nil, err
		}
		if fs.NArg() != 0 || *limit < 1 || *limit > 1000 || *afterSeq < 0 {
			return "", nil, fmt.Errorf("usage: sift timeline [--run ID] [--project ID] [--type T] [--after-seq N] [--limit N]")
		}
		params := map[string]any{"run_id": nullableStringCLI(*run), "project_id": nullableStringCLI(*project), "type": nullableStringCLI(*eventType), "after_seq": *afterSeq, "limit": *limit}
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

func runAttach(response controlplane.Response, home config.Home, stdout, stderr io.Writer) int {
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
		report(stderr, fmt.Errorf("tmux unavailable: %w", err))
		return 1
	}
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
		"logs":            {"查看指定运行的日志", "sift logs <run-id>", "sift logs run-123"},
		"timeline":        {"查看事件时间线", "sift timeline", "sift timeline --limit 20"},
		"metrics":         {"查看运行指标", "sift metrics", "sift metrics --project demo"},
		"worktree":        {"查看运行对应的工作树", "sift worktree <run-id>", "sift worktree run-123"},
		"attach":          {"只读连接到运行会话", "sift attach <run-id>", "sift attach run-123"},
		"kill":            {"停止指定运行", "sift kill <run-id> --expected-version N --request-key KEY", "sift kill run-123 --expected-version 2 --request-key stop-1"},
		"retry":           {"重试指定运行", "sift retry <run-id> --expected-version N --request-key KEY", "sift retry run-123 --expected-version 2 --request-key retry-1"},
		"report":          {"向运行提交报告", "sift report <kind> --key KEY --payload JSON", "sift report review --key run-123 --payload '{}'"},
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
	if action == hosting.ActionInstall && spec.Backend == hosting.BackendLaunchd {
		if err := migrateLegacyLaunchd(stdout); err != nil {
			report(stderr, err)
			return 1
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
	if action == hosting.ActionUninstall && hosting.IsAlreadyUnloaded(execErr) {
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
	if len(out) > 0 && action != hosting.ActionStatus {
		stdout.Write(out)
	}
	switch {
	case execErr == nil:
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
func migrateLegacyLaunchd(stdout io.Writer) error {
	oldUnit, err := hosting.LegacyLaunchdUnitPath()
	if err != nil {
		return err
	}
	_, statErr := os.Stat(oldUnit)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat legacy launchd unit %s: %w", oldUnit, statErr)
	}
	legacyFile := statErr == nil
	_, probeErr := hosting.Exec(hosting.LegacyLaunchdStatusPlan())
	legacyLoaded := probeErr == nil
	if !legacyFile && !legacyLoaded {
		return nil
	}
	if !errors.Is(probeErr, hosting.ErrNoBackend) {
		_, bootoutErr := hosting.Exec(hosting.LegacyLaunchdBootoutPlan())
		if bootoutErr != nil && !hosting.IsAlreadyUnloaded(bootoutErr) {
			return bootoutErr
		}
	}
	if err := hosting.Write(hosting.Plan{Action: hosting.ActionUninstall, WriteFile: oldUnit}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "迁移：已移除旧 label %s\n", hosting.LegacyLabel)
	return nil
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
	if err := fs.Parse(args[1:]); err != nil {
		report(stderr, err)
		return 2
	}
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
			if err := printJSON(stdout, resp); err != nil {
				report(stderr, err)
				return 1
			}
			return 0
		}
		if resp.Error.Code != "not_ready" {
			if err := printJSON(stdout, resp); err != nil {
				report(stderr, err)
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
