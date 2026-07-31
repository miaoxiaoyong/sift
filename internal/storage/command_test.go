package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/command"
)

// cmdRun is a valid 32-lowercase-hex run id, as required by the /sift grammar.
const cmdRun = "0123456789abcdef0123456789abcdef"

func seedCommandRun(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, cmdRun, "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "task-01", cmdRun, 1)
	insertAttempt(t, db, cmdRun, 1, "task-01")
}

func emitDesignApprovalInterrupt(t *testing.T, db *DB, ctx context.Context, target command.CommandTarget) (interruptID, nonce string) {
	t.Helper()
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptDesignApproval,
		Facts:      map[string]string{"risk_summary": "risk", "recommended_action": "approve", "task_spec_ref": "/task"},
		Generation: InterruptGeneration{TaskSpecSnapshotID: "task-01"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	})
	if err != nil {
		t.Fatalf("emit design_approval: %v", err)
	}
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	return in.ID, nonce
}

func emitStartupStallInterrupt(t *testing.T, db *DB, ctx context.Context) (interruptID, nonce string) {
	t.Helper()
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptStartupStall,
		Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: 1,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceRecovery, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	})
	if err != nil {
		t.Fatalf("emit startup_stall: %v", err)
	}
	mustExec(t, db, `UPDATE attempts SET isolation_state='frozen',isolation_reason='startup_stall',isolated_at_ms=? WHERE run_id=? AND attempt_no=1`, testNow, cmdRun)
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	return in.ID, nonce
}

func commentEnv(t *testing.T, projectID, remoteID, body string) command.CommandEventEnvelopeV1 {
	t.Helper()
	key, err := command.RecomputeEventKey(projectID, command.SourceForgeComment, remoteID)
	if err != nil {
		t.Fatal(err)
	}
	actor := "alice"
	return command.CommandEventEnvelopeV1{
		SchemaVersion: 1, EventKey: key, ProjectID: projectID, Source: command.SourceForgeComment,
		RemoteEventID: remoteID, Target: command.CommandTarget{Kind: command.TargetIssue, ID: "42"},
		Actor: &actor, RawDigest: strings.Repeat("a", 64), OccurredAtMS: testNow + 1,
		Comment: &command.CommandComment{ID: remoteID, Body: body},
	}
}

func runStatus(t *testing.T, db *DB) string {
	t.Helper()
	var status string
	if err := db.db.QueryRow(`SELECT status FROM runs WHERE id=?`, cmdRun).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestApplyCommandReject(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	body := "/sift reject " + cmdRun + " " + nonce + " too risky"
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s, want applied", res.Outcome)
	}
	if status := runStatus(t, db); status != "failed" {
		t.Fatalf("run status = %s, want failed", status)
	}
	var reason string
	db.db.QueryRow(`SELECT failure_reason FROM runs WHERE id=?`, cmdRun).Scan(&reason)
	if reason != "human_reject" {
		t.Fatalf("failure_reason = %s", reason)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
	assertCount(t, db, "ledger_entries WHERE entry_kind='human_decision'", 1)
	assertCount(t, db, "ledger_entries WHERE entry_kind='semantic_material'", 1)
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 1)
	assertCount(t, db, "command_receipts WHERE disposition='accepted'", 1)
	if res.AckOperationKey != command.AckOperationKey(env.EventKey) || res.FinalEventID == "" {
		t.Fatalf("result missing ack/event: %+v", res)
	}
}

func TestApplyCommandHoldRotatesNonce(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	body := "/sift hold " + cmdRun + " " + nonce + " 30m"
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied || res.NextNonce == "" || res.NextNonce == nonce {
		t.Fatalf("hold outcome/nonce wrong: %+v", res)
	}
	var state, heldReason, newNonce string
	var expires int64
	db.db.QueryRow(`SELECT dispatch_state,held_reason,nonce,expires_at_ms FROM interrupts WHERE id=?`, interruptID).Scan(&state, &heldReason, &newNonce, &expires)
	if state != "held" || heldReason != "manual" || newNonce != res.NextNonce || expires != testNow+5+30*60*1000 {
		t.Fatalf("hold row = %s/%s/%s/%d", state, heldReason, newNonce, expires)
	}
}

func TestApplyCommandAskWritesSemanticMaterial(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	mustExec(t, db, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES ('report-event',?,1,'project','report','agent',1,'{}',?,?)`, cmdRun, testNow, testNow)
	mustExec(t, db, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,received_at_ms) VALUES ('report-1',?,1,'report-1','blocker','digest','report-event',?)`, cmdRun, testNow)
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptAgentBlocked,
		Facts:      map[string]string{"blocker_summary": "blocked", "attempted_summary": "tried", "recommended_action": "ask", "agent_log_ref": "/log"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, ReportID: "report-1"}, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		ExpiresAfterMS: 10, OnExpire: ExpireEscalate, OnMaxEscalations: ExpireHold, MaxEscalations: 1,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "voice", Capabilities: []string{"voice"}}},
	})
	if err != nil {
		t.Fatalf("emit agent_blocked: %v", err)
	}
	var nonce string
	db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce)

	body := "/sift ask " + cmdRun + " " + nonce + " what next?"
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	var nl string
	if err := db.db.QueryRow(`SELECT natural_language FROM ledger_entries WHERE entry_kind='semantic_material'`).Scan(&nl); err != nil {
		t.Fatal(err)
	}
	if nl != "what next?" {
		t.Fatalf("ask text = %q", nl)
	}
}

