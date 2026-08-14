package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/xsift/sift/internal/storage"
)

const reportConfigJSON = `{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":4,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":2,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}`

const reportToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func reportTokenHash() string {
	sum := sha256.Sum256([]byte(reportToken))
	return hex.EncodeToString(sum[:])
}

// startReportServer opens a storage DB seeded with a run/attempt/claim bound to
// reportToken and starts the daemon so run.sock RPCs reach RecordReport.
func startReportServer(t *testing.T, phase string) *Server {
	t.Helper()
	home := testHome(t)
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home.Path, "sift.db"), BinaryVersion: Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", 0); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecForTest(ctx, query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report','report-hash',1,?,1,0,?)`, reportConfigJSON, Version)
	exec(`UPDATE runs SET config_snapshot_id='cfg-report',status='running' WHERE id='run'`)
	exec(`INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','d',0)`)
	exec(`INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,created_at_ms,updated_at_ms) VALUES ('run',1,'pending',1,'process','agent','task','/wt','b','main','abc',0,0)`)
	switch phase {
	case "finished":
		exec(`UPDATE attempts SET phase='finished',result_exit_code=0,result_signal=NULL,result_digest='abc',result_observed_at_ms=0,finished_at_ms=0 WHERE run_id='run' AND attempt_no=1`)
	default:
		exec(`UPDATE attempts SET phase=?,agent_pid=2,agent_started_at_ms=0,agent_executable='/agent' WHERE run_id='run' AND attempt_no=1`, phase)
	}
	exec(`INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,dispatch_id,bootstrap_nonce_hash,run_token_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','dispatch','bootstrap',?,0,0)`, reportTokenHash())
	s, err := Start(home, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home.Path, "run.sock"))
	return s
}

func reportRequest(token, kind string, generation int) Request {
	return Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "report.submit", Auth: Auth{Kind: "run_token", Token: token}, Params: map[string]any{"run_id": "run", "attempt_no": 1, "generation": generation, "report_key": "0123456789abcdef0123456789abcdef", "kind": kind, "payload": map[string]any{"message": "progress"}}}
}

func TestReportSubmitRunningAccepted(t *testing.T) {
	s := startReportServer(t, "running")
	resp := call(t, filepath.Join(s.Home.Path, "run.sock"), reportRequest(reportToken, "progress", 1))
	if !resp.OK || resp.Result.(map[string]any)["disposition"] != "accepted" {
		t.Fatalf("running report = %#v", resp)
	}
}

func TestReportSubmitSpawningReturnsNotReadyPolicy(t *testing.T) {
	s := startReportServer(t, "spawning")
	resp := call(t, filepath.Join(s.Home.Path, "run.sock"), reportRequest(reportToken, "progress", 1))
	if resp.OK || resp.Error.Code != "not_ready" || !resp.Error.Retryable {
		t.Fatalf("spawning response = %#v", resp)
	}
	policy, ok := resp.Error.Details["retry_policy"].(map[string]any)
	if !ok || policy["initial_delay_ms"] != float64(100) || policy["max_delay_ms"] != float64(1000) || policy["total_timeout_ms"] != float64(10000) || policy["multiplier_micros"] != float64(2000000) {
		t.Fatalf("retry_policy = %#v", resp.Error.Details)
	}
}

func TestReportSubmitWrongGenerationStale(t *testing.T) {
	s := startReportServer(t, "running")
	resp := call(t, filepath.Join(s.Home.Path, "run.sock"), reportRequest(reportToken, "progress", 2))
	if resp.OK || resp.Error.Code != "stale" || resp.Error.Retryable {
		t.Fatalf("wrong generation = %#v", resp)
	}
}

func TestReportSubmitWrongTokenUnauthorized(t *testing.T) {
	s := startReportServer(t, "running")
	resp := call(t, filepath.Join(s.Home.Path, "run.sock"), reportRequest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "progress", 1))
	if resp.OK || resp.Error.Code != "unauthorized" {
		t.Fatalf("wrong token = %#v", resp)
	}
}

func TestReportSubmitFinishedPermanentConflict(t *testing.T) {
	s := startReportServer(t, "finished")
	resp := call(t, filepath.Join(s.Home.Path, "run.sock"), reportRequest(reportToken, "progress", 1))
	if resp.OK || resp.Error.Code != "conflict" || resp.Error.Retryable {
		t.Fatalf("finished phase = %#v", resp)
	}
}

func TestReportSubmitOperatorTokenRejectedOnRunSock(t *testing.T) {
	s := startReportServer(t, "running")
	resp := call(t, filepath.Join(s.Home.Path, "run.sock"), Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "report.submit", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": "run", "attempt_no": 1, "generation": 1, "report_key": "0123456789abcdef0123456789abcdef", "kind": "progress", "payload": map[string]any{"message": "x"}}})
	if resp.OK || resp.Error.Code != "unauthorized" {
		t.Fatalf("operator token on run.sock = %#v", resp)
	}
}

func TestReportSubmitQuotaExhaustedReturnsConflict(t *testing.T) {
	s := startReportServer(t, "running")
	blocker := func(key string) Response {
		return call(t, filepath.Join(s.Home.Path, "run.sock"), Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "report.submit", Auth: Auth{Kind: "run_token", Token: reportToken}, Params: map[string]any{"run_id": "run", "attempt_no": 1, "generation": 1, "report_key": key, "kind": "blocker", "payload": map[string]any{"blocker_summary": "b" + key[:1], "attempted_summary": "t", "recommended_action": "ask"}}})
	}
	if r := blocker("0123456789abcdef0123456789abcdef"); !r.OK {
		t.Fatalf("first blocker = %#v", r)
	}
	if r := blocker("1123456789abcdef0123456789abcdef"); !r.OK {
		t.Fatalf("second blocker = %#v", r)
	}
	r := blocker("2123456789abcdef0123456789abcdef")
	if r.OK || r.Error.Code != "conflict" || r.Error.Details["code"] != "report_interrupt_quota_exhausted" {
		t.Fatalf("quota exhausted = %#v", r)
	}
}
