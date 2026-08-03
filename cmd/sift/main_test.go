package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// freshHome returns a 0700 temp dir suitable for use as SIFT_HOME. It creates
// the directory directly in the OS temp root rather than under t.TempDir's
// per-test subdirectory, so the resolved socket path stays within the Unix
// domain socket length limit for the online test.
func freshHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "sift")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIFT_HOME", home)
	return home
}

// withDatabase provisions a readable sift.db so the doctor's sqlite and
// permission checks do not report errors, leaving only the unavoidable
// unsafe-local warning.
func withDatabase(t *testing.T, home string) {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorExitCode extracts the §7 exit status from every shape the doctor
// result can take: a Go int (offline, direct) and a JSON float64 (online, after
// wire decode), plus the degenerate cases that must default to 0.
func TestHookBootstrapRequestRequiresExplicitProject(t *testing.T) {
	method, params, err := request("hooks-bootstrap", []string{"project"})
	if err != nil || method != "ops.hooks-bootstrap" || params["project_id"] != "project" {
		t.Fatalf("bootstrap request = %q %#v %v", method, params, err)
	}
	if _, _, err := request("hooks-bootstrap", nil); err == nil {
		t.Fatal("missing project bootstrap request succeeded")
	}
}

func TestDoctorExitCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result any
		want   int
	}{
		{"offline int clean", map[string]any{"exit_code": int(0)}, 0},
		{"offline int warning", map[string]any{"exit_code": int(1)}, 1},
		{"offline int error", map[string]any{"exit_code": int(2)}, 2},
		{"online float clean", map[string]any{"exit_code": float64(0)}, 0},
		{"online float warning", map[string]any{"exit_code": float64(1)}, 1},
		{"online float error", map[string]any{"exit_code": float64(2)}, 2},
		{"missing exit_code", map[string]any{"checks": nil}, 0},
		{"malformed exit_code", map[string]any{"exit_code": "2"}, 0},
		{"not a map", []any{"checks"}, 0},
		{"nil", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorExitCode(tc.result); got != tc.want {
				t.Fatalf("doctorExitCode(%v) = %d, want %d", tc.result, got, tc.want)
			}
		})
	}
}

// TestRunDoctorOfflineExitsWithError reproduces the issue #34 baseline: an
// empty SIFT_HOME cannot have a database, so offline doctor must surface the
// sqlite error as a non-zero (2) process exit, not silently exit 0.
func TestRunDoctorOfflineExitsWithError(t *testing.T) {
	freshHome(t) // no sift.db -> sqlite check errors
	var out bytes.Buffer
	code := run([]string{"sift", "doctor", "--offline"}, &out, io.Discard)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; output:\n%s", code, out.String())
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["exit_code"] != float64(2) {
		t.Fatalf("doctor exit_code = %v, want 2", result["exit_code"])
	}
	if result["offline"] != true {
		t.Fatalf("doctor offline = %v, want true", result["offline"])
	}
}

// TestRunDoctorOfflineExitsWithWarning verifies the warning-only path (exit 1):
// with a healthy database the only remaining finding is the always-on
// unsafe-local warning.
func TestRunDoctorOfflineExitsWithWarning(t *testing.T) {
	home := freshHome(t)
	withDatabase(t, home)
	code := run([]string{"sift", "doctor", "--offline"}, &bytes.Buffer{}, io.Discard)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// TestRunDoctorOnlineExitsWithWarning drives the online path end to end: a live
// daemon returns the doctor result in response.Result, and the process must
// exit with the daemon-computed exit_code (1, unsafe-local warning).
func TestRunDoctorOnlineExitsWithWarning(t *testing.T) {
	home := freshHome(t)
	withDatabase(t, home)
	s, err := controlplane.Start(config.Home{Path: home})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))

	var out bytes.Buffer
	code := run([]string{"sift", "doctor"}, &out, io.Discard)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != true {
		t.Fatalf("response ok = %v, want true", response["ok"])
	}
	result, _ := response["result"].(map[string]any)
	if result["exit_code"] != float64(1) {
		t.Fatalf("doctor exit_code = %v, want 1", result["exit_code"])
	}
	if result["offline"] != false {
		t.Fatalf("doctor offline = %v, want false", result["offline"])
	}
}

// TestRunDoctorOnlineExitsOneWhenDaemonUnavailable confirms the daemon-missing
// path still surfaces as a non-zero process exit.
func TestRunDoctorOnlineExitsOneWhenDaemonUnavailable(t *testing.T) {
	freshHome(t) // no daemon, no token, no socket
	var stderr bytes.Buffer
	code := run([]string{"sift", "doctor"}, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("daemon unavailable")) {
		t.Fatalf("stderr = %q, want daemon unavailable message", stderr.String())
	}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("socket %s not created", path)
}

