package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Gate re-evaluation terminal protocol v1 (storage.md §8.1).
//
// CompleteGateReEvaluation is the sole write port that closes a
// gate_re_evaluation outbox operation. The worker assembles and verifies Gate
// facts outside the transaction, then submits the closed
// GateReEvaluationResultV1 canonical bytes. This method alone allocates the
// evaluation/event IDs, performs the lease + Run + source-Interrupt
// assertions, writes the terminal event, applies the Run CAS and persists any
// frozen successor in one transaction. It never calls EmitInterrupt or
// RecordGateEvaluation from the outside: the succeeded arm reuses the internal
// snapshot/cache/evaluation recorder inside this transaction.
//
// Scope of this implementation (storage.md §8.1 matrix):
//   - succeeded + verdicts with no successor Interrupt/operation:
//     failed/change_not_open, failed/hard_guardrail (Run -> failed(gate_verdict)),
//     wait_checks/checks_pending (Run -> running),
//     ready/no_auto_merge (Run -> done(gate_passed_no_auto_merge)).
//   - succeeded ready/merge: Run -> running(gate_merge_requested) plus the
//     sole merge_change successor operation enqueued in the same transaction
//     (Run CAS + terminal gate.reevaluation.ready.merge event).
//   - failed result union (forge_read_failed | gate_input_assembly_failed |
//     gate_contract_failed): terminal gate.reevaluation.failed event + Run CAS
//     (waiting_human, version+1) + failure_review Interrupt successor.
//   - conflict: replacement-head successor operation + Run CAS + terminal
//     gate.reevaluation.conflict event.
//
// Deferred to a follow-up slice (returned as ErrGateReEvaluationSuccessorNotWired
// so the worker can terminate the operation rather than leave it pending):
//   - HITL verdict successors (failure_review Interrupt emission),
//   - retry_checks/flaky_retry (rerun_checks successor operation).
//
// The failed-arm failure_review successor and ready/merge -> merge_change are
// minimal honest closures; they do not claim once-charge, the full §8.1 matrix,
// or M5.
//
// The exact digest vectors in storage.md §8.1 for the failed result union and
// the continuous conflict→replacement Complete are reproduced by the tests.

// ErrGateReEvaluationContract is a closed contract violation: non-canonical
// result bytes, an unknown result kind, a hash/version/digest mismatch, or an
// illegal verdict payload.
var ErrGateReEvaluationContract = errors.New("storage: gate re-evaluation contract violation")

// ErrGateReEvaluationSuccessorNotWired signals that the submitted result
// requires a successor (HITL Interrupt or rerun_checks operation) whose
// emission is not yet wired in this slice. The failed-arm failure_review
// successor is wired; this error remains for HITL verdict and rerun_checks arms.
var ErrGateReEvaluationSuccessorNotWired = errors.New("storage: gate re-evaluation successor not wired")

// ErrGateReEvaluationAssertion signals that the frozen lease/Run/Interrupt/
// close-event/binding precondition did not hold.
var ErrGateReEvaluationAssertion = errors.New("storage: gate re-evaluation assertion failed")

