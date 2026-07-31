package storage

import (
	"context"
	"errors"
	"testing"
)

func freezeAttemptForRace(t *testing.T) (*DB, context.Context, int64) {
	t.Helper()
	db, ctx := seedTerminationAttempt(t)
	if _, err := db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{
		RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1,
		Source: TerminationRecovery, DiagnosticCause: "termination_unconfirmed",
		AttentionDailyQuota: interruptQuota(), NowMS: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE attempts SET phase='spawning' WHERE run_id='run' AND attempt_no=1`)
	run, err := db.Run(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	return db, ctx, run.Version
}

func TestV4HumanStateInterleavings(t *testing.T) {
	agent := AgentIdentity{PID: 101, StartedAtMS: testNow + 1, Executable: "/agent"}
	t.Run("interrupt_commits_before_started", func(t *testing.T) {
		db, ctx, version := freezeAttemptForRace(t)
		got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version, FactKey: "v4-after-interrupt", NowMS: testNow + 1, Agent: &agent})
		if err != nil || got != AttemptRaceSupersededByFact {
			t.Fatalf("disposition=%s err=%v", got, err)
		}
	})
	t.Run("started_commits_before_interrupt", func(t *testing.T) {
		db, ctx := seedTerminationAttempt(t)
		mustExec(t, db, `UPDATE attempts SET phase='spawning' WHERE run_id='run' AND attempt_no=1`)
		got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: 1, FactKey: "v4-before-interrupt", NowMS: testNow + 1, Agent: &agent})
		if err != nil || got != AttemptRaceRunning {
			t.Fatalf("disposition=%s err=%v", got, err)
		}
		_, err = db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1, Source: TerminationRecovery, DiagnosticCause: "termination_unconfirmed", AttentionDailyQuota: interruptQuota(), NowMS: testNow + 2})
		if !errors.Is(err, ErrRejectedStale) {
			t.Fatalf("late interrupt transaction error=%v", err)
		}
		assertCount(t, db, "interrupts WHERE run_id='run'", 0)
	})
	t.Run("decision_commits_before_started", func(t *testing.T) {
		db, ctx, version := freezeAttemptForRace(t)
		got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version, FactKey: "v4-decision-first", NowMS: testNow + 1, Reject: true})
		if err != nil || got != AttemptRaceDecisionApplied {
			t.Fatalf("decision=%s err=%v", got, err)
		}
		got, err = db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, FactKey: "v4-late-started", NowMS: testNow + 2, Agent: &agent})
		if err != nil || got != AttemptRaceSupersededByDecision {
			t.Fatalf("late fact=%s err=%v", got, err)
		}
	})
	t.Run("started_commits_before_decision", func(t *testing.T) {
		db, ctx, version := freezeAttemptForRace(t)
		got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version, FactKey: "v4-fact-first", NowMS: testNow + 1, Agent: &agent})
		if err != nil || got != AttemptRaceSupersededByFact {
			t.Fatalf("fact=%s err=%v", got, err)
		}
		run, err := db.Run(ctx, "run")
		if err != nil {
			t.Fatal(err)
		}
		got, err = db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: run.Version, FactKey: "v4-late-decision", NowMS: testNow + 2, Reject: true})
		if err != nil || got != AttemptRaceDecisionApplied {
			t.Fatalf("late decision=%s err=%v", got, err)
		}
	})
}

func TestResolveAttemptRaceFactWinsWhileFrozen(t *testing.T) {
	db, ctx, version := freezeAttemptForRace(t)
	got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
		RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version,
		FactKey: "recovery-started", NowMS: testNow + 1,
		Agent: &AgentIdentity{PID: 101, StartedAtMS: testNow + 1, Executable: "/agent"},
	})
	if err != nil || got != AttemptRaceSupersededByFact {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	run, err := db.Run(ctx, "run")
	if err != nil || run.Status != RunRunning {
		t.Fatalf("run = %#v, %v", run, err)
	}
	var phase, isolation, interruptStatus, closeReason string
	if err := db.db.QueryRow(`SELECT phase,isolation_state FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase, &isolation); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT status,close_reason FROM interrupts WHERE run_id='run'`).Scan(&interruptStatus, &closeReason); err != nil {
		t.Fatal(err)
	}
	if phase != "running" || isolation != "frozen" || interruptStatus != "closed" || closeReason != "superseded_by_fact" {
		t.Fatalf("phase/isolation/interrupt/close = %q/%q/%q/%q", phase, isolation, interruptStatus, closeReason)
	}
}

func TestResolveAttemptRacePersistsLateResult(t *testing.T) {
	db, ctx, version := freezeAttemptForRace(t)
	exit := 0
	agent := AgentIdentity{PID: 101, StartedAtMS: testNow + 1, Executable: "/agent"}
	got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
		RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version,
		FactKey: "late-result:digest", NowMS: testNow + 1, Agent: &agent,
		Result: &AttemptResult{Agent: agent, ExitCode: &exit, Digest: "digest", FinishedAtMS: testNow + 1},
	})
	if err != nil || got != AttemptRaceSupersededByFact {
		t.Fatalf("result = %q, %v", got, err)
	}
	var phase, digest string
	if err := db.db.QueryRow(`SELECT phase,result_digest FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase, &digest); err != nil {
		t.Fatal(err)
	}
	if phase != "finished" || digest != "digest" {
		t.Fatalf("phase/digest = %q/%q", phase, digest)
	}
}

func TestResolveAttemptRaceDecisionAbsorbsLateFact(t *testing.T) {
	db, ctx, version := freezeAttemptForRace(t)
	got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
		RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version,
		FactKey: "interrupt-reject", NowMS: testNow + 1, Reject: true,
	})
	if err != nil || got != AttemptRaceDecisionApplied {
		t.Fatalf("reject = %q, %v", got, err)
	}
	got, err = db.ResolveAttemptRace(ctx, AttemptRaceCommand{
		RunID: "run", AttemptNo: 1, ExpectedGeneration: 1,
		FactKey: "late-result-started", NowMS: testNow + 2,
		Agent: &AgentIdentity{PID: 101, StartedAtMS: testNow + 2, Executable: "/agent"},
	})
	if err != nil || got != AttemptRaceSupersededByDecision {
		t.Fatalf("late fact = %q, %v", got, err)
	}
	run, err := db.Run(ctx, "run")
	if err != nil || run.Status != RunFailed {
		t.Fatalf("run = %#v, %v", run, err)
	}
	var resolution, isolation, closeReason string
	var pid int
	if err := db.db.QueryRow(`SELECT attempt_resolution,isolation_state,agent_pid FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&resolution, &isolation, &pid); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE run_id='run'`).Scan(&closeReason); err != nil {
		t.Fatal(err)
	}
	if resolution != "reject" || isolation != "frozen" || pid != 101 || closeReason != "responded" {
		t.Fatalf("resolution/isolation/pid/close = %q/%q/%d/%q", resolution, isolation, pid, closeReason)
	}
}