// TestRequestMetricsMaps verifies the metrics command builds the closed ops.metrics
// param set, including the --project scope.
func TestRequestMetricsMaps(t *testing.T) {
	method, params, err := request("metrics", []string{})
	if err != nil || method != "ops.metrics" {
		t.Fatalf("metrics default = %q %v err=%v", method, params, err)
	}
	if _, ok := params["project_id"]; !ok {
		t.Fatalf("metrics params missing project_id: %v", params)
	}
	method, params, err = request("metrics", []string{"--project", "proj-1"})
	if err != nil || params["project_id"] != "proj-1" {
		t.Fatalf("metrics --project = %q %v err=%v", method, params, err)
	}
}

// TestRequestTimelineMaps verifies the timeline command builds the closed
// ops.timeline param set with keyset/type filters.
func TestRequestTimelineMaps(t *testing.T) {
	method, params, err := request("timeline", []string{"--run", "run-1", "--type", "report.progress", "--after-seq", "5", "--limit", "10"})
	if err != nil || method != "ops.timeline" {
		t.Fatalf("timeline = %q %v err=%v", method, params, err)
	}
	if params["run_id"] != "run-1" || params["type"] != "report.progress" || params["after_seq"] != int64(5) || params["limit"] != 10 {
		t.Fatalf("timeline params = %v", params)
	}
}

