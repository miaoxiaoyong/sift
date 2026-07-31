package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/command"
)

// gateHITLFixture is the frozen identity shared by a Gate HITL Interrupt and
// its command effect tests.
type gateHITLFixture struct {
	interruptID, nonce, runID, changeID, headSHA, ruleID, matchedPathsDigest, policyHash string
}

// eventRef is a valid sift://event/<32-hex> link accepted by validLink.
var eventRef = "sift://event/" + strings.Repeat("a", 32)

// seedGateHITLInterrupt builds a Gate HITL Interrupt for reason through
// RecordGateEvaluationAndEmitInterrupt, so its immutable effect binding and
// calibration→snapshot chain are populated exactly as in production. The Run
// is left waiting_human with attempt 1 (optionally marked failed for
// failure_review new_attempt/gate_recheck).
func seedGateHITLInterrupt(t *testing.T, db *DB, ctx context.Context, reason InterruptReason, attemptFailed bool) gateHITLFixture {
	t.Helper()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, cmdRun, "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "task-01", cmdRun, 1)
	insertAttempt(t, db, cmdRun, 1, "task-01")
	head := "0123456789012345678901234567890123456789"
	if err := db.SetRunChangeHeadForTest(ctx, cmdRun, "change-01", head); err != nil {
		t.Fatal(err)
	}
	if attemptFailed {
		if err := db.SeedFailedAttemptForTest(ctx, cmdRun, 1, testNow); err != nil {
			t.Fatal(err)
		}
	}
	mergeability := "mergeable"
	if reason == InterruptMergeConflict {
		mergeability = "conflicting"
	}
	snapshotJSON, err := canonicalJSON(map[string]any{
		"schema_version": 1,
		"identity":       map[string]any{"run_id": cmdRun, "change_id": "change-01"},
		"change":         map[string]any{"head_sha": head, "mergeability": mergeability},
	})
	if err != nil {
		t.Fatal(err)
	}
	policyHash := strings.Repeat("c", 64)
	ruleID := "hard-protected-paths"
	matchedPathsDigest := strings.Repeat("b", 64)
	record := GateEvaluationRecord{
		RunID: cmdRun, GateInputHash: strings.Repeat("a", 64), GateVersion: "gate/v1", SnapshotSchemaVersion: 1,
		SnapshotJSON: snapshotJSON, HeadSHA: head, EffectivePolicyHash: policyHash,
		CertificationVersion: strings.Repeat("d", 64), RiskSourceVersion: "T3/fallback/v1",
		VerdictDigest: strings.Repeat("e", 64), ShadowDecision: "block", FeaturesJSON: []byte(`{"schema_version":1}`), NowMS: testNow,
	}
	attempt := 1
	cmd := EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: reason,
		GatePhase: GateReview, GuardrailLevel: GuardrailSoft,
		Generation:          InterruptGeneration{ChangeID: "change-01", HeadSHA: head, ViolationCode: ruleID, SubjectDigest: matchedPathsDigest, PolicySnapshotID: policyHash, AttemptNo: 1, Generation: 1},
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	}
	switch reason {
	case InterruptGuardrailViolation:
		cmd.Facts = map[string]string{"rule_id": ruleID, "impact_scope": "matched_paths:" + matchedPathsDigest, "recommended_action": "approve", "policy_evidence_ref": eventRef}
		record.VerdictJSON = []byte(`{"schema_version":1,"kind":"hitl","code":"guardrail_violation","matched_paths_digest":"` + matchedPathsDigest + `","rule_id":"` + ruleID + `"}`)
	case InterruptCodeReview:
		cmd.GuardrailLevel = GuardrailNone
		cmd.Facts = map[string]string{"change_ref": "https://github.com/o/r/pull/1", "head_sha": head, "review_requirement": "required", "recommended_action": "approve", "diff_ref": eventRef}
		record.VerdictJSON = []byte(`{"schema_version":1,"kind":"hitl","code":"code_review"}`)
	case InterruptMergeConflict:
		cmd.GuardrailLevel = GuardrailNone
		cmd.GatePhase = GateMerge
		cmd.Generation.ConflictDigest = MergeConflictDigest("change-01", head)
		cmd.Facts = map[string]string{"change_ref": "https://github.com/o/r/pull/1", "head_sha": head, "conflict_summary": "mergeability=conflicting", "recommended_action": "retry", "conflict_evidence_ref": eventRef}
		record.VerdictJSON = []byte(`{"schema_version":1,"kind":"hitl","code":"merge_conflict","mergeability":"conflicting"}`)
	case InterruptFailureReview:
		cmd.GuardrailLevel = GuardrailNone
		cmd.FailureReviewVariant = FailureReviewAttempt
		cmd.FailureReviewRetryKind = FailureReviewGateRecheck
		cmd.Generation.FailureDigest = strings.Repeat("f", 64)
		cmd.Facts = map[string]string{"failure_class": "checks_failed", "failure_evidence_ref": eventRef, "recommended_action": "retry"}
		record.VerdictJSON = []byte(`{"schema_version":1,"kind":"hitl","code":"failure_review","classification":"failed"}`)
	}
	_, in, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, record, cmd)
	if err != nil {
		t.Fatalf("emit %s HITL: %v", reason, err)
	}
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	return gateHITLFixture{interruptID: in.ID, nonce: nonce, runID: cmdRun, changeID: "change-01", headSHA: head, ruleID: ruleID, matchedPathsDigest: matchedPathsDigest, policyHash: policyHash}
}

