package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// Brain write/read ports for specs/storage.md §10.1. The three-table model
// splits a logical Brain call into the mutable seq counter, the
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
type BrainAttemptCmd struct {
	CallID             string
	ProviderAttempt    int
	Outcome            string
	ProviderErrorCode  string // required iff Outcome == provider_error
	RequestDigest      string // must equal the call's frozen input_digest
	RawOutputText      *string
	RawOutputDigest    *string
	RawOutputBytes     *int64
	RawOutputTruncated bool
	StderrSummary      *string
	StderrTruncated    bool
	ExitCode           *int
	InputTokens        *int64
	OutputTokens       *int64
	StartedAtMS        int64
	FinishedAtMS       int64
	// TokenLimit is the daily_token_limit in effect for the post-charge
	// counter row and the overage alert decision.
	TokenLimit int64
}

// BrainAttemptResult reports the persisted attempt and the token charge.
type BrainAttemptResult struct {
	AttemptID      string
	ChargedTokens  int64 // tokens charged by this call (0 when none/replay)
	ConsumedTokens int64 // day-bucket counter after the charge
	OverLimit      bool  // counter now beyond the daily limit
}

// BrainTokenOperationKey is the sole token charging operation key
// (brain.md §6).
func BrainTokenOperationKey(logicalCallID string, providerAttempt int) string {
	return fmt.Sprintf("brain:%s:provider:%d", logicalCallID, providerAttempt)
}

// TokenBucketStartMS is the UTC natural-day bucket start for a timestamp
// (brain.md §6: the bucket freezes at attempt start).
func TokenBucketStartMS(nowMS int64) int64 {
	const dayMS = 24 * 60 * 60 * 1000
	return nowMS - mod(nowMS, dayMS)
}