func TestApplyCommandApproveQueuesRun(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	body := "/sift approve " + cmdRun + " " + nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if status := runStatus(t, db); status != "queued" {
		t.Fatalf("run status = %s, want queued", status)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}

func TestApplyCommandUntrustedActorIgnored(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	env := commentEnv(t, "project", "c1", "/sift approve "+cmdRun+" ignored")
	carol := "carol"
	env.Actor = &carol
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if !res.Ignored {
		t.Fatalf("expected ignored, got %+v", res)
	}
	assertCount(t, db, "command_receipts WHERE disposition='ignored_untrusted_actor'", 1)
	assertCount(t, db, "events WHERE type='command.event'", 0)
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 0)
	assertCount(t, db, "ledger_entries", 0)
	if status := runStatus(t, db); status != "waiting_human" {
		t.Fatalf("run status changed to %s", status)
	}
}

func TestApplyCommandNullActorIgnored(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	env := commentEnv(t, "project", "c1", "/sift approve "+cmdRun+" ignored")
	env.Actor = nil
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if !res.Ignored {
		t.Fatalf("expected ignored, got %+v", res)
	}
	assertCount(t, db, "command_receipts WHERE disposition='ignored_missing_actor'", 1)
}

func TestApplyCommandSyntaxError(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	env := commentEnv(t, "project", "c1", "/sift bogus "+cmdRun)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeRejectedSyntax {
		t.Fatalf("outcome = %s, want rejected_syntax", res.Outcome)
	}
	assertCount(t, db, "command_receipts WHERE disposition='accepted'", 1)
	var payload string
	if err := db.db.QueryRow(`SELECT payload_json FROM events WHERE type='command.event'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"outcome":"rejected_syntax"`) {
		t.Fatalf("event payload missing outcome: %s", payload)
	}
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 1)
}

func TestApplyCommandWrongNonceRejectedStale(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	env := commentEnv(t, "project", "c1", "/sift approve "+cmdRun+" "+strings.Repeat("0", 32))
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeRejectedStale {
		t.Fatalf("outcome = %s, want rejected_stale", res.Outcome)
	}
}

func TestApplyCommandReplayReturnsStoredOutcome(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	env := commentEnv(t, "project", "c1", "/sift hold "+cmdRun+" "+nonce+" 30m")
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatal(err)
	}
	res2, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 9})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res2.Outcome != command.OutcomeApplied || res2.FinalEventID == "" {
		t.Fatalf("replay result wrong: %+v", res2)
	}
	assertCount(t, db, "events WHERE type='command.event'", 1)
	assertCount(t, db, "command_receipts", 1)
	assertCount(t, db, "ledger_entries", 1)
	var state string
	db.db.QueryRow(`SELECT dispatch_state FROM interrupts WHERE id=?`, interruptID).Scan(&state)
	if state != "held" {
		t.Fatalf("replay mutated interrupt: %s", state)
	}
}

func TestApplyCommandStartupStallApproveRejectedOption(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	_, nonce := emitStartupStallInterrupt(t, db, ctx)

	env := commentEnv(t, "project", "c1", "/sift approve "+cmdRun+" "+nonce)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeRejectedOption {
		t.Fatalf("startup_stall approve must be rejected_option, got %s", res.Outcome)
	}
}

func TestApplyCommandStartupStallRejectFailsRunHoldsIsolation(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)

	env := commentEnv(t, "project", "c1", "/sift reject "+cmdRun+" "+nonce)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if status := runStatus(t, db); status != "failed" {
		t.Fatalf("run = %s", status)
	}
	var isoState, isoReason, resolution string
	db.db.QueryRow(`SELECT isolation_state,isolation_reason FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&isoState, &isoReason)
	db.db.QueryRow(`SELECT attempt_resolution FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&resolution)
	if isoState != "frozen" || isoReason != "startup_stall" {
		t.Fatalf("isolation released: %s/%s", isoState, isoReason)
	}
	if resolution != "reject" {
		t.Fatalf("attempt_resolution = %s", resolution)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}

