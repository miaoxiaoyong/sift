package storage

import (
	"context"
	"errors"
	"testing"
)

func TestV1RunTransitionGraphAndCAS(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		from, to RunStatus
		ok       bool
	}{
		{RunQueued, RunRunning, true}, {RunQueued, RunWaitingHuman, true}, {RunQueued, RunFailed, true},
		{RunRunning, RunWaitingHuman, true}, {RunRunning, RunDone, true}, {RunRunning, RunFailed, true},
		{RunWaitingHuman, RunRunning, true}, {RunWaitingHuman, RunQueued, true}, {RunWaitingHuman, RunDone, true}, {RunWaitingHuman, RunFailed, true},
		{RunFailed, RunQueued, true}, {RunDone, RunQueued, false}, {RunQueued, RunDone, true}, {RunFailed, RunRunning, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"-"+string(tc.to), func(t *testing.T) {
			db, _ := openTestDB(t)
			insertConfigSnapshot(t, db, "cfg1")
			insertProject(t, db, "p1", "cfg1")
			insertManualRun(t, db, "r1", "p1", "cfg1")
			if tc.from != RunQueued {
				mustExec(t, db, `UPDATE runs SET status=?, version=2, completed_at_ms=CASE WHEN ? IN ('done','failed') THEN ? ELSE NULL END, change_id=CASE WHEN ?='done' THEN 'c1' ELSE NULL END, failure_reason=CASE WHEN ?='failed' THEN 'operator_kill' ELSE NULL END WHERE id='r1'`, tc.from, tc.from, testNow, tc.from, tc.from)
			}
			cmd := DomainCommand{To: tc.to, Source: SourceSystem, OccurredAtMS: testNow + 1}
			if tc.to == RunDone {
				cmd.ChangeID = "c2"
			}
			if tc.to == RunFailed {
				cmd.FailureReason = "operator_kill"
			}
			_, err := db.TransitionRun(ctx, "r1", map[bool]int64{true: 2, false: 1}[tc.from != RunQueued], cmd)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok && !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("err=%v, want illegal", err)
			}
		})
	}
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	_, err := db.TransitionRun(ctx, "r1", 99, DomainCommand{To: RunRunning, Source: SourceSystem, OccurredAtMS: testNow})
	if !errors.Is(err, ErrRejectedStale) {
		t.Fatalf("stale err=%v", err)
	}
}

func TestV1IllegalTransitionIsAudited(t *testing.T) {
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	_, err := db.TransitionRun(context.Background(), "r1", 1, DomainCommand{To: RunQueued, Source: SourceOperator, OccurredAtMS: testNow})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatal(err)
	}
	var status string
	var events int
	if err := db.db.QueryRow(`SELECT status FROM runs WHERE id='r1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(RunQueued) {
		t.Fatalf("status=%s", status)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='run.transition_rejected'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events=%d", events)
	}
}

func TestV2TransitionCrashAtomicity(t *testing.T) {
	// A statement failure after the CAS update (duplicate stable operation key)
	// rolls back projection, event and outbox together.
	db, _ := openTestDB(t)
	insertConfigSnapshot(t, db, "cfg1")
	insertProject(t, db, "p1", "cfg1")
	insertManualRun(t, db, "r1", "p1", "cfg1")
	insertOutboxOperation(t, db, "o1", "comment:interrupt:r1:1")
	_, err := db.TransitionRun(context.Background(), "r1", 1, DomainCommand{To: RunRunning, Source: SourceSystem, OccurredAtMS: testNow + 1, Operation: &Operation{Key: "comment:interrupt:r1:1", Kind: OperationForgeComment, Payload: []byte(`{"different":true}`)}})
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("err=%v", err)
	}
	r, err := db.Run(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RunQueued || r.Version != 1 {
		t.Fatalf("run after rollback=%+v", r)
	}
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE run_id='r1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("events=%d", count)
	}
}