func gateReEvalOpCount(t *testing.T, db *DB) int {
	t.Helper()
	return rowCount(t, db, "outbox_operations WHERE kind='gate_re_evaluation'")
}

func readOpPayload(t *testing.T, db *DB, key string) string {
	t.Helper()
	var payload string
	if err := db.db.QueryRow(`SELECT payload_json FROM outbox_operations WHERE operation_key=?`, key).Scan(&payload); err != nil {
		t.Fatalf("read op payload %s: %v", key, err)
	}
	return payload
}

func TestApplyCommandGuardrailApproveEnqueuesGateReEval(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedGateHITLInterrupt(t, db, ctx, InterruptGuardrailViolation, false)

	body := "/sift approve " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s, want applied", res.Outcome)
	}
	// one_time_exemption effect fact persisted from the frozen binding; it
	// carries run/head/rule/path-digest and never a change_id.
	assertCount(t, db, "command_effects WHERE effect_kind='one_time_exemption'", 1)
	var rid, mpd, hs, cid string
	db.db.QueryRow(`SELECT rule_id,matched_paths_digest,head_sha,change_id FROM command_effects WHERE effect_kind='one_time_exemption'`).Scan(&rid, &mpd, &hs, &cid)
	if rid != f.ruleID || mpd != f.matchedPathsDigest || hs != f.headSHA {
		t.Fatalf("exemption binding fields = %s/%s/%s", rid, mpd, hs)
	}
	if cid != "" {
		t.Fatalf("exemption must not carry change_id, got %q", cid)
	}
	effects, err := db.GateCommandEffectsForInput(ctx, cmdRun, f.changeID, f.headSHA, f.policyHash)
	if err != nil || len(effects.Exemptions) != 1 || effects.Exemptions[0].RuleID != f.ruleID {
		t.Fatalf("Gate exemption effects=%+v err=%v", effects, err)
	}
	// Interrupt closed responded; Run stays waiting_human behind the re-eval.
	var closeReason, status string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, f.interruptID).Scan(&closeReason)
	db.db.QueryRow(`SELECT status FROM runs WHERE id=?`, cmdRun).Scan(&status)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
	if status != "waiting_human" {
		t.Fatalf("run = %s, want waiting_human (re-eval pending)", status)
	}
	// Exactly one Gate re-evaluation enqueued from the frozen head.
	if c := gateReEvalOpCount(t, db); c != 1 {
		t.Fatalf("gate_re_evaluation ops = %d, want 1", c)
	}
	opKey := GateReEvaluationOperationKey(f.interruptID, f.headSHA)
	payload := readOpPayload(t, db, opKey)
	for _, want := range []string{
		`"head_sha":"` + f.headSHA + `"`,
		`"change_id":"change-01"`,
		`"effect_binding_digest":`,
		`"operation_key":"` + opKey + `"`,
		`"gate_version":"gate/v1"`,
		`"source_run_version":`,
		`"source_command_event_id":"` + res.FinalEventID + `"`,
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("gate re-eval payload missing %s: %s", want, payload)
		}
	}
	assertCount(t, db, "ledger_entries WHERE entry_kind='human_decision'", 1)
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 1)
	assertCount(t, db, "command_receipts WHERE disposition='accepted'", 1)
}

