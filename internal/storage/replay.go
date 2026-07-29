package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/miaoxiaoyong/sift/internal/decode"
)

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
