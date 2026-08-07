package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

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
		Risk     struct {
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
		"schema_version":           1,
		"operation_key":            row.op.OperationKey,
		"replacement_head_sha":     p.ReplacementHeadSHA,
		"replacement_input_hash":   p.ReplacementInputHash,
		"replacement_gate_version": p.ReplacementGateVersion,
		"result_digest":            R,
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

// insertGateReEvalRerunChecksSuccessorTx enqueues the retry_checks/flaky_retry
// successor rerun_checks operation plus one check_rerun_consumptions row in the
// same transaction as the terminal event and Run CAS (storage.md §8.1, §8.2).
// It mirrors the merge successor write. The successor payload carries only the
// §8.1 closed fields; created_from_event_id is byte-for-byte the terminal event
// id event:<K>. The operation key is frozen by (run_id, head_sha, check_run_id,
// retry_no) so a replayed transaction cannot create a second successor
// (insertOperation dedupes by key); the consumption row PK is the same
// (run, head, check, retry) quadruple and operation_id is UNIQUE, so it is
// at-most-one too. The production RerunChecksWorker claims it by kind and
// commits the §8.5 request-start boundary before calling Forge.
func insertGateReEvalRerunChecksSuccessorTx(ctx context.Context, tx *sql.Tx, row gateReEvalAttemptRow, p gateReEvalSucceededPayload, v gateReEvalVerdictProjection, eventKey string, nowMS int64) error {
	triageDigest, err := gateReEvalTriageSourceDigest(p.GateInputJSON)
	if err != nil {
		return err
	}
	key := RerunChecksOperationKey(row.op.RunID, row.op.HeadSHA, v.CheckRunID, v.RetryNo)
	payload, err := canonicalJSON(map[string]any{
		"run_id":                row.op.RunID,
		"change_id":             row.op.ChangeID,
		"head_sha":              row.op.HeadSHA,
		"check_run_id":          v.CheckRunID,
		"retry_no":              v.RetryNo,
		"triage_source_digest":  triageDigest,
		"created_from_event_id": "event:" + eventKey,
	})
	if err != nil {
		return err
	}
	if err := insertOperation(ctx, tx, Operation{
		Key: key, Kind: OperationRerunChecks, Payload: payload, RunID: row.op.RunID,
	}, row.op.RunID, "", nowMS); err != nil {
		return err
	}
	// The operation id is the durable link to the consumption row. It is read
	// back by key (the row was just inserted or deduped by insertOperation) so
	// both the fresh and the contract-stable replay path resolve the same id.
	var opID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM outbox_operations WHERE operation_key=?`, key).Scan(&opID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO check_rerun_consumptions (run_id, head_sha, check_run_id, retry_no, operation_id, created_at_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		row.op.RunID, row.op.HeadSHA, v.CheckRunID, v.RetryNo, opID, nowMS)
	return err
}

// gateReEvalTriageSourceDigest returns SHA-256(canonical_json(GateInputV1.checks.triage.source))
// (storage.md §8.1). A retry_checks/flaky_retry successor requires a non-empty
// frozen triage source; a missing source is a contract violation, never a
// field reconstructed from current Change or checks state.
func gateReEvalTriageSourceDigest(gateInputJSON string) (string, error) {
	var src struct {
		Checks struct {
			Triage *struct {
				Source json.RawMessage `json:"source"`
			} `json:"triage"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(gateInputJSON), &src); err != nil {
		return "", fmt.Errorf("%w: gate input decode for triage source: %v", ErrGateReEvaluationContract, err)
	}
	if src.Checks.Triage == nil || len(src.Checks.Triage.Source) == 0 || string(src.Checks.Triage.Source) == "null" {
		return "", fmt.Errorf("%w: retry_checks verdict requires checks.triage.source", ErrGateReEvaluationContract)
	}
	var node any
	if err := json.Unmarshal(src.Checks.Triage.Source, &node); err != nil {
		return "", fmt.Errorf("%w: triage source is not JSON: %v", ErrGateReEvaluationContract, err)
	}
	canon, err := canonicalJSON(node)
	if err != nil {
		return "", fmt.Errorf("%w: triage source canonical encode: %v", ErrGateReEvaluationContract, err)
	}
	return sha256Hex(canon), nil
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