func TestApplyCommandCodeReviewApproveInsertsReviewApproval(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedGateHITLInterrupt(t, db, ctx, InterruptCodeReview, false)

	body := "/sift approve " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	assertCount(t, db, "command_effects WHERE effect_kind='human_review_approval'", 1)
	var cid, hs, psd string
	db.db.QueryRow(`SELECT change_id,head_sha,review_policy_snapshot_digest FROM command_effects WHERE effect_kind='human_review_approval'`).Scan(&cid, &hs, &psd)
	if cid != f.changeID || hs != f.headSHA || psd != f.policyHash {
		t.Fatalf("review approval binding = %s/%s/%s", cid, hs, psd)
	}
	effects, err := db.GateCommandEffectsForInput(ctx, cmdRun, f.changeID, f.headSHA, f.policyHash)
	if err != nil || !effects.ReviewApproved {
		t.Fatalf("exact Gate command effects=%+v err=%v", effects, err)
	}
	effects, err = db.GateCommandEffectsForInput(ctx, cmdRun, f.changeID, strings.Repeat("b", 40), f.policyHash)
	if err != nil || effects.ReviewApproved {
		t.Fatalf("cross-head Gate command effects=%+v err=%v", effects, err)
	}
	effects, err = db.GateCommandEffectsForInput(ctx, cmdRun, f.changeID, f.headSHA, strings.Repeat("e", 64))
	if err != nil || effects.ReviewApproved {
		t.Fatalf("cross-policy Gate command effects=%+v err=%v", effects, err)
	}
	if c := gateReEvalOpCount(t, db); c != 1 {
		t.Fatalf("gate_re_evaluation ops = %d, want 1", c)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, f.interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}

func TestApplyCommandMergeConflictRetryEnqueuesGateReEvalNoAttempt(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedGateHITLInterrupt(t, db, ctx, InterruptMergeConflict, false)

	body := "/sift retry " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if c := gateReEvalOpCount(t, db); c != 1 {
		t.Fatalf("gate_re_evaluation ops = %d, want 1", c)
	}
	// merge_conflict retry creates no attempt/merge/create operation.
	assertCount(t, db, "outbox_operations WHERE kind='merge_change'", 0)
	assertCount(t, db, "outbox_operations WHERE kind='create_change'", 0)
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"'", 1)
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, f.interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}

func TestApplyCommandFailureReviewGateRecheckRetry(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedGateHITLInterrupt(t, db, ctx, InterruptFailureReview, true)

	body := "/sift retry " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if c := gateReEvalOpCount(t, db); c != 1 {
		t.Fatalf("gate_re_evaluation ops = %d, want 1", c)
	}
	// gate_recheck retry does NOT spawn a new attempt or terminalize the bound one.
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"'", 1)
	assertCount(t, db, "outbox_operations WHERE kind='launch_agent'", 0)
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, f.interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}

func TestApplyCommandFailureReviewNewAttemptRetrySpawnsNextAttempt(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedFailureReviewNewAttemptInterrupt(t, db, ctx)

	body := "/sift retry " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	// new_attempt retry spawns the next pending attempt/claim/launch and queues
	// the Run; no Gate re-evaluation is enqueued for this arm.
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2 AND phase='pending'", 1)
	assertCount(t, db, "attempt_claims WHERE run_id='"+cmdRun+"' AND attempt_no=2", 1)
	assertCount(t, db, "outbox_operations WHERE kind='launch_agent'", 1)
	if status := runStatus(t, db); status != "queued" {
		t.Fatalf("run = %s, want queued", status)
	}
	if c := gateReEvalOpCount(t, db); c != 0 {
		t.Fatalf("gate_re_evaluation ops = %d, want 0", c)
	}
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, f.interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
}

