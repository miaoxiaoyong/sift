package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ----- failed arm ----------------------------------------------------------

type gateReEvalFailedPayload struct {
	FailureClass    string          `json:"failure_class"`
	FailureEvidence json.RawMessage `json:"failure_evidence"`
}

func (d *DB) completeGateReEvalFailedTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, row gateReEvalAttemptRow, payload json.RawMessage, resultCanon []byte, nowMS int64) error {
	var p gateReEvalFailedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("%w: failed payload: %v", ErrGateReEvaluationContract, err)
	}
	if err := validateGateReEvalFailure(p.FailureClass, p.FailureEvidence); err != nil {
		return err
	}
	R := sha256Hex(resultCanon)
	eventKey := row.op.OperationKey + ":failed"
	evPayload, err := canonicalJSON(map[string]any{
		"schema_version":   1,
		"operation_key":    row.op.OperationKey,
		"result_digest":    R,
		"failure_class":    p.FailureClass,
		"failure_evidence": json.RawMessage(p.FailureEvidence),
	})
	if err != nil {
		return err
	}
	if err := insertGateReEvalEventTx(ctx, tx, row, "gate.reevaluation.failed", eventKey, evPayload, nowMS); err != nil {
		return err
	}
	if err := gateReEvalBumpRunVersionTx(ctx, tx, row.op, nowMS); err != nil {
		return err
	}
	if _, err := d.emitGateReEvalFailedFailureReviewTx(ctx, tx, row, p.FailureClass, eventKey, nowMS); err != nil {
		return err
	}
	return finalizeGateReEvalOpTx(ctx, tx, claim, OperationFailed, R, nowMS)
}

func (d *DB) emitGateReEvalFailedFailureReviewTx(ctx context.Context, tx *sql.Tx, row gateReEvalAttemptRow, failureClass, eventKey string, nowMS int64) (Interrupt, error) {
	cfg := d.gateReEvalInterruptEmission()
	if cfg.AttentionDailyQuota == nil {
		return Interrupt{}, errors.New("storage: gate re-eval interrupt emission not configured")
	}
	calibrationID, err := gateReEvalFailureReviewCalibrationTx(ctx, tx, row, nowMS)
	if err != nil {
		return Interrupt{}, err
	}
	facts := map[string]string{
		"failure_class":        failureClass,
		"failure_evidence_ref": "sift://event/event:" + eventKey,
		"recommended_action":   "retry",
	}
	factsJSON, err := canonicalJSON(facts)
	if err != nil {
		return Interrupt{}, err
	}
	failureDigest := sha256Hex(factsJSON)
	attemptNo := row.op.AttemptNo
	cmd := EmitInterruptCmd{
		RunID:                  row.op.RunID,
		ExpectedRunVersion:     row.op.SourceRunVersion + 1,
		AttemptNo:              &attemptNo,
		Reason:                 InterruptFailureReview,
		FailureReviewVariant:   FailureReviewAttempt,
		FailureReviewRetryKind: FailureReviewGateRecheck,
		Facts:                  facts,
		Generation: InterruptGeneration{
			AttemptNo:     row.op.AttemptNo,
			Generation:    row.op.Generation,
			ChangeID:      row.op.ChangeID,
			HeadSHA:       row.op.HeadSHA,
			FailureDigest: failureDigest,
		},
		GatePhase:           GateReview,
		GuardrailLevel:      GuardrailNone,
		AttentionDailyQuota: cfg.AttentionDailyQuota,
		DayTimezone:         cfg.DayTimezone,
		DailySummaryAt:      cfg.DailySummaryAt,
		MaxEscalations:      cfg.MaxEscalations,
		CriticalWindowMS:    cfg.CriticalWindowMS,
		CriticalTotalLimit:  cfg.CriticalTotalLimit,
		CriticalPerRunLimit: cfg.CriticalPerRunLimit,
		Channels:            cfg.Channels,
		CalibrationID:       calibrationID,
		Source:              SourceSystem,
		NowMS:               nowMS,
	}
	return d.emitInterruptInExistingTx(ctx, tx, cmd, false)
}

