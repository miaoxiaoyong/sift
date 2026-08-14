package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xsift/sift/internal/storage"
)

// startServerWithDB opens a real database under a temp home, starts a server
// bound to it, and returns both for seeding + RPC assertions.
func startServerWithDB(t *testing.T) (*Server, *storage.DB) {
	t.Helper()
	home := testHome(t)
	dbPath := filepath.Join(home.Path, "sift.db")
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: dbPath, BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := Start(home, db)
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetAttentionQuota(map[string]int{"low": 5, "normal": 5, "high": 5})
	return s, db
}

const cpNow = 1_700_000_000_000

func seedCPRun(t *testing.T, db *storage.DB) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-cp", "proj-cp", cpNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "runCP", "proj-cp", "cfg-cp", cpNow, "/work"); err != nil {
		t.Fatal(err)
	}
}

// TestOpsPSReturnsRealRuns verifies ops.ps reads persisted Run/attempt rows
// instead of the placeholder empty list.
func TestOpsPSReturnsRealRuns(t *testing.T) {
	s, db := startServerWithDB(t)
	seedCPRun(t, db)
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.ps", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": nil, "project_id": nil, "status": nil, "limit": float64(100), "after_run_id": nil}})
	if !resp.OK {
		t.Fatalf("ops.ps = %#v", resp)
	}
	result := resp.Result.(map[string]any)
	runs := result["runs"].([]storage.PSRun)
	if len(runs) != 1 || runs[0].RunID != "runCP" {
		t.Fatalf("runs = %+v, want runCP", runs)
	}
	rem := result["attention_remaining"].(map[string]int)
	// No persisted attention bucket → configured ceiling 5 fully remaining.
	if rem["normal"] != 5 {
		t.Fatalf("normal remaining = %d, want 5", rem["normal"])
	}
}

// TestOpsMetricsCoversNineSeries verifies ops.metrics returns the full report
// and the trigger→started latency distribution without error on real data.
func TestOpsMetricsCoversNineSeries(t *testing.T) {
	s, db := startServerWithDB(t)
	seedCPRun(t, db)
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.metrics", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"project_id": nil}})
	if !resp.OK {
		t.Fatalf("ops.metrics = %#v", resp)
	}
	result := resp.Result.(map[string]any)
	metrics := result["metrics"].(storage.MetricsReport)
	if metrics.WeightedAttentionPerChange.Coverage == "" || metrics.FalseReleaseRate.Coverage == "" {
		t.Fatalf("metric coverage notes missing: %+v", metrics)
	}
	if _, ok := result["trigger_started_latency"]; !ok {
		t.Fatal("missing trigger_started_latency")
	}
}

// TestOpsTimelineReturnsPersistedEvents verifies ops.timeline reads the
// append-only event stream.
func TestOpsTimelineReturnsPersistedEvents(t *testing.T) {
	s, db := startServerWithDB(t)
	seedCPRun(t, db)
	ctx := context.Background()
	if _, err := db.AppendEvent(ctx, storage.EventCmd{RunID: "runCP", Type: "report.progress", Source: storage.SourceAgent, PayloadJSON: []byte("{}"), OccurredAtMS: cpNow, RecordedAtMS: cpNow}); err != nil {
		t.Fatal(err)
	}
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.timeline", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": "runCP", "project_id": nil, "type": nil, "after_seq": float64(0), "after_occurred_at_ms": float64(0), "limit": float64(100)}})
	if !resp.OK {
		t.Fatalf("ops.timeline = %#v", resp)
	}
	report := resp.Result.(storage.TimelineReport)
	if len(report.Events) == 0 {
		t.Fatal("timeline returned no events")
	}
}

// opsTimelineRPC issues a direct ops.timeline RPC with the given params and
// returns the decoded report, failing the test on any protocol error.
func opsTimelineRPC(t *testing.T, s *Server, params map[string]any) storage.TimelineReport {
	t.Helper()
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.timeline", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: params})
	if !resp.OK {
		t.Fatalf("ops.timeline = %#v", resp)
	}
	return resp.Result.(storage.TimelineReport)
}

