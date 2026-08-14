package storage

import (
	"context"
	"database/sql"
	"testing"

	"github.com/xsift/sift/internal/command"
)

// This file closes WBS §5.4's unchecked startup_stall item end-to-end: a real
// retry probe failure (ApplyRetryProbeResult Succeeded=false) must keep the same
// Interrupt open, rotate the nonce, emit the absence_unconfirmed ack, and leave
// the Interrupt escalate-able through the unique AdvanceInterrupt / supervisor
// tick expiry path until max_escalations, where startup_stall must hold (never
// auto_reject) without writing a resolution. At the cap a probe failure applies
// the frozen capped hold itself (specs/command.md §5), with no second advance.

// emitStartupStallForE2E emits a startup_stall Interrupt with a configurable
// escalation cap/expiry and freezes isolation exactly like the production
// RecordTerminationObservation path (interrupt.md §6).
func emitStartupStallForE2E(t *testing.T, db *DB, ctx context.Context, expiresAfterMS int64, maxEscalations int) (string, string) {
	t.Helper()
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: expiresAfterMS, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: maxEscalations,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceRecovery, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	})
	if err != nil {
		t.Fatalf("emit startup_stall: %v", err)
	}
	mustExec(t, db, `UPDATE attempts SET isolation_state='frozen',isolation_reason='startup_stall',isolated_at_ms=? WHERE run_id=? AND attempt_no=1`, testNow, cmdRun)
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	return in.ID, nonce
}

// retryProbeFail drives the two-phase startup_stall retry request and then marks
// the probe failed via the unique ApplyRetryProbeResult port. It asserts the
// absence_unconfirmed outcome and returns the rotated nonce.
func retryProbeFail(t *testing.T, db *DB, ctx context.Context, interruptID, nonce, remoteID string, nowMS int64) string {
	t.Helper()
	env := commentEnv(t, "project", remoteID, "/sift retry "+cmdRun+" "+nonce)
	r, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: nowMS})
	if err != nil {
		t.Fatalf("retry request %q: %v", remoteID, err)
	}
	if r.Outcome != command.OutcomeRetryPending {
		t.Fatalf("retry request %q outcome = %s, want retry_pending", remoteID, r.Outcome)
	}
	var probeID string
	if err := db.db.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=? AND state='pending'`, interruptID).Scan(&probeID); err != nil {
		t.Fatalf("locate pending probe: %v", err)
	}
	res, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: probeID, Succeeded: false, NowMS: nowMS})
	if err != nil {
		t.Fatalf("probe fail %q: %v", remoteID, err)
	}
	if res.Outcome != command.OutcomeAbsenceUnconfirmed {
		t.Fatalf("probe fail %q outcome = %s, want absence_unconfirmed", remoteID, res.Outcome)
	}
	return res.NextNonce
}

type interruptSnapshot struct {
	status, dispatchState, heldReason, closeReason, nonce, severity, delivery string
	version, escalation                                                       int64
}

func readInterruptSnapshot(t *testing.T, db *DB, id string) interruptSnapshot {
	t.Helper()
	var s interruptSnapshot
	if err := db.db.QueryRow(`SELECT status,dispatch_state,COALESCE(held_reason,''),COALESCE(close_reason,''),nonce,severity,COALESCE(delivery,''),version,escalation_count FROM interrupts WHERE id=?`, id).
		Scan(&s.status, &s.dispatchState, &s.heldReason, &s.closeReason, &s.nonce, &s.severity, &s.delivery, &s.version, &s.escalation); err != nil {
		t.Fatalf("read interrupt: %v", err)
	}
	return s
}

func assertIsolationFrozen(t *testing.T, db *DB) {
	t.Helper()
	var state, resolution sql.NullString
	if err := db.db.QueryRow(`SELECT isolation_state,attempt_resolution FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&state, &resolution); err != nil {
		t.Fatal(err)
	}
	if state.String != "frozen" {
		t.Fatalf("isolation_state = %q, want frozen", state.String)
	}
	if resolution.Valid && resolution.String != "" {
		t.Fatalf("attempt_resolution = %q, startup_stall must not write a resolution", resolution.String)
	}
}