func TestApplyCommandAgentBlockedRetrySpawnsNextAttempt(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedAgentBlockedInterrupt(t, db, ctx)

	body := "/sift retry " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	// agent_blocked retry terminalizes the bound blocked (running) attempt and
	// spawns the next pending attempt/claim/launch without a Task Spec change.
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2 AND phase='pending'", 1)
	assertCount(t, db, "attempt_claims WHERE run_id='"+cmdRun+"' AND attempt_no=2", 1)
	assertCount(t, db, "outbox_operations WHERE kind='launch_agent'", 1)
	var boundPhase string
	db.db.QueryRow(`SELECT phase FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&boundPhase)
	if boundPhase != "orphaned" {
		t.Fatalf("bound attempt phase = %s, want orphaned", boundPhase)
	}
	if status := runStatus(t, db); status != "queued" {
		t.Fatalf("run = %s, want queued", status)
	}
	// Task Spec unchanged: the new attempt reuses the bound snapshot.
	var spec1, spec2 string
	db.db.QueryRow(`SELECT task_spec_snapshot_id FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&spec1)
	db.db.QueryRow(`SELECT task_spec_snapshot_id FROM attempts WHERE run_id=? AND attempt_no=2`, cmdRun).Scan(&spec2)
	if spec1 != spec2 || spec1 == "" {
		t.Fatalf("Task Spec changed: %q -> %q", spec1, spec2)
	}
}

