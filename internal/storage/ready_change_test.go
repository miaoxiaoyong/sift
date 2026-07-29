package storage

import (
	"context"
	"testing"
)

func TestReadyChangeCandidatesRequireCompleteSuccessfulEvidence(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "run", "project", "cfg", testNow, "/tmp/worktree"); err != nil {
		t.Fatal(err)
	}
	q := `UPDATE attempts SET phase='finished',agent_pid=123,agent_started_at_ms=?,agent_executable='/bin/agent',
		result_exit_code=0,result_signal=NULL,final_head_sha=?,result_digest='digest',result_observed_at_ms=?,finished_at_ms=? WHERE run_id='run' AND attempt_no=1`
	if _, err := db.db.ExecContext(ctx, q, testNow, "0123456789012345678901234567890123456789", testNow, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := db.ReadyChangeCandidates(ctx, "project")
	if err != nil || len(got) != 1 {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
	if got[0].RunID != "run" || got[0].ExitCode != 0 || got[0].Agent.Executable != "/bin/agent" {
		t.Fatalf("candidate=%+v", got[0])
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE attempts SET result_exit_code=NULL,result_signal='TERM' WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
	got, err = db.ReadyChangeCandidates(ctx, "project")
	if err != nil || len(got) != 0 {
		t.Fatalf("signaled candidates=%+v err=%v", got, err)
	}
}
