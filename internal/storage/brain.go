package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// ReservedBrainCall is the outcome of a successful reservation.
type ReservedBrainCall struct {
	ID      string
	CallSeq int64
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
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+brainCallColumns+` FROM brain_calls WHERE status='running' ORDER BY started_at_ms, id`)
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
