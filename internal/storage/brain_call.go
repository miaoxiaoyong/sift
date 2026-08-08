package storage

import (
	"context"
	"errors"
	"fmt"

	"database/sql"
	"encoding/json"
)

// single-finalize call row (frozen identity/input) and immutable per-attempt
// rows. Token charging is the dedicated post-charge exception of §9.1 (brain.md §6):
// threshold-check before a physical attempt, full post-charge after it, one
// over-limit crossing allowed, idempotent via a unique operation key.

// Brain call identities and lifecycle states (storage.md §10.1).
const (
	BrainScopeIntake    = "intake"
	BrainScopeRun       = "run"
	BrainScopeAggregate = "aggregate"

	BrainCallRunning  = "running"
	BrainCallValid    = "valid"
	BrainCallFallback = "fallback"

	BrainAttemptValid         = "valid"
	BrainAttemptInvalidOutput = "invalid_output"
	BrainAttemptProviderError = "provider_error"
	BrainAttemptFallback      = "fallback"
)

// Stable provider_error_code vocabulary (brain.md §3).
const (
	ProviderErrTimeout         = "timeout"
	ProviderErrNonzeroExit     = "nonzero_exit"
	ProviderErrOutputTooLarge  = "output_too_large"
	ProviderErrInvalidEnvelope = "invalid_envelope"
	ProviderErrUsageMissing    = "usage_missing"
	ProviderErrUsageInvalid    = "usage_invalid"
	ProviderErrSpawnFailed     = "spawn_failed"
)

var (
	// ErrBrainCallFinalized is returned when a call row is not in the running
	// state: attempts and finalize are accepted only while running.
	ErrBrainCallFinalized = errors.New("storage: brain call already finalized")
	// ErrBrainRequestDrift is returned when an attempt's request_digest does
	// not equal the call's frozen input_digest (brain.md §3).
	ErrBrainRequestDrift = errors.New("storage: brain attempt request digest drift")
)

// ReserveBrainCallCmd carries the frozen identity and input of a logical call.
type ReserveBrainCallCmd struct {
	Scope               string
	SubjectKey          string
	ProjectID           string // T1: required
	RunID               string // T2–T6: required
	AttemptNo           *int   // T3–T6 only when bound to a concrete attempt
	Touchpoint          string
	PromptVersion       string
	OutputSchemaVersion int
	InputJSON           []byte // canonical JSON, stored once per call
	InputDigest         string // sha256 of the exact provider request bytes
	StartedAtMS         int64
	T7AggregateOnce     bool // scheduler-owned aggregate window idempotency
}

// ReservedBrainCall is the outcome of a successful reservation.
type ReservedBrainCall struct {
	ID       string
	CallSeq  int64
	Existing bool
}

