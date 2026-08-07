package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// GateReEvaluationPayload is the frozen GateReEvaluationOperationV1 payload
// (storage.md §8.1). Every field is frozen identity sourced from the
// immutable source Interrupt binding; none is reconstructed from current
// state. It is exported so the worker can decode the claimed operation.
type GateReEvaluationPayload struct {
	SourceInterruptID    string `json:"source_interrupt_id"`
	SourceCommandEventID string `json:"source_command_event_id"`
	SourceRunVersion     int64  `json:"source_run_version"`
	RunID                string `json:"run_id"`
	AttemptNo            int    `json:"attempt_no"`
	Generation           int    `json:"generation"`
	ChangeID             string `json:"change_id"`
	HeadSHA              string `json:"head_sha"`
	GateInputSnapshotID  string `json:"gate_input_snapshot_id"`
	GateInputHash        string `json:"gate_input_hash"`
	GateVersion          string `json:"gate_version"`
	EffectBindingDigest  string `json:"effect_binding_digest"`
	OperationKey         string `json:"operation_key"`
}

// gateReEvalAttemptRow is the source Interrupt/Run context read inside the
// precondition transaction.
type gateReEvalAttemptRow struct {
	op                  GateReEvaluationPayload
	projectID, taskKind string
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
	var runStatus, taskKind string
	var runVersion int64
	var projectID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status,version,project_id,COALESCE(kind,'') FROM runs WHERE id=?`, op.RunID).Scan(&runStatus, &runVersion, &projectID, &taskKind); err != nil {
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
	return gateReEvalAttemptRow{op: op, projectID: projectID.String, taskKind: taskKind}, nil
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
