package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// canonicalResult builds the canonical GateReEvaluationResultV1 bytes for the
// given kind/payload. It mirrors how the worker builds result bytes.
func canonicalResult(t *testing.T, kind string, payload map[string]any) []byte {
	t.Helper()
	b, err := CanonicalJSON(map[string]any{"schema_version": 1, "kind": kind, "payload": payload})
	if err != nil {
		t.Fatalf("canonical result: %v", err)
	}
	return b
}

// TestGateReEvalFailedResultDigestVectors reproduces the exact failed-result
// digest vectors from storage.md §8.1. These depend only on the closed result
// bytes, not on any operation identity, so they are verified as a pure
// computation.
func TestGateReEvalFailedResultDigestVectors(t *testing.T) {
	// forge_read_failed
	forgeRead := canonicalResult(t, "failed", map[string]any{
		"failure_class": "forge_read_failed",
		"failure_evidence": map[string]any{
			"error_class":     "transient",
			"evidence_digest": strings.Repeat("a", 64),
			"stage":           "get_checks",
		},
	})
	if got := SHA256Hex(forgeRead); got != "0b7d2e6f44608d3e2a03e92a41dbb95dcfe37c90dc11da883057afc23a655659" {
		t.Fatalf("forge_read_failed R = %s", got)
	}
	if got := string(forgeRead); got != `{"kind":"failed","payload":{"failure_class":"forge_read_failed","failure_evidence":{"error_class":"transient","evidence_digest":"`+strings.Repeat("a", 64)+`","stage":"get_checks"}},"schema_version":1}` {
		t.Fatalf("forge_read_failed bytes = %s", got)
	}
	// gate_contract_failed
	contract := canonicalResult(t, "failed", map[string]any{
		"failure_class": "gate_contract_failed",
		"failure_evidence": map[string]any{
			"code": "verdict_digest_mismatch",
		},
	})
	if got := SHA256Hex(contract); got != "d5a8c1706563ff4ce16fa5419591fdbc56b7d1fd2a942ac644ea78a1f0fac978" {
		t.Fatalf("gate_contract_failed R = %s", got)
	}
}

func TestGateReEvalDispatchTablesCoverWiredCases(t *testing.T) {
	allSucceeded := []string{
		"failed/change_not_open", "failed/hard_guardrail", "wait_checks/checks_pending",
		"ready/no_auto_merge", "ready/merge", "retry_checks/flaky_retry",
		"hitl/checks_timeout", "hitl/failure_review", "hitl/guardrail_violation",
		"hitl/code_review", "hitl/merge_conflict", "hitl/mergeability_unknown", "hitl/input_unknown",
	}
	for _, key := range allSucceeded {
		t.Run("succeeded/"+key, func(t *testing.T) {
			if _, ok := gateReEvalSucceededVerdicts[key]; !ok {
				t.Fatalf("missing succeeded dispatch case %q", key)
			}
		})
	}
	for _, key := range []string{
		"hitl/checks_timeout", "hitl/failure_review", "hitl/mergeability_unknown", "hitl/input_unknown",
		"hitl/guardrail_violation", "hitl/code_review", "hitl/merge_conflict",
	} {
		t.Run(key, func(t *testing.T) {
			if _, ok := gateReEvalHITLHandlers[key]; !ok {
				t.Fatalf("missing HITL dispatch case %q", key)
			}
		})
	}
}

func gateReEvalInterruptCfg() GateReEvalInterruptEmission {
	return GateReEvalInterruptEmission{
		AttentionDailyQuota: interruptQuota(),
		DayTimezone:         "UTC",
		Channels:            []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	}
}

// seedClaimedGateReEval sets up the full production precondition (closed source
// Interrupt with effect binding + gate snapshot, command close event, one
// claimed gate_re_evaluation operation) and returns the claim plus the frozen
// identity. inputJSON/inputHash/verdictJSON/verdictDigest parameterize the
// frozen gate snapshot so succeeded-result tests can supply a self-consistent
// hash; pass empty strings to reuse the fixture defaults.
func seedClaimedGateReEval(t *testing.T, db *DB, ctx context.Context, reason InterruptReason, inputJSON, inputHash, verdictJSON, verdictDigest string) (ClaimedOperation, gateHITLFixture) {
	t.Helper()
	db.SetGateReEvalInterruptEmission(gateReEvalInterruptCfg())
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, cmdRun, "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE runs SET kind='bug' WHERE id=?`, cmdRun)
	insertTaskSpec(t, db, "task-01", cmdRun, 1)
	insertAttempt(t, db, cmdRun, 1, "task-01")
	head := "0123456789012345678901234567890123456789"
	if err := db.SetRunChangeHeadForTest(ctx, cmdRun, "change-01", head); err != nil {
		t.Fatal(err)
	}
	policyHash := strings.Repeat("c", 64)
	ruleID := "hard-protected-paths"
	matchedPathsDigest := strings.Repeat("b", 64)
	if inputJSON == "" {
		inputJSON = mustCanon(t, map[string]any{
			"schema_version": 1,
			"identity":       map[string]any{"run_id": cmdRun, "change_id": "change-01"},
			"change":         map[string]any{"head_sha": head, "mergeability": "mergeable"},
		})
		inputHash = strings.Repeat("a", 64)
	}
	if verdictJSON == "" {
		verdictJSON = `{"schema_version":1,"kind":"hitl","code":"guardrail_violation","matched_paths_digest":"` + matchedPathsDigest + `","rule_id":"` + ruleID + `"}`
		verdictDigest = strings.Repeat("e", 64)
	}
	record := GateEvaluationRecord{
		RunID: cmdRun, GateInputHash: inputHash, GateVersion: "gate/v1", SnapshotSchemaVersion: 1,
		SnapshotJSON: json.RawMessage(inputJSON), HeadSHA: head, EffectivePolicyHash: policyHash,
		CertificationVersion: strings.Repeat("d", 64), RiskSourceVersion: "T3/fallback/v1",
		VerdictJSON: json.RawMessage(verdictJSON), VerdictDigest: verdictDigest, ShadowDecision: "block",
		FeaturesJSON: []byte(`{"schema_version":1}`), NowMS: testNow,
	}
	// Content-addressed snapshots are immutable: if the frozen re-eval input is
	// already conflicting, seed must persist conflict_digest so Complete's
	// merge_conflict successor can satisfy binding provenance.
	if strings.Contains(inputJSON, `"mergeability":"conflicting"`) {
		record.ConflictDigest = MergeConflictDigest("change-01", head)
	}
	if strings.Contains(inputJSON, `"mergeability":"conflicting"`) {
		record.ConflictDigest = MergeConflictDigest("change-01", head)
	}
	if strings.Contains(verdictJSON, `"code":"code_review"`) {
		var vf struct {
			ReviewPolicy string `json:"review_policy"`
		}
		_ = json.Unmarshal([]byte(verdictJSON), &vf)
		if d, err := gateReEvalCodeReviewPolicyDigest(inputHash, vf.ReviewPolicy); err == nil {
			record.ReviewPolicySnapshotDigest = d
		}
	}
	attempt := 1
	cmd := EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, AttemptNo: &attempt, Reason: reason,
		GatePhase: GateReview, GuardrailLevel: GuardrailSoft,
		Generation:          InterruptGeneration{ChangeID: "change-01", HeadSHA: head, ViolationCode: ruleID, SubjectDigest: matchedPathsDigest, PolicySnapshotID: policyHash, AttemptNo: 1, Generation: 1},
		Facts:               map[string]string{"rule_id": ruleID, "impact_scope": "matched_paths:" + matchedPathsDigest, "recommended_action": "approve", "policy_evidence_ref": eventRef},
		AttentionDailyQuota: interruptQuota(), DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	}
	_, in, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, record, cmd)
	if err != nil {
		t.Fatalf("emit %s HITL: %v", reason, err)
	}
	var nonce string
	if err := db.db.QueryRow(`SELECT nonce FROM interrupts WHERE id=?`, in.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	// Approve closes the Interrupt responded and enqueues exactly one
	// gate_re_evaluation of the frozen head.
	body := "/sift approve " + cmdRun + " " + nonce
	env := commentEnv(t, "project", "c1", body)
	if _, err := db.ApplyCommandEvent(ctx, ApplyCommandEventCmd{Envelope: env, Allowlist: []string{"alice"}, NowMS: testNow + 5}); err != nil {
		t.Fatalf("ApplyCommandEvent: %v", err)
	}
	claim, err := db.ClaimOutboxOperationKind(ctx, "worker", OperationGateReEvaluation, testNow+10, 60_000)
	if err != nil || claim == nil {
		t.Fatalf("claim gate_re_evaluation: %v claim=%v", err, claim)
	}
	return *claim, gateHITLFixture{interruptID: in.ID, nonce: nonce, runID: cmdRun, changeID: "change-01", headSHA: head, ruleID: ruleID, matchedPathsDigest: matchedPathsDigest, policyHash: policyHash}
}