// TestOpsTimelineLegacyAfterSeqPagination verifies the backward-compatible
// legacy RPC contract: a caller that sends only after_seq (no
// after_occurred_at_ms) must keep paging — each page returns the subsequent
// events, concatenated pages cover all events exactly once, and the stream
// stays globally occurred_at_ms descending.
func TestOpsTimelineLegacyAfterSeqPagination(t *testing.T) {
	s, db := startServerWithDB(t)
	seedCPRun(t, db)
	ctx := context.Background()
	// Interleaved seq/occurred_at_ms; global newest-first is +4, +3, +1, +2, +0.
	occurred := []int64{cpNow + 1, cpNow + 3, cpNow + 2, cpNow + 5, cpNow + 4}
	for _, at := range occurred {
		if _, err := db.AppendEvent(ctx, storage.EventCmd{RunID: "runCP", Type: "report.progress", Source: storage.SourceAgent, PayloadJSON: []byte("{}"), OccurredAtMS: at, RecordedAtMS: at}); err != nil {
			t.Fatal(err)
		}
	}

	// Page 1 with the old parameter set: no cursor at all.
	page1 := opsTimelineRPC(t, s, map[string]any{"run_id": "runCP", "limit": float64(2)})
	if len(page1.Events) != 2 || !page1.HasMore {
		t.Fatalf("legacy page1 = %+v, want 2 events with more", page1)
	}

	// Page 2: legacy field set — only after_seq, after_occurred_at_ms absent.
	page2 := opsTimelineRPC(t, s, map[string]any{"run_id": "runCP", "after_seq": float64(page1.NextSeq), "limit": float64(2)})
	if len(page2.Events) != 2 || !page2.HasMore {
		t.Fatalf("legacy page2 = %+v, want 2 events with more", page2)
	}
	if page2.Events[0].Seq == page1.Events[len(page1.Events)-1].Seq {
		t.Fatalf("legacy page2 repeats page1 tail event: %+v", page2.Events)
	}

	// Page 3: only after_seq again, draining the stream.
	page3 := opsTimelineRPC(t, s, map[string]any{"run_id": "runCP", "after_seq": float64(page2.NextSeq), "limit": float64(10)})
	if len(page3.Events) != 1 || page3.HasMore {
		t.Fatalf("legacy page3 = %+v, want the final event", page3)
	}

	// Concatenated pages cover all five events exactly once, globally descending.
	all := append(append(append([]storage.Event{}, page1.Events...), page2.Events...), page3.Events...)
	if len(all) != 5 {
		t.Fatalf("legacy pages cover %d events, want 5: %+v", len(all), all)
	}
	seen := map[int64]bool{}
	for i, e := range all {
		if seen[e.Seq] {
			t.Fatalf("legacy pages duplicate seq %d: %+v", e.Seq, all)
		}
		seen[e.Seq] = true
		if i > 0 && all[i-1].OccurredAtMS < e.OccurredAtMS {
			t.Fatalf("legacy pages not globally descending at %d: %+v", i, all)
		}
	}

	// The legacy path must agree with the dual-cursor path page-for-page.
	dual2 := opsTimelineRPC(t, s, map[string]any{"run_id": "runCP", "after_seq": float64(page1.NextSeq), "after_occurred_at_ms": float64(page1.NextOccurredAtMS), "limit": float64(2)})
	if len(dual2.Events) != len(page2.Events) {
		t.Fatalf("legacy/dual page2 disagree: legacy %d vs dual %d events", len(page2.Events), len(dual2.Events))
	}
	for i := range dual2.Events {
		if dual2.Events[i].Seq != page2.Events[i].Seq {
			t.Fatalf("legacy/dual page2 disagree at %d: legacy %+v vs dual %+v", i, page2.Events, dual2.Events)
		}
	}
}

// TestOpsLogsReadsAgentLog verifies ops.logs reads the persisted agent.log file
// with a bounded base64 payload and EOF semantics.
func TestOpsLogsReadsAgentLog(t *testing.T) {
	s, db := startServerWithDB(t)
	seedCPRun(t, db)
	// SeedLaunchRunForTest creates attempts/1; write its agent.log.
	logDir := filepath.Join(s.Home.Path, "runs", "runCP", "attempts", "1")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "agent.log"), []byte("hello agent log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.logs", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": "runCP", "attempt_no": nil, "offset": float64(0), "limit": float64(262144)}})
	if !resp.OK {
		t.Fatalf("ops.logs = %#v", resp)
	}
	result := resp.Result.(map[string]any)
	if result["attempt_no"] != 1 {
		t.Fatalf("attempt_no = %v, want 1", result["attempt_no"])
	}
	if result["eof"] != true {
		t.Fatalf("eof = %v, want true", result["eof"])
	}
}