func assertAbsenceUnconfirmedAckCount(t *testing.T, db *DB, want int) {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("command_ack operations = %d, want %d", n, want)
	}
	// Every ack produced by a probe failure carries the absence_unconfirmed
	// disposition; none of them may be applied/auto_reject.
	var bad int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='command_ack' AND payload_json NOT LIKE '%"disposition":"absence_unconfirmed"%'`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d command_ack operation(s) lack absence_unconfirmed disposition", bad)
	}
}

// TestStartupStallProbeFailureEscalatesToCapHold proves the full E2E (#796
// scope 1): a retry probe failure keeps the same Interrupt open, rotates the
// nonce and emits the absence_unconfirmed ack, then the supervisor tick drives
// AdvanceInterrupt expiry escalations until max_escalations, where startup_stall
// holds (never auto_rejects) with isolation still frozen and no resolution.
func TestStartupStallProbeFailureEscalatesToCapHold(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	const expiry = int64(10)
	const max = 2
	interruptID, n0 := emitStartupStallForE2E(t, db, ctx, expiry, max)

	// Real two-phase retry request -> probe failure at the expiry instant.
	n2 := retryProbeFail(t, db, ctx, interruptID, n0, "c1", testNow+5)
	// retryProbeFail returns the post-probe nonce; the retry itself rotated it
	// once already, so both differ from the emit nonce.
	if n2 == n0 {
		t.Fatalf("probe failure did not rotate nonce: %q == %q", n2, n0)
	}

	// Probe failure assertions: same Interrupt stays open, escalate-able
	// (batched, not the old held/NULL dead state), isolation frozen, no
	// resolution, and exactly one absence_unconfirmed ack.
	s := readInterruptSnapshot(t, db, interruptID)
	if s.status != "open" || s.dispatchState != "batched" || s.heldReason != "" {
		t.Fatalf("after probe fail = status=%s dispatch=%s held=%q, want open/batched/none", s.status, s.dispatchState, s.heldReason)
	}
	if s.escalation != 0 {
		t.Fatalf("escalation_count = %d after probe fail, want 0 (escalation is the tick's job)", s.escalation)
	}
	assertIsolationFrozen(t, db)
	assertAbsenceUnconfirmedAckCount(t, db, 1)

	// Drive expiry escalations through the supervisor tick (the I4 production
	// seam) until the cap. Each tick lands at the current expires_at so expiry
	// is the only advance that fires; dispatch candidates are CAS-stale.
	severityByCount := map[int64]string{0: string(SeverityHigh), 1: string(SeverityCritical), 2: string(SeverityCritical)}
	now := testNow + expiry
	prevNonce := s.nonce
	for step := 1; step <= max; step++ {
		if err := db.SupervisorInterruptTick(ctx, now); err != nil {
			t.Fatalf("escalate tick #%d: %v", step, err)
		}
		es := readInterruptSnapshot(t, db, interruptID)
		if es.status != "open" {
			t.Fatalf("tick #%d closed the Interrupt: %s", step, es.status)
		}
		if es.escalation != int64(step) {
			t.Fatalf("tick #%d escalation_count = %d, want %d", step, es.escalation, step)
		}
		if es.nonce == prevNonce {
			t.Fatalf("tick #%d did not rotate nonce", step)
		}
		if es.severity != severityByCount[int64(step)] {
			t.Fatalf("tick #%d severity = %s, want %s", step, es.severity, severityByCount[int64(step)])
		}
		if es.closeReason != "" {
			t.Fatalf("tick #%d wrote close_reason %q, startup_stall must not close", step, es.closeReason)
		}
		assertIsolationFrozen(t, db)
		// Escalation never creates a second ack; still only the probe ack.
		assertAbsenceUnconfirmedAckCount(t, db, 1)
		prevNonce = es.nonce
		now += expiry // expires_at resets to now+expiry on each escalate
	}

	// One more tick at the cap must hold (startup_stall forbids auto_reject),
	// keeping the Interrupt open with held_reason=max_escalations and no
	// resolution write. This is the unique AdvanceInterrupt path, not a probe.
	if err := db.SupervisorInterruptTick(ctx, now); err != nil {
		t.Fatalf("cap-hold tick: %v", err)
	}
	cap := readInterruptSnapshot(t, db, interruptID)
	if cap.status != "open" || cap.dispatchState != "held" || cap.heldReason != "max_escalations" {
		t.Fatalf("at cap = status=%s dispatch=%s held=%q, want open/held/max_escalations", cap.status, cap.dispatchState, cap.heldReason)
	}
	if cap.closeReason != "" {
		t.Fatalf("at cap close_reason = %q, startup_stall must never auto_reject", cap.closeReason)
	}
	if cap.escalation != int64(max) {
		t.Fatalf("at cap escalation_count = %d, want %d (cap holds, does not over-escalate)", cap.escalation, max)
	}
	assertIsolationFrozen(t, db)
	assertAbsenceUnconfirmedAckCount(t, db, 1)

	// A stale replay of the original probe result must be rejected; the
	// Interrupt has long since rotated past that nonce/version.
	if _, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{InterruptID: interruptID, ProbeID: "no-such-probe", Succeeded: false, NowMS: now + 1}); err != ErrRejectedStale {
		t.Fatalf("stale probe replay = %v, want ErrRejectedStale", err)
	}
}

// TestStartupStallProbeFailureAtCapAppliesFrozenCappedHold proves #796 scope 2:
// when a probe failure happens while the Interrupt is already at its escalation
// cap, probeFailedTx applies the frozen capped hold directly (command.md §5 "or
// applies frozen capped hold") — open/held/max_escalations, no auto_reject, no
// resolution — without invoking a second advance/write port.
func TestStartupStallProbeFailureAtCapAppliesFrozenCappedHold(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	const expiry = int64(10)
	const max = 1
	interruptID, n0 := emitStartupStallForE2E(t, db, ctx, expiry, max)

	// Probe failure below the cap reverts to the escalate-able batched state.
	if n1 := retryProbeFail(t, db, ctx, interruptID, n0, "c1", testNow+5); n1 == n0 {
		t.Fatalf("probe failure did not rotate nonce")
	}
	if s := readInterruptSnapshot(t, db, interruptID); s.dispatchState != "batched" || s.escalation != 0 {
		t.Fatalf("below-cap probe fail = dispatch=%s escalation=%d, want batched/0", s.dispatchState, s.escalation)
	}

	// Escalate to the cap via the unique tick path.
	if err := db.SupervisorInterruptTick(ctx, testNow+expiry); err != nil {
		t.Fatalf("escalate tick: %v", err)
	}
	atCap := readInterruptSnapshot(t, db, interruptID)
	if atCap.escalation != int64(max) {
		t.Fatalf("escalation_count = %d, want %d (at cap)", atCap.escalation, max)
	}

	// A second retry+probe failure now lands while escalation_count == max.
	// probeFailedTx must apply the frozen capped hold itself (no second advance).
	n3 := retryProbeFail(t, db, ctx, interruptID, atCap.nonce, "c2", testNow+expiry+5)
	if n3 == atCap.nonce {
		t.Fatalf("at-cap probe failure did not rotate nonce")
	}
	hold := readInterruptSnapshot(t, db, interruptID)
	if hold.status != "open" || hold.dispatchState != "held" || hold.heldReason != "max_escalations" || hold.delivery != "held" {
		t.Fatalf("at-cap probe fail = status=%s dispatch=%s held=%q delivery=%q, want open/held/max_escalations/held",
			hold.status, hold.dispatchState, hold.heldReason, hold.delivery)
	}
	if hold.closeReason != "" {
		t.Fatalf("at-cap probe fail close_reason = %q, startup_stall must never auto_reject", hold.closeReason)
	}
	if hold.escalation != int64(max) {
		t.Fatalf("at-cap probe fail escalation_count = %d, want %d (frozen hold does not over-escalate)", hold.escalation, max)
	}
	// No resolution write and isolation still frozen.
	assertIsolationFrozen(t, db)
	// Two probe failures -> two absence_unconfirmed acks; the frozen hold added
	// no auto_reject/apply ack.
	assertAbsenceUnconfirmedAckCount(t, db, 2)

	// The hold is terminal-ish for the expiry scan (held/non-manual is excluded),
	// so a later tick must not re-open, re-escalate or auto_reject it.
	if err := db.SupervisorInterruptTick(ctx, testNow+3*expiry); err != nil {
		t.Fatalf("post-hold tick: %v", err)
	}
	after := readInterruptSnapshot(t, db, interruptID)
	if after.status != "open" || after.heldReason != "max_escalations" || after.escalation != int64(max) {
		t.Fatalf("post-hold tick mutated frozen hold = status=%s held=%q escalation=%d", after.status, after.heldReason, after.escalation)
	}
}

// TestStartupStallProbeFailureStateIsEscalateableByDirectAdvance is the minimal
// storage-level proof that, below the cap, the post-probe-failure state is not
// dead: a direct AdvanceInterrupt(AdvanceExpiry) advances escalation_count via
// the unique port (no tick scanning required).
func TestStartupStallProbeFailureStateIsEscalateableByDirectAdvance(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, n0 := emitStartupStallForE2E(t, db, ctx, 10, 2)
	n1 := retryProbeFail(t, db, ctx, interruptID, n0, "c1", testNow+5)
	s := readInterruptSnapshot(t, db, interruptID)
	ok, err := db.AdvanceInterrupt(ctx, AdvanceInterruptCmd{InterruptID: interruptID, ExpectedVersion: s.version, ExpectedNonce: n1, Kind: AdvanceExpiry, NowMS: testNow + 10})
	if err != nil || !ok {
		t.Fatalf("direct AdvanceInterrupt after probe fail = %v, %v: probe failure left a dead state", ok, err)
	}
	if es := readInterruptSnapshot(t, db, interruptID); es.escalation != 1 {
		t.Fatalf("escalation_count = %d, want 1", es.escalation)
	}
}