func mustCanon(t *testing.T, v any) string {
	t.Helper()
	b, err := canonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCompleteGateReEvaluationFailedArm verifies the failed result union
// terminal protocol: terminal gate.reevaluation.failed event, Run version bump
// (waiting_human retained), failure_review Interrupt successor and operation terminal.
func TestCompleteGateReEvaluationFailedArm(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, "", "", "", "")
	result := canonicalResult(t, "failed", map[string]any{
		"failure_class": "forge_read_failed",
		"failure_evidence": map[string]any{
			"error_class":     "transient",
			"evidence_digest": strings.Repeat("a", 64),
			"stage":           "get_checks",
		},
	})
	R := SHA256Hex(result)
	var runVersionBefore int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersionBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Operation terminal: the gate could not be evaluated, so the operation
	// terminates as failed rather than succeeded.
	var state string
	if err := db.db.QueryRow(`SELECT state FROM outbox_operations WHERE id=?`, claim.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("op state = %s, want failed", state)
	}
	// Terminal event at key O:failed with id event:O:failed.
	evKey := claim.Key + ":failed"
	var evType, evID, evPayload string
	if err := db.db.QueryRow(`SELECT type, id, payload_json FROM events WHERE idempotency_key=?`, evKey).Scan(&evType, &evID, &evPayload); err != nil {
		t.Fatalf("event: %v", err)
	}
	if evType != "gate.reevaluation.failed" || evID != "event:"+evKey {
		t.Fatalf("event type/id = %s / %s", evType, evID)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(evPayload), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["result_digest"] != R || ev["operation_key"] != claim.Key || ev["schema_version"] != float64(1) {
		t.Fatalf("event payload = %+v", ev)
	}
	// Run remains waiting_human but version incremented.
	var runStatus string
	var runVersion int64
	if err := db.db.QueryRow(`SELECT status, version FROM runs WHERE id=?`, cmdRun).Scan(&runStatus, &runVersion); err != nil {
		t.Fatal(err)
	}
	if runStatus != "waiting_human" {
		t.Fatalf("run status = %s, want waiting_human", runStatus)
	}
	if runVersion != runVersionBefore+1 {
		t.Fatalf("run version = %d, want %d", runVersion, runVersionBefore+1)
	}
	// failure_review Interrupt + frozen gate_recheck binding (storage.md §8.1).
	facts := map[string]string{
		"failure_class":        "forge_read_failed",
		"failure_evidence_ref": "sift://event/event:" + evKey,
		"recommended_action":   "retry",
	}
	factsJSON, err := canonicalJSON(facts)
	if err != nil {
		t.Fatal(err)
	}
	failureDigest := SHA256Hex(factsJSON)
	head := "0123456789012345678901234567890123456789"
	var intrCount int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM interrupts WHERE run_id=? AND reason='failure_review' AND generation_key IS NOT NULL`, cmdRun).Scan(&intrCount); err != nil || intrCount != 1 {
		t.Fatalf("failure_review interrupts = %d, want 1", intrCount)
	}
	var bindingJSON string
	if err := db.db.QueryRow(`
SELECT b.binding_json FROM interrupt_command_effect_bindings b
JOIN interrupts i ON i.id=b.interrupt_id
WHERE i.run_id=? AND i.reason='failure_review' AND json_extract(b.binding_json,'$.retry_kind')='gate_recheck'
AND json_extract(b.binding_json,'$.change_id')='change-01'
ORDER BY i.created_at_ms DESC LIMIT 1`, cmdRun).Scan(&bindingJSON); err != nil {
		t.Fatalf("failed-arm binding: %v", err)
	}
	wantBinding := `{"arm":"failure_review_attempt","attempt_no":1,"change_id":"change-01","generation":1,"head_sha":"` + head + `","retry_kind":"gate_recheck","run_id":"` + cmdRun + `","terminal_attempt_no":null,"terminal_generation":null}`
	if bindingJSON != wantBinding {
		t.Fatalf("binding = %s, want %s", bindingJSON, wantBinding)
	}
	wantKey, err := interruptGenerationKey(cmdRun, InterruptFailureReview, InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: failureDigest})
	if err != nil {
		t.Fatal(err)
	}
	var genKey string
	if err := db.db.QueryRow(`SELECT generation_key FROM interrupts WHERE run_id=? AND reason='failure_review'`, cmdRun).Scan(&genKey); err != nil {
		t.Fatalf("generation key: %v", err)
	}
	if genKey != wantKey {
		t.Fatalf("generation key = %s, want %s (facts digest %s)", genKey, wantKey, failureDigest)
	}
	// at-most-one: a second Complete under the same lease is rejected.
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+30); !errors.Is(err, ErrRejectedStaleWorker) {
		t.Fatalf("second Complete err = %v, want ErrRejectedStaleWorker", err)
	}
}

// TestCompleteGateReEvaluationFailedArmConcurrentComplete verifies concurrent
// Complete calls produce at most one failed-arm failure_review Interrupt.
func TestCompleteGateReEvaluationFailedArmConcurrentComplete(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, "", "", "", "")
	result := canonicalResult(t, "failed", map[string]any{
		"failure_class": "gate_contract_failed",
		"failure_evidence": map[string]any{
			"code": "verdict_digest_mismatch",
		},
	})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.CompleteGateReEvaluation(ctx, claim, result, testNow+20+int64(i))
		}(i)
	}
	wg.Wait()
	var ok int
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one Complete should succeed, got %d; errs=%v", ok, errs)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM interrupts WHERE run_id=? AND reason='failure_review' AND json_extract((SELECT binding_json FROM interrupt_command_effect_bindings WHERE interrupt_id=interrupts.id),'$.retry_kind')='gate_recheck'`, cmdRun).Scan(&n); err != nil || n != 1 {
		t.Fatalf("gate_recheck failure_review count = %d, want 1", n)
	}
}

