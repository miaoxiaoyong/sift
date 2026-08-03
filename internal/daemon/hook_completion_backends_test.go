package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHookCompletionRecheckConsumesProcessAndTmuxResults(t *testing.T) {
	for _, backend := range []string{"process", "tmux"} {
		t.Run(backend, func(t *testing.T) {
			db, raw, attempt, now := seedRecoveryCoordinator(t, "running", 0)
			if _, err := db.ExecForTest(context.Background(), `UPDATE attempts SET backend=? WHERE run_id='run' AND attempt_no=1`, backend); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			path := filepath.Join(root, "runs", "run", "attempts", "1", "result.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			result, _ := json.Marshal(map[string]any{"schema_version": 1, "run_id": "run", "attempt_no": 1, "generation": 1, "wrapper_instance_id": "instance", "agent_identity": map[string]any{"pid": 11, "started_at_ms": 1001, "executable": "/agent"}, "exit_code": 0, "signal": nil, "finished_at_ms": now.UnixMilli()})
			if err := os.WriteFile(path, result, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			coordinator := &TerminationCoordinator{DB: db, ControlRoot: root, Now: func() time.Time { return now }, HookRecheck: func(ctx context.Context, runID string, attemptNo int) error {
				calls++
				return db.CompleteHookRecheck(ctx, runID, attemptNo, now.UnixMilli())
			}}
			if _, err := coordinator.resolveLateFact(context.Background(), attempt); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := raw.QueryRow(`SELECT state FROM hook_recheck_receipts WHERE run_id='run' AND attempt_no=1`).Scan(&state); err != nil || state != "completed" || calls != 1 {
				t.Fatalf("hook receipt/calls = %q/%d, %v", state, calls, err)
			}
		})
	}
}