// TestApplyCommandAgentBlockedAskFullContract proves the agent_blocked|ask row
// (command.md §4): the command-event-sourced Task Spec snapshot, Interrupt
// close, bound-attempt terminalization, next attempt/claim/launch, Ledger
// semantic material and Run queue — all in one transaction, with the
// clarification kept task-layer only (no project/global Context promotion) and
// the historical snapshot left untouched.
func TestApplyCommandAgentBlockedAskFullContract(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	f := seedAgentBlockedInterrupt(t, db, ctx)

	body := "/sift ask " + cmdRun + " " + f.nonce + " use the cached token"
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeApplied {
		t.Fatalf("outcome = %s, want applied", res.Outcome)
	}

	// Ledger: one ask human_decision + the unmodified ask_text semantic material.
	assertCount(t, db, "ledger_entries WHERE entry_kind='human_decision'", 1)
	assertCount(t, db, "ledger_entries WHERE entry_kind='semantic_material'", 1)
	var nl string
	db.db.QueryRow(`SELECT natural_language FROM ledger_entries WHERE entry_kind='semantic_material'`).Scan(&nl)
	if nl != "use the cached token" {
		t.Fatalf("ask semantic material = %q, want unmodified text", nl)
	}

	// Append-only Task Spec snapshot sourced by the command event; the Run's
	// current pointer moves without overwriting the historical snapshot.
	var snapCount int
	db.db.QueryRow(`SELECT count(*) FROM task_spec_snapshots WHERE run_id=?`, cmdRun).Scan(&snapCount)
	if snapCount != 2 {
		t.Fatalf("task_spec_snapshots = %d, want 2 (initial + clarification)", snapCount)
	}
	var newSpec, srcEvent, canon string
	db.db.QueryRow(`SELECT id,source_event_id,canonical_json FROM task_spec_snapshots WHERE run_id=? AND version=2`, cmdRun).Scan(&newSpec, &srcEvent, &canon)
	if srcEvent != res.FinalEventID {
		t.Fatalf("clarification snapshot source_event_id = %q, want command event %q", srcEvent, res.FinalEventID)
	}
	if !strings.Contains(canon, "use the cached token") {
		t.Fatalf("clarification snapshot canonical_json = %s", canon)
	}
	var currentSpec string
	db.db.QueryRow(`SELECT current_task_spec_id FROM runs WHERE id=?`, cmdRun).Scan(&currentSpec)
	if currentSpec != newSpec {
		t.Fatalf("runs.current_task_spec_id = %q, want clarification snapshot %q", currentSpec, newSpec)
	}

	// Interrupt closed responded; bound blocked attempt terminalized; next
	// pending attempt/claim/launch spawned from the clarification snapshot.
	var closeReason string
	db.db.QueryRow(`SELECT close_reason FROM interrupts WHERE id=?`, f.interruptID).Scan(&closeReason)
	if closeReason != "responded" {
		t.Fatalf("close_reason = %s", closeReason)
	}
	var boundPhase string
	db.db.QueryRow(`SELECT phase FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&boundPhase)
	if boundPhase != "orphaned" {
		t.Fatalf("bound attempt phase = %s, want orphaned", boundPhase)
	}
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"' AND attempt_no=2 AND phase='pending'", 1)
	assertCount(t, db, "attempt_claims WHERE run_id='"+cmdRun+"' AND attempt_no=2", 1)
	assertCount(t, db, "outbox_operations WHERE kind='launch_agent'", 1)
	// The new attempt references the clarification snapshot; the historical
	// attempt keeps the snapshot it started from (no overwrite).
	var spec1, spec2 string
	db.db.QueryRow(`SELECT task_spec_snapshot_id FROM attempts WHERE run_id=? AND attempt_no=1`, cmdRun).Scan(&spec1)
	db.db.QueryRow(`SELECT task_spec_snapshot_id FROM attempts WHERE run_id=? AND attempt_no=2`, cmdRun).Scan(&spec2)
	if spec1 != "task-01" {
		t.Fatalf("historical attempt task_spec_snapshot_id = %q, want task-01", spec1)
	}
	if spec2 != newSpec || spec2 == spec1 {
		t.Fatalf("new attempt task_spec_snapshot_id = %q, want clarification %q", spec2, newSpec)
	}

	// Run queued for launch.
	if status := runStatus(t, db); status != "queued" {
		t.Fatalf("run = %s, want queued", status)
	}

	// Task-layer clarification only: no project/global Context promotion. The
	// ask never creates a context proposal_draft (the T7 promotion path).
	assertCount(t, db, "proposal_drafts WHERE proposal_kind='context'", 0)

	// Exactly one command event + ack + accepted receipt.
	assertCount(t, db, "outbox_operations WHERE kind='command_ack'", 1)
	assertCount(t, db, "command_receipts WHERE disposition='accepted'", 1)

	// Crash/replay atomicity: replaying the same candidate returns the stored
	// outcome and writes no second snapshot, attempt, claim, launch or Ledger
	// entry (command.md §7 — all-or-nothing).
	replay, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 999})
	if err != nil {
		t.Fatalf("replay ApplyCommandEvent: %v", err)
	}
	if replay.Outcome != command.OutcomeApplied || replay.FinalEventID != res.FinalEventID {
		t.Fatalf("replay = %s/%s, want stored %s/%s", replay.Outcome, replay.FinalEventID, command.OutcomeApplied, res.FinalEventID)
	}
	assertCount(t, db, "task_spec_snapshots WHERE run_id='"+cmdRun+"'", 2)
	assertCount(t, db, "attempts WHERE run_id='"+cmdRun+"'", 2)
	assertCount(t, db, "outbox_operations WHERE kind='launch_agent'", 1)
	assertCount(t, db, "ledger_entries WHERE entry_kind='semantic_material'", 1)
	assertCount(t, db, "command_receipts WHERE disposition='accepted'", 1)
}

// TestApplyCommandReasonActionMatrix is the table-driven reason×action proof
// that the canonical newly-wired rows apply and persist their successors.
func TestApplyCommandReasonActionMatrix(t *testing.T) {
	ctx := context.Background()
	rows := []struct {
		name   string
		reason InterruptReason
		action command.CommandAction
		failed bool
	}{
		{"guardrail approve", InterruptGuardrailViolation, command.ActionApprove, false},
		{"code_review approve", InterruptCodeReview, command.ActionApprove, false},
		{"merge_conflict retry", InterruptMergeConflict, command.ActionRetry, false},
		{"failure_review gate_recheck retry", InterruptFailureReview, command.ActionRetry, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			db, _ := openTestDB(t)
			f := seedGateHITLInterrupt(t, db, ctx, r.reason, r.failed)
			body := "/sift " + string(r.action) + " " + cmdRun + " " + f.nonce
			env := commentEnv(t, "project", "m-"+r.name, body)
			res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
			if err != nil {
				t.Fatalf("ApplyCommandEvent: %v", err)
			}
			if res.Outcome != command.OutcomeApplied {
				t.Fatalf("outcome = %s, want applied", res.Outcome)
			}
		})
	}
}

// TestApplyCommandNonCanonicalApproveRejectedOption proves approve on a reason
// whose option set has no approve (merge_conflict) remains honestly
// rejected_option at compile, never reaching the effect.
func TestApplyCommandNonCanonicalApproveRejectedOption(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	f := seedGateHITLInterrupt(t, db, ctx, InterruptMergeConflict, false)
	body := "/sift approve " + cmdRun + " " + f.nonce
	env := commentEnv(t, "project", "c1", body)
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeRejectedOption {
		t.Fatalf("merge_conflict approve outcome = %s, want rejected_option", res.Outcome)
	}
	assertCount(t, db, "command_effects", 0)
	assertCount(t, db, "outbox_operations WHERE kind='gate_re_evaluation'", 0)
}

// TestApplyCommandGuardrailApproveAtomicNoEffectOnStaleCAS proves a stale nonce
// (rejected_stale) writes no command_effects fact and no Gate re-eval op: the
// effect and its successors are all-or-nothing with the Interrupt close CAS.
func TestApplyCommandGuardrailApproveAtomicNoEffectOnStaleCAS(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedGateHITLInterrupt(t, db, ctx, InterruptGuardrailViolation, false)

	env := commentEnv(t, "project", "c1", "/sift approve "+cmdRun+" "+strings.Repeat("0", 32))
	res, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5})
	if err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	if res.Outcome != command.OutcomeRejectedStale {
		t.Fatalf("outcome = %s, want rejected_stale", res.Outcome)
	}
	assertCount(t, db, "command_effects", 0)
	assertCount(t, db, "outbox_operations WHERE kind='gate_re_evaluation'", 0)
}

// seedFailureReviewNewAttemptInterrupt emits a failure_review new_attempt HITL
// over a failed attempt 1.
func seedFailureReviewNewAttemptInterrupt(t *testing.T, db *DB, ctx context.Context) gateHITLFixture {
	t.Helper()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, cmdRun, "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	insertTaskSpec(t, db, "task-01", cmdRun, 1)
	insertAttempt(t, db, cmdRun, 1, "task-01")
	if err := db.SeedFailedAttemptForTest(ctx, cmdRun, 1, testNow); err != nil {
		t.Fatal(err)
	}
	head := "0123456789012345678901234567890123456789"
	snapshotJSON, err := canonicalJSON(map[string]any{
		"schema_version": 1,
		"identity":       map[string]any{"run_id": cmdRun, "change_id": "change-01"},
		"change":         map[string]any{"head_sha": head, "mergeability": "mergeable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := GateEvaluationRecord{
		RunID: cmdRun, GateInputHash: strings.Repeat("a", 64), GateVersion: "gate/v1", SnapshotSchemaVersion: 1,
		SnapshotJSON: snapshotJSON, HeadSHA: head, EffectivePolicyHash: strings.Repeat("c", 64),
		CertificationVersion: strings.Repeat("d", 64), RiskSourceVersion: "T3/fallback/v1",
		VerdictDigest: strings.Repeat("e", 64), ShadowDecision: "block", FeaturesJSON: []byte(`{"schema_version":1}`), NowMS: testNow,
		VerdictJSON: []byte(`{"schema_version":1,"kind":"hitl","code":"failure_review","classification":"failed"}`),
	}
	attempt := 1
	cmd := EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptFailureReview,
		FailureReviewVariant: FailureReviewAttempt, FailureReviewRetryKind: FailureReviewNewAttempt,
		Facts:      map[string]string{"failure_class": "agent_failed", "failure_evidence_ref": eventRef, "recommended_action": "retry"},
		Generation: InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: strings.Repeat("f", 64)},
		GatePhase:  GateReview, GuardrailLevel: GuardrailNone,
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	}
	_, in, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, record, cmd)
	if err != nil {
		t.Fatalf("emit failure_review new_attempt: %v", err)
	}
	var nonce string
	db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce)
	return gateHITLFixture{interruptID: in.ID, nonce: nonce, runID: cmdRun}
}

// seedAgentBlockedInterrupt emits an agent_blocked Interrupt (non-Gate HITL)
// bound to a running attempt + report, mirroring the ask bootstrap test.
func seedAgentBlockedInterrupt(t *testing.T, db *DB, ctx context.Context) gateHITLFixture {
	t.Helper()
	seedCommandRun(t, db, ctx)
	mustExec(t, db, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES ('report-event',?,1,'project','report','agent',1,'{}',?,?)`, cmdRun, testNow, testNow)
	mustExec(t, db, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,received_at_ms) VALUES ('report-1',?,1,'report-1','blocker','digest','report-event',?)`, cmdRun, testNow)
	attempt := 1
	in, err := db.EmitInterrupt(ctx, EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: InterruptAgentBlocked,
		Facts:      map[string]string{"blocker_summary": "blocked", "attempted_summary": "tried", "recommended_action": "retry", "agent_log_ref": "/log"},
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
	return gateHITLFixture{interruptID: in.ID, nonce: nonce, runID: cmdRun}
}