// TestCompleteGateReEvaluationSucceededReady verifies the succeeded ready/
// no_auto_merge arm: Run -> done(gate_passed_no_auto_merge), one evaluation,
// terminal event and operation terminal.
func TestCompleteGateReEvaluationSucceededReady(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	inputJSON := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk": map[string]any{
			"source": map[string]any{"version": "T3/fallback/v1"},
		},
	})
	inputHash := SHA256Hex([]byte(inputJSON))
	verdictJSON := mustCanon(t, map[string]any{"schema_version": 1, "kind": "ready", "code": "no_auto_merge", "head_sha": head})
	verdictDigest := SHA256Hex([]byte(verdictJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	var runVersionBefore int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersionBefore); err != nil {
		t.Fatal(err)
	}

	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON,
		"gate_input_hash": inputHash,
		"gate_version":    "gate/v1",
		"verdict_json":    verdictJSON,
		"verdict_digest":  verdictDigest,
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var runStatus string
	var runVersion int64
	var changeID string
	if err := db.db.QueryRow(`SELECT status, version, change_id FROM runs WHERE id=?`, cmdRun).Scan(&runStatus, &runVersion, &changeID); err != nil {
		t.Fatal(err)
	}
	if runStatus != "done" || runVersion != runVersionBefore+1 || changeID != "change-01" {
		t.Fatalf("run = %s v%d change=%s, want done v%d change-01", runStatus, runVersion, changeID, runVersionBefore+1)
	}
	// One new gate evaluation row.
	var evalCount int
	if err := db.db.QueryRow(`SELECT count(*) FROM gate_evaluations WHERE run_id=?`, cmdRun).Scan(&evalCount); err != nil {
		t.Fatal(err)
	}
	if evalCount < 2 {
		t.Fatalf("gate_evaluations = %d, want >= 2 (source + re-eval)", evalCount)
	}
	// Terminal event.
	evKey := claim.Key + ":verdict:ready:no_auto_merge"
	var evType string
	if err := db.db.QueryRow(`SELECT type FROM events WHERE idempotency_key=?`, evKey).Scan(&evType); err != nil {
		t.Fatalf("event: %v", err)
	}
	if evType != "gate.reevaluation.ready.no_auto_merge" {
		t.Fatalf("event type = %s", evType)
	}
}

func TestCompleteGateReEvaluationRejectsChangedInputIdentity(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	sourceInput := mustCanon(t, map[string]any{
		"schema_version": 1, "effective_policy_hash": strings.Repeat("c", 64), "certification_version": strings.Repeat("d", 64),
		"change":   map[string]any{"head_sha": head},
		"identity": map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":     map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
	})
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, sourceInput, SHA256Hex([]byte(sourceInput)), "", "")
	changed := mustCanon(t, map[string]any{
		"schema_version": 1, "effective_policy_hash": strings.Repeat("c", 64), "certification_version": strings.Repeat("d", 64),
		"change":   map[string]any{"head_sha": head},
		"identity": map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "other-project", "task_kind": "bug"},
		"risk":     map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
	})
	verdict := mustCanon(t, map[string]any{"schema_version": 1, "kind": "ready", "code": "no_auto_merge", "head_sha": head})
	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": changed, "gate_input_hash": SHA256Hex([]byte(changed)), "gate_version": "gate/v1",
		"verdict_json": verdict, "verdict_digest": SHA256Hex([]byte(verdict)),
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("changed identity error=%v", err)
	}
}

// TestCompleteGateReEvaluationSucceededReadyMerge verifies the ready/merge arm
// (storage.md §8.1): Run -> running(gate_merge_requested), one terminal event,
// and exactly one merge_change successor enqueued in the same transaction with
// the closed payload fields and created_from_event_id byte-for-byte event:<K>.
// A replayed Complete cannot double-enqueue, and the successor is claimable by
// the wired MergeWorker's per-project claim path.
func TestCompleteGateReEvaluationSucceededReadyMerge(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	inputJSON := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk": map[string]any{
			"source": map[string]any{"version": "T3/fallback/v1"},
		},
	})
	inputHash := SHA256Hex([]byte(inputJSON))
	verdictJSON := mustCanon(t, map[string]any{"schema_version": 1, "kind": "ready", "code": "merge", "head_sha": head, "change_id": "change-01", "expected_head_sha": head})
	verdictDigest := SHA256Hex([]byte(verdictJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	var runVersionBefore int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersionBefore); err != nil {
		t.Fatal(err)
	}

	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON,
		"gate_input_hash": inputHash,
		"gate_version":    "gate/v1",
		"verdict_json":    verdictJSON,
		"verdict_digest":  verdictDigest,
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Run -> running, version bumped; no completion timestamp.
	var runStatus string
	var runVersion int64
	var completedAt sql.NullInt64
	if err := db.db.QueryRow(`SELECT status, version, completed_at_ms FROM runs WHERE id=?`, cmdRun).Scan(&runStatus, &runVersion, &completedAt); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || runVersion != runVersionBefore+1 {
		t.Fatalf("run = %s v%d, want running v%d", runStatus, runVersion, runVersionBefore+1)
	}
	if completedAt.Valid {
		t.Fatalf("run completed_at_ms = %d, want unset", completedAt.Int64)
	}

	// Terminal event: type gate.reevaluation.ready.merge at key O:verdict:ready:merge.
	evKey := claim.Key + ":verdict:ready:merge"
	var evType, evID string
	if err := db.db.QueryRow(`SELECT type, id FROM events WHERE idempotency_key=?`, evKey).Scan(&evType, &evID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if evType != "gate.reevaluation.ready.merge" || evID != "event:"+evKey {
		t.Fatalf("event type/id = %s / %s", evType, evID)
	}

	// Exactly one merge_change successor with the closed payload fields and
	// created_from_event_id == the terminal event id.
	mergeKey := MergeChangeOperationKey(cmdRun, head)
	var mergeCount int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='merge_change'`).Scan(&mergeCount); err != nil {
		t.Fatal(err)
	}
	if mergeCount != 1 {
		t.Fatalf("merge_change count = %d, want 1", mergeCount)
	}
	var mergePayload, mergeKind string
	if err := db.db.QueryRow(`SELECT payload_json, kind FROM outbox_operations WHERE operation_key=?`, mergeKey).Scan(&mergePayload, &mergeKind); err != nil {
		t.Fatalf("successor merge_change: %v", err)
	}
	if mergeKind != "merge_change" {
		t.Fatalf("successor kind = %s", mergeKind)
	}
	var merge map[string]string
	if err := json.Unmarshal([]byte(mergePayload), &merge); err != nil {
		t.Fatal(err)
	}
	// The §8.1 Gate-provenance closed fields plus routing/method fields.
	want := map[string]string{
		"project_id": "project", "run_id": cmdRun, "change_id": "change-01",
		"expected_head_sha": head, "method": "merge",
		"verdict_digest":        verdictDigest,
		"created_from_event_id": "event:" + evKey,
	}
	for k, v := range want {
		if merge[k] != v {
			t.Fatalf("merge_change %s = %q, want %q (payload=%s)", k, merge[k], v, mergePayload)
		}
	}
	if merge["gate_evaluation_id"] == "" || merge["gate_input_snapshot_id"] == "" {
		t.Fatalf("merge_change missing E/S (payload=%s)", mergePayload)
	}

	// at-most-one: a second Complete under the now-cleared lease cannot
	// double-enqueue the merge_change successor.
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+30); !errors.Is(err, ErrRejectedStaleWorker) {
		t.Fatalf("second Complete err = %v, want ErrRejectedStaleWorker", err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='merge_change'`).Scan(&mergeCount); err != nil {
		t.Fatal(err)
	}
	if mergeCount != 1 {
		t.Fatalf("merge_change count after replay = %d, want 1", mergeCount)
	}

	// The successor is claimable by the wired MergeWorker's per-project claim
	// path (storage.md §8.1: merge_change can be claimed).
	c, err := db.ClaimOutboxOperationKindProject(ctx, "siftd:merge:project", OperationMergeChange, "project", testNow+40, 60_000)
	if err != nil || c == nil {
		t.Fatalf("claim merge_change: %v claim=%v", err, c)
	}
	if c.Key != mergeKey {
		t.Fatalf("claimed key = %s, want %s", c.Key, mergeKey)
	}
}