func TestApplyCommandStartupStallRetryRequestPendingNoAck(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)

	env := commentEnv(t, "project", "c1", "/sift retry "+cmdRun+" "+nonce)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeRetryPending {
		t.Fatalf("outcome = %s, want retry_pending", res.Outcome)
	}
	if res.AckOperationKey != "" {
		t.Fatalf("retry request must have no ack, got %s", res.AckOperationKey)
	}
	var status, state, newNonce string
	db.db.QueryRow(`SELECT status,dispatch_state,nonce FROM interrupts WHERE id=?`, interruptID).Scan(&status, &state, &newNonce)
	if status != "open" || state != "probe_in_progress" || newNonce == nonce {
		t.Fatalf("interrupt after retry request = %s/%s/%s", status, state, newNonce)
	}
	assertCount(t, db, "attempt_probes WHERE state='pending'", 1)
	var resolution *string
	db.db.QueryRow(`SELECT attempt_resolution FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&resolution)
	if resolution != nil && *resolution != "" {
		t.Fatalf("retry request must not write resolution, got %v", resolution)
	}
	assertCount(t, db, "command_event_outcomes WHERE state='pending'", 1)
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 0)
}

func TestApplyCommandStartupStallProbeInProgressRejects(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)

	env1 := commentEnv(t, "project", "c1", "/sift retry "+cmdRun+" "+nonce)
	r1, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env1, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Outcome != command.OutcomeRetryPending {
		t.Fatalf("first retry = %s", r1.Outcome)
	}
	env2 := commentEnv(t, "project", "c2", "/sift retry "+cmdRun+" "+r1.NextNonce)
	r2, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env2, Allowlist: []string{"alice"}, NowMS: testNow + 6})
	if err != nil {
		t.Fatalf("second candidate: %v", err)
	}
	if r2.Outcome != command.OutcomeProbeInProgress {
		t.Fatalf("second candidate = %s, want probe_in_progress", r2.Outcome)
	}
	assertCount(t, db, "attempt_probes WHERE interrupt_id='"+interruptID+"'", 1)
}

func TestApplyRetryProbeResultSuccessClosesAndQueues(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)

	env := commentEnv(t, "project", "c1", "/sift retry "+cmdRun+" "+nonce)
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatal(err)
	}
	var probeID string
	db.db.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=? AND state='pending'`, interruptID).Scan(&probeID)

	res, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{
		InterruptID: interruptID, ProbeID: probeID, Succeeded: true,
		AbsenceEvidenceJSON: json.RawMessage(`{"killed":true}`), NowMS: testNow + 10,
	})
	if err != nil {
		t.Fatalf("ApplyRetryProbeResult: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	var resolution, isoState string
	var released *int64
	db.db.QueryRow(`SELECT attempt_resolution,isolation_state,isolation_released_at_ms FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&resolution, &isoState, &released)
	if resolution != "retry_after_absence" || isoState != "none" || released == nil {
		t.Fatalf("probe success attempt = %s/%s/%v", resolution, isoState, released)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
	if status := runStatus(t, db); status != "queued" {
		t.Fatalf("run = %s", status)
	}
	assertCount(t, db, "command_event_outcomes WHERE state='pending'", 0)
	assertCount(t, db, "command_event_outcomes WHERE state='final'", 1)
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 1)
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2 AND phase='pending'", 1)
	assertCount(t, db, "attempt_claims WHERE run_id='"+cmdRun+"' AND attempt_no=2", 1)
	assertCount(t, db, "outbox_operations WHERE kind='launch_agent' AND run_id='"+cmdRun+"' AND attempt_no=2", 1)
}

func TestApplyRetryProbeResultFailureAbsenceUnconfirmed(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, nonce := emitStartupStallInterrupt(t, db, ctx)

	env := commentEnv(t, "project", "c1", "/sift retry "+cmdRun+" "+nonce)
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatal(err)
	}
	var probeID string
	db.db.QueryRow(`SELECT id FROM attempt_probes WHERE interrupt_id=? AND state='pending'`, interruptID).Scan(&probeID)

	res, err := db.ApplyRetryProbeResult(ctx, RetryProbeResultCmd{
		InterruptID: interruptID, ProbeID: probeID, Succeeded: false, NowMS: testNow + 10,
	})
	if err != nil {
		t.Fatalf("ApplyRetryProbeResult: %v", err)
	}
	if res.Outcome != command.OutcomeAbsenceUnconfirmed {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	var status, isoState string
	db.db.QueryRow(`SELECT status FROM interrupts WHERE id=?`, interruptID).Scan(&status)
	db.db.QueryRow(`SELECT isolation_state FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&isoState)
	if status != "open" || isoState != "frozen" {
		t.Fatalf("probe failure must retain interrupt/isolation: %s/%s", status, isoState)
	}
	assertCount(t, db, "command_event_outcomes WHERE state='pending'", 0)
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 1)
}

func TestApplyCommandApprovalLabelApprove(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedCommandRun(t, db, ctx)
	interruptID, _ := emitDesignApprovalInterrupt(t, db, ctx, command.CommandTarget{Kind: command.TargetIssue, ID: "42"})

	key, _ := command.RecomputeEventKey("project", command.SourceApprovalLabel, "l1")
	actor := "alice"
	pos := "100"
	env := command.CommandEventEnvelopeV1{
		SchemaVersion: 1, EventKey: key, ProjectID: "project", Source: command.SourceApprovalLabel,
		RemoteEventID: "l1", Target: command.CommandTarget{Kind: command.TargetIssue, ID: "42"},
		Actor: &actor, RawDigest: strings.Repeat("a", 64), OccurredAtMS: testNow + 1,
		Label: &command.CommandLabel{EventID: "l1", Name: "approved", Action: "added"}, LabelPosition: &pos,
	}
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if status := runStatus(t, db); status != "queued" {
		t.Fatalf("run = %s", status)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}
