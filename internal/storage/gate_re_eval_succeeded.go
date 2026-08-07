package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ----- succeeded arm -------------------------------------------------------

type gateReEvalSucceededPayload struct {
	GateInputJSON string `json:"gate_input_json"`
	GateInputHash string `json:"gate_input_hash"`
	GateVersion   string `json:"gate_version"`
	VerdictJSON   string `json:"verdict_json"`
	VerdictDigest string `json:"verdict_digest"`
}

type gateReEvalVerdictProjection struct {
	Kind            string `json:"kind"`
	Code            string `json:"code"`
	HeadSHA         string `json:"head_sha"`
	ChangeID        string `json:"change_id"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
	CheckRunID      string `json:"check_run_id"`
	RetryNo         int    `json:"retry_no"`
}

// gateReEvalVerdictFields carries the closed HITL verdict payload fields
// needed to assemble ?8.1 successor facts and bindings.
type gateReEvalVerdictFields struct {
	ExternalURL        string `json:"external_url"`
	Classification     string `json:"classification"`
	RuleID             string `json:"rule_id"`
	MatchedPathsDigest string `json:"matched_paths_digest"`
	ReviewPolicy       string `json:"review_policy"`
	Mergeability       string `json:"mergeability"`
	Field              string `json:"field"`
}

type gateInputIdentityJSON struct {
	RunID     string `json:"run_id"`
	ProjectID string `json:"project_id"`
	TaskKind  string `json:"task_kind"`
	ChangeID  string `json:"change_id"`
}

func (d *DB) completeGateReEvalSucceededTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, row gateReEvalAttemptRow, payload json.RawMessage, resultCanon []byte, nowMS int64) error {
	var p gateReEvalSucceededPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("%w: succeeded payload: %v", ErrGateReEvaluationContract, err)
	}
	if p.GateInputJSON == "" || p.GateInputHash == "" || p.GateVersion == "" || p.VerdictJSON == "" || p.VerdictDigest == "" {
		return fmt.Errorf("%w: succeeded payload is not closed", ErrGateReEvaluationContract)
	}
	// Validate input hash and gate version against the frozen operation.
	if sha256Hex([]byte(p.GateInputJSON)) != p.GateInputHash {
		return fmt.Errorf("%w: gate_input_hash mismatch", ErrGateReEvaluationContract)
	}
	if p.GateVersion != row.op.GateVersion {
		return fmt.Errorf("%w: gate_version differs from operation", ErrGateReEvaluationContract)
	}
	if sha256Hex([]byte(p.VerdictJSON)) != p.VerdictDigest {
		return fmt.Errorf("%w: verdict_digest mismatch", ErrGateReEvaluationContract)
	}
	var v gateReEvalVerdictProjection
	var vf gateReEvalVerdictFields
	if err := json.Unmarshal([]byte(p.VerdictJSON), &v); err != nil {
		return fmt.Errorf("%w: verdict decode: %v", ErrGateReEvaluationContract, err)
	}
	if v.HeadSHA != row.op.HeadSHA {
		return fmt.Errorf("%w: verdict head differs from operation", ErrGateReEvaluationContract)
	}
	if v.Kind == "ready" && v.Code == "merge" && (v.ChangeID != row.op.ChangeID || v.ExpectedHeadSHA != row.op.HeadSHA) {
		return fmt.Errorf("%w: merge verdict identity differs from operation", ErrGateReEvaluationContract)
	}
	if v.Kind == "hitl" {
		if err := json.Unmarshal([]byte(p.VerdictJSON), &vf); err != nil {
			return fmt.Errorf("%w: hitl verdict decode: %v", ErrGateReEvaluationContract, err)
		}
	}
	// The wired succeeded matrix: no-successor verdicts, all HITL arms, plus
	// ready/merge whose merge_change successor and retry_checks/flaky_retry whose
	// rerun_checks successor are enqueued below.
	switch v.Kind + "/" + v.Code {
	case "failed/change_not_open", "failed/hard_guardrail", "wait_checks/checks_pending", "ready/no_auto_merge", "ready/merge", "retry_checks/flaky_retry",
		"hitl/checks_timeout", "hitl/failure_review", "hitl/guardrail_violation", "hitl/code_review",
		"hitl/merge_conflict", "hitl/mergeability_unknown", "hitl/input_unknown":
	default:
		return fmt.Errorf("%w: succeeded verdict %s/%s successor not wired", ErrGateReEvaluationSuccessorNotWired, v.Kind, v.Code)
	}
	// retry_checks/flaky_retry carries the closed check_run_id and 1-based
	// retry_no that identify the rerun_checks successor operation (storage.md
	// §8.1). Both must be non-empty / >= 1; the verdict head already matched
	// the frozen operation head above.
	if v.Kind == "retry_checks" && v.Code == "flaky_retry" {
		if v.CheckRunID == "" || v.RetryNo < 1 {
			return fmt.Errorf("%w: retry_checks verdict missing check_run_id/retry_no", ErrGateReEvaluationContract)
		}
	}
	// Record the evaluation inside this transaction: insert-or-return snapshot
	// and cache, allocate one evaluation (E), calibration and gate_sample. The
	// recorder derives every field from the decoded canonical input.
	rec, err := gateReEvalRecord(row, p, nowMS)
	if err != nil {
		return err
	}
	recorded, err := recordGateEvaluationTxWithIDs(ctx, tx, rec, RecordedGateEvaluation{})
	if err != nil {
		return err
	}
	R := sha256Hex(resultCanon)
	// Run CAS per verdict matrix (storage.md §8.1).
	if err := gateReEvalRunCASTx(ctx, tx, row.op, v.Kind, v.Code, nowMS); err != nil {
		return err
	}
	eventKey := row.op.OperationKey + ":verdict:" + v.Kind + ":" + v.Code
	eventType := "gate.reevaluation." + v.Kind + "." + v.Code
	evPayload, err := canonicalJSON(map[string]any{
		"schema_version":          1,
		"operation_key":           row.op.OperationKey,
		"source_interrupt_id":     row.op.SourceInterruptID,
		"source_command_event_id": row.op.SourceCommandEventID,
		"gate_input_snapshot_id":  recorded.SnapshotID,
		"gate_evaluation_id":      recorded.EvaluationID,
		"gate_input_hash":         p.GateInputHash,
		"gate_version":            p.GateVersion,
		"verdict_json":            p.VerdictJSON,
		"verdict_digest":          p.VerdictDigest,
		"result_digest":           R,
	})
	if err != nil {
		return err
	}
	if err := insertGateReEvalEventTx(ctx, tx, row, eventType, eventKey, evPayload, nowMS); err != nil {
		return err
	}
	if v.Kind == "hitl" {
		if _, err := d.emitGateReEvalHITLSuccessorTx(ctx, tx, row, v, vf, p, recorded, eventKey, nowMS); err != nil {
			return err
		}
	}
	// ready/merge successor: enqueue the sole merge_change operation in the same
	// transaction as the terminal event and Run CAS (storage.md §8.1). This
	// mirrors the conflict arm's replacement-head successor write. insertOperation
	// dedupes by key, so a replayed transaction cannot double-enqueue.
	if v.Kind == "ready" && v.Code == "merge" {
		if err := insertGateReEvalMergeSuccessorTx(ctx, tx, row, recorded, p, eventKey, nowMS); err != nil {
			return err
		}
	}
	// retry_checks/flaky_retry successor: enqueue the sole rerun_checks operation
	// and one check_rerun_consumptions row in the same transaction as the terminal
	// event and Run CAS (storage.md §8.1, §8.2). Mirrors the merge successor write;
	// the consumption row is at-most-one per (run, head, check, retry_no).
	if v.Kind == "retry_checks" && v.Code == "flaky_retry" {
		if err := insertGateReEvalRerunChecksSuccessorTx(ctx, tx, row, p, v, eventKey, nowMS); err != nil {
			return err
		}
	}
	return finalizeGateReEvalOpTx(ctx, tx, claim, OperationSucceeded, R, nowMS)
}

// gateReEvalRecord builds the GateEvaluationRecord from the decoded canonical
// input and verdict, deriving shadow decision, features and brain-input links
// without importing the gate package (which would form an import cycle).
func gateReEvalRecord(row gateReEvalAttemptRow, p gateReEvalSucceededPayload, nowMS int64) (GateEvaluationRecord, error) {
	op := row.op
	var proj struct {
		SchemaVersion        int    `json:"schema_version"`
		EffectivePolicyHash  string `json:"effective_policy_hash"`
		CertificationVersion string `json:"certification_version"`
		Change               struct {
			HeadSHA      string `json:"head_sha"`
			Mergeability string `json:"mergeability"`
		} `json:"change"`
		Identity gateInputIdentityJSON `json:"identity"`
		Risk     struct {
			Source struct {
				Version       string `json:"version"`
				PromptVersion string `json:"prompt_version"`
				LogicalCallID string `json:"logical_call_id"`
			} `json:"source"`
		} `json:"risk"`
		Checks struct {
			Triage *struct {
				Source struct {
					LogicalCallID string `json:"logical_call_id"`
				} `json:"source"`
			} `json:"triage"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(p.GateInputJSON), &proj); err != nil {
		return GateEvaluationRecord{}, fmt.Errorf("%w: gate input decode: %v", ErrGateReEvaluationContract, err)
	}
	if proj.SchemaVersion < 1 || proj.Change.HeadSHA == "" || proj.EffectivePolicyHash == "" || proj.CertificationVersion == "" {
		return GateEvaluationRecord{}, fmt.Errorf("%w: gate input missing projected fields", ErrGateReEvaluationContract)
	}
	if proj.Identity.RunID != op.RunID || proj.Identity.ProjectID != row.projectID || proj.Identity.TaskKind != row.taskKind || proj.Identity.ChangeID != op.ChangeID || proj.Change.HeadSHA != op.HeadSHA {
		return GateEvaluationRecord{}, fmt.Errorf("%w: gate input identity differs from operation", ErrGateReEvaluationContract)
	}
	riskVersion := proj.Risk.Source.Version
	if riskVersion == "" {
		riskVersion = proj.Risk.Source.PromptVersion
	}
	if riskVersion == "" {
		return GateEvaluationRecord{}, fmt.Errorf("%w: gate input risk source version missing", ErrGateReEvaluationContract)
	}
	var v gateReEvalVerdictProjection
	_ = json.Unmarshal([]byte(p.VerdictJSON), &v)
	features, _ := canonicalJSON(map[string]any{
		"run":    map[string]string{"kind": proj.Identity.TaskKind},
		"change": map[string]string{"id": proj.Identity.ChangeID, "head_sha": proj.Change.HeadSHA},
	})
	var links []GateBrainInputLink
	if proj.Risk.Source.LogicalCallID != "" {
		links = append(links, GateBrainInputLink{LogicalCallID: proj.Risk.Source.LogicalCallID, Touchpoint: "T3"})
	}
	if proj.Checks.Triage != nil && proj.Checks.Triage.Source.LogicalCallID != "" {
		links = append(links, GateBrainInputLink{LogicalCallID: proj.Checks.Triage.Source.LogicalCallID, Touchpoint: "T5"})
	}
	rec := GateEvaluationRecord{
		RunID:                 op.RunID,
		GateInputHash:         p.GateInputHash,
		GateVersion:           p.GateVersion,
		SnapshotSchemaVersion: proj.SchemaVersion,
		SnapshotJSON:          json.RawMessage(p.GateInputJSON),
		VerdictJSON:           json.RawMessage(p.VerdictJSON),
		HeadSHA:               proj.Change.HeadSHA,
		EffectivePolicyHash:   proj.EffectivePolicyHash,
		CertificationVersion:  proj.CertificationVersion,
		RiskSourceVersion:     riskVersion,
		VerdictDigest:         p.VerdictDigest,
		ShadowDecision:        gateShadowDecision(v.Kind, v.Code),
		FeaturesJSON:          features,
		BrainInputLinks:       links,
		CacheHit:              false,
		NowMS:                 nowMS,
	}
	if v.Kind == "hitl" && v.Code == "merge_conflict" {
		rec.ConflictDigest = MergeConflictDigest(proj.Identity.ChangeID, proj.Change.HeadSHA)
	} else if proj.Change.Mergeability == "conflicting" {
		rec.ConflictDigest = MergeConflictDigest(proj.Identity.ChangeID, proj.Change.HeadSHA)
	}
	if v.Kind == "hitl" && v.Code == "code_review" {
		var vf gateReEvalVerdictFields
		if err := json.Unmarshal([]byte(p.VerdictJSON), &vf); err != nil {
			return GateEvaluationRecord{}, fmt.Errorf("%w: code_review verdict decode: %v", ErrGateReEvaluationContract, err)
		}
		digest, err := gateReEvalCodeReviewPolicyDigest(p.GateInputHash, vf.ReviewPolicy)
		if err != nil {
			return GateEvaluationRecord{}, err
		}
		rec.ReviewPolicySnapshotDigest = digest
	}
	return rec, nil
}