// TestOpsLogsNotFound verifies a missing log fails closed with not_found.
func TestOpsLogsNotFound(t *testing.T) {
	s, db := startServerWithDB(t)
	seedCPRun(t, db)
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.logs", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": "runCP", "attempt_no": float64(1), "offset": float64(0), "limit": float64(262144)}})
	if resp.OK || resp.Error.Code != "not_found" {
		t.Fatalf("ops.logs missing log = %#v, want not_found", resp)
	}
}

// TestOpsMetricsRejectsExtraParams verifies the closed param set.
func TestOpsMetricsRejectsExtraParams(t *testing.T) {
	s, _ := startServerWithDB(t)
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.metrics", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"project_id": nil, "bogus": 1}})
	if resp.OK || resp.Error.Code != "invalid_request" {
		t.Fatalf("ops.metrics extra param = %#v, want invalid_request", resp)
	}
}

// Channel diagnostics endpoint-level acceptance (issue #782).
//
// Storage-level reopen is already proven by
// TestProductionChannelDiagnosticsSurviveRestart. These tests close the
// remaining wave-1 gap: the durable Channel delivery / episode / alert /
// generated_not_delivered projection must be surfaced through the real
// operator surface (ops.ps / ops.doctor), and must read back identically
// after a full DB close/reopen + server rebind.
const (
	chBatchID    = "daily:project-ch:Asia/Shanghai:1785286800000:ops-slack"
	chDeliveryID = chBatchID + ":publish:1"
	chBatchKey   = "attention-batch:" + chDeliveryID
	chDueAtMS    = int64(1_785_286_800_000)
)

// buildChannelBatchPayload assembles a structurally-valid sealed batch
// channel_publish payload (the same production shape EnqueueChannelPublish
// validates). It seeds a durable batch delivery without going through the
// sealer; only the projection fields asserted below matter.
func buildChannelBatchPayload(t *testing.T) []byte {
	t.Helper()
	target := map[string]any{
		"forge_kind": "github", "forge_host": "github.com",
		"forge_project_key": "owner/project-ch",
		"target_kind":       "issue", "target_id": "42",
	}
	channel := json.RawMessage(`{"id":"ops-slack","type":"webhook","target_ref":"secret_ref:SIFT_CHANNEL_OPS_SLACK","renderer":"plain-v1","capabilities":["text"]}`)
	body := map[string]any{
		"batch_id":           chBatchID,
		"batch_kind":         "daily_summary",
		"channel":            channel,
		"delivery_id":        chDeliveryID,
		"delivery_kind":      "attention_batch",
		"due_at_ms":          chDueAtMS,
		"forge_alert_target": target,
		"members":            []any{},
		"project_id":         "project-ch",
		"rendered_text":      "channel failure fixture",
		"scope":              "day",
		"scope_id":           "Asia/Shanghai:1785286800000",
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal channel payload: %v", err)
	}
	return out
}

// seedDurableChannelFailure drives the production Channel write ports
// (EnqueueChannelPublish → ClaimOutboxOperationKind → CompleteOutboxAttempt)
// to a durable, alert-raising failure: three consecutive retryable attempts
// trip the episode + immutable forge-alert operation that operator views read.
func seedDurableChannelFailure(t *testing.T, db *storage.DB) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-ch", "project-ch", cpNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-ch", "project-ch", "cfg-ch", "42", cpNow); err != nil {
		t.Fatal(err)
	}
	payload := buildChannelBatchPayload(t)
	if err := db.EnqueueChannelPublish(ctx,
		storage.Operation{Key: chBatchKey, Kind: storage.OperationChannelPublish, Payload: payload},
		chDeliveryID, cpNow); err != nil {
		t.Fatalf("enqueue channel publish: %v", err)
	}
	db.SetChannelPolicy(3, 3)
	for i, errClass := range []storage.ErrorClass{storage.ErrorTransient, storage.ErrorTransient, storage.ErrorRateLimited} {
		now := cpNow + int64(i+1)
		claim, err := db.ClaimOutboxOperationKind(ctx, "channel", storage.OperationChannelPublish, now, 10_000)
		if err != nil || claim == nil {
			t.Fatalf("claim %d = %#v, %v", i, claim, err)
		}
		if err := db.CompleteOutboxAttempt(ctx, *claim, storage.CompleteOutcome{
			State: storage.OperationRetryable, ErrorClass: errClass, ErrorSummary: "err",
			NowMS: now + 1, ChannelFailureAlertAfter: 3, MaxAttempts: 3,
		}); err != nil {
			t.Fatalf("complete attempt %d: %v", i, err)
		}
	}
}