// TestCompleteGateReEvaluationSucceededRerunChecks verifies the retry_checks/
// flaky_retry arm (storage.md §8.1, §8.2): Run -> running(gate_retry_checks),
// one terminal event, exactly one rerun_checks successor enqueued with the
// closed payload fields and created_from_event_id byte-for-byte event:<K>, and
// exactly one check_rerun_consumptions row linking to that operation. A
// replayed Complete cannot double-enqueue or double-consume, and the successor
// is claimable by a future RerunCheck worker via ClaimOutboxOperationKind.
func TestCompleteGateReEvaluationSucceededRerunChecks(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	// A flaky triage source. No logical_call_id is set so no brain input link is
	// created (the link is orthogonal to the successor/consumption wiring under
	// test); triage_source_digest is SHA-256 of these canonical bytes regardless.
	triageSource := map[string]any{
		"kind": "fallback", "version": "T5/fallback/v1", "reason": "provider_disabled",
	}
	inputJSON := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head, "mergeability": "mergeable"},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":                  map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
		"checks": map[string]any{
			"conclusion": "failure", "external_url": "https://ci.example/runs/1",
			"failed_jobs":        []any{},
			"flaky_retries_used": 0,
			"triage": map[string]any{
				"classification": "flaky", "retry_check_id": "cr-1",
				"source": triageSource,
			},
		},
	})
	inputHash := SHA256Hex([]byte(inputJSON))
	checkRunID := "cr-1"
	retryNo := 1
	verdictJSON := mustCanon(t, map[string]any{
		"schema_version": 1, "kind": "retry_checks", "code": "flaky_retry",
		"head_sha": head, "check_run_id": checkRunID, "retry_no": retryNo,
	})
	verdictDigest := SHA256Hex([]byte(verdictJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	var runVersionBefore int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersionBefore); err != nil {
		t.Fatal(err)
	}

	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON,
		"gate_input_hash": inputHash,
		"gate_version":    "gate/v1",
		"verdict_json":    verdictJSON,
		"verdict_digest":  verdictDigest,
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Run -> running, version bumped; no completion timestamp.
	var runStatus string
	var runVersion int64
	var completedAt sql.NullInt64
	if err := db.db.QueryRow(`SELECT status, version, completed_at_ms FROM runs WHERE id=?`, cmdRun).Scan(&runStatus, &runVersion, &completedAt); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || runVersion != runVersionBefore+1 {
		t.Fatalf("run = %s v%d, want running v%d", runStatus, runVersion, runVersionBefore+1)
	}
	if completedAt.Valid {
		t.Fatalf("run completed_at_ms = %d, want unset", completedAt.Int64)
	}

	// Terminal event: type gate.reevaluation.retry_checks.flaky_retry at key
	// O:verdict:retry_checks:flaky_retry.
	evKey := claim.Key + ":verdict:retry_checks:flaky_retry"
	var evType, evID string
	if err := db.db.QueryRow(`SELECT type, id FROM events WHERE idempotency_key=?`, evKey).Scan(&evType, &evID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if evType != "gate.reevaluation.retry_checks.flaky_retry" || evID != "event:"+evKey {
		t.Fatalf("event type/id = %s / %s", evType, evID)
	}

	// Exactly one rerun_checks successor with the closed payload fields and
	// created_from_event_id == the terminal event id.
	rerunKey := RerunChecksOperationKey(cmdRun, head, checkRunID, retryNo)
	var rerunCount int
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='rerun_checks'`).Scan(&rerunCount); err != nil {
		t.Fatal(err)
	}
	if rerunCount != 1 {
		t.Fatalf("rerun_checks count = %d, want 1", rerunCount)
	}
	var rerunPayload, rerunKind, rerunOpID string
	if err := db.db.QueryRow(`SELECT payload_json, kind, id FROM outbox_operations WHERE operation_key=?`, rerunKey).Scan(&rerunPayload, &rerunKind, &rerunOpID); err != nil {
		t.Fatalf("successor rerun_checks: %v", err)
	}
	if rerunKind != "rerun_checks" {
		t.Fatalf("successor kind = %s", rerunKind)
	}
	var rerun map[string]any
	if err := json.Unmarshal([]byte(rerunPayload), &rerun); err != nil {
		t.Fatal(err)
	}
	// The §8.1 closed payload is exactly these seven fields.
	wantFields := map[string]any{
		"run_id": cmdRun, "change_id": "change-01", "head_sha": head,
		"check_run_id": checkRunID, "retry_no": float64(retryNo),
		"created_from_event_id": "event:" + evKey,
	}
	for k, want := range wantFields {
		if rerun[k] != want {
			t.Fatalf("rerun_checks %s = %v, want %v (payload=%s)", k, rerun[k], want, rerunPayload)
		}
	}
	if got := rerun["triage_source_digest"]; got == nil || got == "" {
		t.Fatalf("rerun_checks missing triage_source_digest (payload=%s)", rerunPayload)
	}
	// triage_source_digest == SHA-256(canonical_json(checks.triage.source)).
	canonSrc, err := CanonicalJSON(triageSource)
	if err != nil {
		t.Fatal(err)
	}
	wantTriageDigest := SHA256Hex(canonSrc)
	if rerun["triage_source_digest"] != wantTriageDigest {
		t.Fatalf("triage_source_digest = %v, want %s", rerun["triage_source_digest"], wantTriageDigest)
	}
	if len(rerun) != 7 {
		t.Fatalf("rerun_checks payload has %d fields, want 7 (payload=%s)", len(rerun), rerunPayload)
	}

	// Exactly one check_rerun_consumptions row linking to the successor op,
	// keyed by (run, head, check, retry) and UNIQUE on operation_id.
	var consCount, consOpID string
	var consRetryNo int
	if err := db.db.QueryRow(`SELECT count(*), operation_id, retry_no FROM check_rerun_consumptions WHERE run_id=? AND head_sha=? AND check_run_id=?`, cmdRun, head, checkRunID).Scan(&consCount, &consOpID, &consRetryNo); err != nil {
		t.Fatalf("consumption row: %v", err)
	}
	if consCount != "1" || consOpID != rerunOpID || consRetryNo != retryNo {
		t.Fatalf("consumption = count=%s op=%s retry=%d, want count=1 op=%s retry=%d", consCount, consOpID, consRetryNo, rerunOpID, retryNo)
	}

	// at-most-one: a second Complete under the now-cleared lease cannot
	// double-enqueue the successor or double-consume the retry quota.
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+30); !errors.Is(err, ErrRejectedStaleWorker) {
		t.Fatalf("second Complete err = %v, want ErrRejectedStaleWorker", err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM outbox_operations WHERE kind='rerun_checks'`).Scan(&rerunCount); err != nil {
		t.Fatal(err)
	}
	if rerunCount != 1 {
		t.Fatalf("rerun_checks count after replay = %d, want 1", rerunCount)
	}
	var consTotal int
	if err := db.db.QueryRow(`SELECT count(*) FROM check_rerun_consumptions`).Scan(&consTotal); err != nil {
		t.Fatal(err)
	}
	if consTotal != 1 {
		t.Fatalf("consumption rows after replay = %d, want 1", consTotal)
	}

	// The successor is claimable by a future RerunCheck worker via the
	// kind-scoped claim path (storage.md §8.1: rerun_checks can be claimed).
	c, err := db.ClaimOutboxOperationKind(ctx, "siftd:rerun:worker", OperationRerunChecks, testNow+40, 60_000)
	if err != nil || c == nil {
		t.Fatalf("claim rerun_checks: %v claim=%v", err, c)
	}
	if c.Key != rerunKey {
		t.Fatalf("claimed key = %s, want %s", c.Key, rerunKey)
	}
	// Once request-start is durable, an expired lease is terminal conflict:
	// no replacement attempt is created and a unique failure_review is emitted
	// in the same reclaim transaction.
	if err := db.MarkOutboxAttemptRequestStarted(ctx, *c, testNow+41); err != nil {
		t.Fatalf("mark request start: %v", err)
	}
	if err := db.MarkOutboxAttemptRequestStarted(ctx, *c, testNow+42); err != nil {
		t.Fatalf("idempotent request start: %v", err)
	}
	if next, err := db.ClaimOutboxOperationKind(ctx, "replacement", OperationRerunChecks, c.LeaseExpiresAtMS+1, 60_000); err != nil || next != nil {
		t.Fatalf("post-start reclaim = %v err=%v, want terminal conflict", next, err)
	}
	var state string
	var attempts int
	if err := db.db.QueryRow(`SELECT state,attempt_count FROM outbox_operations WHERE id=?`, c.ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "conflict" || attempts != 1 {
		t.Fatalf("rerun state=%s attempts=%d, want conflict/1", state, attempts)
	}
	var failureReviews int
	if err := db.db.QueryRow(`SELECT count(*) FROM interrupts WHERE run_id=? AND reason='failure_review'`, cmdRun).Scan(&failureReviews); err != nil || failureReviews != 1 {
		t.Fatalf("failure_review count=%d err=%v", failureReviews, err)
	}
}

// TestCompleteGateReEvaluationRerunChecksContractRejections covers the
// retry_checks/flaky_retry verdict field contract violations.
func TestCompleteGateReEvaluationRerunChecksContractRejections(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	baseInput := map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":                  map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
		"checks": map[string]any{
			"conclusion": "failure", "external_url": "https://ci.example/runs/1",
			"failed_jobs":        []any{},
			"flaky_retries_used": 0,
			"triage": map[string]any{
				"classification": "flaky", "retry_check_id": "cr-1",
				"source": map[string]any{"kind": "fallback", "version": "T5/fallback/v1", "reason": "provider_disabled"},
			},
		},
	}

	// Missing check_run_id.
	inputJSON := mustCanon(t, baseInput)
	inputHash := SHA256Hex([]byte(inputJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")
	vJSON := mustCanon(t, map[string]any{"schema_version": 1, "kind": "retry_checks", "code": "flaky_retry", "head_sha": head, "retry_no": 1})
	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
		"verdict_json": vJSON, "verdict_digest": SHA256Hex([]byte(vJSON)),
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("missing check_run_id err = %v, want ErrGateReEvaluationContract", err)
	}

	// retry_no < 1.
	vJSON2 := mustCanon(t, map[string]any{"schema_version": 1, "kind": "retry_checks", "code": "flaky_retry", "head_sha": head, "check_run_id": "cr-1", "retry_no": 0})
	result2 := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
		"verdict_json": vJSON2, "verdict_digest": SHA256Hex([]byte(vJSON2)),
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result2, testNow+21); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("retry_no<1 err = %v, want ErrGateReEvaluationContract", err)
	}
}

