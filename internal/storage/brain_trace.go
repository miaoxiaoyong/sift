package storage

import (
	"context"
	"errors"
	"fmt"
	"io"

	"database/sql"
	"encoding/json"
)

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