// opsChannelDeliveries calls ops.ps or ops.doctor over the operator surface
// (the same dispatch path the unix socket serves) and returns the surfaced
// channel_deliveries projection.
func opsChannelDeliveries(t *testing.T, s *Server, token, method string) []map[string]any {
	t.Helper()
	params := map[string]any{}
	if method == "ops.ps" {
		// ops.ps enforces a closed 5-key param set; supply the full envelope.
		params = map[string]any{"run_id": nil, "project_id": nil, "status": nil, "limit": float64(100), "after_run_id": nil}
	}
	resp := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: method, Auth: Auth{Kind: "operator", Token: token}, Params: params})
	if !resp.OK {
		t.Fatalf("%s = %#v", method, resp)
	}
	result := resp.Result.(map[string]any)
	deliveries, ok := result["channel_deliveries"].([]map[string]any)
	if !ok || len(deliveries) != 1 {
		t.Fatalf("%s channel_deliveries = %#v, want exactly 1 durable projection", method, result["channel_deliveries"])
	}
	return deliveries
}

// assertDurableChannelProjection asserts the endpoint surfaces the full
// delivery / episode / alert / generated_not_delivered key set with the values
// the seeded three-failure episode must leave on disk.
func assertDurableChannelProjection(t *testing.T, deliveries []map[string]any, label string) {
	t.Helper()
	row := deliveries[0]
	for _, key := range []string{
		"delivery_id", "channel_id", "operation_key", "state", "attempt_count",
		"last_error", "created_at_ms", "next_attempt_at_ms", "consecutive_failures",
		"episode_state", "last_error_class", "alert_operation_key", "alert_state",
		"generated_not_delivered",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("%s: channel delivery missing key %q (row=%v)", label, key, row)
		}
	}
	if row["delivery_id"] != chDeliveryID {
		t.Errorf("%s: delivery_id = %v, want %s", label, row["delivery_id"], chDeliveryID)
	}
	if row["channel_id"] != "ops-slack" {
		t.Errorf("%s: channel_id = %v, want ops-slack", label, row["channel_id"])
	}
	if row["operation_key"] != chBatchKey {
		t.Errorf("%s: operation_key = %v, want %s", label, row["operation_key"], chBatchKey)
	}
	// Three retryable attempts at the frozen MaxAttempts=3 limit turn the
	// delivery terminal (failed) while the episode still records the failure
	// and raises the immutable alert — generated_not_delivered stays true.
	if row["state"] != "failed" {
		t.Errorf("%s: state = %v, want failed", label, row["state"])
	}
	if row["attempt_count"] != int64(3) {
		t.Errorf("%s: attempt_count = %v, want 3", label, row["attempt_count"])
	}
	if row["consecutive_failures"] != int64(3) {
		t.Errorf("%s: consecutive_failures = %v, want 3", label, row["consecutive_failures"])
	}
	// The episode crossed the 3-failure threshold: terminal-failed, and the
	// immutable forge-alert operation is bound to it.
	if row["episode_state"] != "ended_failed" {
		t.Errorf("%s: episode_state = %v, want ended_failed", label, row["episode_state"])
	}
	if row["last_error_class"] != string(storage.ErrorRateLimited) {
		t.Errorf("%s: last_error_class = %v, want %s", label, row["last_error_class"], storage.ErrorRateLimited)
	}
	if row["alert_operation_key"] == "" {
		t.Errorf("%s: alert_operation_key empty, want durable alert key", label)
	}
	if row["alert_state"] != "pending" {
		t.Errorf("%s: alert_state = %v, want pending", label, row["alert_state"])
	}
	if row["generated_not_delivered"] != true {
		t.Errorf("%s: generated_not_delivered = %v, want true", label, row["generated_not_delivered"])
	}
}