// TestCompleteGateReEvaluationConflict verifies the conflict arm: Run version
// bump, terminal conflict event and a verified different-head successor op.
func TestCompleteGateReEvaluationConflict(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, "", "", "", "")
	replHead := "fedcba9876543210fedcba9876543210fedcba98"
	replInput := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": replHead},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":                  map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
	})
	replHash := SHA256Hex([]byte(replInput))
	result := canonicalResult(t, "conflict", map[string]any{
		"replacement_head_sha":     replHead,
		"replacement_input_json":   replInput,
		"replacement_input_hash":   replHash,
		"replacement_gate_version": "gate/v1",
	})
	var runVersionBefore int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersionBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Operation terminal conflict.
	var state string
	if err := db.db.QueryRow(`SELECT state FROM outbox_operations WHERE id=?`, claim.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "conflict" {
		t.Fatalf("op state = %s, want conflict", state)
	}
	// Run version bumped, still waiting_human.
	var runVersion int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	if runVersion != runVersionBefore+1 {
		t.Fatalf("run version = %d, want %d", runVersion, runVersionBefore+1)
	}
	// Successor op created with the replacement head and post-CAS version.
	succKey := GateReEvaluationOperationKey(claim.InterruptID, replHead)
	var succPayload string
	if err := db.db.QueryRow(`SELECT payload_json FROM outbox_operations WHERE operation_key=?`, succKey).Scan(&succPayload); err != nil {
		t.Fatalf("successor op: %v", err)
	}
	var succ GateReEvaluationPayload
	if err := json.Unmarshal([]byte(succPayload), &succ); err != nil {
		t.Fatal(err)
	}
	if succ.HeadSHA != replHead || succ.SourceRunVersion != runVersionBefore+1 || succ.GateInputHash != replHash || succ.OperationKey != succKey {
		t.Fatalf("successor payload = %+v", succ)
	}
	// Terminal conflict event.
	evKey := claim.Key + ":conflict"
	var evType string
	if err := db.db.QueryRow(`SELECT type FROM events WHERE idempotency_key=?`, evKey).Scan(&evType); err != nil {
		t.Fatalf("event: %v", err)
	}
	if evType != "gate.reevaluation.conflict" {
		t.Fatalf("event type = %s", evType)
	}
}