// CompleteGateReEvaluation closes one claimed gate_re_evaluation operation
// using the §8.1 terminal protocol. resultJSON must be canonical
// GateReEvaluationResultV1 bytes.
func (d *DB) CompleteGateReEvaluation(ctx context.Context, claim ClaimedOperation, resultJSON []byte, nowMS int64) error {
	if claim.Kind != OperationGateReEvaluation {
		return fmt.Errorf("%w: not a gate_re_evaluation operation", ErrGateReEvaluationContract)
	}
	if nowMS <= 0 {
		return errors.New("storage: NowMS is required")
	}
	// Reject non-canonical bytes: the worker must submit canonical JSON. Any
	// drift (key order, whitespace, escaping) is a contract violation.
	canon, kind, payload, err := decodeGateReEvalResult(resultJSON)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	op, err := assertGateReEvalPreconditionsTx(ctx, tx, claim, nowMS)
	if err != nil {
		return err
	}
	switch kind {
	case "succeeded":
		err = d.completeGateReEvalSucceededTx(ctx, tx, claim, op, payload, canon, nowMS)
	case "failed":
		err = d.completeGateReEvalFailedTx(ctx, tx, claim, op, payload, canon, nowMS)
	case "conflict":
		err = d.completeGateReEvalConflictTx(ctx, tx, claim, op, payload, canon, nowMS)
	default:
		return fmt.Errorf("%w: unknown result kind %q", ErrGateReEvaluationContract, kind)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.wakeOutbox()
	return nil
}

// GateReEvaluationPayload is the frozen GateReEvaluationOperationV1 payload
// (storage.md §8.1). Every field is frozen identity sourced from the
// immutable source Interrupt binding; none is reconstructed from current
// state. It is exported so the worker can decode the claimed operation.
type GateReEvaluationPayload struct {
	SourceInterruptID     string `json:"source_interrupt_id"`
	SourceCommandEventID  string `json:"source_command_event_id"`
	SourceRunVersion      int64  `json:"source_run_version"`
	RunID                 string `json:"run_id"`
	AttemptNo             int    `json:"attempt_no"`
	Generation            int    `json:"generation"`
	ChangeID              string `json:"change_id"`
	HeadSHA               string `json:"head_sha"`
	GateInputSnapshotID   string `json:"gate_input_snapshot_id"`
	GateInputHash         string `json:"gate_input_hash"`
	GateVersion           string `json:"gate_version"`
	EffectBindingDigest   string `json:"effect_binding_digest"`
	OperationKey          string `json:"operation_key"`
}

// gateReEvalAttemptRow is the source Interrupt/Run context read inside the
// precondition transaction.
type gateReEvalAttemptRow struct {
	op        GateReEvaluationPayload
	projectID string
}

func assertGateReEvalPreconditionsTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, nowMS int64) (gateReEvalAttemptRow, error) {
	// Lease CAS: the operation must still be executing under this lease.
	var leaseOne int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM outbox_operations WHERE id=? AND state='executing' AND lease_owner=? AND lease_expires_at_ms=?`, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS).Scan(&leaseOne); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return gateReEvalAttemptRow{}, ErrRejectedStaleWorker
		}
		return gateReEvalAttemptRow{}, err
	}
	var op GateReEvaluationPayload
	if err := json.Unmarshal(claim.Payload, &op); err != nil {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: corrupt operation payload: %v", ErrGateReEvaluationContract, err)
	}
	if op.OperationKey == "" || op.SourceInterruptID == "" || op.RunID == "" || op.SourceRunVersion < 1 || op.GateInputHash == "" || op.GateVersion == "" || op.EffectBindingDigest == "" || op.SourceCommandEventID == "" {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: incomplete operation identity", ErrGateReEvaluationContract)
	}
	if op.OperationKey != claim.Key {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: operation key mismatch", ErrGateReEvaluationContract)
	}
	// Run assertion: (id=run_id, status=waiting_human, version=source_run_version).
	var runStatus string
	var runVersion int64
	var projectID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, version, project_id FROM runs WHERE id=?`, op.RunID).Scan(&runStatus, &runVersion, &projectID); err != nil {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: source run missing: %v", ErrGateReEvaluationAssertion, err)
	}
	if RunStatus(runStatus) != RunWaitingHuman || runVersion != op.SourceRunVersion {
		return gateReEvalAttemptRow{}, ErrRejectedStale
	}
	// Source Interrupt assertion: (id, status=closed, close_reason=responded).
	var iStatus, iCloseReason string
	if err := tx.QueryRowContext(ctx, `SELECT status, close_reason FROM interrupts WHERE id=?`, op.SourceInterruptID).Scan(&iStatus, &iCloseReason); err != nil {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: source interrupt missing: %v", ErrGateReEvaluationAssertion, err)
	}
	if iStatus != "closed" || iCloseReason != "responded" {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: source interrupt not closed/responded", ErrGateReEvaluationAssertion)
	}
	// Source Command close event exists with the frozen id.
	var eventOne int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id=?`, op.SourceCommandEventID).Scan(&eventOne); err != nil {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: source command close event missing: %v", ErrGateReEvaluationAssertion, err)
	}
	// effect_binding_digest must equal the immutable source Interrupt binding.
	var bindingDigest string
	if err := tx.QueryRowContext(ctx, `SELECT binding_digest FROM interrupt_command_effect_bindings WHERE interrupt_id=?`, op.SourceInterruptID).Scan(&bindingDigest); err != nil {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: source effect binding missing: %v", ErrGateReEvaluationAssertion, err)
	}
	if bindingDigest != op.EffectBindingDigest {
		return gateReEvalAttemptRow{}, fmt.Errorf("%w: effect binding digest mismatch", ErrGateReEvaluationAssertion)
	}
	return gateReEvalAttemptRow{op: op, projectID: projectID.String}, nil
}

// decodeGateReEvalResult canonicalizes the result bytes and extracts the kind
// and payload. Non-canonical bytes or an unknown envelope is a contract
// violation.
func decodeGateReEvalResult(resultJSON []byte) (canonical []byte, kind string, payload json.RawMessage, err error) {
	var node any
	if uerr := json.Unmarshal(resultJSON, &node); uerr != nil {
		return nil, "", nil, fmt.Errorf("%w: result is not JSON: %v", ErrGateReEvaluationContract, uerr)
	}
	canon, cerr := canonicalJSON(node)
	if cerr != nil {
		return nil, "", nil, fmt.Errorf("%w: canonical encode: %v", ErrGateReEvaluationContract, cerr)
	}
	if !bytes.Equal(canon, resultJSON) {
		return nil, "", nil, fmt.Errorf("%w: result bytes are not canonical", ErrGateReEvaluationContract)
	}
	var env struct {
		SchemaVersion int             `json:"schema_version"`
		Kind          string          `json:"kind"`
		Payload       json.RawMessage `json:"payload"`
	}
	if jerr := json.Unmarshal(canon, &env); jerr != nil {
		return nil, "", nil, fmt.Errorf("%w: result envelope: %v", ErrGateReEvaluationContract, jerr)
	}
	if env.SchemaVersion != 1 {
		return nil, "", nil, fmt.Errorf("%w: unsupported result schema_version %d", ErrGateReEvaluationContract, env.SchemaVersion)
	}
	return canon, env.Kind, env.Payload, nil
}

// ----- succeeded arm -------------------------------------------------------

type gateReEvalSucceededPayload struct {
	GateInputJSON  string `json:"gate_input_json"`
	GateInputHash  string `json:"gate_input_hash"`
	GateVersion    string `json:"gate_version"`
	VerdictJSON    string `json:"verdict_json"`
	VerdictDigest  string `json:"verdict_digest"`
}

type gateReEvalVerdictProjection struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	HeadSHA string `json:"head_sha"`
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
	if p.GateInputHash != row.op.GateInputHash {
		return fmt.Errorf("%w: succeeded input hash differs from operation", ErrGateReEvaluationContract)
	}
	if p.GateVersion != row.op.GateVersion {
		return fmt.Errorf("%w: gate_version differs from operation", ErrGateReEvaluationContract)
	}
	if sha256Hex([]byte(p.VerdictJSON)) != p.VerdictDigest {
		return fmt.Errorf("%w: verdict_digest mismatch", ErrGateReEvaluationContract)
	}
	var v gateReEvalVerdictProjection
	if err := json.Unmarshal([]byte(p.VerdictJSON), &v); err != nil {
		return fmt.Errorf("%w: verdict decode: %v", ErrGateReEvaluationContract, err)
	}
	if v.HeadSHA != row.op.HeadSHA {
		return fmt.Errorf("%w: verdict head differs from operation", ErrGateReEvaluationContract)
	}
	// The wired succeeded matrix: no-successor verdicts plus ready/merge, whose
	// merge_change successor is enqueued below in the same transaction.
	switch v.Kind + "/" + v.Code {
	case "failed/change_not_open", "failed/hard_guardrail", "wait_checks/checks_pending", "ready/no_auto_merge", "ready/merge":
	default:
		return fmt.Errorf("%w: succeeded verdict %s/%s successor not wired", ErrGateReEvaluationSuccessorNotWired, v.Kind, v.Code)
	}
	// Record the evaluation inside this transaction: insert-or-return snapshot
	// and cache, allocate one evaluation (E), calibration and gate_sample. The
	// recorder derives every field from the decoded canonical input.
	rec, err := gateReEvalRecord(row.op, p, nowMS)
	if err != nil {
		return err
	}
	recorded, err := recordGateEvaluationTxWithIDs(ctx, tx, rec, RecordedGateEvaluation{})
	if err != nil {
		return err
	}
	if recorded.SnapshotID != row.op.GateInputSnapshotID {
		return fmt.Errorf("%w: reused snapshot id drift", ErrGateReEvaluationContract)
	}
	R := sha256Hex(resultCanon)
	// Run CAS per verdict matrix (storage.md §8.1).
	if err := gateReEvalRunCASTx(ctx, tx, row.op, v.Kind, v.Code, nowMS); err != nil {
		return err
	}
	eventKey := row.op.OperationKey + ":verdict:" + v.Kind + ":" + v.Code
	eventType := "gate.reevaluation." + v.Kind + "." + v.Code
	evPayload, err := canonicalJSON(map[string]any{
		"schema_version":         1,
		"operation_key":          row.op.OperationKey,
		"source_interrupt_id":    row.op.SourceInterruptID,
		"source_command_event_id": row.op.SourceCommandEventID,
		"gate_input_snapshot_id": recorded.SnapshotID,
		"gate_evaluation_id":     recorded.EvaluationID,
		"gate_input_hash":        p.GateInputHash,
		"gate_version":           p.GateVersion,
		"verdict_json":           p.VerdictJSON,
		"verdict_digest":         p.VerdictDigest,
		"result_digest":          R,
	})
	if err != nil {
		return err
	}
	if err := insertGateReEvalEventTx(ctx, tx, row, eventType, eventKey, evPayload, nowMS); err != nil {
		return err
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
	return finalizeGateReEvalOpTx(ctx, tx, claim, OperationSucceeded, R, nowMS)
}

// gateReEvalRecord builds the GateEvaluationRecord from the decoded canonical
// input and verdict, deriving shadow decision, features and brain-input links
// without importing the gate package (which would form an import cycle).
func gateReEvalRecord(op GateReEvaluationPayload, p gateReEvalSucceededPayload, nowMS int64) (GateEvaluationRecord, error) {
	var proj struct {
		SchemaVersion        int    `json:"schema_version"`
		EffectivePolicyHash  string `json:"effective_policy_hash"`
		CertificationVersion string `json:"certification_version"`
		Change               struct {
			HeadSHA string `json:"head_sha"`
		} `json:"change"`
		Identity gateInputIdentityJSON `json:"identity"`
		Risk struct {
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
	return GateEvaluationRecord{
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
	}, nil
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
	case "wait_checks/checks_pending", "ready/merge":
		base += `, status='running'`
	case "ready/no_auto_merge":
		base += `, status='done', change_id=COALESCE(?, change_id), change_head_sha=COALESCE(?, change_head_sha), completed_at_ms=?`
		args = append(args, nullable(op.ChangeID), nullable(op.HeadSHA), nowMS)
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
		"schema_version":  1,
		"operation_key":   row.op.OperationKey,
		"result_digest":   R,
		"failure_class":   p.FailureClass,
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
		"failure_class":         failureClass,
		"failure_evidence_ref":  "sift://event/event:" + eventKey,
		"recommended_action":    "retry",
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

// ----- conflict arm --------------------------------------------------------

type gateReEvalConflictPayload struct {
	ReplacementHeadSHA     string `json:"replacement_head_sha"`
	ReplacementInputJSON   string `json:"replacement_input_json"`
	ReplacementInputHash   string `json:"replacement_input_hash"`
	ReplacementGateVersion string `json:"replacement_gate_version"`
}

func (d *DB) completeGateReEvalConflictTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, row gateReEvalAttemptRow, payload json.RawMessage, resultCanon []byte, nowMS int64) error {
	var p gateReEvalConflictPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("%w: conflict payload: %v", ErrGateReEvaluationContract, err)
	}
	if p.ReplacementHeadSHA == "" || p.ReplacementInputJSON == "" || p.ReplacementInputHash == "" || p.ReplacementGateVersion == "" {
		return fmt.Errorf("%w: conflict payload is not closed", ErrGateReEvaluationContract)
	}
	if p.ReplacementHeadSHA == row.op.HeadSHA {
		return fmt.Errorf("%w: replacement head equals operation head", ErrGateReEvaluationContract)
	}
	if sha256Hex([]byte(p.ReplacementInputJSON)) != p.ReplacementInputHash {
		return fmt.Errorf("%w: replacement_input_hash mismatch", ErrGateReEvaluationContract)
	}
	// The replacement input must be a complete canonical GateInputV1 bound to
	// the same run/change and the replacement head.
	var rep struct {
		SchemaVersion        int    `json:"schema_version"`
		EffectivePolicyHash  string `json:"effective_policy_hash"`
		CertificationVersion string `json:"certification_version"`
		Change               struct {
			HeadSHA string `json:"head_sha"`
		} `json:"change"`
		Identity gateInputIdentityJSON `json:"identity"`
		Risk struct {
			Source struct {
				Version       string `json:"version"`
				PromptVersion string `json:"prompt_version"`
			} `json:"source"`
		} `json:"risk"`
	}
	if err := json.Unmarshal([]byte(p.ReplacementInputJSON), &rep); err != nil {
		return fmt.Errorf("%w: replacement input decode: %v", ErrGateReEvaluationContract, err)
	}
	if rep.SchemaVersion < 1 || rep.Change.HeadSHA != p.ReplacementHeadSHA || rep.Identity.RunID != row.op.RunID || rep.Identity.ChangeID != row.op.ChangeID {
		return fmt.Errorf("%w: replacement input identity mismatch", ErrGateReEvaluationContract)
	}
	riskVersion := rep.Risk.Source.Version
	if riskVersion == "" {
		riskVersion = rep.Risk.Source.PromptVersion
	}
	if rep.EffectivePolicyHash == "" || rep.CertificationVersion == "" || riskVersion == "" {
		return fmt.Errorf("%w: replacement input missing projected fields", ErrGateReEvaluationContract)
	}
	R := sha256Hex(resultCanon)
	// Run CAS: waiting_human, version+1. The post-CAS version is the source for
	// the successor operation.
	if err := gateReEvalBumpRunVersionTx(ctx, tx, row.op, nowMS); err != nil {
		return err
	}
	// Insert-or-return the replacement snapshot; S is the sole transaction-
	// assigned value carried into the successor operation.
	S, err := insertOrReturnGateSnapshotTx(ctx, tx, p.ReplacementInputHash, rep.SchemaVersion, p.ReplacementInputJSON, p.ReplacementHeadSHA, rep.EffectivePolicyHash, rep.CertificationVersion, riskVersion, nowMS)
	if err != nil {
		return err
	}
	eventKey := row.op.OperationKey + ":conflict"
	evPayload, err := canonicalJSON(map[string]any{
		"schema_version":         1,
		"operation_key":          row.op.OperationKey,
		"replacement_head_sha":   p.ReplacementHeadSHA,
		"replacement_input_hash": p.ReplacementInputHash,
		"replacement_gate_version": p.ReplacementGateVersion,
		"result_digest":          R,
	})
	if err != nil {
		return err
	}
	if err := insertGateReEvalEventTx(ctx, tx, row, "gate.reevaluation.conflict", eventKey, evPayload, nowMS); err != nil {
		return err
	}
	// Successor operation: same source identity, replacement head, post-CAS Run
	// version, copied effect binding digest (storage.md §8.1). The new key
	// cannot self-cycle because the head differs.
	newKey := GateReEvaluationOperationKey(row.op.SourceInterruptID, p.ReplacementHeadSHA)
	succPayload, err := canonicalJSON(map[string]any{
		"source_interrupt_id":     row.op.SourceInterruptID,
		"source_command_event_id": row.op.SourceCommandEventID,
		"source_run_version":      row.op.SourceRunVersion + 1,
		"run_id":                  row.op.RunID,
		"attempt_no":              row.op.AttemptNo,
		"generation":              row.op.Generation,
		"change_id":               row.op.ChangeID,
		"head_sha":                p.ReplacementHeadSHA,
		"gate_input_snapshot_id":  S,
		"gate_input_hash":         p.ReplacementInputHash,
		"gate_version":            p.ReplacementGateVersion,
		"effect_binding_digest":   row.op.EffectBindingDigest,
		"operation_key":           newKey,
	})
	if err != nil {
		return err
	}
	attemptNo := row.op.AttemptNo
	if err := insertOperation(ctx, tx, Operation{
		Key: newKey, Kind: OperationGateReEvaluation, Payload: succPayload,
		RunID: row.op.RunID, AttemptNo: &attemptNo, InterruptID: row.op.SourceInterruptID,
	}, row.op.RunID, "", nowMS); err != nil {
		return err
	}
	return finalizeGateReEvalOpTx(ctx, tx, claim, OperationConflict, R, nowMS)
}

// insertGateReEvalMergeSuccessorTx enqueues the ready/merge successor
// merge_change operation in the same transaction as the terminal event and Run
// CAS (storage.md §8.1). It mirrors the conflict arm's replacement-head
// successor write. The successor payload carries the §8.1 Gate-provenance
// closed fields (gate_input_snapshot_id, gate_evaluation_id, verdict_digest,
// created_from_event_id) plus the routing/method fields the wired MergeWorker
// needs to claim and execute the merge: project_id drives the per-project claim
// filter (outbox claim queries json_extract(payload_json,'$.project_id')), and
// method is the Forge merge method (the production reconciler uses "merge").
// created_from_event_id is byte-for-byte the terminal event id event:<K>. The
// operation key is frozen by (run_id, head_sha) so a replay cannot create a
// second successor (insertOperation dedupes by key).
func insertGateReEvalMergeSuccessorTx(ctx context.Context, tx *sql.Tx, row gateReEvalAttemptRow, recorded RecordedGateEvaluation, p gateReEvalSucceededPayload, eventKey string, nowMS int64) error {
	key := MergeChangeOperationKey(row.op.RunID, row.op.HeadSHA)
	payload, err := canonicalJSON(map[string]any{
		"project_id":             row.projectID,
		"run_id":                 row.op.RunID,
		"change_id":              row.op.ChangeID,
		"expected_head_sha":      row.op.HeadSHA,
		"gate_input_snapshot_id": recorded.SnapshotID,
		"gate_evaluation_id":     recorded.EvaluationID,
		"verdict_digest":         p.VerdictDigest,
		"created_from_event_id":  "event:" + eventKey,
		"method":                 "merge",
	})
	if err != nil {
		return err
	}
	return insertOperation(ctx, tx, Operation{
		Key: key, Kind: OperationMergeChange, Payload: payload, RunID: row.op.RunID,
	}, row.op.RunID, "", nowMS)
}

// insertOrReturnGateSnapshotTx inserts a gate_input_snapshots row for hash if
// absent and returns its id. An existing row is reused only when its canonical
// JSON and schema version match; otherwise it is a contract violation.
func insertOrReturnGateSnapshotTx(ctx context.Context, tx *sql.Tx, hash string, schemaVersion int, canonicalJSONStr, headSHA, effectivePolicyHash, certificationVersion, riskSourceVersion string, nowMS int64) (string, error) {
	canonDigest := sha256Hex([]byte(canonicalJSONStr))
	if canonDigest != hash {
		return "", fmt.Errorf("%w: replacement canonical bytes do not hash to replacement_input_hash", ErrGateReEvaluationContract)
	}
	var existingID, existingCanon string
	var existingSchema int
	err := tx.QueryRowContext(ctx, `SELECT id, schema_version, canonical_json FROM gate_input_snapshots WHERE gate_input_hash=?`, hash).Scan(&existingID, &existingSchema, &existingCanon)
	if err == nil {
		if existingSchema != schemaVersion || existingCanon != canonicalJSONStr {
			return "", fmt.Errorf("%w: existing snapshot is not byte-identical", ErrGateReEvaluationContract)
		}
		return existingID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_input_snapshots (id,gate_input_hash,schema_version,canonical_json,head_sha,effective_policy_hash,certification_version,risk_source_version,created_at_ms) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, hash, schemaVersion, canonicalJSONStr, headSHA, effectivePolicyHash, certificationVersion, riskSourceVersion, nowMS); err != nil {
		return "", err
	}
	return id, nil
}

// ----- shared helpers ------------------------------------------------------

// gateReEvalBumpRunVersionTx increments the Run version while keeping it
// waiting_human. Used by the failed and conflict arms.
func gateReEvalBumpRunVersionTx(ctx context.Context, tx *sql.Tx, op GateReEvaluationPayload, nowMS int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE runs SET version=version+1, updated_at_ms=? WHERE id=? AND status='waiting_human' AND version=?`, nowMS, op.RunID, op.SourceRunVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRejectedStale
	}
	return nil
}

