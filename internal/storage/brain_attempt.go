package storage

import (
	"context"
	"errors"
	"fmt"

	"database/sql"
	"encoding/json"
)

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