// startServerWithDB opens a real database under home and starts the operator
// server bound to it, for online metrics/timeline/ps assertions.
func startServerWithDB(t *testing.T, home string) *controlplane.Server {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SeedProjectForTest(context.Background(), "cfg-cli", "proj-cli", 1_700_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(context.Background(), "runCLI", "proj-cli", "cfg-cli", "issue-1", 1_700_000_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(context.Background(), storage.EventCmd{RunID: "runCLI", Type: "report.progress", Source: storage.SourceAgent, PayloadJSON: []byte("{}"), OccurredAtMS: 1_700_000_000_000, RecordedAtMS: 1_700_000_000_000}); err != nil {
		t.Fatal(err)
	}
	s, err := controlplane.Start(config.Home{Path: home}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))
	return s
}

// TestRunMetricsOnline prints the nine-series report over a real daemon.
func TestRunMetricsOnline(t *testing.T) {
	home := freshHome(t)
	startServerWithDB(t, home)
	var out bytes.Buffer
	code := run([]string{"sift", "metrics"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	metrics := result["metrics"].(map[string]any)
	// The north-star and false-release series are present with coverage notes.
	w := metrics["weighted_attention_per_merged_change"].(map[string]any)
	if _, ok := w["coverage"]; !ok {
		t.Fatalf("weighted attention missing coverage: %v", w)
	}
}

// TestRunTimelineOnline prints the persisted event stream over a real daemon.
func TestRunTimelineOnline(t *testing.T) {
	home := freshHome(t)
	startServerWithDB(t, home)
	var out bytes.Buffer
	code := run([]string{"sift", "timeline", "--run", "runCLI"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	events := result["events"].([]any)
	if len(events) == 0 {
		t.Fatal("timeline returned no events")
	}
}

// TestRunPSOnline prints persisted runs over a real daemon.
func TestRunPSOnline(t *testing.T) {
	home := freshHome(t)
	startServerWithDB(t, home)
	var out bytes.Buffer
	code := run([]string{"sift", "ps"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	runs := result["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
}

// seedCLIDurableChannelFailure drives the production Channel write ports to a
// durable, alert-raising failure. It mirrors the controlplane acceptance
// fixture so the thin client can assert the projection end-to-end over the
// real unix socket (with full JSON round-trip) without exporting a test-only
// helper across packages.
func seedCLIDurableChannelFailure(t *testing.T, db *storage.DB) {
	t.Helper()
	ctx := context.Background()
	const now = int64(1_700_000_000_000)
	if err := db.SeedProjectForTest(ctx, "cfg-ch", "project-ch", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-ch", "project-ch", "cfg-ch", "42", now); err != nil {
		t.Fatal(err)
	}
	const (
		batchID    = "daily:project-ch:Asia/Shanghai:1785286800000:ops-slack"
		deliveryID = batchID + ":publish:1"
		batchKey   = "attention-batch:" + deliveryID
	)
	payload, err := json.Marshal(map[string]any{
		"batch_id": batchID, "batch_kind": "daily_summary",
		"channel":     json.RawMessage(`{"id":"ops-slack","type":"webhook","target_ref":"secret_ref:SIFT_CHANNEL_OPS_SLACK","renderer":"plain-v1","capabilities":["text"]}`),
		"delivery_id": deliveryID, "delivery_kind": "attention_batch",
		"due_at_ms": int64(1_785_286_800_000),
		"forge_alert_target": map[string]any{
			"forge_kind": "github", "forge_host": "github.com",
			"forge_project_key": "owner/project-ch", "target_kind": "issue", "target_id": "42",
		},
		"members": []any{}, "project_id": "project-ch", "rendered_text": "channel failure fixture",
		"scope": "day", "scope_id": "Asia/Shanghai:1785286800000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueChannelPublish(ctx,
		storage.Operation{Key: batchKey, Kind: storage.OperationChannelPublish, Payload: payload},
		deliveryID, now); err != nil {
		t.Fatalf("enqueue channel publish: %v", err)
	}
	db.SetChannelPolicy(3, 3)
	for i, ec := range []storage.ErrorClass{storage.ErrorTransient, storage.ErrorTransient, storage.ErrorRateLimited} {
		attemptAt := now + int64(i+1)
		claim, err := db.ClaimOutboxOperationKind(ctx, "channel", storage.OperationChannelPublish, attemptAt, 10_000)
		if err != nil || claim == nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if err := db.CompleteOutboxAttempt(ctx, *claim, storage.CompleteOutcome{
			State: storage.OperationRetryable, ErrorClass: ec, ErrorSummary: "err",
			NowMS: attemptAt + 1, ChannelFailureAlertAfter: 3, MaxAttempts: 3,
		}); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}
}

// startServerWithChannelFailure opens a database seeded with a durable
// Channel failure and serves a real daemon so a CLI command can talk to it
// over the operator socket.
func startServerWithChannelFailure(t *testing.T, home string) {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	seedCLIDurableChannelFailure(t, db)
	s, err := controlplane.Start(config.Home{Path: home}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))
}

// channelDeliveryFromCLI runs one CLI command, parses its JSON response and
// returns the surfaced channel_deliveries projection. It checks the RPC-level
// `ok` flag rather than the process exit code: `sift doctor` legitimately
// exits 0/1/2 to project health while still returning a successful RPC.
func channelDeliveryFromCLI(t *testing.T, command string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	run([]string{"sift", command}, &out, io.Discard)
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("sift %s unmarshal: %v; output:\n%s", command, err, out.String())
	}
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("sift %s RPC ok=false; output:\n%s", command, out.String())
	}
	result := response["result"].(map[string]any)
	deliveries := result["channel_deliveries"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("sift %s channel_deliveries = %d rows, want 1", command, len(deliveries))
	}
	return deliveries[0].(map[string]any)
}

// TestRunPSDoctorOnlineExposeChannelDeliveries verifies the thin client
// surfaces the durable Channel delivery/episode/alert/generated_not_delivered
// projection over the real operator socket (full JSON round-trip) for both
// `sift ps` and `sift doctor`, closing the CLI half of #715 note 4 / #782.
func TestRunPSDoctorOnlineExposeChannelDeliveries(t *testing.T) {
	home := freshHome(t)
	startServerWithChannelFailure(t, home)

	for _, command := range []string{"ps", "doctor"} {
		row := channelDeliveryFromCLI(t, command)
		for _, key := range []string{
			"delivery_id", "channel_id", "operation_key", "state", "attempt_count",
			"consecutive_failures", "episode_state", "last_error_class",
			"alert_operation_key", "alert_state", "generated_not_delivered",
		} {
			if _, ok := row[key]; !ok {
				t.Errorf("sift %s channel delivery missing key %q (row=%v)", command, key, row)
			}
		}
		// JSON round-trip coerces integers to float64.
		if row["attempt_count"] != float64(3) {
			t.Errorf("sift %s attempt_count = %v, want 3", command, row["attempt_count"])
		}
		if row["consecutive_failures"] != float64(3) {
			t.Errorf("sift %s consecutive_failures = %v, want 3", command, row["consecutive_failures"])
		}
		if row["state"] != "failed" {
			t.Errorf("sift %s state = %v, want failed", command, row["state"])
		}
		if row["episode_state"] != "ended_failed" {
			t.Errorf("sift %s episode_state = %v, want ended_failed", command, row["episode_state"])
		}
		if row["last_error_class"] != string(storage.ErrorRateLimited) {
			t.Errorf("sift %s last_error_class = %v, want %s", command, row["last_error_class"], storage.ErrorRateLimited)
		}
		if row["alert_state"] != "pending" {
			t.Errorf("sift %s alert_state = %v, want pending", command, row["alert_state"])
		}
		if row["generated_not_delivered"] != true {
			t.Errorf("sift %s generated_not_delivered = %v, want true", command, row["generated_not_delivered"])
		}
	}
}
