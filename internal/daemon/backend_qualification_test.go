package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func tmuxObservationFixture(t *testing.T, mode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\ncase \"$5\" in\nhas-session) "
	if mode == "absent" {
		script += "exit 1 ;;\n"
	} else {
		script += "exit 0 ;;\n"
	}
	script += "show-environment) printf 'SIFT_TMUX_BINDING=%s\\n' \"${7#=sift-}\" ;;\nlist-panes) printf '0\\n' ;;\nshow-options) printf 'off\\n' ;;\n*) exit 99 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecoveryTmuxSessionPresentWrapperAbsentDiagnostic(t *testing.T) {
	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	if _, err := raw.Exec(`UPDATE attempts SET backend='tmux' WHERE run_id='run'; UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='nonce',run_token_hash='token' WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
	before := recoveryExecutionProjection(t, raw)
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{}, Signaler: &recoverySignaler{}}, Runtime: config.Runtime{AbsenceRecheckCount: 1}, AttentionDailyQuota: recoveryQuota(), TmuxPath: tmuxObservationFixture(t, "present"), TmuxSocketPath: filepath.Join(t.TempDir(), "socket"), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSessionDiagnostic(t, raw, "backend_session_present_wrapper_absent")
	if after := recoveryExecutionProjection(t, raw); after != before {
		t.Fatalf("session diagnostic rewrote execution evidence: before=%q after=%q", before, after)
	}
}

func TestRecoveryTmuxSessionAbsentWrapperPresentDiagnostic(t *testing.T) {
	db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
	if _, err := raw.Exec(`UPDATE attempts SET backend='tmux' WHERE run_id='run'; UPDATE attempt_claims SET dispatch_id='dispatch',bootstrap_nonce_hash='nonce',run_token_hash='token' WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
	before := recoveryExecutionProjection(t, raw)
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{observation: runtimepkg.ProcessObservation{Exists: true, ProcessIdentity: runtimepkg.ProcessIdentity{PID: attempt.WrapperPID, StartedAtMS: attempt.WrapperStartedAtMS, Executable: attempt.WrapperExecutable, PGID: attempt.WrapperPGID, ControlNonceHash: attempt.ControlNonceHash}}}}, AttentionDailyQuota: recoveryQuota(), TmuxPath: tmuxObservationFixture(t, "absent"), TmuxSocketPath: filepath.Join(t.TempDir(), "socket"), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSessionDiagnostic(t, raw, "backend_session_lost")
	if after := recoveryExecutionProjection(t, raw); after != before {
		t.Fatalf("session loss rewrote execution evidence: before=%q after=%q", before, after)
	}
}

func recoveryExecutionProjection(t *testing.T, raw *sql.DB) string {
	t.Helper()
	var started, finished, owner, replacement sql.NullString
	if err := raw.QueryRow(`SELECT CAST(agent_started_at_ms AS TEXT),CAST(finished_at_ms AS TEXT),wrapper_instance_id,(SELECT CAST(attempt_no AS TEXT) FROM attempts WHERE run_id='run' ORDER BY attempt_no DESC LIMIT 1) FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&started, &finished, &owner, &replacement); err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{started.String, finished.String, owner.String, replacement.String}, "|")
}

func assertSessionDiagnostic(t *testing.T, raw *sql.DB, code string) {
	t.Helper()
	var payload string
	if err := raw.QueryRow(`SELECT payload_json FROM events WHERE type='backend.session_diagnostic'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(payload), &got); err != nil || got["diagnostic_code"] != code {
		t.Fatalf("diagnostic=%q parsed=%v err=%v, want %s", payload, got, err, code)
	}
}

func TestQualificationRecoveryVerifiedAbsenceConfirmed(t *testing.T) {
	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	q := detachedQualification(t)
	q.ID, q.Status, q.Reason = "verified", "process-group-verified", "qualified"
	var err error
	q.EvidenceJSON, err = storage.TopologyQualificationEvidenceJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordTopologyQualification(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE attempts SET topology_qualification_key=? WHERE run_id='run'`, q.QualificationKey); err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, Terminator: runtimepkg.Terminator{Inspector: recoveryInspector{}, Signaler: &recoverySignaler{}}, Runtime: config.Runtime{AbsenceRecheckCount: 1}, ProcessGroupQualified: func(key string) bool { ok, _ := db.ProcessGroupQualified(context.Background(), key); return ok }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var phase string
	var stalls int
	if err := raw.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='run' AND reason='startup_stall'`).Scan(&stalls); err != nil {
		t.Fatal(err)
	}
	if phase != "orphaned" || stalls != 0 {
		t.Fatalf("verified absence phase=%q startup_stalls=%d", phase, stalls)
	}
}

func TestDetachedDescendantIsUnverifiedAndCannotRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid fixture is POSIX")
	}
	cmd := exec.Command("sh", "-c", "setsid sleep 30 >/dev/null 2>&1 & echo $!")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) < 5 || fields[4] == strconv.Itoa(syscall.Getpgrp()) {
			t.Fatalf("fixture did not detach process group: %q", data)
		}
	}

	db, raw, _, now := seedRecoveryCoordinator(t, "running", 0)
	q := detachedQualification(t)
	if err := db.RecordTopologyQualification(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE attempts SET topology_qualification_key=?,wrapper_pid=NULL,wrapper_started_at_ms=NULL,wrapper_executable=NULL,wrapper_pgid=NULL,wrapper_instance_id=NULL,control_nonce_hash=NULL WHERE run_id='run'`, q.QualificationKey); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(context.Background(), "hash-cfg", "test", 1, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &TerminationCoordinator{DB: db, ProcessGroupQualified: func(key string) bool { ok, _ := db.ProcessGroupQualified(context.Background(), key); return ok }, AttentionDailyQuota: recoveryQuota(), Now: func() time.Time { return now }}
	if err := coordinator.RecoverStartup(context.Background(), boot); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	var attempts, stalls int
	if err := raw.QueryRow(`SELECT isolation_state,isolation_reason FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM attempts WHERE run_id='run'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id='run' AND reason='startup_stall' AND status='open'`).Scan(&stalls); err != nil {
		t.Fatal(err)
	}
	if state != "frozen" || reason != "process_group_unverified" || attempts != 1 || stalls != 1 {
		t.Fatalf("detached recovery = state=%s reason=%s attempts=%d stalls=%d", state, reason, attempts, stalls)
	}
}

func detachedQualification(t *testing.T) storage.TopologyQualification {
	t.Helper()
	projection := runtimepkg.Qualification{MethodVersion: runtimepkg.TopologyMethodVersion, AgentID: "agent", AgentDefinitionHash: strings.Repeat("a", 64), ExecutablePath: "/fixture/agent", ExecutableSHA256: strings.Repeat("b", 64), VersionOutputDigest: strings.Repeat("c", 64), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	key, err := runtimepkg.QualificationKey(projection)
	if err != nil {
		t.Fatal(err)
	}
	q := storage.TopologyQualification{ID: "detached", QualificationKey: key, MethodVersion: projection.MethodVersion, AgentID: projection.AgentID, AgentDefinitionHash: projection.AgentDefinitionHash, ExecutablePath: projection.ExecutablePath, ExecutableSHA256: projection.ExecutableSHA256, VersionOutputDigest: projection.VersionOutputDigest, GOOS: projection.GOOS, GOARCH: projection.GOARCH, Status: "process-group-unverified", Reason: "detached_descendant", RecordedAtMS: nowMillis(time.Now())}
	q.EvidenceJSON, err = storage.TopologyQualificationEvidenceJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