func insertGateReEvalEventTx(ctx context.Context, tx *sql.Tx, row gateReEvalAttemptRow, eventType, key string, payloadJSON []byte, nowMS int64) error {
	id := "event:" + key
	attemptNo := row.op.AttemptNo
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,?,?, 'system', 1, ?, ?, ?, ?)`,
		id, nullable(row.op.RunID), nullableInt(&attemptNo), nullable(row.projectID), eventType, string(payloadJSON), key, nowMS, nowMS); err != nil {
		return err
	}
	return nil
}

// finalizeGateReEvalOpTx writes the immutable attempt result and terminals the
// operation under the same lease CAS.
func finalizeGateReEvalOpTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, state OperationState, resultDigest string, nowMS int64) error {
	outcome := map[OperationState]string{OperationSucceeded: "success", OperationConflict: "conflict", OperationFailed: "failed"}[state]
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_results (attempt_id,finished_at_ms,outcome,error_class,error_summary,evidence_digest) VALUES (?, ?, ?, NULL, NULL, ?)`, claim.AttemptID, nowMS, outcome, resultDigest); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state=?, lease_owner=NULL, lease_expires_at_ms=NULL, completed_at_ms=?, updated_at_ms=? WHERE id=? AND lease_owner=? AND lease_expires_at_ms=?`,
		state, nowMS, nowMS, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS); err != nil {
		return err
	}
	return nil
}

