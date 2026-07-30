package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/miaoxiaoyong/sift/internal/decode"
)

// ProjectForgeRef returns the immutable project routing facts needed by an
// outbox worker; workers never reconstruct them from a Change payload.
func (d *DB) ProjectForgeRef(ctx context.Context, projectID string) (kind, host, projectKey string, err error) {
	if projectID == "" {
		return "", "", "", errors.New("storage: project id is required")
	}
	err = d.db.QueryRowContext(ctx, `SELECT forge_kind,forge_host,forge_project_key FROM projects WHERE id=?`, projectID).Scan(&kind, &host, &projectKey)
	return
}

// RecordCreatedChange persists the remote identity after either marker
// recovery or fresh creation. A different identity is a semantic conflict and
// can never silently replace a previously converged Change.
func (d *DB) RecordCreatedChange(ctx context.Context, runID, changeID string, nowMS int64) error {
	if runID == "" || changeID == "" || nowMS <= 0 {
		return errors.New("storage: invalid created change")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT change_id FROM runs WHERE id=?`, runID).Scan(&old); err != nil {
		return err
	}
	if old.Valid && old.String != changeID {
		return ErrOperationConflict
	}
	if !old.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET change_id=?,version=version+1,updated_at_ms=? WHERE id=? AND change_id IS NULL`, changeID, nowMS, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReadyChangeCandidate is the persisted evidence a Gate-side verifier needs
// before it may enqueue create_change. It is read-only: EvaluateSuccess remains
// the authority that validates the worktree and agent identity.
type ReadyChangeCandidate struct {
	RunID, ProjectID, WorktreePath, Branch, Base string
	AttemptNo, Generation                        int
	Agent                                        AgentIdentity
	ExitCode                                     int
	FinalHeadSHA, Digest                         string
	FinishedAtMS                                 int64
}

// ReadyChangeCandidates returns successful finished attempts that have not
// yet converged to a remote Change. Re-reading a candidate is intentional:
// create_change is keyed by the verified head and is durably idempotent.
func (d *DB) ReadyChangeCandidates(ctx context.Context, projectID string) ([]ReadyChangeCandidate, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT a.run_id,r.project_id,a.worktree_path,a.branch_name,a.base_ref,a.attempt_no,a.generation,
		a.agent_pid,a.agent_started_at_ms,a.agent_executable,a.result_exit_code,a.final_head_sha,a.result_digest,a.finished_at_ms
		FROM attempts a JOIN runs r ON r.id=a.run_id
		WHERE r.project_id=? AND r.change_id IS NULL AND r.status NOT IN ('done','failed')
		AND a.phase='finished' AND a.result_exit_code=0 AND a.result_signal IS NULL
		AND a.final_head_sha IS NOT NULL AND a.result_digest IS NOT NULL
		AND a.agent_pid IS NOT NULL AND a.agent_started_at_ms IS NOT NULL AND a.agent_executable IS NOT NULL
		ORDER BY a.finished_at_ms,a.run_id,a.attempt_no`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadyChangeCandidate
	for rows.Next() {
		var c ReadyChangeCandidate
		if err := rows.Scan(&c.RunID, &c.ProjectID, &c.WorktreePath, &c.Branch, &c.Base, &c.AttemptNo, &c.Generation,
			&c.Agent.PID, &c.Agent.StartedAtMS, &c.Agent.Executable, &c.ExitCode, &c.FinalHeadSHA, &c.Digest, &c.FinishedAtMS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReverseSyncCandidate is the forge identity needed to reconcile one active
// Run. It is deliberately a small read projection; state changes still enter
// through TransitionRun.
type ReverseSyncCandidate struct {
	RunID     string
	Version   int64
	ProjectID string
	IssueID   string
	ChangeID  string
}

// ReverseSyncCandidates returns non-terminal forge Runs for a project. The
// reconciler re-reads forge facts for these rows on every tick, so a closed or
// merged object cannot remain an active local Run after a restart.
func (d *DB) ReverseSyncCandidates(ctx context.Context, projectID string) ([]ReverseSyncCandidate, error) {
	if projectID == "" {
		return nil, errors.New("storage: reverse sync requires project")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id,version,project_id,issue_id,COALESCE(change_id,'')
		FROM runs WHERE project_id=? AND issue_id IS NOT NULL AND status IN ('queued','running','waiting_human') ORDER BY created_at_ms,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReverseSyncCandidate
	for rows.Next() {
		var c ReverseSyncCandidate
		if err := rows.Scan(&c.RunID, &c.Version, &c.ProjectID, &c.IssueID, &c.ChangeID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReverseSyncIssue applies the Issue fact back to Sift. Issue closure is a
// fact observation and therefore has no actor gate.
func (d *DB) ReverseSyncIssue(ctx context.Context, projectID, issueID string, closed bool, nowMS int64) error {
	if projectID == "" || issueID == "" || nowMS <= 0 {
		return errors.New("storage: invalid reverse sync issue")
	}
	if !closed {
		return nil
	}
	var id string
	var version int64
	err := d.db.QueryRowContext(ctx, `SELECT id,version FROM runs WHERE project_id=? AND issue_id=? AND status IN ('queued','running','waiting_human') ORDER BY created_at_ms DESC LIMIT 1`, projectID, issueID).Scan(&id, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = d.TransitionRun(ctx, id, version, DomainCommand{To: RunFailed, Source: SourceForge, FailureReason: "closed_upstream", OccurredAtMS: nowMS})
	return err
}

// ExportReplayJSONL exports the complete M4 replay set. It reads immutable Gate
// snapshots/evaluations and independent Brain call traces only; it never
// reconstructs input from current Forge, policy, certification, or Brain data.
func (d *DB) ExportReplayJSONL(ctx context.Context, w io.Writer) error {
	type row struct {
		at      int64
		typ, id string
		record  any
	}
	var records []row

	gateRows, err := d.db.QueryContext(ctx, `SELECT e.id, e.created_at_ms, e.gate_version, s.id, s.canonical_json, e.verdict_json
		FROM gate_evaluations e JOIN gate_input_snapshots s ON s.id=e.snapshot_id
		ORDER BY e.created_at_ms, e.id`)
	if err != nil {
		return err
	}
	for gateRows.Next() {
		var id, version, snapshotID, input, verdict string
		var at int64
		if err := gateRows.Scan(&id, &at, &version, &snapshotID, &input, &verdict); err != nil {
			gateRows.Close()
			return err
		}
		records = append(records, row{at: at, typ: "gate", id: id, record: map[string]any{
			"record_type": "gate", "schema_version": 1, "record_id": id, "recorded_at_ms": at,
			"snapshot_id": snapshotID, "input": json.RawMessage(input), "gate_version": version,
			"expected_verdict": json.RawMessage(verdict),
		}})
	}
	if err := gateRows.Err(); err != nil {
		gateRows.Close()
		return err
	}
	gateRows.Close()

	calls, err := d.replayBrainCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		record, err := d.brainReplayRecord(ctx, call)
		if err != nil {
			return err
		}
		records = append(records, row{at: call.StartedAtMS, typ: "brain_call", id: call.ID, record: record})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].at != records[j].at {
			return records[i].at < records[j].at
		}
		if records[i].typ != records[j].typ {
			return records[i].typ < records[j].typ
		}
		return records[i].id < records[j].id
	})
	for _, record := range records {
		line, err := decode.Canonical(record.record)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) replayBrainCalls(ctx context.Context) ([]BrainCall, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+brainCallColumns+` FROM brain_calls ORDER BY started_at_ms, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []BrainCall
	for rows.Next() {
		call, err := scanBrainCall(rows)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

func (d *DB) brainReplayRecord(ctx context.Context, call BrainCall) (map[string]any, error) {
	attempts, err := d.BrainCallAttempts(ctx, call.ID)
	if err != nil {
		return nil, err
	}
	linkRows, err := d.db.QueryContext(ctx, `SELECT gate_input_snapshot_id FROM brain_gate_input_links WHERE logical_call_id=? ORDER BY gate_input_snapshot_id`, call.ID)
	if err != nil {
		return nil, err
	}
	var snapshots []string
	for linkRows.Next() {
		var id string
		if err := linkRows.Scan(&id); err != nil {
			linkRows.Close()
			return nil, err
		}
		snapshots = append(snapshots, id)
	}
	if err := linkRows.Err(); err != nil {
		linkRows.Close()
		return nil, err
	}
	linkRows.Close()

	trace := make([]any, 0, len(attempts))
	for _, a := range attempts {
		trace = append(trace, map[string]any{
			"provider_attempt": a.ProviderAttempt, "outcome": a.Outcome,
			"provider_error_code": nullable(a.ProviderErrorCode), "raw_output": a.RawOutputText,
			"raw_output_digest": a.RawOutputDigest, "raw_output_bytes": a.RawOutputBytes,
			"raw_output_truncated": a.RawOutputTruncated, "input_tokens": a.InputTokens,
			"output_tokens": a.OutputTokens, "started_at_ms": a.StartedAtMS, "finished_at_ms": a.FinishedAtMS,
		})
	}
	return map[string]any{
		"record_type": "brain_call", "schema_version": 2, "record_id": call.ID, "recorded_at_ms": call.StartedAtMS,
		"scope": call.Scope, "subject_key": call.SubjectKey, "touchpoint": call.Touchpoint, "call_seq": call.CallSeq,
		"prompt_version": call.PromptVersion, "output_schema_version": call.OutputSchemaVersion, "status": call.Status,
		"selected_attempt_no": call.SelectedAttemptNo, "fallback_reason": nullable(call.FallbackReason),
		"input": json.RawMessage(call.InputJSON), "input_digest": call.InputDigest,
		"validated_output": rawOrNull(call.ValidatedOutputJSON), "attempts": trace,
		"gate_input_snapshot_ids": snapshots,
	}, nil
}

// ExportBrainCallsJSONLV2 is useful to consumers that only need Brain traces
// but still require their real Gate-input associations.
func (d *DB) ExportBrainCallsJSONLV2(ctx context.Context, w io.Writer) error {
	calls, err := d.replayBrainCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		record, err := d.brainReplayRecord(ctx, call)
		if err != nil {
			return err
		}
		line, err := decode.Canonical(record)
		if err != nil {
			return fmt.Errorf("canonical brain replay record: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}