func gateReEvalCodeReviewPolicyDigest(gateInputHash, reviewPolicy string) (string, error) {
	if gateInputHash == "" || reviewPolicy == "" {
		return "", fmt.Errorf("%w: code_review policy digest inputs incomplete", ErrGateReEvaluationContract)
	}
	b, err := canonicalJSON(map[string]string{"gate_input_hash": gateInputHash, "review_policy": reviewPolicy})
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// GateReEvaluationInterruptV1 is the closed successor input to EmitInterrupt
// (storage.md section 8.1). Fields are frozen from the terminal Complete
// transaction; the emitter must not derive alternate binding or generation keys.
type GateReEvaluationInterruptV1 struct {
	RunID              string
	AttemptNo          int
	Generation         int
	Reason             InterruptReason
	Facts              map[string]string
	BindingJSON        []byte
	BindingDigest      string
	GenerationKey      string
	SourceInterruptID  string
	CreatedFromEventID string
}

func validateGateReEvalInterruptV1(seam GateReEvaluationInterruptV1, cmd EmitInterruptCmd) error {
	if seam.RunID != cmd.RunID || seam.SourceInterruptID == "" || seam.CreatedFromEventID == "" {
		return fmt.Errorf("%w: incomplete gate re-eval interrupt seam", ErrGateReEvaluationContract)
	}
	bindingJSON, bindingReason := interruptEffectBinding(cmd)
	if bindingReason != string(cmd.Reason) {
		return fmt.Errorf("%w: binding reason mismatch", ErrGateReEvaluationContract)
	}
	if !bytes.Equal(bindingJSON, seam.BindingJSON) {
		return fmt.Errorf("%w: binding_json drift", ErrGateReEvaluationContract)
	}
	if sha256Hex(bindingJSON) != seam.BindingDigest {
		return fmt.Errorf("%w: binding_digest mismatch", ErrGateReEvaluationContract)
	}
	key, err := interruptGenerationKeyFor(cmd)
	if err != nil {
		return err
	}
	if key != seam.GenerationKey {
		return fmt.Errorf("%w: generation_key drift", ErrGateReEvaluationContract)
	}
	return nil
}

func gateShadowDecision(kind, code string) string {
	switch kind + "/" + code {
	case "failed/change_not_open", "failed/hard_guardrail", "hitl/guardrail_violation", "hitl/failure_review":
		return "block"
	case "ready/merge", "ready/no_auto_merge":
		return "allow"
	default:
		return "inconclusive"
	}
}

// gateReEvalRunCASTx applies the per-verdict Run CAS. Every branch increments
// the Run version from the frozen source_run_version; any mismatch rolls back.
func gateReEvalRunCASTx(ctx context.Context, tx *sql.Tx, op GateReEvaluationPayload, kind, code string, nowMS int64) error {
	base := `UPDATE runs SET version=version+1, updated_at_ms=?`
	args := []any{nowMS}
	switch kind + "/" + code {
	case "failed/change_not_open", "failed/hard_guardrail":
		base += `, status='failed', failure_reason='gate_verdict', completed_at_ms=?`
		args = append(args, nowMS)
	case "wait_checks/checks_pending", "ready/merge", "retry_checks/flaky_retry":
		base += `, status='running'`
	case "ready/no_auto_merge":
		base += `, status='done', change_id=COALESCE(?, change_id), change_head_sha=COALESCE(?, change_head_sha), completed_at_ms=?`
		args = append(args, nullable(op.ChangeID), nullable(op.HeadSHA), nowMS)
	case "hitl/checks_timeout", "hitl/failure_review", "hitl/guardrail_violation", "hitl/code_review",
		"hitl/merge_conflict", "hitl/mergeability_unknown", "hitl/input_unknown":
		// HITL matrix: Run stays waiting_human; version increments only.
	default:
		return fmt.Errorf("%w: run CAS for %s/%s not wired", ErrGateReEvaluationSuccessorNotWired, kind, code)
	}
	base += ` WHERE id=? AND status='waiting_human' AND version=?`
	args = append(args, op.RunID, op.SourceRunVersion)
	res, err := tx.ExecContext(ctx, base, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrRejectedStale
	}
	return nil
}