// gateReEvalFailureReviewCalibrationTx allocates a fresh calibration_entries
// row tied to the source Interrupt's gate evaluation. The failed arm records no
// new gate evaluation (it has no VerdictV1), so the failure_review(gate_recheck)
// successor inherits the source snapshot's change/head provenance. A fresh row
// is required because interrupts.calibration_id is unique (migration 0007), so
// the successor cannot reuse the source Interrupt's calibration directly. This
// is what satisfies the failure_review_gate_recheck_provenance_insert trigger
// (migration 0051).
func gateReEvalFailureReviewCalibrationTx(ctx context.Context, tx *sql.Tx, row gateReEvalAttemptRow, nowMS int64) (string, error) {
	var gateEvaluationID, predictedDecision, featuresJSON string
	if err := tx.QueryRowContext(ctx, `SELECT c.gate_evaluation_id,c.predicted_decision,c.features_json FROM calibration_entries c JOIN interrupts i ON i.calibration_id=c.id WHERE i.id=?`, row.op.SourceInterruptID).Scan(&gateEvaluationID, &predictedDecision, &featuresJSON); err != nil {
		return "", fmt.Errorf("%w: source calibration missing: %v", ErrGateReEvaluationAssertion, err)
	}
	id := newID()
	// gate_sample_entry_id is left NULL: the successor inherits the source
	// evaluation's change/head provenance but owns no distinct gate sample, and
	// the column carries a per-row unique index (migration 0008).
	if _, err := tx.ExecContext(ctx, `INSERT INTO calibration_entries (id,run_id,gate_evaluation_id,predicted_decision,features_json,gate_sample_entry_id,predicted_at_ms) VALUES (?,?,?,?,?,?,?)`, id, row.op.RunID, gateEvaluationID, predictedDecision, featuresJSON, sql.NullString{}, nowMS); err != nil {
		return "", err
	}
	return id, nil
}

