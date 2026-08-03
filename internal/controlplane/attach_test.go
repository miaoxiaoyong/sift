package controlplane

import (
	"context"
	"os"
	"path/filepath"
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
