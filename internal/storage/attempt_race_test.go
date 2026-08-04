package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/command"
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

func TestV4HumanStateInterleavingsIncludeLateResults(t *testing.T) {
	agent := AgentIdentity{PID: 101, StartedAtMS: testNow + 1, Executable: "/agent"}
	exit := 0
	cases := []struct {
		name       string
		first      string
		wantResult string
		wantClose  string
	}{
		{"interrupt_before_result", "interrupt", AttemptRaceDuplicate, "superseded_by_fact"},
		{"result_before_interrupt", "result", AttemptRaceSupersededByFact, "superseded_by_fact"},
		{"decision_before_result", "decision", AttemptRaceSupersededByDecision, "responded"},
		{"result_before_decision", "result", AttemptRaceSupersededByFact, "superseded_by_fact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx, version := freezeAttemptForRace(t)
			result := &AttemptResult{Agent: agent, ExitCode: &exit, Digest: "late-" + tc.name, FinishedAtMS: testNow + 1}
			if tc.first == "interrupt" {
				if _, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version, FactKey: "late-interrupt-" + tc.name, NowMS: testNow + 1, Agent: &agent}); err != nil {
					t.Fatal(err)
				}
			} else if tc.first == "decision" {
				if _, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version, FactKey: "late-decision-" + tc.name, NowMS: testNow + 1, Reject: true}); err != nil {
					t.Fatal(err)
				}
			} else {
				got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version, FactKey: "late-result-" + tc.name, NowMS: testNow + 1, Agent: &agent, Result: result})
				if err != nil || got != tc.wantResult {
					t.Fatalf("first result=%q err=%v", got, err)
				}
			}
			if tc.first == "interrupt" {
				got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, FactKey: "late-result-" + tc.name, NowMS: testNow + 2, Agent: &agent, Result: result})
				if err != nil || got != tc.wantResult {
					t.Fatalf("late result=%q err=%v", got, err)
				}
			} else if tc.first == "decision" {
				got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, FactKey: "late-result-" + tc.name, NowMS: testNow + 2, Agent: &agent, Result: result})
				if err != nil || got != tc.wantResult {
					t.Fatalf("late result=%q err=%v", got, err)
				}
			} else if tc.name == "result_before_decision" {
				run, err := db.Run(ctx, "run")
				if err != nil {
					t.Fatal(err)
				}
				got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: run.Version, FactKey: "late-decision-" + tc.name, NowMS: testNow + 2, Reject: true})
				if err != nil || got != AttemptRaceDecisionApplied {
					t.Fatalf("late decision=%q err=%v", got, err)
				}
			}
			var status, closeReason string
			if err := db.db.QueryRow(`SELECT status,COALESCE(close_reason,'') FROM interrupts WHERE run_id='run'`).Scan(&status, &closeReason); err != nil {
				t.Fatal(err)
			}
			if closeReason != tc.wantClose || status != "closed" {
				t.Fatalf("interrupt=%s/%s", status, closeReason)
			}
		})
	}
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
		Result: &AttemptResult{Agent: agent, ExitCode: &exit, FailureReason: "agent_log_relay_failed", Digest: "digest", FinishedAtMS: testNow + 1},
	})
	if err != nil || got != AttemptRaceSupersededByFact {
		t.Fatalf("result = %q, %v", got, err)
	}
	var phase, digest, failureReason string
	if err := db.db.QueryRow(`SELECT phase,result_digest,result_failure_reason FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&phase, &digest, &failureReason); err != nil {
		t.Fatal(err)
	}
	if phase != "finished" || digest != "digest" || failureReason != "agent_log_relay_failed" {
		t.Fatalf("phase/digest/failure_reason = %q/%q/%q", phase, digest, failureReason)
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

// TestV4HumanStateInterleavingsNoInitialInterrupt covers the
// `result_before_interrupt` half of the spec when there is no pre-existing
// startup_stall Interrupt. The existing subcase in
// TestV4HumanStateInterleavingsIncludeLateResults always runs after
// freezeAttemptForRace, which already creates an open Interrupt, so the
// "result first" branch always closes an existing interrupt. This new
// function drives the same semantics from a clean slate: a result commits
// first (no Interrupt exists), then RecordTerminationObservation must be a
// stale/no-op — the run-version CAS bumped by ResolveAttemptRace rejects it
// — leaving the interrupt count at zero. The reverse order
// (`interrupt_before_result`) keeps the existing semantic: the termination
// observation creates the Interrupt first, and the late result closes it as
// `superseded_by_fact`.
func TestV4HumanStateInterleavingsNoInitialInterrupt(t *testing.T) {
	agent := AgentIdentity{PID: 101, StartedAtMS: testNow + 1, Executable: "/agent"}
	exit := 0
	result := &AttemptResult{Agent: agent, ExitCode: &exit, Digest: "no-init-interrupt", FinishedAtMS: testNow + 2}

	t.Run("result_before_interrupt", func(t *testing.T) {
		db, ctx := seedTerminationAttempt(t)
		mustExec(t, db, `UPDATE attempts SET phase='spawning' WHERE run_id='run' AND attempt_no=1`)
		// Commit a legal result first: spawning -> running -> finished in one
		// transaction; the run version increments because the status transition
		// queued -> running is recorded. There is no open Interrupt at this
		// point, so disposition is plain running (no superseded_by_fact).
		got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
			RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: 1,
			FactKey: "no-init-result", NowMS: testNow + 1, Agent: &agent, Result: result,
		})
		if err != nil || got != AttemptRaceRunning {
			t.Fatalf("first result = %q err=%v", got, err)
		}
		var runStatus, attemptPhase, attemptResolution string
		if err := db.db.QueryRow(`SELECT status FROM runs WHERE id='run'`).Scan(&runStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT phase,COALESCE(attempt_resolution,'') FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&attemptPhase, &attemptResolution); err != nil {
			t.Fatal(err)
		}
		if runStatus != string(RunRunning) {
			t.Fatalf("run status after first result = %q, want running", runStatus)
		}
		if attemptPhase != "finished" || attemptResolution != "" {
			t.Fatalf("attempt phase/resolution = %q/%q, want finished/empty", attemptPhase, attemptResolution)
		}
		// A late RecordTerminationObservation must be stale/no-op: the run
		// version is now 2, the helper passes ExpectedRunVersion=1 so the
		// EmitInterrupt CAS rejects with ErrRejectedStale.
		_, err = db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{
			RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1,
			Source: TerminationRecovery, DiagnosticCause: "termination_unconfirmed",
			AttentionDailyQuota: interruptQuota(), NowMS: testNow + 3,
		})
		if !errors.Is(err, ErrRejectedStale) {
			t.Fatalf("late termination observation error = %v, want ErrRejectedStale", err)
		}
		// No Interrupt was created at any point in this ordering.
		assertCount(t, db, "interrupts WHERE run_id='run'", 0)
	})
	t.Run("interrupt_before_result", func(t *testing.T) {
		db, ctx := seedTerminationAttempt(t)
		mustExec(t, db, `UPDATE attempts SET phase='spawning' WHERE run_id='run' AND attempt_no=1`)
		// Termination observation first: it creates the startup_stall Interrupt
		// (waiting_human) and bumps the run version 1 -> 2.
		if _, err := db.RecordTerminationObservation(ctx, RecordTerminationObservationCmd{
			RunID: "run", AttemptNo: 1, ExpectedRunVersion: 1, ExpectedGeneration: 1,
			Source: TerminationRecovery, DiagnosticCause: "termination_unconfirmed",
			AttentionDailyQuota: interruptQuota(), NowMS: testNow + 1,
		}); err != nil {
			t.Fatalf("termination observation = %v", err)
		}
		assertCount(t, db, "interrupts WHERE run_id='run'", 1)
		// A legal result then arrives. The interrupt is open+waiting_human so
		// the race primitive closes it as superseded_by_fact and the attempt
		// goes running -> finished in the same transaction.
		got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
			RunID: "run", AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: 2,
			FactKey: "no-init-interrupt-then-result", NowMS: testNow + 2, Agent: &agent, Result: result,
		})
		if err != nil || got != AttemptRaceSupersededByFact {
			t.Fatalf("result after interrupt = %q err=%v", got, err)
		}
		var status, closeReason, attemptPhase string
		if err := db.db.QueryRow(`SELECT status,COALESCE(close_reason,'') FROM interrupts WHERE run_id='run'`).Scan(&status, &closeReason); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT phase FROM attempts WHERE run_id='run' AND attempt_no=1`).Scan(&attemptPhase); err != nil {
			t.Fatal(err)
		}
		if status != "closed" || closeReason != "superseded_by_fact" || attemptPhase != "finished" {
			t.Fatalf("interrupt/attempt = %s/%s/%s, want closed/superseded_by_fact/finished", status, closeReason, attemptPhase)
		}
	})
}