// emitGateReEvalHITLSuccessorTx emits the section 8.1 HITL verdict Interrupt successor
// inside the same CompleteGateReEvaluation transaction as the terminal event and
// Run CAS. The caller must have already recorded the gate evaluation so
// recorded.CalibrationID satisfies binding provenance triggers.
func (d *DB) emitGateReEvalHITLSuccessorTx(ctx context.Context, tx *sql.Tx, row gateReEvalAttemptRow, v gateReEvalVerdictProjection, vf gateReEvalVerdictFields, p gateReEvalSucceededPayload, recorded RecordedGateEvaluation, eventKey string, nowMS int64) (Interrupt, error) {
	cfg := d.gateReEvalInterruptEmission()
	if cfg.AttentionDailyQuota == nil {
		return Interrupt{}, errors.New("storage: gate re-eval interrupt emission not configured")
	}
	eventRef := "sift://event/event:" + eventKey
	changeRef := "sift://change/" + row.op.ChangeID
	attemptNo := row.op.AttemptNo
	cmd := EmitInterruptCmd{
		RunID:               row.op.RunID,
		ExpectedRunVersion:  row.op.SourceRunVersion + 1,
		AttemptNo:           &attemptNo,
		GatePhase:           GateReview,
		GuardrailLevel:      GuardrailNone,
		AttentionDailyQuota: cfg.AttentionDailyQuota,
		DayTimezone:         cfg.DayTimezone,
		DailySummaryAt:      cfg.DailySummaryAt,
		MaxEscalations:      cfg.MaxEscalations,
		CriticalWindowMS:    cfg.CriticalWindowMS,
		CriticalTotalLimit:  cfg.CriticalTotalLimit,
		CriticalPerRunLimit: cfg.CriticalPerRunLimit,
		Channels:            cfg.Channels,
		CalibrationID:       recorded.CalibrationID,
		Source:              SourceSystem,
		NowMS:               nowMS,
		Generation: InterruptGeneration{
			AttemptNo:  row.op.AttemptNo,
			Generation: row.op.Generation,
			ChangeID:   row.op.ChangeID,
			HeadSHA:    row.op.HeadSHA,
		},
	}
	switch v.Kind + "/" + v.Code {
	case "hitl/checks_timeout":
		if vf.ExternalURL == "" {
			return Interrupt{}, fmt.Errorf("%w: checks_timeout missing external_url", ErrGateReEvaluationContract)
		}
		cmd.Reason = InterruptFailureReview
		cmd.FailureReviewVariant = FailureReviewAttempt
		cmd.FailureReviewRetryKind = FailureReviewGateRecheck
		cmd.Facts = map[string]string{
			"failure_class":        "checks_timeout",
			"failure_evidence_ref": vf.ExternalURL,
			"recommended_action":   "retry",
		}
	case "hitl/failure_review":
		if vf.ExternalURL == "" || vf.Classification == "" {
			return Interrupt{}, fmt.Errorf("%w: failure_review missing external_url or classification", ErrGateReEvaluationContract)
		}
		cmd.Reason = InterruptFailureReview
		cmd.FailureReviewVariant = FailureReviewAttempt
		cmd.FailureReviewRetryKind = FailureReviewGateRecheck
		cmd.Facts = map[string]string{
			"failure_class":        "checks_" + vf.Classification,
			"failure_evidence_ref": vf.ExternalURL,
			"recommended_action":   "retry",
		}
	case "hitl/mergeability_unknown":
		cmd.Reason = InterruptFailureReview
		cmd.FailureReviewVariant = FailureReviewAttempt
		cmd.FailureReviewRetryKind = FailureReviewGateRecheck
		cmd.Facts = map[string]string{
			"failure_class":        "mergeability_unknown",
			"failure_evidence_ref": eventRef,
			"recommended_action":   "retry",
		}
	case "hitl/input_unknown":
		if vf.Field == "" {
			return Interrupt{}, fmt.Errorf("%w: input_unknown missing field", ErrGateReEvaluationContract)
		}
		cmd.Reason = InterruptFailureReview
		cmd.FailureReviewVariant = FailureReviewAttempt
		cmd.FailureReviewRetryKind = FailureReviewGateRecheck
		cmd.Facts = map[string]string{
			"failure_class":        "gate_input_unknown:" + vf.Field,
			"failure_evidence_ref": eventRef,
			"recommended_action":   "retry",
		}
	case "hitl/guardrail_violation":
		if vf.RuleID == "" || vf.MatchedPathsDigest == "" {
			return Interrupt{}, fmt.Errorf("%w: guardrail_violation missing rule_id or matched_paths_digest", ErrGateReEvaluationContract)
		}
		var proj struct {
			EffectivePolicyHash string `json:"effective_policy_hash"`
		}
		if err := json.Unmarshal([]byte(p.GateInputJSON), &proj); err != nil || proj.EffectivePolicyHash == "" {
			return Interrupt{}, fmt.Errorf("%w: gate input missing effective_policy_hash", ErrGateReEvaluationContract)
		}
		cmd.Reason = InterruptGuardrailViolation
		cmd.GuardrailLevel = GuardrailSoft
		cmd.Generation.PolicySnapshotID = proj.EffectivePolicyHash
		cmd.Generation.ViolationCode = vf.RuleID
		cmd.Generation.SubjectDigest = vf.MatchedPathsDigest
		cmd.Facts = map[string]string{
			"rule_id":             vf.RuleID,
			"impact_scope":        "matched_paths:" + vf.MatchedPathsDigest,
			"recommended_action":  "approve",
			"policy_evidence_ref": eventRef,
		}
	case "hitl/code_review":
		if vf.ReviewPolicy == "" {
			return Interrupt{}, fmt.Errorf("%w: code_review missing review_policy", ErrGateReEvaluationContract)
		}
		policyDigest, err := gateReEvalCodeReviewPolicyDigest(p.GateInputHash, vf.ReviewPolicy)
		if err != nil {
			return Interrupt{}, err
		}
		cmd.Reason = InterruptCodeReview
		cmd.Generation.PolicySnapshotID = policyDigest
		cmd.Facts = map[string]string{
			"change_ref":         changeRef,
			"head_sha":           row.op.HeadSHA,
			"review_requirement": vf.ReviewPolicy,
			"recommended_action": "approve",
			"diff_ref":           eventRef,
		}
	case "hitl/merge_conflict":
		cmd.Reason = InterruptMergeConflict
		cmd.GatePhase = GateMerge
		cmd.Generation.ConflictDigest = MergeConflictDigest(row.op.ChangeID, row.op.HeadSHA)
		cmd.Facts = map[string]string{
			"change_ref":            changeRef,
			"head_sha":              row.op.HeadSHA,
			"conflict_summary":      "mergeability=conflicting",
			"recommended_action":    "retry",
			"conflict_evidence_ref": eventRef,
		}
	default:
		return Interrupt{}, fmt.Errorf("%w: hitl verdict %s/%s not wired", ErrGateReEvaluationSuccessorNotWired, v.Kind, v.Code)
	}
	if cmd.Reason == InterruptFailureReview {
		factsJSON, err := canonicalJSON(cmd.Facts)
		if err != nil {
			return Interrupt{}, err
		}
		cmd.Generation.FailureDigest = sha256Hex(factsJSON)
	}
	bindingJSON, bindingReason := interruptEffectBinding(cmd)
	if bindingReason != string(cmd.Reason) {
		return Interrupt{}, fmt.Errorf("%w: binding reason mismatch for %s", ErrGateReEvaluationContract, cmd.Reason)
	}
	genKey, err := interruptGenerationKeyFor(cmd)
	if err != nil {
		return Interrupt{}, err
	}
	seam := GateReEvaluationInterruptV1{
		RunID:              cmd.RunID,
		AttemptNo:          row.op.AttemptNo,
		Generation:         row.op.Generation,
		Reason:             cmd.Reason,
		Facts:              cmd.Facts,
		BindingJSON:        bindingJSON,
		BindingDigest:      sha256Hex(bindingJSON),
		GenerationKey:      genKey,
		SourceInterruptID:  row.op.SourceInterruptID,
		CreatedFromEventID: "event:" + eventKey,
	}
	if err := validateGateReEvalInterruptV1(seam, cmd); err != nil {
		return Interrupt{}, err
	}
	if in, err := gateReEvalReplayOrRejectInterruptTx(ctx, tx, seam); err != nil {
		return Interrupt{}, err
	} else if in.ID != "" {
		return in, nil
	}
	in, err := d.emitInterruptInExistingTx(ctx, tx, cmd, false)
	if err != nil {
		return Interrupt{}, err
	}
	if err := gateReEvalPersistInterruptSeamTx(ctx, tx, in.ID, seam, nowMS); err != nil {
		return Interrupt{}, err
	}
	return in, nil
}

