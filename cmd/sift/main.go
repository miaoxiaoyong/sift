// Command sift is the operator CLI and local control-plane daemon.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/schema"
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
		usage(stderr)
		return 2
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
	if command == "doctor" && len(args) == 3 && args[2] == "--offline" {
		result := controlplane.OfflineDoctor(home)
		return emitDoctor(stdout, stderr, result)
	}
	if command == "report" {
		return runReport(args[2:], home, stdout, stderr)
	}
	method, params, err := request(command, args[2:])
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
	if err := printJSON(stdout, response); err != nil {
		report(stderr, err)
		return 1
	}
	if command == "doctor" {
		if !response.OK {
			return 1
		}
		return doctorExitCode(response.Result)
	}
	if !response.OK {
		return 1
	}
	return 0
}

// emitDoctor prints the offline doctor result and maps its exit_code to the
// process exit status (config.md §7).
func emitDoctor(stdout, stderr io.Writer, result map[string]any) int {
	if err := printJSON(stdout, result); err != nil {
		report(stderr, err)
		return 1
	}
	return doctorExitCode(result)
}

// doctorExitCode extracts the process exit status from a doctor result. The
// doctor computes exit_code as 0 (clean), 1 (warning) or 2 (error); this only
// projects it. The offline result carries a Go int, the online result arrives
// from JSON as a float64. A missing or malformed value defaults to 0, matching
// a healthy result that must always set it.
func doctorExitCode(result any) int {
	m, ok := result.(map[string]any)
	if !ok {
		return 0
	}
	switch code := m["exit_code"].(type) {
	case int:
		return code
	case float64:
		return int(code)
	}
	return 0
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
	if !response.OK || response.ProtocolMajor != controlplane.ProtocolMajor || response.ProtocolMinor > controlplane.ProtocolMinor || response.ServerVersion == "" {
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
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: sift daemon|ps|logs|worktree|metrics|timeline|attach|doctor [--offline]|hooks-bootstrap <project-id>|kill|retry|report <kind> --key KEY --payload JSON")
}

// nullableStringCLI emits nil for an empty string so the RPC param set stays
// exactly the closed keys the server validates.
func nullableStringCLI(s string) any {
	if s == "" {
		return nil
	}
	return s
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