// TestResolveAttemptRaceProbeStartedFactWinsInvalidates closes X15
// (docs/testing/runtime-matrix.md X15, specs/command.md §5 fact-wins):
// when a legal started fact commits while a startup_stall retry probe is in
// flight, the Interrupt closes superseded_by_fact, the probe becomes
// superseded, and exactly one superseded_by_fact command ack is created. A
// replay of the same fact or any later fact touching the same bound attempt
// must not increment the probe, ack or final-event counts.
func TestResolveAttemptRaceProbeStartedFactWinsInvalidates(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)
	mustExec(t, db, `UPDATE attempts SET phase='spawning' WHERE run_id=? AND attempt_no=1`, cmdRun)

	// Two-phase retry request: persists the initial retry event, creates a
	// pending probe, and flips the Interrupt into probe_in_progress.
	env := commentEnv(t, "project", "x15-retry", "/sift retry "+cmdRun+" "+nonce)
	retryRes, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("retry request: %v", err)
	}
	if retryRes.Outcome != command.OutcomeRetryPending {
		t.Fatalf("retry outcome = %s, want retry_pending", retryRes.Outcome)
	}
	var probeID, initialEventID string
	if err := db.db.QueryRow(`SELECT id,requested_by_event_id FROM attempt_probes WHERE interrupt_id=? AND state='pending'`, interruptID).Scan(&probeID, &initialEventID); err != nil {
		t.Fatalf("locate pending probe: %v", err)
	}

	// Read the run version after the retry request so the started fact's
	// ExpectedRunVersion matches the post-emit projection (the Interrupt
	// emit and the retry request both bump the version).
	var version int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&version); err != nil {
		t.Fatal(err)
	}

	// Legal started fact commits while the probe is still pending.
	agent := AgentIdentity{PID: 4242, StartedAtMS: testNow + 50, Executable: "/agent"}
	got, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
		RunID: cmdRun, AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: version,
		FactKey: "x15-started-fact", NowMS: testNow + 100, Agent: &agent,
	})
	if err != nil || got != AttemptRaceSupersededByFact {
		t.Fatalf("fact race = %q err=%v", got, err)
	}

	// Run must be running and the bound attempt must be running; the Interrupt
	// is closed with the expected close_reason and the probe is superseded.
	var runStatus, attemptPhase string
	if err := db.db.QueryRow(`SELECT status FROM runs WHERE id=?`, cmdRun).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT phase FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&attemptPhase); err != nil {
		t.Fatal(err)
	}
	if runStatus != string(RunRunning) {
		t.Fatalf("run status after fact wins = %q, want running", runStatus)
	}
	if attemptPhase != "running" {
		t.Fatalf("attempt phase = %q, want running", attemptPhase)
	}

	var interruptStatus, closeReason, dispatchState string
	if err := db.db.QueryRow(`SELECT status,COALESCE(close_reason,''),dispatch_state FROM interrupts WHERE id=?`, interruptID).Scan(&interruptStatus, &closeReason, &dispatchState); err != nil {
		t.Fatal(err)
	}
	if interruptStatus != "closed" || closeReason != "superseded_by_fact" {
		t.Fatalf("interrupt = %s/%s, want closed/superseded_by_fact", interruptStatus, closeReason)
	}

	var probeState string
	var finishedMS int64
	if err := db.db.QueryRow(`SELECT state,COALESCE(finished_at_ms,0) FROM attempt_probes WHERE id=?`, probeID).Scan(&probeState, &finishedMS); err != nil {
		t.Fatal(err)
	}
	if probeState != "superseded" || finishedMS != testNow+100 {
		t.Fatalf("probe = state=%s finished=%d, want superseded/%d", probeState, finishedMS, testNow+100)
	}

	// Exactly one superseded_by_fact command ack (no applied/auto_reject/etc.).
	var acks int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack'`).Scan(&acks); err != nil {
		t.Fatal(err)
	}
	if acks != 1 {
		t.Fatalf("command_ack operations = %d, want exactly 1", acks)
	}
	var bad int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack' AND payload_json NOT LIKE '%"disposition":"superseded_by_fact"%'`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d command_ack operation(s) lack superseded_by_fact disposition", bad)
	}

	// The outcome relation moved pending -> final and the final event is the
	// retry-final for that event_key with stage final:fact-wins.
	var outcomeState, finalEventID string
	if err := db.db.QueryRow(`SELECT state,COALESCE(final_event_id,'') FROM command_event_outcomes WHERE initial_event_id=?`, initialEventID).Scan(&outcomeState, &finalEventID); err != nil {
		t.Fatal(err)
	}
	if outcomeState != "final" || finalEventID == "" {
		t.Fatalf("outcome = state=%s final=%s, want final/non-empty", outcomeState, finalEventID)
	}
	var idempotencyKey string
	if err := db.db.QueryRow(`SELECT idempotency_key FROM events WHERE id=?`, finalEventID).Scan(&idempotencyKey); err != nil {
		t.Fatal(err)
	}
	wantStage := "command:" + retryRes.FinalEventID + ":final:fact-wins"
	if idempotencyKey != wantStage {
		// retryRes.FinalEventID is the *initial* event_id we returned; the stage
		// key is built from the event_key, not the id. Look it up directly.
		var eventKey string
		if err := db.db.QueryRow(`SELECT event_key FROM command_event_outcomes WHERE initial_event_id=?`, initialEventID).Scan(&eventKey); err != nil {
			t.Fatal(err)
		}
		wantStage = "command:" + eventKey + ":final:fact-wins"
		if idempotencyKey != wantStage {
			t.Fatalf("final idempotency_key = %q, want %q", idempotencyKey, wantStage)
		}
	}

	// Replay of the same fact: ResolveAttemptRace idempotency CAS rejects it
	// before the fact-wins writer runs; counts must not move. The first call
	// bumped the run version (waiting_human -> running), so the replay must
	// pass the new version; an old version would be a stale CAS rejection,
	// which is also acceptable evidence that the helper was not re-entered.
	var postVersion int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&postVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveAttemptRace(ctx, AttemptRaceCommand{
		RunID: cmdRun, AttemptNo: 1, ExpectedGeneration: 1, ExpectedRunVersion: postVersion,
		FactKey: "x15-started-fact", NowMS: testNow + 200, Agent: &agent,
	}); err != nil {
		t.Fatalf("replay fact race: %v", err)
	}
	if err := db.db.QueryRow(`SELECT state FROM attempt_probes WHERE id=?`, probeID).Scan(&probeState); err != nil {
		t.Fatal(err)
	}
	if probeState != "superseded" {
		t.Fatalf("probe after replay = %s, want superseded (idempotency must hold)", probeState)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack'`).Scan(&acks); err != nil {
		t.Fatal(err)
	}
	if acks != 1 {
		t.Fatalf("command_ack after replay = %d, want 1 (no duplicate ack)", acks)
	}
	var outcomes int
	if err := db.db.QueryRow(`SELECT count(*) FROM command_event_outcomes WHERE initial_event_id=?`, initialEventID).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != 1 {
		t.Fatalf("command_event_outcomes rows after replay = %d, want 1 (no duplicate outcome CAS)", outcomes)
	}
}