func mod(a, b int64) int64 {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

// RecordBrainAttempt appends the immutable attempt row and, when usage is
// known and positive, post-charges tokens in the same transaction (attempt
// trace + budget entry + counter, storage.md §10.1/§9.1). Replaying an
// already-recorded attempt with the same identity returns the original
// charge without double billing.
func (d *DB) RecordBrainAttempt(ctx context.Context, cmd BrainAttemptCmd) (BrainAttemptResult, error) {
	if cmd.ProviderAttempt < 0 || cmd.ProviderAttempt > 2 {
		return BrainAttemptResult{}, fmt.Errorf("storage: invalid provider_attempt %d", cmd.ProviderAttempt)
	}
	if cmd.RequestDigest == "" || cmd.StartedAtMS <= 0 || cmd.FinishedAtMS < cmd.StartedAtMS {
		return BrainAttemptResult{}, errors.New("storage: brain attempt requires digest and ordered timestamps")
	}
	if (cmd.Outcome == BrainAttemptProviderError) != (cmd.ProviderErrorCode != "") {
		return BrainAttemptResult{}, errors.New("storage: provider_error outcome requires provider_error_code and vice versa")
	}
	onlyNil := (cmd.InputTokens == nil) != (cmd.OutputTokens == nil)
	if onlyNil {
		return BrainAttemptResult{}, errors.New("storage: token usage must be both-set or both-nil")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return BrainAttemptResult{}, err
	}
	defer tx.Rollback()

	var callDigest, callStatus, callRunID string
	err = tx.QueryRowContext(ctx, `SELECT input_digest, status, COALESCE(run_id, '') FROM brain_calls WHERE id=?`, cmd.CallID).
		Scan(&callDigest, &callStatus, &callRunID)
	if err != nil {
		return BrainAttemptResult{}, fmt.Errorf("storage: load brain call: %w", err)
	}
	if cmd.RequestDigest != callDigest {
		return BrainAttemptResult{}, ErrBrainRequestDrift
	}

	// Idempotent replay: an identical attempt row already exists.
	var existingID, existingOutcome, existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT id, outcome, request_digest FROM brain_attempts
		WHERE logical_call_id=? AND provider_attempt=?`, cmd.CallID, cmd.ProviderAttempt).
		Scan(&existingID, &existingOutcome, &existingDigest)
	if err == nil {
		if existingOutcome != cmd.Outcome || existingDigest != cmd.RequestDigest {
			return BrainAttemptResult{}, fmt.Errorf("storage: brain attempt %d already recorded with different facts", cmd.ProviderAttempt)
		}
		res, err := d.existingCharge(ctx, tx, cmd)
		if err != nil {
			return BrainAttemptResult{}, err
		}
		res.AttemptID = existingID
		return res, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BrainAttemptResult{}, err
	}
	if callStatus != BrainCallRunning {
		return BrainAttemptResult{}, ErrBrainCallFinalized
	}

	attemptID := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO brain_attempts
		(id, logical_call_id, provider_attempt, outcome, provider_error_code, request_digest,
		 raw_output_text, raw_output_digest, raw_output_bytes, raw_output_truncated,
		 stderr_summary, stderr_truncated, exit_code, input_tokens, output_tokens,
		 started_at_ms, finished_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attemptID, cmd.CallID, cmd.ProviderAttempt, cmd.Outcome, nullable(cmd.ProviderErrorCode), cmd.RequestDigest,
		cmd.RawOutputText, cmd.RawOutputDigest, cmd.RawOutputBytes, boolInt(cmd.RawOutputTruncated),
		cmd.StderrSummary, boolInt(cmd.StderrTruncated), cmd.ExitCode, cmd.InputTokens, cmd.OutputTokens,
		cmd.StartedAtMS, cmd.FinishedAtMS); err != nil {
		return BrainAttemptResult{}, fmt.Errorf("storage: insert brain attempt: %w", err)
	}

	res := BrainAttemptResult{AttemptID: attemptID}
	if cmd.InputTokens != nil {
		total := *cmd.InputTokens + *cmd.OutputTokens
		if total > 0 {
			charged, consumed, err := d.postChargeTokens(ctx, tx, cmd, total, callRunID)
			if err != nil {
				return BrainAttemptResult{}, err
			}
			res.ChargedTokens, res.ConsumedTokens = charged, consumed
			res.OverLimit = cmd.TokenLimit > 0 && consumed > cmd.TokenLimit
		} else {
			// Zero usage: trace only, no budget entry (brain.md §6).
			consumed, err := d.tokenConsumedTx(ctx, tx, TokenBucketStartMS(cmd.StartedAtMS))
			if err != nil {
				return BrainAttemptResult{}, err
			}
			res.ConsumedTokens = consumed
		}
	}
	if err := tx.Commit(); err != nil {
		return BrainAttemptResult{}, fmt.Errorf("storage: commit brain attempt: %w", err)
	}
	if res.OverLimit {
		d.wakeOutbox()
	}
	return res, nil
}

// existingCharge returns the original charge for a replayed attempt without
// re-billing (unique operation key idempotency, brain.md §6).
func (d *DB) existingCharge(ctx context.Context, tx *sql.Tx, cmd BrainAttemptCmd) (BrainAttemptResult, error) {
	var res BrainAttemptResult
	var amount int64
	err := tx.QueryRowContext(ctx, `SELECT amount FROM budget_entries WHERE operation_key=?`,
		BrainTokenOperationKey(cmd.CallID, cmd.ProviderAttempt)).Scan(&amount)
	if err == nil {
		res.ChargedTokens = amount
	} else if !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	consumed, err := d.tokenConsumedTx(ctx, tx, TokenBucketStartMS(cmd.StartedAtMS))
	if err != nil {
		return res, err
	}
	res.ConsumedTokens = consumed
	res.OverLimit = cmd.TokenLimit > 0 && consumed > cmd.TokenLimit
	return res, nil
}

func (d *DB) tokenConsumedTx(ctx context.Context, tx *sql.Tx, bucketStartMS int64) (int64, error) {
	var consumed int64
	err := tx.QueryRowContext(ctx, `SELECT consumed_value FROM budget_counters
		WHERE kind='token' AND scope='global' AND scope_id='global' AND bucket_start_ms=?`, bucketStartMS).
		Scan(&consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return consumed, nil
}

// postChargeTokens applies the token-specific post-charge (storage.md §9.1
// exception): full amount appended even when the counter crosses the limit,
// with the unique operation key as the idempotency anchor. On crossing it
// also enqueues the per-bucket forge_alert (outbox.md §5.1).
func (d *DB) postChargeTokens(ctx context.Context, tx *sql.Tx, cmd BrainAttemptCmd, total int64, runID string) (charged, consumed int64, err error) {
	bucketStart := TokenBucketStartMS(cmd.StartedAtMS)
	bucketEnd := bucketStart + 24*60*60*1000
	opKey := BrainTokenOperationKey(cmd.CallID, cmd.ProviderAttempt)

	var existing int64
	scanErr := tx.QueryRowContext(ctx, `SELECT amount FROM budget_entries WHERE operation_key=?`, opKey).Scan(&existing)
	if scanErr == nil {
		return 0, 0, fmt.Errorf("storage: token operation key %q already exists", opKey)
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		return 0, 0, scanErr
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_counters
		(kind, scope, scope_id, bucket_start_ms, bucket_end_ms, limit_value, consumed_value, version, updated_at_ms)
		VALUES ('token', 'global', 'global', ?, ?, ?, 0, 1, ?)
		ON CONFLICT (kind, scope, scope_id, bucket_start_ms) DO NOTHING`,
		bucketStart, bucketEnd, cmd.TokenLimit, cmd.FinishedAtMS); err != nil {
		return 0, 0, fmt.Errorf("storage: ensure token counter: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE budget_counters
		SET consumed_value=consumed_value+?, version=version+1, updated_at_ms=?
		WHERE kind='token' AND scope='global' AND scope_id='global' AND bucket_start_ms=?`,
		total, cmd.FinishedAtMS, bucketStart)
	if err != nil {
		return 0, 0, fmt.Errorf("storage: post-charge token counter: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, 0, errors.New("storage: token counter update affected no rows")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_entries
		(id, kind, scope, scope_id, bucket_start_ms, amount, reason, run_id, operation_key, created_at_ms)
		VALUES (?, 'token', 'global', 'global', ?, ?, 'brain token usage', ?, ?, ?)`,
		newID(), bucketStart, total, nullable(runID), opKey, cmd.FinishedAtMS); err != nil {
		return 0, 0, fmt.Errorf("storage: insert token budget entry: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT consumed_value FROM budget_counters
		WHERE kind='token' AND scope='global' AND scope_id='global' AND bucket_start_ms=?`, bucketStart).
		Scan(&consumed); err != nil {
		return 0, 0, err
	}

	if cmd.TokenLimit > 0 && consumed > cmd.TokenLimit {
		// Overage alert: stable key and stable payload, at most one operation
		// per UTC day bucket (outbox.md §5.1). insertOperation dedupes by key.
		payload, _ := json.Marshal(map[string]any{
			"purpose":         "token_budget_exceeded",
			"bucket_start_ms": bucketStart,
		})
		op := Operation{
			Key:     AlertOperationKey("token_budget_exceeded", fmt.Sprintf("global:%d", bucketStart), 1),
			Kind:    OperationForgeAlert,
			Payload: payload,
		}
		if err := insertOperation(ctx, tx, op, "", "", cmd.FinishedAtMS); err != nil {
			return 0, 0, err
		}
	}
	return total, consumed, nil
}

// FinalizeBrainCallCmd is the one-time running → valid|fallback transition.
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
type BrainCall struct {
	ID                  string
	Scope               string
	SubjectKey          string
	ProjectID           string
	RunID               string
	AttemptNo           *int
	Touchpoint          string
	CallSeq             int64
	PromptVersion       string
	OutputSchemaVersion int
	InputJSON           []byte
	InputDigest         string
	Status              string
	SelectedAttemptNo   *int
	FallbackReason      string
	ValidatedOutputJSON []byte
	StartedAtMS         int64
	FinishedAtMS        *int64
}

// BrainAttempt is the read model of a brain_attempts row.
type BrainAttempt struct {
	ID                 string
	LogicalCallID      string
	ProviderAttempt    int
	Outcome            string
	ProviderErrorCode  string
	RequestDigest      string
	RawOutputText      *string
	RawOutputDigest    *string
	RawOutputBytes     *int64
	RawOutputTruncated bool
	StderrSummary      *string
	StderrTruncated    bool
	ExitCode           *int
	InputTokens        *int64
	OutputTokens       *int64
	StartedAtMS        int64
	FinishedAtMS       int64
}

func scanBrainCall(row interface{ Scan(...any) error }) (BrainCall, error) {
	var c BrainCall
	var projectID, runID, fallback, validated sql.NullString
	var input string
	var attemptNo, selected sql.NullInt64
	var finished sql.NullInt64
	err := row.Scan(&c.ID, &c.Scope, &c.SubjectKey, &projectID, &runID, &attemptNo, &c.Touchpoint,
		&c.CallSeq, &c.PromptVersion, &c.OutputSchemaVersion, &input, &c.InputDigest, &c.Status,
		&selected, &fallback, &validated, &c.StartedAtMS, &finished)
	if err != nil {
		return BrainCall{}, err
	}
	c.ProjectID, c.RunID, c.FallbackReason = projectID.String, runID.String, fallback.String
	c.InputJSON = []byte(input)
	if validated.Valid {
		c.ValidatedOutputJSON = []byte(validated.String)
	}
	if attemptNo.Valid {
		v := int(attemptNo.Int64)
		c.AttemptNo = &v
	}
	if selected.Valid {
		v := int(selected.Int64)
		c.SelectedAttemptNo = &v
	}
	if finished.Valid {
		c.FinishedAtMS = &finished.Int64
	}
	return c, nil
}

const brainCallColumns = `id, scope, subject_key, COALESCE(project_id,''), COALESCE(run_id,''), attempt_no,
	touchpoint, call_seq, prompt_version, output_schema_version, input_json, input_digest, status,
	selected_attempt_no, fallback_reason, validated_output_json, started_at_ms, finished_at_ms`

// BrainCallTrace loads one call with its attempts ordered by
// provider_attempt (storage.md §10.8 ordering).
func (d *DB) BrainCallTrace(ctx context.Context, callID string) (BrainCall, []BrainAttempt, error) {
	call, err := scanBrainCall(d.db.QueryRowContext(ctx,
		`SELECT `+brainCallColumns+` FROM brain_calls WHERE id=?`, callID))
	if err != nil {
		return BrainCall{}, nil, fmt.Errorf("storage: load brain call: %w", err)
	}
	attempts, err := d.BrainCallAttempts(ctx, callID)
	if err != nil {
		return BrainCall{}, nil, err
	}
	return call, attempts, nil
}

// BrainCallAttempts lists the attempts of a call ordered by provider_attempt.
func (d *DB) BrainCallAttempts(ctx context.Context, callID string) ([]BrainAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id, logical_call_id, provider_attempt, outcome,
		COALESCE(provider_error_code,''), request_digest, raw_output_text, raw_output_digest, raw_output_bytes,
		raw_output_truncated, stderr_summary, stderr_truncated, exit_code, input_tokens, output_tokens,
		started_at_ms, finished_at_ms
		FROM brain_attempts WHERE logical_call_id=? ORDER BY provider_attempt`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BrainAttempt
	for rows.Next() {
		var a BrainAttempt
		var rawText, rawDigest, stderr sql.NullString
		var rawBytes, exitCode, inTok, outTok sql.NullInt64
		var rawTrunc, stderrTrunc int
		if err := rows.Scan(&a.ID, &a.LogicalCallID, &a.ProviderAttempt, &a.Outcome, &a.ProviderErrorCode,
			&a.RequestDigest, &rawText, &rawDigest, &rawBytes, &rawTrunc, &stderr, &stderrTrunc,
			&exitCode, &inTok, &outTok, &a.StartedAtMS, &a.FinishedAtMS); err != nil {
			return nil, err
		}
		if rawText.Valid {
			a.RawOutputText = &rawText.String
		}
		if rawDigest.Valid {
			a.RawOutputDigest = &rawDigest.String
		}
		if rawBytes.Valid {
			a.RawOutputBytes = &rawBytes.Int64
		}
		if stderr.Valid {
			a.StderrSummary = &stderr.String
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			a.ExitCode = &v
		}
		if inTok.Valid {
			a.InputTokens = &inTok.Int64
		}
		if outTok.Valid {
			a.OutputTokens = &outTok.Int64
		}
		a.RawOutputTruncated, a.StderrTruncated = rawTrunc != 0, stderrTrunc != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunningBrainCalls lists leftover running calls in reserve order, used by
// crash recovery convergence (brain.md §5).
func (d *DB) RunningBrainCalls(ctx context.Context) ([]BrainCall, error) {
	return d.runningBrainCalls(ctx, `FROM brain_calls WHERE status='running'`)
}

// RunningT7AggregateCalls returns only scheduler-owned T7 calls. Explicit
// replay calls with the same aggregate subject are outside the restart cursor.
func (d *DB) RunningT7AggregateCalls(ctx context.Context) ([]BrainCall, error) {
	return d.runningBrainCalls(ctx, `FROM brain_calls JOIN t7_aggregate_call_bindings x ON x.logical_call_id=brain_calls.id WHERE brain_calls.status='running'`)
}

func (d *DB) runningBrainCalls(ctx context.Context, from string) ([]BrainCall, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+brainCallColumns+` `+from+` ORDER BY started_at_ms, brain_calls.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BrainCall
	for rows.Next() {
		c, err := scanBrainCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TokenConsumed returns the UTC day-bucket token counter for the gate check
// (brain.md §6: consumed >= limit blocks a new physical attempt).
func (d *DB) TokenConsumed(ctx context.Context, bucketStartMS int64) (int64, error) {
	var consumed int64
	err := d.db.QueryRowContext(ctx, `SELECT consumed_value FROM budget_counters
		WHERE kind='token' AND scope='global' AND scope_id='global' AND bucket_start_ms=?`, bucketStartMS).
		Scan(&consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return consumed, nil
}

// ExportBrainCallsJSONL exports Brain replay schema v2, including every real
// Gate-input association. ExportReplayJSONL additionally interleaves Gate
// evaluation records in stable chronological order.
func (d *DB) ExportBrainCallsJSONL(ctx context.Context, w io.Writer) error {
	return d.ExportBrainCallsJSONLV2(ctx, w)
}

func rawOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// HumanDecisionAction is the closed set of audited human actions.
type HumanDecisionAction string

const (
	DecisionApprove     HumanDecisionAction = "approve"
	DecisionReject      HumanDecisionAction = "reject"
	DecisionRetry       HumanDecisionAction = "retry"
	DecisionHold        HumanDecisionAction = "hold"
	DecisionAsk         HumanDecisionAction = "ask"
	DecisionManualMerge HumanDecisionAction = "manual_merge"
	DecisionManualClose HumanDecisionAction = "manual_close"
)

// RecordHumanDecisionCmd intentionally has no calibration ID: it is resolved
// exclusively through the immutable interrupt or Forge-fact binding.
type RecordHumanDecisionCmd struct {
	Action                                        HumanDecisionAction
	CommandEventID, InterruptID, ForgeFactEventID string
	SemanticMaterial                              string
	NowMS                                         int64
	Certification                                 config.Certification
}

type HumanDecisionResult struct{ LedgerEntryID, CalibrationID, CertificationVersion string }

// AppendExternalMergeFact records the authoritative Forge merge observation.
// Its fact must not depend on a Gate or Ledger binding: pre-Gate, missing, and
// ambiguous bindings remain auditable facts but cannot settle a calibration.
func (d *DB) AppendExternalMergeFact(ctx context.Context, cmd EventCmd, headSHA string) (string, error) {
	if cmd.Type != "forge_change_merged" || cmd.RunID == "" || cmd.ProjectID == "" || headSHA == "" || !validSource(cmd.Source) || !json.Valid(cmd.PayloadJSON) || cmd.OccurredAtMS <= 0 || cmd.RecordedAtMS < cmd.OccurredAtMS {
		return "", errors.New("storage: invalid external merge fact")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	if cmd.IdempotencyKey != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE idempotency_key=?`, cmd.IdempotencyKey).Scan(&id)
		if err == nil {
			return id, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	id = newID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO events (id,run_id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,? ,?,1,?,?,?,?)`, id, cmd.RunID, cmd.ProjectID, cmd.Type, cmd.Source, string(cmd.PayloadJSON), nullable(cmd.IdempotencyKey), cmd.OccurredAtMS, cmd.RecordedAtMS); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// BindExternalMergeFact attaches a previously observed fact to the exact Gate
// identity that was frozen with a waiting-human Interrupt. Callers must not
// infer this identity from mutable Run or Forge state.
func (d *DB) BindExternalMergeFact(ctx context.Context, forgeFactEventID, gateEvaluationID, calibrationID string, nowMS int64) error {
	if forgeFactEventID == "" || gateEvaluationID == "" || calibrationID == "" || nowMS <= 0 {
		return errors.New("storage: invalid external merge binding")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var factRunID, calibrationRunID string
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM events WHERE id=? AND type='forge_change_merged'`, forgeFactEventID).Scan(&factRunID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT run_id, predicted_decision IN ('allow','block','inconclusive') FROM calibration_entries WHERE id=? AND gate_evaluation_id=?`, calibrationID, gateEvaluationID).Scan(&calibrationRunID, &valid); err != nil {
		return err
	}
	if !valid || factRunID != calibrationRunID {
		return errors.New("storage: external merge binding is not an exact Gate calibration for this run")
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT calibration_id FROM external_decision_bindings WHERE forge_fact_event_id=?`, forgeFactEventID).Scan(&existing)
	if err == nil {
		if existing != calibrationID {
			return errors.New("storage: external decision binding conflict")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_decision_bindings (forge_fact_event_id,calibration_id,created_at_ms) VALUES (?,?,?)`, forgeFactEventID, calibrationID, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// WaitingHumanGateBinding returns the one Gate identity that was atomically
// frozen with the currently displayed HITL Interrupt. It is causal state, not
// a heuristic lookup over Gate history.
func (d *DB) WaitingHumanGateBinding(ctx context.Context, runID string) (gateEvaluationID, calibrationID string, err error) {
	if runID == "" {
		return "", "", errors.New("storage: waiting-human gate binding requires run")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT c.gate_evaluation_id,c.id
		FROM runs r
		JOIN interrupts i ON i.run_id=r.id
		JOIN calibration_entries c ON c.id=i.calibration_id
		WHERE r.id=? AND r.status='waiting_human' AND i.status='open'
		ORDER BY i.created_at_ms,i.id`, runID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var evaluationID, candidateCalibrationID string
		if err := rows.Scan(&evaluationID, &candidateCalibrationID); err != nil {
			return "", "", err
		}
		if calibrationID != "" {
			return "", "", errors.New("storage: ambiguous waiting-human gate binding")
		}
		gateEvaluationID, calibrationID = evaluationID, candidateCalibrationID
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if gateEvaluationID == "" || calibrationID == "" {
		return "", "", errors.New("storage: waiting-human run has no Gate binding")
	}
	return gateEvaluationID, calibrationID, nil
}

func (d *DB) BindExternalDecision(ctx context.Context, forgeFactEventID, calibrationID string, nowMS int64) error {
	if forgeFactEventID == "" || calibrationID == "" || nowMS <= 0 {
		return errors.New("storage: invalid external decision binding")
	}
	var existing string
	err := d.db.QueryRowContext(ctx, `SELECT calibration_id FROM external_decision_bindings WHERE forge_fact_event_id=?`, forgeFactEventID).Scan(&existing)
	if err == nil {
		if existing == calibrationID {
			return nil
		}
		return errors.New("storage: external decision binding conflict")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO external_decision_bindings (forge_fact_event_id,calibration_id,created_at_ms) VALUES (?,?,?)`, forgeFactEventID, calibrationID, nowMS)
	return err
}

// RecordHumanDecision is the only Ledger settlement port for commands and
// externally observed manual merge/close facts. It never guesses a calibration.
func (d *DB) RecordHumanDecision(ctx context.Context, cmd RecordHumanDecisionCmd) (HumanDecisionResult, error) {
	if err := validateHumanDecision(cmd); err != nil {
		return HumanDecisionResult{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	defer tx.Rollback()
	result, err := recordHumanDecisionTx(ctx, tx, cmd)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return HumanDecisionResult{}, err
	}
	return result, nil
}

// validateHumanDecision enforces the closed input contract shared by the
// public port and the in-transaction command path.
func validateHumanDecision(cmd RecordHumanDecisionCmd) error {
	if cmd.NowMS <= 0 || !validHumanAction(cmd.Action) {
		return errors.New("storage: invalid human decision")
	}
	isExternal := cmd.Action == DecisionManualMerge || cmd.Action == DecisionManualClose
	if isExternal != (cmd.ForgeFactEventID != "") || (!isExternal && (cmd.CommandEventID == "" || cmd.InterruptID == "")) {
		return errors.New("storage: invalid human decision identity")
	}
	if cmd.SemanticMaterial != "" && cmd.Action != DecisionReject && cmd.Action != DecisionAsk {
		return errors.New("storage: semantic material is only valid for reject or ask")
	}
	return nil
}

// recordHumanDecisionTx is the single in-transaction Ledger settlement core.
// ApplyCommandEvent calls it inside its own transaction so command, Run
// transition, outbox and Ledger remain all-or-nothing; there is no second
// Ledger path. The caller owns the transaction (begin/commit/rollback).
func recordHumanDecisionTx(ctx context.Context, tx *sql.Tx, cmd RecordHumanDecisionCmd) (HumanDecisionResult, error) {
	isExternal := cmd.Action == DecisionManualMerge || cmd.Action == DecisionManualClose
	idempotency := cmd.CommandEventID
	if isExternal {
		idempotency = cmd.ForgeFactEventID
	}
	var existing HumanDecisionResult
	err := tx.QueryRowContext(ctx, `SELECT r.ledger_entry_id,COALESCE(r.calibration_id,'') FROM human_decision_receipts r WHERE r.idempotency_id=?`, idempotency).Scan(&existing.LedgerEntryID, &existing.CalibrationID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return HumanDecisionResult{}, err
	}

	var calibrationID, shadow, runID string
	if isExternal {
		err = tx.QueryRowContext(ctx, `SELECT b.calibration_id,c.predicted_decision,c.run_id FROM external_decision_bindings b JOIN calibration_entries c ON c.id=b.calibration_id WHERE b.forge_fact_event_id=?`, cmd.ForgeFactEventID).Scan(&calibrationID, &shadow, &runID)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT i.calibration_id,c.predicted_decision,c.run_id FROM interrupts i JOIN calibration_entries c ON c.id=i.calibration_id WHERE i.id=?`, cmd.InterruptID).Scan(&calibrationID, &shadow, &runID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		// An unbound external fact is still audit evidence. Its event supplies
		// the Run identity, but cannot settle (or fabricate) a calibration.
		if !isExternal {
			// A command interrupt without a Gate calibration (for example a
			// pre-start design_approval or a startup_stall) still records its
			// human decision: it settles no calibration, but the Ledger entry,
			// semantic material and receipt are written in this transaction.
			var nullableCal sql.NullString
			if calErr := tx.QueryRowContext(ctx, `SELECT COALESCE(calibration_id,''),run_id FROM interrupts WHERE id=?`, cmd.InterruptID).Scan(&nullableCal, &runID); calErr != nil {
				return HumanDecisionResult{}, errors.New("storage: interrupt has no calibration binding")
			}
			calibrationID, shadow = "", "inconclusive"
		} else {
			var nullableRun sql.NullString
			if eventErr := tx.QueryRowContext(ctx, `SELECT run_id FROM events WHERE id=?`, cmd.ForgeFactEventID).Scan(&nullableRun); eventErr != nil || !nullableRun.Valid {
				return HumanDecisionResult{}, errors.New("storage: external fact has no run identity")
			}
			calibrationID, shadow, runID = "", "inconclusive", nullableRun.String
		}
	} else if err != nil {
		return HumanDecisionResult{}, err
	}
	decision := decisionFor(cmd.Action)
	if calibrationID == "" || shadow == "inconclusive" {
		decision = ""
	}
	if decision != "" {
		var prior sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT human_decision FROM calibration_entries WHERE id=?`, calibrationID).Scan(&prior); err != nil {
			return HumanDecisionResult{}, err
		}
		if prior.Valid {
			return HumanDecisionResult{}, errors.New("storage: calibration already settled")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE calibration_entries SET human_decision=?,decision_source=?,decided_at_ms=?,gate_bypassed=? WHERE id=? AND human_decision IS NULL`, decision, decisionSource(cmd.Action), cmd.NowMS, gateBoolInt(cmd.Action == DecisionManualMerge), calibrationID); err != nil {
			return HumanDecisionResult{}, err
		}
	}
	features, err := canonicalHumanDecision(cmd, calibrationID, decision)
	if err != nil {
		return HumanDecisionResult{}, err
	}
	entryID := newID()
	digest := sha256Hex(features)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (id,run_id,interrupt_id,entry_kind,features_schema_version,features_json,features_digest,natural_language,created_at_ms) VALUES (?,?,?,?,1,?,?,?,?)`, entryID, nullable(runID), nullable(cmd.InterruptID), "human_decision", string(features), digest, nullable(cmd.SemanticMaterial), cmd.NowMS); err != nil {
		return HumanDecisionResult{}, err
	}
	if cmd.SemanticMaterial != "" {
		if err := appendSemanticMaterial(ctx, tx, cmd, runID); err != nil {
			return HumanDecisionResult{}, err
		}
	}
	result := HumanDecisionResult{LedgerEntryID: entryID, CalibrationID: calibrationID}
	if decision != "" {
		// The decision at cmd.NowMS belongs to the projection written by this
		// transaction; use the next millisecond as the half-open window's as_of.
		result.CertificationVersion, err = recomputeCertification(ctx, tx, runID, cmd.Certification, cmd.NowMS+1)
		if err != nil {
			return HumanDecisionResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO human_decision_receipts (idempotency_id,ledger_entry_id,calibration_id) VALUES (?,?,?)`, idempotency, entryID, nullable(calibrationID)); err != nil {
		return HumanDecisionResult{}, err
	}
	return result, nil
}

func validHumanAction(a HumanDecisionAction) bool {
	switch a {
	case DecisionApprove, DecisionReject, DecisionRetry, DecisionHold, DecisionAsk, DecisionManualMerge, DecisionManualClose:
		return true
	}
	return false
}
func decisionFor(a HumanDecisionAction) string {
	switch a {
	case DecisionApprove, DecisionManualMerge:
		return "allow"
	case DecisionReject, DecisionManualClose:
		return "block"
	}
	return ""
}
func decisionSource(a HumanDecisionAction) string {
	if a == DecisionManualMerge {
		return "manual_merge"
	}
	if a == DecisionManualClose {
		return "manual_close"
	}
	return "command"
}

func appendSemanticMaterial(ctx context.Context, tx *sql.Tx, cmd RecordHumanDecisionCmd, runID string) error {
	b, err := canonicalJSON(map[string]any{"schema_version": 1, "command_event_id": cmd.CommandEventID, "interrupt_id": cmd.InterruptID, "material_kind": map[bool]string{true: "reject_reason", false: "ask_text"}[cmd.Action == DecisionReject], "text": cmd.SemanticMaterial})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger_entries (id,run_id,interrupt_id,entry_kind,features_schema_version,features_json,features_digest,natural_language,created_at_ms) VALUES (?,?,?,?,1,?,?,?,?)`, newID(), nullable(runID), nullable(cmd.InterruptID), "semantic_material", string(b), sha256Hex(b), cmd.SemanticMaterial, cmd.NowMS)
	return err
}

func canonicalHumanDecision(cmd RecordHumanDecisionCmd, calibrationID, decision string) ([]byte, error) {
	return canonicalJSON(map[string]any{"schema_version": 1, "action": cmd.Action, "calibration_decision": nullable(decision), "calibration_id": nullable(calibrationID), "interrupt_id": nullable(cmd.InterruptID), "command_event_id": nullable(cmd.CommandEventID), "decision_source": decisionSource(cmd.Action), "gate_bypassed": cmd.Action == DecisionManualMerge, "response_interval_ms": nil})
}
func canonicalJSON(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var x any
	if e = json.Unmarshal(b, &x); e != nil {
		return nil, e
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if e = encoder.Encode(x); e != nil {
		return nil, e
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func recomputeCertification(ctx context.Context, tx *sql.Tx, runID string, rules config.Certification, asOf int64) (string, error) {
	if rules.Window <= 0 {
		// Certification tracking is optional: when the Run's config defines no
		// certification window, the human decision still settles the calibration
		// and Ledger entry, but no certification projection is recomputed.
		return "", nil
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM runs WHERE id=?`, runID).Scan(&kind); err != nil {
		return "", err
	}
	start := asOf - rules.Window.Milliseconds()
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.predicted_decision,c.human_decision,c.decided_at_ms FROM calibration_entries c JOIN runs r ON r.id=c.run_id WHERE r.kind=? AND c.human_decision IS NOT NULL AND c.predicted_decision IN ('allow','block') AND c.decided_at_ms>=? AND c.decided_at_ms<? ORDER BY c.id`, kind, start, asOf)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type sample struct {
		ID, P, H string
		At       int64
	}
	var ss []sample
	total, negative, leaks, falseBlocks := 0, 0, 0, 0
	for rows.Next() {
		var s sample
		if err := rows.Scan(&s.ID, &s.P, &s.H, &s.At); err != nil {
			return "", err
		}
		ss = append(ss, s)
		total++
		if s.H == "block" {
			negative++
		}
		if s.P == "allow" && s.H == "block" {
			leaks++
		}
		if s.P == "block" && s.H == "allow" {
			falseBlocks++
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rv, err := config.CertificationRulesVersion(rules)
	if err != nil {
		return "", err
	}
	evidence, err := canonicalJSON(map[string]any{"samples": ss, "total_samples": total, "negative_samples": negative, "leak_count": leaks, "false_block_count": falseBlocks, "window_start_ms": start, "window_end_ms": asOf, "certification_rules_version": rv})
	if err != nil {
		return "", err
	}
	ed := sha256Hex(evidence)
	version := sha256Hex([]byte(kind + "\x00" + rv + "\x00" + ed))
	certified := negative > 0 && total >= rules.TotalSamplesMin && negative >= rules.NegativeSamplesMin && float64(leaks)/float64(negative) <= rules.LeakRateMax && float64(falseBlocks)/float64(total) <= rules.FalseBlockRateMax
	_, err = tx.ExecContext(ctx, `INSERT INTO certifications (task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(task_kind,certification_version) DO NOTHING`, kind, version, total, negative, leaks, falseBlocks, gateBoolInt(certified), ed, asOf, rv, start, asOf)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO certification_current (task_kind,certification_version,version,updated_at_ms) VALUES (?,?,1,?) ON CONFLICT(task_kind) DO UPDATE SET certification_version=excluded.certification_version,version=certification_current.version+1,updated_at_ms=excluded.updated_at_ms`, kind, version, asOf)
	return version, err
}

// CertificationProjection returns only the permitted category-level view.
type CertificationProjection struct {
	TaskKind, CertificationVersion                            string
	Certified                                                 bool
	TotalSamples, NegativeSamples, LeakCount, FalseBlockCount int
	WindowStartMS, WindowEndMS                                int64
}

func (d *DB) Certification(ctx context.Context, kind string) (CertificationProjection, error) {
	var p CertificationProjection
	var certified int
	err := d.db.QueryRowContext(ctx, `SELECT c.task_kind,c.certification_version,c.certified,c.total_samples,c.negative_samples,c.leak_count,c.false_block_count,c.window_start_ms,c.window_end_ms FROM certification_current x JOIN certifications c ON c.task_kind=x.task_kind AND c.certification_version=x.certification_version WHERE x.task_kind=?`, kind).Scan(&p.TaskKind, &p.CertificationVersion, &certified, &p.TotalSamples, &p.NegativeSamples, &p.LeakCount, &p.FalseBlockCount, &p.WindowStartMS, &p.WindowEndMS)
	p.Certified = certified != 0
	return p, err
}

// ProposalDraft is the inert persistence shape for a terminal T7 proposal.
// There is intentionally no approval, policy, context, Gate, or action field.
type ProposalDraft struct {
	ID                  string
	LogicalCallID       string
	PromptVersion       string
	OutputSchemaVersion int
	AggregateKey        string
	ProposalKind        string
	TargetScope         string
	Title               string
	Body                string
	EvidenceEntryIDs    []string
	Status              string
	CreatedAtMS         int64
}

type SaveProposalDraftCmd struct {
	LogicalCallID       string
	PromptVersion       string
	OutputSchemaVersion int
	AggregateKey        string
	ProposalKind        string
	TargetScope         string
	Title               string
	Body                string
	EvidenceEntryIDs    []string
	CreatedAtMS         int64
}

// SaveProposalDraft is the only proposal write port. It accepts only a
// terminal valid T7 call and insert-or-returns the identical draft. It does
// not create an outbox operation or touch any Gate, Interrupt, policy, or
// context projection.
func (d *DB) SaveProposalDraft(ctx context.Context, cmd SaveProposalDraftCmd) (ProposalDraft, error) {
	if cmd.LogicalCallID == "" || cmd.PromptVersion == "" || cmd.AggregateKey == "" || cmd.Title == "" || cmd.Body == "" || cmd.CreatedAtMS < 0 {
		return ProposalDraft{}, errors.New("storage: incomplete proposal draft")
	}
	if cmd.OutputSchemaVersion < 1 || (cmd.ProposalKind != "policy" && cmd.ProposalKind != "context") || (cmd.TargetScope != "project" && cmd.TargetScope != "global") || len(cmd.EvidenceEntryIDs) == 0 || len(cmd.Title) > 160 || len(cmd.Body) > 8192 || !utf8.ValidString(cmd.Title) || !utf8.ValidString(cmd.Body) || strings.ContainsAny(cmd.Title, "\r\n") || strings.ContainsAny(cmd.Body, "\r\x00") || strings.IndexFunc(cmd.Title, unicode.IsControl) >= 0 || strings.IndexFunc(cmd.Body, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return ProposalDraft{}, errors.New("storage: invalid proposal draft contract")
	}
	ids := append([]string(nil), cmd.EvidenceEntryIDs...)
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] >= id) {
			return ProposalDraft{}, errors.New("storage: proposal evidence IDs must be sorted and unique")
		}
	}
	evidence, err := json.Marshal(ids)
	if err != nil {
		return ProposalDraft{}, fmt.Errorf("storage: encode proposal evidence: %w", err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalDraft{}, err
	}
	defer tx.Rollback()
	var touchpoint, status, callPrompt string
	var callSchema int
	if err := tx.QueryRowContext(ctx, `SELECT touchpoint,status,prompt_version,output_schema_version FROM brain_calls WHERE id=?`, cmd.LogicalCallID).Scan(&touchpoint, &status, &callPrompt, &callSchema); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProposalDraft{}, errors.New("storage: proposal call not found")
		}
		return ProposalDraft{}, err
	}
	if touchpoint != "T7" || status != BrainCallValid || callPrompt != cmd.PromptVersion || callSchema != cmd.OutputSchemaVersion {
		return ProposalDraft{}, errors.New("storage: proposal requires terminal valid T7 call")
	}
	requestedEvidence := string(evidence)
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO proposal_drafts
		(id,logical_call_id,prompt_version,output_schema_version,aggregate_key,proposal_kind,target_scope,title,body,evidence_entry_ids,status,created_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(logical_call_id) DO NOTHING`, id, cmd.LogicalCallID, cmd.PromptVersion, cmd.OutputSchemaVersion, cmd.AggregateKey, cmd.ProposalKind, cmd.TargetScope, cmd.Title, cmd.Body, string(evidence), "pending_human_approval", cmd.CreatedAtMS)
	if err != nil {
		return ProposalDraft{}, fmt.Errorf("storage: insert proposal draft: %w", err)
	}
	var out ProposalDraft
	err = tx.QueryRowContext(ctx, `SELECT id,logical_call_id,prompt_version,output_schema_version,aggregate_key,proposal_kind,target_scope,title,body,evidence_entry_ids,status,created_at_ms FROM proposal_drafts WHERE logical_call_id=?`, cmd.LogicalCallID).Scan(&out.ID, &out.LogicalCallID, &out.PromptVersion, &out.OutputSchemaVersion, &out.AggregateKey, &out.ProposalKind, &out.TargetScope, &out.Title, &out.Body, &evidence, &out.Status, &out.CreatedAtMS)
	if err != nil {
		return ProposalDraft{}, err
	}
	if out.PromptVersion != cmd.PromptVersion || out.OutputSchemaVersion != cmd.OutputSchemaVersion || out.AggregateKey != cmd.AggregateKey || out.ProposalKind != cmd.ProposalKind || out.TargetScope != cmd.TargetScope || out.Title != cmd.Title || out.Body != cmd.Body || string(evidence) != requestedEvidence || out.Status != "pending_human_approval" {
		return ProposalDraft{}, errors.New("storage: proposal draft content conflicts with existing call")
	}
	if err := json.Unmarshal(evidence, &out.EvidenceEntryIDs); err != nil {
		return ProposalDraft{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProposalDraft{}, err
	}
	return out, nil
}

func (d *DB) ProposalDraft(ctx context.Context, logicalCallID string) (ProposalDraft, error) {
	var out ProposalDraft
	var evidence string
	err := d.db.QueryRowContext(ctx, `SELECT id,logical_call_id,prompt_version,output_schema_version,aggregate_key,proposal_kind,target_scope,title,body,evidence_entry_ids,status,created_at_ms FROM proposal_drafts WHERE logical_call_id=?`, logicalCallID).Scan(&out.ID, &out.LogicalCallID, &out.PromptVersion, &out.OutputSchemaVersion, &out.AggregateKey, &out.ProposalKind, &out.TargetScope, &out.Title, &out.Body, &evidence, &out.Status, &out.CreatedAtMS)
	if err != nil {
		return ProposalDraft{}, err
	}
	if err := json.Unmarshal([]byte(evidence), &out.EvidenceEntryIDs); err != nil {
		return ProposalDraft{}, err
	}
	return out, nil
}