// ReserveBrainCall increments the per-subject counter and inserts the
// status=running call row in one transaction (storage.md §10.1). It never
// uses SELECT max()+1.
func (d *DB) ReserveBrainCall(ctx context.Context, cmd ReserveBrainCallCmd) (ReservedBrainCall, error) {
	if cmd.Scope == "" || cmd.SubjectKey == "" || cmd.Touchpoint == "" || cmd.PromptVersion == "" {
		return ReservedBrainCall{}, errors.New("storage: brain call identity is incomplete")
	}
	if len(cmd.InputJSON) == 0 || !json.Valid(cmd.InputJSON) || cmd.InputDigest == "" {
		return ReservedBrainCall{}, errors.New("storage: brain call input must be valid JSON with a digest")
	}
	if cmd.OutputSchemaVersion < 1 || cmd.StartedAtMS <= 0 {
		return ReservedBrainCall{}, errors.New("storage: brain call schema version and start time are required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ReservedBrainCall{}, err
	}
	defer tx.Rollback()

	// A T7 aggregate subject names one frozen window. Returning its existing
	// logical call is the crash-safe cursor between provider completion and the
	// append-only aggregate completion row.
	if cmd.T7AggregateOnce {
		if cmd.Scope != BrainScopeAggregate || cmd.Touchpoint != "T7" {
			return ReservedBrainCall{}, errors.New("storage: T7 aggregate idempotency on another call kind")
		}
		var existing ReservedBrainCall
		var projectID, runID string
		var attemptNo sql.NullInt64
		var promptVersion string
		var schemaVersion int
		err := tx.QueryRowContext(ctx, `SELECT b.id,b.call_seq,COALESCE(b.project_id,''),COALESCE(b.run_id,''),b.attempt_no,b.prompt_version,b.output_schema_version
			FROM t7_aggregate_call_bindings x JOIN brain_calls b ON b.id=x.logical_call_id
			WHERE x.aggregate_key=?`, cmd.SubjectKey).
			Scan(&existing.ID, &existing.CallSeq, &projectID, &runID, &attemptNo, &promptVersion, &schemaVersion)
		if err == nil {
			if projectID != cmd.ProjectID || runID != "" || attemptNo.Valid || promptVersion != cmd.PromptVersion || schemaVersion != cmd.OutputSchemaVersion {
				return ReservedBrainCall{}, errors.New("storage: T7 aggregate call identity conflicts with frozen window")
			}
			existing.Existing = true
			return existing, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ReservedBrainCall{}, err
		}
	}

	var callSeq int64
	var next, version int64
	err = tx.QueryRowContext(ctx, `SELECT next_call_seq, version FROM brain_call_counters
		WHERE scope=? AND subject_key=? AND touchpoint=?`, cmd.Scope, cmd.SubjectKey, cmd.Touchpoint).Scan(&next, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		callSeq = 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO brain_call_counters
			(scope, subject_key, touchpoint, next_call_seq, version, updated_at_ms)
			VALUES (?, ?, ?, 2, 1, ?)`, cmd.Scope, cmd.SubjectKey, cmd.Touchpoint, cmd.StartedAtMS); err != nil {
			return ReservedBrainCall{}, fmt.Errorf("storage: insert brain call counter: %w", err)
		}
	case err != nil:
		return ReservedBrainCall{}, err
	default:
		callSeq = next
		res, err := tx.ExecContext(ctx, `UPDATE brain_call_counters
			SET next_call_seq=next_call_seq+1, version=version+1, updated_at_ms=?
			WHERE scope=? AND subject_key=? AND touchpoint=? AND version=?`,
			cmd.StartedAtMS, cmd.Scope, cmd.SubjectKey, cmd.Touchpoint, version)
		if err != nil {
			return ReservedBrainCall{}, fmt.Errorf("storage: bump brain call counter: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ReservedBrainCall{}, ErrRejectedStale
		}
	}

	id := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO brain_calls
		(id, scope, subject_key, project_id, run_id, attempt_no, touchpoint, call_seq,
		 prompt_version, output_schema_version, input_json, input_digest, status, started_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?)`,
		id, cmd.Scope, cmd.SubjectKey, nullable(cmd.ProjectID), nullable(cmd.RunID), cmd.AttemptNo,
		cmd.Touchpoint, callSeq, cmd.PromptVersion, cmd.OutputSchemaVersion,
		string(cmd.InputJSON), cmd.InputDigest, cmd.StartedAtMS); err != nil {
		return ReservedBrainCall{}, fmt.Errorf("storage: insert brain call: %w", err)
	}
	if cmd.T7AggregateOnce {
		if _, err := tx.ExecContext(ctx, `INSERT INTO t7_aggregate_call_bindings(aggregate_key,logical_call_id) VALUES(?,?)`, cmd.SubjectKey, id); err != nil {
			return ReservedBrainCall{}, fmt.Errorf("storage: bind T7 aggregate call: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ReservedBrainCall{}, fmt.Errorf("storage: commit reserve brain call: %w", err)
	}
	return ReservedBrainCall{ID: id, CallSeq: callSeq}, nil
}

// BrainAttemptCmd describes one physical provider attempt (1|2) or the
// synthesized pre-flight fallback row (0). Token fields are nil whenever
// usage is unknown; both must be set for a valid attempt.
type FinalizeBrainCallCmd struct {
	CallID              string
	Status              string // valid | fallback
	SelectedAttemptNo   *int   // valid: the chosen valid attempt
	ValidatedOutputJSON []byte // valid: canonical validated output
	FallbackReason      string // fallback: required
	FinishedAtMS        int64
}

// FinalizeBrainCall performs the single finalize of a running call
// (storage.md §10.1). The database trigger makes the one-time rule a fact;
// this port additionally verifies that a valid finalize points at a valid
// attempt of the same call.
func (d *DB) FinalizeBrainCall(ctx context.Context, cmd FinalizeBrainCallCmd) error {
	if cmd.Status != BrainCallValid && cmd.Status != BrainCallFallback {
		return fmt.Errorf("storage: invalid finalize status %q", cmd.Status)
	}
	if cmd.FinishedAtMS <= 0 {
		return errors.New("storage: finalize requires FinishedAtMS")
	}
	if cmd.Status == BrainCallValid {
		if cmd.SelectedAttemptNo == nil || len(cmd.ValidatedOutputJSON) == 0 || !json.Valid(cmd.ValidatedOutputJSON) {
			return errors.New("storage: valid finalize requires selected attempt and validated output")
		}
	} else if cmd.FallbackReason == "" {
		return errors.New("storage: fallback finalize requires a reason")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM brain_calls WHERE id=?`, cmd.CallID).Scan(&status); err != nil {
		return fmt.Errorf("storage: load brain call: %w", err)
	}
	if status != BrainCallRunning {
		return ErrBrainCallFinalized
	}
	if cmd.Status == BrainCallValid {
		var outcome string
		err := tx.QueryRowContext(ctx, `SELECT outcome FROM brain_attempts
			WHERE logical_call_id=? AND provider_attempt=?`, cmd.CallID, *cmd.SelectedAttemptNo).Scan(&outcome)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("storage: selected attempt %d does not exist", *cmd.SelectedAttemptNo)
		}
		if err != nil {
			return err
		}
		if outcome != BrainAttemptValid {
			return fmt.Errorf("storage: selected attempt %d is not valid (outcome %s)", *cmd.SelectedAttemptNo, outcome)
		}
	}

	res, err := tx.ExecContext(ctx, `UPDATE brain_calls
		SET status=?, selected_attempt_no=?, fallback_reason=?, validated_output_json=?, finished_at_ms=?
		WHERE id=? AND status='running'`,
		cmd.Status, cmd.SelectedAttemptNo, nullable(cmd.FallbackReason),
		nullableBytes(cmd.ValidatedOutputJSON), cmd.FinishedAtMS, cmd.CallID)
	if err != nil {
		return fmt.Errorf("storage: finalize brain call: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrBrainCallFinalized
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit finalize brain call: %w", err)
	}
	return nil
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// BrainCall is the read model of a brain_calls row.