// TestCompleteGateReEvaluationContractRejections covers the closed contract
// violations: non-canonical bytes, unknown kind, hash mismatch and an unwired
// verdict successor.
func TestCompleteGateReEvaluationContractRejections(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	inputJSON := mustCanon(t, map[string]any{"schema_version": 1, "effective_policy_hash": strings.Repeat("c", 64), "certification_version": strings.Repeat("d", 64), "change": map[string]any{"head_sha": head}, "identity": map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"}, "risk": map[string]any{"source": map[string]any{"version": "v1"}}})
	inputHash := SHA256Hex([]byte(inputJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	// Non-canonical bytes (extra whitespace).
	if err := db.CompleteGateReEvaluation(ctx, claim, []byte(`{"kind":"failed", "payload":{},"schema_version":1}`+"\n"), testNow+20); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("non-canonical err = %v", err)
	}
	// Unknown kind.
	unknown := canonicalResult(t, "bogus", map[string]any{"x": 1})
	if err := db.CompleteGateReEvaluation(ctx, claim, unknown, testNow+21); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("unknown kind err = %v", err)
	}
	// retry_checks/flaky_retry is now wired (see TestCompleteGateReEvaluationSucceededRerunChecks);
	// a gate input lacking checks.triage.source is a closed contract violation, not
	// a deferred successor.
	vJSON := mustCanon(t, map[string]any{"schema_version": 1, "kind": "retry_checks", "code": "flaky_retry", "head_sha": head, "check_run_id": "cr-1", "retry_no": 1})
	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON,
		"gate_input_hash": inputHash,
		"gate_version":    "gate/v1",
		"verdict_json":    vJSON,
		"verdict_digest":  SHA256Hex([]byte(vJSON)),
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+22); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("retry_checks missing triage source err = %v, want ErrGateReEvaluationContract", err)
	}
}

// TestCompleteGateReEvaluationHITLGuardrailViolation verifies the succeeded
// hitl/guardrail_violation arm: terminal event, Run version bump (waiting_human
// retained), and guardrail_violation Interrupt successor with closed binding.
func TestCompleteGateReEvaluationHITLGuardrailViolation(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	policyHash := strings.Repeat("c", 64)
	ruleID := "soft-recheck-rule"
	matchedPathsDigest := strings.Repeat("f", 64)
	inputJSON := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": policyHash,
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head, "mergeability": "mergeable"},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":                  map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
	})
	inputHash := SHA256Hex([]byte(inputJSON))
	verdictJSON := mustCanon(t, map[string]any{
		"schema_version": 1, "kind": "hitl", "code": "guardrail_violation", "head_sha": head,
		"rule_id": ruleID, "matched_paths_digest": matchedPathsDigest,
	})
	verdictDigest := SHA256Hex([]byte(verdictJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	var runVersionBefore int64
	if err := db.db.QueryRow(`SELECT version FROM runs WHERE id=?`, cmdRun).Scan(&runVersionBefore); err != nil {
		t.Fatal(err)
	}
	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
		"verdict_json": verdictJSON, "verdict_digest": verdictDigest,
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var runStatus string
	var runVersion int64
	if err := db.db.QueryRow(`SELECT status, version FROM runs WHERE id=?`, cmdRun).Scan(&runStatus, &runVersion); err != nil {
		t.Fatal(err)
	}
	if runStatus != "waiting_human" || runVersion != runVersionBefore+1 {
		t.Fatalf("run = %s v%d, want waiting_human v%d", runStatus, runVersion, runVersionBefore+1)
	}
	evKey := claim.Key + ":verdict:hitl:guardrail_violation"
	var evType string
	if err := db.db.QueryRow(`SELECT type FROM events WHERE idempotency_key=?`, evKey).Scan(&evType); err != nil {
		t.Fatalf("event: %v", err)
	}
	if evType != "gate.reevaluation.hitl.guardrail_violation" {
		t.Fatalf("event type = %s", evType)
	}
	var intrCount int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM interrupts WHERE run_id=? AND reason='guardrail_violation'`, cmdRun).Scan(&intrCount); err != nil || intrCount != 2 {
		t.Fatalf("guardrail_violation interrupts = %d, want 2 (source + successor)", intrCount)
	}
	wantBinding := `{"arm":"guardrail_violation","head_sha":"` + head + `","matched_paths_digest":"` + matchedPathsDigest + `","rule_id":"` + ruleID + `","run_id":"` + cmdRun + `"}`
	var bindingJSON string
	if err := db.db.QueryRow(`
SELECT b.binding_json FROM interrupt_command_effect_bindings b
JOIN interrupts i ON i.id=b.interrupt_id
WHERE i.run_id=? AND i.reason='guardrail_violation' AND b.binding_json LIKE '%`+matchedPathsDigest+`%'
ORDER BY i.created_at_ms DESC LIMIT 1`, cmdRun).Scan(&bindingJSON); err != nil {
		t.Fatalf("binding: %v", err)
	}
	if bindingJSON != wantBinding {
		t.Fatalf("binding = %s, want %s", bindingJSON, wantBinding)
	}
}

// TestCompleteGateReEvaluationHITLChecksTimeout verifies the succeeded
// hitl/checks_timeout arm emits a failure_review gate_recheck successor.
func TestCompleteGateReEvaluationHITLChecksTimeout(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	externalURL := "https://forge.example/checks/1"
	inputJSON := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head, "mergeability": "mergeable"},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":                  map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
	})
	inputHash := SHA256Hex([]byte(inputJSON))
	verdictJSON := mustCanon(t, map[string]any{
		"schema_version": 1, "kind": "hitl", "code": "checks_timeout", "head_sha": head, "external_url": externalURL,
	})
	verdictDigest := SHA256Hex([]byte(verdictJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
		"verdict_json": verdictJSON, "verdict_digest": verdictDigest,
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	facts := map[string]string{
		"failure_class": "checks_timeout", "failure_evidence_ref": externalURL, "recommended_action": "retry",
	}
	factsJSON, _ := canonicalJSON(facts)
	failureDigest := SHA256Hex(factsJSON)
	wantKey, err := interruptGenerationKey(cmdRun, InterruptFailureReview, InterruptGeneration{AttemptNo: 1, Generation: 1, FailureDigest: failureDigest})
	if err != nil {
		t.Fatal(err)
	}
	var genKey string
	if err := db.db.QueryRow(`SELECT generation_key FROM interrupts WHERE run_id=? AND reason='failure_review' AND generation_key=?`, cmdRun, wantKey).Scan(&genKey); err != nil {
		t.Fatalf("checks_timeout failure_review: %v (want key %s)", err, wantKey)
	}
}

func gateReEvalHITLInputJSON(t *testing.T, head, mergeability string) (string, string) {
	t.Helper()
	inputJSON := mustCanon(t, map[string]any{
		"schema_version":        1,
		"effective_policy_hash": strings.Repeat("c", 64),
		"certification_version": strings.Repeat("d", 64),
		"change":                map[string]any{"head_sha": head, "mergeability": mergeability},
		"identity":              map[string]any{"change_id": "change-01", "run_id": cmdRun, "project_id": "project", "task_kind": "bug"},
		"risk":                  map[string]any{"source": map[string]any{"version": "T3/fallback/v1"}},
	})
	return inputJSON, SHA256Hex([]byte(inputJSON))
}

// TestCompleteGateReEvaluationHITLAllArms verifies all seven HITL verdict arms
// complete with the expected terminal event type and a successor Interrupt.
func TestCompleteGateReEvaluationHITLAllArms(t *testing.T) {
	head := "0123456789012345678901234567890123456789"
	cases := []struct {
		code, eventSuffix, wantReason string
		verdictExtra                  map[string]any
		mergeability                  string
	}{
		{"checks_timeout", "hitl:checks_timeout", "failure_review", map[string]any{"external_url": "https://forge.example/checks/1"}, "mergeable"},
		{"failure_review", "hitl:failure_review", "failure_review", map[string]any{"external_url": "https://forge.example/checks/2", "classification": "failure"}, "mergeable"},
		{"guardrail_violation", "hitl:guardrail_violation", "guardrail_violation", map[string]any{"rule_id": "rule-all-arms", "matched_paths_digest": strings.Repeat("a", 64)}, "mergeable"},
		{"code_review", "hitl:code_review", "code_review", map[string]any{"review_policy": "always"}, "mergeable"},
		{"merge_conflict", "hitl:merge_conflict", "merge_conflict", map[string]any{"mergeability": "conflicting"}, "conflicting"},
		{"mergeability_unknown", "hitl:mergeability_unknown", "failure_review", map[string]any{"mergeability": "unknown"}, "unknown"},
		{"input_unknown", "hitl:input_unknown", "failure_review", map[string]any{"field": "checks.conclusion"}, "mergeable"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			db, _ := openTestDB(t)
			ctx := context.Background()
			inputJSON, inputHash := gateReEvalHITLInputJSON(t, head, tc.mergeability)
			payload := map[string]any{"schema_version": 1, "kind": "hitl", "code": tc.code, "head_sha": head}
			for k, v := range tc.verdictExtra {
				payload[k] = v
			}
			verdictJSON := mustCanon(t, payload)
			verdictDigest := SHA256Hex([]byte(verdictJSON))
			claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")
			result := canonicalResult(t, "succeeded", map[string]any{
				"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
				"verdict_json": verdictJSON, "verdict_digest": verdictDigest,
			})
			if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			evKey := claim.Key + ":verdict:" + tc.eventSuffix
			var evType string
			if err := db.db.QueryRow(`SELECT type FROM events WHERE idempotency_key=?`, evKey).Scan(&evType); err != nil {
				t.Fatalf("event %s: %v", evKey, err)
			}
			wantEvent := "gate.reevaluation." + strings.ReplaceAll(tc.eventSuffix, ":", ".")
			if evType != wantEvent {
				t.Fatalf("event type = %s, want %s", evType, wantEvent)
			}
			var n int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM interrupts WHERE run_id=? AND reason=?`, cmdRun, tc.wantReason).Scan(&n); err != nil || n < 1 {
				t.Fatalf("successor interrupts reason=%s count=%d", tc.wantReason, n)
			}
		})
	}
}

