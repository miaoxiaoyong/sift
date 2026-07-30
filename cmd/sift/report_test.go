package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// reportConfigFast is a running-friendly snapshot; reportConfigFastNotReady
// shrinks the not_ready budget so the CLI retry loop exhausts in milliseconds.
const reportConfigFast = `{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":4,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":2,"max_payload_bytes":65536,"not_ready_initial_delay":100000000,"not_ready_max_delay":1000000000,"not_ready_total_timeout":10000000000},"runtime":{"retry_multiplier":2}}`

const reportConfigFastNotReady = `{"attention":{"critical_fuse":{"per_run_limit":2,"total_limit":5,"window":900000},"daily_quota":{"high":5,"low":3,"normal":5},"day_timezone":"UTC","daily_summary_at":"09:00","max_escalations":0},"report":{"burst":4,"dedupe_window":0,"events_per_minute":60,"interrupts_per_run_daily_quota":2,"max_payload_bytes":65536,"not_ready_initial_delay":1000000,"not_ready_max_delay":1000000,"not_ready_total_timeout":10000000},"runtime":{"retry_multiplier":1}}`

const cliReportToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func cliTokenHash() string {
	sum := sha256.Sum256([]byte(cliReportToken))
	return hex.EncodeToString(sum[:])
}

// seedReportDaemon starts a daemon with a run/attempt/claim bound to
// cliReportToken in the given phase, and writes a control.json the CLI reads.
func seedReportDaemon(t *testing.T, phase, cfgJSON string) string {
	t.Helper()
	home := freshHome(t)
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run", "project", "cfg", "42", 0); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := db.ExecForTest(ctx, query, args...); e != nil {
			t.Fatalf("exec %q: %v", query, e)
		}
	}
	exec(`INSERT INTO config_snapshots(id,config_hash,schema_version,canonical_json,source_present,loaded_at_ms,binary_version) VALUES ('cfg-report','rh',1,?,1,0,?)`, cfgJSON, controlplane.Version)
	exec(`UPDATE runs SET config_snapshot_id='cfg-report',status='running' WHERE id='run'`)
	exec(`INSERT INTO task_spec_snapshots(id,run_id,version,schema_version,canonical_json,content_digest,created_at_ms) VALUES ('task','run',1,1,'{}','d',0)`)
	exec(`INSERT INTO attempts(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,created_at_ms,updated_at_ms) VALUES ('run',1,'pending',1,'process','agent','task','/wt','b','main','abc',0,0)`)
	exec(`UPDATE attempts SET phase=?,agent_pid=2,agent_started_at_ms=0,agent_executable='/agent' WHERE run_id='run' AND attempt_no=1`, phase)
	exec(`INSERT INTO attempt_claims(run_id,attempt_no,generation,launch_operation_key,dispatch_id,bootstrap_nonce_hash,run_token_hash,created_at_ms,updated_at_ms) VALUES ('run',1,1,'launch','dispatch','bootstrap',?,0,0)`, cliTokenHash())
	s, err := controlplane.Start(config.Home{Path: home}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "run.sock"))
	// Write control.json for the CLI to read.
	runDir := filepath.Join(home, "run-dir")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	control := `{"schema_version":1,"run_id":"run","attempt_no":1,"generation":1,"wrapper_instance_id":"w","wrapper_identity":{"pid":1,"started_at_ms":0,"executable":"/w","pgid":1},"agent_identity":null,"worktree_path":"/wt","task_spec_snapshot_id":"task","control_nonce":"n","run_token":"` + cliReportToken + `","updated_at_ms":0}`
	if err := os.WriteFile(filepath.Join(runDir, "control.json"), []byte(control), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIFT_RUN_DIR", runDir)
	return home
}

func TestRunReportAccepted(t *testing.T) {
	seedReportDaemon(t, "running", reportConfigFast)
	var out bytes.Buffer
	code := run([]string{"sift", "report", "progress", "--key", "0123456789abcdef0123456789abcdef", "--payload", `{"message":"hi"}`}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("response ok = %v", resp["ok"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["disposition"] != "accepted" {
		t.Fatalf("disposition = %v", result["disposition"])
	}
}

func TestRunReportNotReadyTimesOut(t *testing.T) {
	seedReportDaemon(t, "spawning", reportConfigFastNotReady)
	var stderr bytes.Buffer
	start := time.Now()
	code := run([]string{"sift", "report", "progress", "--key", "0123456789abcdef0123456789abcdef", "--payload", `{"message":"hi"}`}, io.Discard, &stderr)
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (timed out)", code)
	}
	// The fast policy totals 10ms; allow generous slack for scheduling while
	// confirming the CLI actually retried rather than failing immediately.
	if elapsed < 5*time.Millisecond {
		t.Fatalf("elapsed = %v, expected at least one not_ready retry", elapsed)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("timed out")) {
		t.Fatalf("stderr = %q, want timed out message", stderr.String())
	}
}

func TestRunReportMissingControlFile(t *testing.T) {
	freshHome(t)
	t.Setenv("SIFT_RUN_DIR", "")
	var stderr bytes.Buffer
	code := run([]string{"sift", "report", "progress", "--key", "0123456789abcdef0123456789abcdef", "--payload", `{"message":"hi"}`}, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunReportRejectsBadArgs(t *testing.T) {
	for _, args := range [][]string{
		{"sift", "report"},
		{"sift", "report", "bogus", "--key", "0123456789abcdef0123456789abcdef", "--payload", `{"message":"x"}`},
		{"sift", "report", "progress", "--payload", `{"message":"x"}`},
		{"sift", "report", "progress", "--key", "0123456789abcdef0123456789abcdef"},
		{"sift", "report", "progress", "--key", "0123456789abcdef0123456789abcdef", "--payload", `"notobject"`},
	} {
		var stderr bytes.Buffer
		if code := run(args, io.Discard, &stderr); code != 2 {
			t.Fatalf("args %v exit = %d, want 2", args, code)
		}
	}
}