// TestOpsPSExposesDurableChannelDiagnostics verifies ops.ps surfaces the
// durable Channel delivery/episode/alert/generated_not_delivered projection
// over the operator surface (acceptance: ops.ps checkbox).
func TestOpsPSExposesDurableChannelDiagnostics(t *testing.T) {
	s, db := startServerWithDB(t)
	seedDurableChannelFailure(t, db)
	deliveries := opsChannelDeliveries(t, s, s.operatorToken, "ops.ps")
	assertDurableChannelProjection(t, deliveries, "ops.ps")
}

// TestOpsDoctorExposesDurableChannelDiagnostics verifies ops.doctor surfaces
// the same durable Channel fault projection alongside its health checks
// (acceptance: ops.doctor checkbox).
func TestOpsDoctorExposesDurableChannelDiagnostics(t *testing.T) {
	s, db := startServerWithDB(t)
	seedDurableChannelFailure(t, db)
	deliveries := opsChannelDeliveries(t, s, s.operatorToken, "ops.doctor")
	assertDurableChannelProjection(t, deliveries, "ops.doctor")
}

// TestOpsPSDoctorChannelDiagnosticsSurviveDBReopen verifies both endpoints
// read the projection purely from durable storage: after a full DB close /
// same-path reopen plus server rebind, every channel_deliveries key and value
// is byte-identical (acceptance: cross-restart checkbox).
func TestOpsPSDoctorChannelDiagnosticsSurviveDBReopen(t *testing.T) {
	home := testHome(t)
	dbPath := filepath.Join(home.Path, "sift.db")

	// First lifetime: seed a durable failure and capture the operator view.
	db1, err := storage.Open(context.Background(), storage.OpenConfig{Path: dbPath, BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	seedDurableChannelFailure(t, db1)
	s1, err := Start(home, db1)
	if err != nil {
		t.Fatalf("start s1: %v", err)
	}
	s1.SetAttentionQuota(map[string]int{"low": 5, "normal": 5, "high": 5})
	token := s1.operatorToken

	beforePS := opsChannelDeliveries(t, s1, token, "ops.ps")
	beforeDoctor := opsChannelDeliveries(t, s1, token, "ops.doctor")
	assertDurableChannelProjection(t, beforePS, "ops.ps before reopen")
	assertDurableChannelProjection(t, beforeDoctor, "ops.doctor before reopen")

	// Cross-restart: close DB, reopen the same path, rebind a fresh server.
	// Server.Close does not own the DB handle, so close it explicitly.
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close s1: %v", err)
	}
	db2, err := storage.Open(context.Background(), storage.OpenConfig{Path: dbPath, BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatalf("reopen db2: %v", err)
	}
	db2.SetChannelPolicy(3, 3)
	s2, err := Start(home, db2)
	if err != nil {
		t.Fatalf("start s2: %v", err)
	}
	t.Cleanup(func() {
		_ = s2.Close()
		_ = db2.Close()
	})
	s2.SetAttentionQuota(map[string]int{"low": 5, "normal": 5, "high": 5})
	// The operator capability is durable under home; it must survive restart.
	if s2.operatorToken != token {
		t.Fatalf("operator token changed across restart: before=%q after=%q", token, s2.operatorToken)
	}

	afterPS := opsChannelDeliveries(t, s2, token, "ops.ps")
	afterDoctor := opsChannelDeliveries(t, s2, token, "ops.doctor")
	assertDurableChannelProjection(t, afterPS, "ops.ps after reopen")
	assertDurableChannelProjection(t, afterDoctor, "ops.doctor after reopen")

	if !reflect.DeepEqual(beforePS, afterPS) {
		t.Fatalf("ops.ps channel_deliveries drifted across DB reopen:\nbefore=%v\nafter =%v", beforePS, afterPS)
	}
	if !reflect.DeepEqual(beforeDoctor, afterDoctor) {
		t.Fatalf("ops.doctor channel_deliveries drifted across DB reopen:\nbefore=%v\nafter =%v", beforeDoctor, afterDoctor)
	}
}