// TestCompleteGateReEvaluationHITLCodeReviewPolicyDigest verifies the closed
// review_policy_snapshot_digest vector from storage.md section 8.1.
func TestCompleteGateReEvaluationHITLCodeReviewPolicyDigest(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	reviewPolicy := "always"
	inputJSON, inputHash := gateReEvalHITLInputJSON(t, head, "mergeable")
	verdictJSON := mustCanon(t, map[string]any{
		"schema_version": 1, "kind": "hitl", "code": "code_review", "head_sha": head, "review_policy": reviewPolicy,
	})
	verdictDigest := SHA256Hex([]byte(verdictJSON))
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")

	policyDigestBytes, err := canonicalJSON(map[string]string{"gate_input_hash": inputHash, "review_policy": reviewPolicy})
	if err != nil {
		t.Fatal(err)
	}
	wantPolicyDigest := SHA256Hex(policyDigestBytes)

	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
		"verdict_json": verdictJSON, "verdict_digest": verdictDigest,
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var evalDigest, bindingDigest string
	if err := db.db.QueryRow(`
SELECT e.review_policy_snapshot_digest, b.binding_json FROM gate_evaluations e
JOIN calibration_entries c ON c.gate_evaluation_id=e.id
JOIN interrupts i ON i.calibration_id=c.id
JOIN interrupt_command_effect_bindings b ON b.interrupt_id=i.id
WHERE i.run_id=? AND i.reason='code_review' AND json_extract(b.binding_json,'$.review_policy_snapshot_digest')=e.review_policy_snapshot_digest
ORDER BY i.created_at_ms DESC LIMIT 1`, cmdRun).Scan(&evalDigest, &bindingDigest); err != nil {
		t.Fatalf("code_review digest row: %v", err)
	}
	if evalDigest != wantPolicyDigest {
		t.Fatalf("evaluation digest = %s, want %s", evalDigest, wantPolicyDigest)
	}
	wantBinding := `{"arm":"code_review","change_id":"change-01","head_sha":"` + head + `","review_policy_snapshot_digest":"` + wantPolicyDigest + `"}`
	if bindingDigest != wantBinding {
		t.Fatalf("binding = %s, want %s", bindingDigest, wantBinding)
	}
}