// gateReEvalPersistInterruptSeamTx records the closed GateReEvaluationInterruptV1
// seam so generation-key replay can verify full-field provenance closure.
func gateReEvalPersistInterruptSeamTx(ctx context.Context, tx *sql.Tx, interruptID string, seam GateReEvaluationInterruptV1, nowMS int64) error {
	if seam.SourceInterruptID == "" || seam.CreatedFromEventID == "" || len(seam.Facts) == 0 {
		return fmt.Errorf("%w: incomplete gate re-eval interrupt seam", ErrGateReEvaluationContract)
	}
	factsJSON, err := canonicalJSON(seam.Facts)
	if err != nil {
		return err
	}
	factsDigest := sha256Hex(factsJSON)
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_re_eval_interrupt_seams (interrupt_id,source_interrupt_id,created_from_event_id,facts_canonical_json,facts_digest,created_at_ms) VALUES (?,?,?,?,?,?)`,
		interruptID, seam.SourceInterruptID, seam.CreatedFromEventID, string(factsJSON), factsDigest, nowMS); err != nil {
		return fmt.Errorf("%w: persist gate re-eval interrupt seam: %v", ErrGateReEvaluationContract, err)
	}
	return nil
}

// gateReEvalReplayOrRejectInterruptTx enforces closed GateReEvaluationInterruptV1
// replay: an existing generation key is idempotent only when reason, binding,
// source_interrupt_id, created_from_event_id, and facts are byte-identical;
// otherwise the transaction must roll back.
func gateReEvalReplayOrRejectInterruptTx(ctx context.Context, tx *sql.Tx, seam GateReEvaluationInterruptV1) (Interrupt, error) {
	existing, found, err := interruptByKeyTx(ctx, tx, seam.GenerationKey)
	if err != nil {
		return Interrupt{}, err
	}
	if !found {
		return Interrupt{}, nil
	}
	var storedBinding, storedReason string
	if err := tx.QueryRowContext(ctx, `SELECT b.binding_json, i.reason FROM interrupt_command_effect_bindings b JOIN interrupts i ON i.id=b.interrupt_id WHERE i.id=?`, existing.ID).Scan(&storedBinding, &storedReason); err != nil {
		return Interrupt{}, fmt.Errorf("%w: generation key collision without binding: %v", ErrGateReEvaluationContract, err)
	}
	if storedReason != string(seam.Reason) || storedBinding != string(seam.BindingJSON) {
		return Interrupt{}, fmt.Errorf("%w: generation key collision with divergent seam", ErrGateReEvaluationContract)
	}
	wantFactsJSON, err := canonicalJSON(seam.Facts)
	if err != nil {
		return Interrupt{}, err
	}
	wantFactsDigest := sha256Hex(wantFactsJSON)
	var storedSource, storedEvent, storedFacts, storedFactsDigest string
	err = tx.QueryRowContext(ctx, `SELECT source_interrupt_id,created_from_event_id,facts_canonical_json,facts_digest FROM gate_re_eval_interrupt_seams WHERE interrupt_id=?`, existing.ID).Scan(&storedSource, &storedEvent, &storedFacts, &storedFactsDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return Interrupt{}, fmt.Errorf("%w: generation key collision without seam provenance", ErrGateReEvaluationContract)
	}
	if err != nil {
		return Interrupt{}, fmt.Errorf("%w: seam provenance lookup: %v", ErrGateReEvaluationContract, err)
	}
	if storedSource != seam.SourceInterruptID {
		return Interrupt{}, fmt.Errorf("%w: generation key collision with divergent source_interrupt_id", ErrGateReEvaluationContract)
	}
	if storedEvent != seam.CreatedFromEventID {
		return Interrupt{}, fmt.Errorf("%w: generation key collision with divergent created_from_event_id", ErrGateReEvaluationContract)
	}
	if storedFacts != string(wantFactsJSON) || storedFactsDigest != wantFactsDigest {
		return Interrupt{}, fmt.Errorf("%w: generation key collision with divergent facts", ErrGateReEvaluationContract)
	}
	return existing, nil
}

func validateGateReEvalFailure(class string, evidence json.RawMessage) error {
	var ev map[string]any
	if err := json.Unmarshal(evidence, &ev); err != nil {
		return fmt.Errorf("%w: failure_evidence not JSON: %v", ErrGateReEvaluationContract, err)
	}
	switch class {
	case "forge_read_failed":
		stage, _ := ev["stage"].(string)
		errorClass, _ := ev["error_class"].(string)
		digest, _ := ev["evidence_digest"].(string)
		if (stage != "get_change" && stage != "get_checks" && stage != "get_reviews") ||
			(errorClass != "transient" && errorClass != "rate_limited" && errorClass != "auth_or_capability") ||
			digest == "" || len(ev) != 3 {
			return fmt.Errorf("%w: invalid forge_read_failed evidence", ErrGateReEvaluationContract)
		}
	case "gate_input_assembly_failed":
		code, _ := ev["code"].(string)
		field, _ := ev["field"].(string)
		if (code != "paths_incomplete" && code != "schema_invalid") || field == "" || len(ev) != 2 {
			return fmt.Errorf("%w: invalid gate_input_assembly_failed evidence", ErrGateReEvaluationContract)
		}
	case "gate_contract_failed":
		code, _ := ev["code"].(string)
		if (code != "input_hash_mismatch" && code != "verdict_digest_mismatch" && code != "verdict_schema_invalid") || len(ev) != 1 {
			return fmt.Errorf("%w: invalid gate_contract_failed evidence", ErrGateReEvaluationContract)
		}
	default:
		return fmt.Errorf("%w: unknown failure_class %q", ErrGateReEvaluationContract, class)
	}
	return nil
}
