package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/controlplane"
)

// psServe serves one ops.ps request and replies with the given run list. The
// daemon is asked for ALL runs (status=nil); the CLI filters client-side, so
// the same fixture exercises every selection.
func psServe(t *testing.T, home string, runs []any) {
	t.Helper()
	serveFakeDoctorResponse(t, home, func(req controlplane.Request) map[string]any {
		if got := req.Params["status"]; got != nil {
			t.Errorf("ops.ps status param = %#v, want nil (CLI filters client-side)", got)
		}
		return fakeDoctorSuccess(req.RequestID, map[string]any{"runs": runs, "next_after_run_id": nil, "attention_remaining": map[string]int{"low": 0, "normal": 0, "high": 0}, "channel_deliveries": []any{}})
	})
}

func runRow(id, status string) map[string]any {
	return map[string]any{"run_id": id, "project_id": "p", "status": status, "version": float64(1), "agent_id": "a", "attempt_no": float64(1), "phase": "running"}
}

// mixedRuns is one run per status, ordered so the filtered view is obvious.
func mixedRuns() []any {
	return []any{
		runRow("r-queued", "queued"),
		runRow("r-running", "running"),
		runRow("r-waiting", "waiting_human"),
		runRow("r-done", "done"),
		runRow("r-failed", "failed"),
	}
}

// TestPsDefaultShowsActiveOnly: `sift ps` lists only non-terminal runs.
func TestPsDefaultShowsActiveOnly(t *testing.T) {
	home := freshHome(t)
	psServe(t, home, mixedRuns())
	var out bytes.Buffer
	if code := run([]string{"sift", "ps", "--ids"}, &out, io.Discard); code != 0 {
		t.Fatalf("ps exit = %d; output=%q", code, out.String())
	}
	want := "r-queued\nr-running\nr-waiting"
	if got := strings.TrimRight(out.String(), "\n"); got != want {
		t.Fatalf("ps default ids = %q, want %q", out.String(), want)
	}
}

// TestPsAllShowsEverything: `sift ps -a` (and --all) include terminal runs.
func TestPsAllShowsEverything(t *testing.T) {
	for _, args := range [][]string{{"sift", "ps", "-a", "--ids"}, {"sift", "ps", "--all", "--ids"}} {
		t.Run(strings.Join(args[1:3], " "), func(t *testing.T) {
			home := freshHome(t)
			psServe(t, home, mixedRuns())
			var out bytes.Buffer
			if code := run(args, &out, io.Discard); code != 0 {
				t.Fatalf("exit = %d; output=%q", code, out.String())
			}
			want := "r-queued\nr-running\nr-waiting\nr-done\nr-failed"
			if got := strings.TrimRight(out.String(), "\n"); got != want {
				t.Fatalf("ps -a ids = %q, want %q", out.String(), want)
			}
		})
	}
}

// TestPsStatusExact: --status keeps only the matching status.
func TestPsStatusExact(t *testing.T) {
	home := freshHome(t)
	psServe(t, home, mixedRuns())
	var out bytes.Buffer
	if code := run([]string{"sift", "ps", "--status", "failed", "--ids"}, &out, io.Discard); code != 0 {
		t.Fatalf("ps --status exit = %d", code)
	}
	if got := strings.TrimRight(out.String(), "\n"); got != "r-failed" {
		t.Fatalf("ps --status failed ids = %q, want r-failed", out.String())
	}
}

// TestPsIdsOmitsTerminalByDefault is the scripting idiom: only active ids.
func TestPsIdsOmitsTerminalByDefault(t *testing.T) {
	home := freshHome(t)
	psServe(t, home, []any{runRow("run-a", "running"), runRow("run-b", "failed"), runRow("run-c", "queued")})
	var out bytes.Buffer
	if code := run([]string{"sift", "ps", "--ids"}, &out, io.Discard); code != 0 {
		t.Fatalf("ps --ids exit = %d", code)
	}
	if got := strings.TrimRight(out.String(), "\n"); got != "run-a\nrun-c" {
		t.Fatalf("ps --ids = %q, want run-a\\nrun-c", out.String())
	}
}

// TestPsRejectsPositionalArg keeps the contract tight (ps takes no run-id).
func TestPsRejectsPositionalArg(t *testing.T) {
	home := freshHome(t)
	serveFakeDoctorResponse(t, home, func(req controlplane.Request) map[string]any {
		return fakeDoctorSuccess(req.RequestID, map[string]any{"runs": []any{}})
	})
	var out bytes.Buffer
	if code := run([]string{"sift", "ps", "run-1"}, io.Discard, &out); code != 2 || !strings.Contains(out.String(), "usage") {
		t.Fatalf("ps positional exit=%d out=%q, want 2 + usage", code, out.String())
	}
}