// TestCodeReviewPolicyDigestProvenance verifies migration 0055 accepts gate re-eval
// code_review bindings keyed by evaluation.review_policy_snapshot_digest.
func TestCodeReviewPolicyDigestProvenance(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	reviewPolicy := "always"
	inputJSON, inputHash := gateReEvalHITLInputJSON(t, head, "mergeable")
	policyDigestBytes, err := canonicalJSON(map[string]string{"gate_input_hash": inputHash, "review_policy": reviewPolicy})
	if err != nil {
		t.Fatal(err)
	}
	policyDigest := SHA256Hex(policyDigestBytes)
	verdictJSON := mustCanon(t, map[string]any{"schema_version": 1, "kind": "hitl", "code": "code_review", "head_sha": head, "review_policy": reviewPolicy})
	record := GateEvaluationRecord{
		RunID: cmdRun, GateInputHash: inputHash, GateVersion: "gate/v1", SnapshotSchemaVersion: 1,
		SnapshotJSON: json.RawMessage(inputJSON), HeadSHA: head, EffectivePolicyHash: strings.Repeat("c", 64),
		CertificationVersion: strings.Repeat("d", 64), RiskSourceVersion: "T3/fallback/v1",
		VerdictJSON: json.RawMessage(verdictJSON), VerdictDigest: SHA256Hex([]byte(verdictJSON)),
		ReviewPolicySnapshotDigest: policyDigest, ShadowDecision: "inconclusive",
		FeaturesJSON: []byte(`{"schema_version":1}`), NowMS: testNow,
	}
	if err := db.SeedProjectForTest(ctx, "cfg", "project", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, cmdRun, "project", "cfg", "42", testNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRunChangeHeadForTest(ctx, cmdRun, "change-01", head); err != nil {
		t.Fatal(err)
	}
	changeRef := "sift://change/change-01"
	eventRef := "sift://event/event:test:verdict:hitl:code_review"
	cmd := EmitInterruptCmd{
		RunID: cmdRun, ExpectedRunVersion: 1, Reason: InterruptCodeReview,
		Generation: InterruptGeneration{ChangeID: "change-01", HeadSHA: head, PolicySnapshotID: policyDigest},
		Facts: map[string]string{
			"change_ref": changeRef, "head_sha": head, "review_requirement": reviewPolicy,
			"recommended_action": "approve", "diff_ref": eventRef,
		},
		GatePhase: GateNone, GuardrailLevel: GuardrailNone, AttentionDailyQuota: interruptQuota(),
		DayTimezone: "UTC", Source: SourceSystem, NowMS: testNow,
		Channels: []InterruptChannel{{ID: "text", Capabilities: []string{"text"}}},
	}
	recorded, err := db.RecordGateEvaluation(ctx, record)
	if err != nil {
		t.Fatalf("RecordGateEvaluation: %v", err)
	}
	var stored sql.NullString
	if err := db.db.QueryRow(`SELECT review_policy_snapshot_digest FROM gate_evaluations WHERE id=?`, recorded.EvaluationID).Scan(&stored); err != nil {
		t.Fatalf("read digest: %v", err)
	}
	if !stored.Valid || stored.String != policyDigest {
		t.Fatalf("stored digest = %v, want %s", stored, policyDigest)
	}
	cmd.CalibrationID = recorded.CalibrationID
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := db.emitInterruptInExistingTx(ctx, tx, cmd, false); err != nil {
		t.Fatalf("emitInterruptInExistingTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestGateReEvalInterruptSeamReplayOrReject verifies generation-key replay is
// idempotent only for byte-identical closed seam fields (binding, provenance, facts).
func TestGateReEvalInterruptSeamReplayOrReject(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	head := "0123456789012345678901234567890123456789"
	inputJSON, inputHash := gateReEvalHITLInputJSON(t, head, "mergeable")
	verdictJSON := mustCanon(t, map[string]any{"schema_version": 1, "kind": "hitl", "code": "code_review", "head_sha": head, "review_policy": "always"})
	claim, fix := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, inputJSON, inputHash, "", "")
	result := canonicalResult(t, "succeeded", map[string]any{
		"gate_input_json": inputJSON, "gate_input_hash": inputHash, "gate_version": "gate/v1",
		"verdict_json": verdictJSON, "verdict_digest": SHA256Hex([]byte(verdictJSON)),
	})
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+20); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	evKey := claim.Key + ":verdict:hitl:code_review"
	var genKey, bindingJSON, storedSource, storedEvent, storedFacts string
	if err := db.db.QueryRow(`SELECT generation_key FROM interrupts WHERE run_id=? AND reason='code_review' ORDER BY created_at_ms DESC LIMIT 1`, cmdRun).Scan(&genKey); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`
SELECT b.binding_json, s.source_interrupt_id, s.created_from_event_id, s.facts_canonical_json
FROM interrupt_command_effect_bindings b
JOIN interrupts i ON i.id=b.interrupt_id
JOIN gate_re_eval_interrupt_seams s ON s.interrupt_id=i.id
WHERE i.generation_key=?`, genKey).Scan(&bindingJSON, &storedSource, &storedEvent, &storedFacts); err != nil {
		t.Fatal(err)
	}
	if storedSource != fix.interruptID || storedEvent != "event:"+evKey {
		t.Fatalf("seam provenance = %s / %s, want %s / event:%s", storedSource, storedEvent, fix.interruptID, evKey)
	}
	var facts map[string]string
	if err := json.Unmarshal([]byte(storedFacts), &facts); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seam := GateReEvaluationInterruptV1{
		RunID: cmdRun, AttemptNo: 1, Generation: 1, Reason: InterruptCodeReview,
		Facts: facts, BindingJSON: []byte(bindingJSON), GenerationKey: genKey,
		SourceInterruptID: fix.interruptID, CreatedFromEventID: "event:" + evKey,
	}
	in, err := gateReEvalReplayOrRejectInterruptTx(ctx, tx, seam)
	if err != nil || in.ID == "" {
		t.Fatalf("idempotent replay = %v in=%v", err, in)
	}
	seam.BindingJSON = []byte(`{"arm":"code_review","change_id":"forged","head_sha":"` + head + `","review_policy_snapshot_digest":"` + strings.Repeat("f", 64) + `"}`)
	if _, err := gateReEvalReplayOrRejectInterruptTx(ctx, tx, seam); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("binding collision err = %v, want ErrGateReEvaluationContract", err)
	}
	seam.BindingJSON = []byte(bindingJSON)
	seam.SourceInterruptID = "int-forged"
	if _, err := gateReEvalReplayOrRejectInterruptTx(ctx, tx, seam); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("source_interrupt_id collision err = %v, want ErrGateReEvaluationContract", err)
	}
	seam.SourceInterruptID = fix.interruptID
	seam.CreatedFromEventID = "event:forged"
	if _, err := gateReEvalReplayOrRejectInterruptTx(ctx, tx, seam); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("created_from_event_id collision err = %v, want ErrGateReEvaluationContract", err)
	}
	seam.CreatedFromEventID = "event:" + evKey
	seam.Facts = map[string]string{"recommended_action": "forged"}
	if _, err := gateReEvalReplayOrRejectInterruptTx(ctx, tx, seam); !errors.Is(err, ErrGateReEvaluationContract) {
		t.Fatalf("facts collision err = %v, want ErrGateReEvaluationContract", err)
	}
}

// TestCompleteGateReEvaluationPreconditionFailures covers the lease/Run/Interrupt
// assertion failures.
func TestCompleteGateReEvaluationPreconditionFailures(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	claim, _ := seedClaimedGateReEval(t, db, ctx, InterruptGuardrailViolation, "", "", "", "")
	result := canonicalResult(t, "failed", map[string]any{
		"failure_class": "gate_contract_failed",
		"failure_evidence": map[string]any{
			"code": "verdict_digest_mismatch",
		},
	})
	// Stale lease owner.
	stale := claim
	stale.LeaseOwner = "other"
	if err := db.CompleteGateReEvaluation(ctx, stale, result, testNow+20); !errors.Is(err, ErrRejectedStaleWorker) {
		t.Fatalf("stale lease err = %v", err)
	}
	// Run moved out of the frozen version: bump it first.
	if _, err := db.db.Exec(`UPDATE runs SET version=version+1 WHERE id=?`, cmdRun); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteGateReEvaluation(ctx, claim, result, testNow+21); !errors.Is(err, ErrRejectedStale) {
		t.Fatalf("moved run err = %v, want ErrRejectedStale", err)
	}
}
