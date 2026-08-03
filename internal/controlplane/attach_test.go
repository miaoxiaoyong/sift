package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

const opsAttachRequestID = "0123456789abcdef0123456789abcdef"

func seedOpsAttachRun(t *testing.T, db *storage.DB, backend string) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-attach", "project-attach", cpNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedAttachRunForTest(ctx, "run-attach", "project-attach", "cfg-attach", backend, cpNow, "/work"); err != nil {
		t.Fatal(err)
	}
}

func installOpsAttachTmux(t *testing.T, digest, mode string) string {
	t.Helper()
	tmux := filepath.Join(t.TempDir(), "tmux")
	has := "exit 0"
	binding := "SIFT_TMUX_BINDING=" + digest
	if mode == "absent" {
		has = "exit 1"
	}
	if mode == "binding-mismatch" {
		binding = "SIFT_TMUX_BINDING=wrong"
	}
	script := "#!/bin/sh\ncase \"$5\" in\nhas-session) " + has + " ;;\nshow-environment) printf '" + binding + "\\n' ;;\nlist-panes) printf '0\\n' ;;\nshow-options) printf 'off\\n' ;;\n*) exit 99 ;;\nesac\n"
	if err := os.WriteFile(tmux, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return tmux
}

func opsAttachRequest(s *Server, runID string) Response {
	return s.operatorRequest(Request{
		RequestID: opsAttachRequestID,
		Method:    "ops.attach",
		Auth:      Auth{Kind: "operator", Token: s.operatorToken},
		Params:    map[string]any{"run_id": runID},
	})
}

func opsAttachDomainProjection(t *testing.T, db *storage.DB) map[string]string {
	t.Helper()
	queries := map[string]string{
		"runs":       `SELECT COALESCE(group_concat(id || '|' || status || '|' || version || '|' || updated_at_ms, char(10)), '') FROM runs ORDER BY id`,
		"attempts":   `SELECT COALESCE(group_concat(run_id || '|' || attempt_no || '|' || phase || '|' || generation || '|' || backend || '|' || COALESCE(attempt_resolution,'') || '|' || isolation_state, char(10)), '') FROM attempts ORDER BY run_id,attempt_no`,
		"claims":     `SELECT COALESCE(group_concat(run_id || '|' || attempt_no || '|' || generation || '|' || launch_operation_key || '|' || COALESCE(dispatch_id,''), char(10)), '') FROM attempt_claims ORDER BY run_id,attempt_no`,
		"gates":      `SELECT COALESCE(group_concat(id || '|' || run_id || '|' || snapshot_id || '|' || verdict_digest, char(10)), '') FROM gate_evaluations ORDER BY id`,
		"interrupts": `SELECT COALESCE(group_concat(id || '|' || run_id || '|' || status || '|' || version, char(10)), '') FROM interrupts ORDER BY id`,
		"outbox":     `SELECT COALESCE(group_concat(id || '|' || operation_key || '|' || state || '|' || attempt_count || '|' || COALESCE(run_id,''), char(10)), '') FROM outbox_operations ORDER BY id`,
		"events":     `SELECT COALESCE(group_concat(id || '|' || type || '|' || source, char(10)), '') FROM events ORDER BY id`,
	}
	projection := make(map[string]string, len(queries))
	for name, query := range queries {
		var value string
		if err := db.QueryRowForTest(context.Background(), query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		projection[name] = value
	}
	return projection
}

func TestOpsAttachReturnsClosedTmuxBinding(t *testing.T) {
	s, db := startServerWithDB(t)
	seedOpsAttachRun(t, db, "tmux")
	name, err := runtime.TmuxSessionName("run-attach", 1, 1, "dispatch-run-attach")
	if err != nil {
		t.Fatal(err)
	}
	s.SetTmuxObserver(installOpsAttachTmux(t, name[len("sift-"):], "present"), "/tmp/sift-private.sock")

	response := opsAttachRequest(s, "run-attach")
	if !response.OK {
		t.Fatalf("ops.attach error = %#v", response.Error)
	}
	result, ok := response.Result.(attachResult)
	if !ok {
		t.Fatalf("ops.attach result = %#v, want attachResult", response.Result)
	}
	want := attachResult{RunID: "run-attach", AttemptNo: 1, Generation: 1, Backend: "tmux", SessionName: name}
	if result != want {
		t.Fatalf("ops.attach result = %#v, want %#v", result, want)
	}
}

func TestOpsAttachRejectsIdentityDriftDuringObservation(t *testing.T) {
	s, db := startServerWithDB(t)
	seedOpsAttachRun(t, db, "tmux")
	name, err := runtime.TmuxSessionName("run-attach", 1, 1, "dispatch-run-attach")
	if err != nil {
		t.Fatal(err)
	}
	s.SetTmuxObserver("/tmux", "/tmp/sift-private.sock")

	observerCalls := 0
	var afterRecovery map[string]string
	s.tmuxObserver = func(_ context.Context, tmuxPath, socketPath, gotName, digest string) error {
		observerCalls++
		if tmuxPath != "/tmux" || socketPath != "/tmp/sift-private.sock" || gotName != name || digest != name[len("sift-"):] {
			t.Fatalf("observer inputs = (%q, %q, %q, %q)", tmuxPath, socketPath, gotName, digest)
		}
		if err := db.AdvanceAttachIdentityForTest(context.Background(), "run-attach", 1, 2, "recovery-dispatch"); err != nil {
			t.Fatal(err)
		}
		afterRecovery = opsAttachDomainProjection(t, db)
		return nil
	}

	response := opsAttachRequest(s, "run-attach")
	if response.OK || response.Error.Code != "conflict" || response.Error.Retryable || response.Result != nil {
		t.Fatalf("ops.attach = %#v, want non-retryable conflict without old session", response)
	}
	if observerCalls != 1 {
		t.Fatalf("observer calls = %d, want 1", observerCalls)
	}
	if after := opsAttachDomainProjection(t, db); !reflect.DeepEqual(after, afterRecovery) {
		t.Fatalf("ops.attach revalidation wrote durable domain state:\nbefore=%#v\nafter =%#v", afterRecovery, after)
	}
}

func TestAttachOpsAttachStaleGenerationFailsClosedBeforeObservation(t *testing.T) {
	s, db := startServerWithDB(t)
	seedOpsAttachRun(t, db, "tmux")
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecForTest(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`UPDATE attempt_claims SET generation=2, dispatch_id='stale-dispatch' WHERE run_id='run-attach' AND attempt_no=1`)

	counter := filepath.Join(t.TempDir(), "tmux-observer-called")
	tmux := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nprintf x >> '" + counter + "'\nexit 0\n"
	if err := os.WriteFile(tmux, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.SetTmuxObserver(tmux, "/tmp/sift-private.sock")

	response := opsAttachRequest(s, "run-attach")
	if response.OK || response.Error.Code != "conflict" || response.Error.Retryable {
		t.Fatalf("ops.attach = %#v, want non-retryable conflict", response)
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatalf("tmux observer call marker exists: %v", err)
	}
}

func TestOpsAttachFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runID    string
		backend  string
		mode     string
		terminal bool
		want     string
	}{
		{name: "missing", runID: "missing", want: "not_found"},
		{name: "process backend", runID: "run-attach", backend: "process", want: "conflict"},
		{name: "terminal", runID: "run-attach", backend: "tmux", terminal: true, want: "conflict"},
		{name: "session absent", runID: "run-attach", backend: "tmux", mode: "absent", want: "conflict"},
		{name: "binding mismatch", runID: "run-attach", backend: "tmux", mode: "binding-mismatch", want: "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := startServerWithDB(t)
			if tc.runID != "missing" {
				seedOpsAttachRun(t, db, tc.backend)
				if tc.terminal {
					if err := db.SeedFailedAttemptForTest(context.Background(), tc.runID, 1, cpNow+1); err != nil {
						t.Fatal(err)
					}
				}
				if tc.backend == "tmux" && !tc.terminal {
					name, err := runtime.TmuxSessionName(tc.runID, 1, 1, "dispatch-"+tc.runID)
					if err != nil {
						t.Fatal(err)
					}
					s.SetTmuxObserver(installOpsAttachTmux(t, name[len("sift-"):], tc.mode), "/tmp/sift-private.sock")
				}
			}
			response := opsAttachRequest(s, tc.runID)
			if response.OK || response.Error.Code != tc.want || response.Error.Retryable {
				t.Fatalf("ops.attach = %#v, want non-retryable %s", response, tc.want)
			}
		})
	}
}

func TestOpsAttachRejectsClosedParamsAndUnauthorized(t *testing.T) {
	s, _ := startServerWithDB(t)
	for _, request := range []Request{
		{RequestID: opsAttachRequestID, Method: "ops.attach", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": "run", "session_name": "attacker"}},
		{RequestID: opsAttachRequestID, Method: "ops.attach", Auth: Auth{Kind: "operator", Token: "wrong"}, Params: map[string]any{"run_id": "run"}},
	} {
		response := s.operatorRequest(request)
		if response.OK || response.Error.Code != "invalid_request" && response.Error.Code != "unauthorized" {
			t.Fatalf("ops.attach closed request = %#v", response)
		}
	}
}
