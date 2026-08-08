package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type Event struct {
	Seq          int64
	ID           string
	RunID        string
	AttemptNo    *int
	ProjectID    string
	Type         string
	Source       string
	Actor        string
	PayloadJSON  []byte
	OccurredAtMS int64
	RecordedAtMS int64
}

// RunEvents returns the events of one Run in seq order (storage.md §7.1
// append-only stream). It is the read port the M1 skeleton uses to locate the
// trigger-observed and agent-started events for the P50 computation.
func (d *DB) RunEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT seq, id, COALESCE(run_id,''), attempt_no, COALESCE(project_id,''),
		type, source, actor, payload_json, occurred_at_ms, recorded_at_ms
		FROM events WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("storage: run events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var actor sql.NullString
		var attemptNo sql.NullInt64
		if err := rows.Scan(&e.Seq, &e.ID, &e.RunID, &attemptNo, &e.ProjectID, &e.Type, &e.Source,
			&actor, &e.PayloadJSON, &e.OccurredAtMS, &e.RecordedAtMS); err != nil {
			return nil, err
		}
		if attemptNo.Valid {
			v := int(attemptNo.Int64)
			e.AttemptNo = &v
		}
		e.Actor = actor.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// FirstEventOfType returns the first event of one type for a Run, or false. The
// skeleton uses it to resolve the P50 anchors deterministically.
func (d *DB) FirstEventOfType(ctx context.Context, runID, eventType string) (Event, bool, error) {
	events, err := d.RunEvents(ctx, runID)
	if err != nil {
		return Event{}, false, err
	}
	for _, e := range events {
		if e.Type == eventType {
			return e, true, nil
		}
	}
	return Event{}, false, nil
}

// CountOperationsByKind reports how many outbox operations of one kind exist.
// The M1 skeleton test uses it to assert no create_change/merge_change
// operations were created (WBS M1 §1.6).
func (d *DB) CountOperationsByKind(ctx context.Context, kind OperationKind) (int, error) {
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_operations WHERE kind=?`, string(kind)).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count outbox by kind: %w", err)
	}
	return n, nil
}

// AppendEvent is the generic append-only event port (storage.md §7.1). The
// events table is append-only by trigger; this port lets the M1 skeleton record
// spine evidence that is not a side effect of another write port — the fake
// agent's completion evidence and the injected forge merge fact (WBS M1 §1.6).
// The stateful write ports (TransitionRun, CreateForgeRun, SetInitialTaskSpec)
// emit their own events as side effects; this port carries only standalone
// evidence events and never mutates a Run.
type EventCmd struct {
	RunID          string // optional
	AttemptNo      *int   // optional
	ProjectID      string // optional
	Type           string
	Source         EventSource
	Actor          string // optional
	PayloadJSON    []byte
	IdempotencyKey string // optional; when set, duplicate inserts are no-ops
	OccurredAtMS   int64
	RecordedAtMS   int64
}

// AppendEvent appends one evidence event. It validates the source enum and
// requires ordered timestamps; the append-only trigger (storage.md §13) makes
// UPDATE/DELETE impossible. An IdempotencyKey collision is a no-op.
func (d *DB) AppendEvent(ctx context.Context, cmd EventCmd) (string, error) {
	if !validSource(cmd.Source) {
		return "", fmt.Errorf("storage: append event source %q invalid", cmd.Source)
	}
	if cmd.Type == "" || cmd.OccurredAtMS <= 0 || cmd.RecordedAtMS < cmd.OccurredAtMS {
		return "", errors.New("storage: append event requires type and ordered timestamps")
	}
	if !json.Valid(cmd.PayloadJSON) {
		return "", errors.New("storage: append event payload must be valid JSON")
	}
	if cmd.IdempotencyKey != "" {
		var existing string
		err := d.db.QueryRowContext(ctx, `SELECT id FROM events WHERE idempotency_key=?`, cmd.IdempotencyKey).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if err != sql.ErrNoRows {
			return "", fmt.Errorf("storage: read idempotent event: %w", err)
		}
	}
	id := newID()
	_, err := d.db.ExecContext(ctx, `INSERT INTO events
		(id, run_id, attempt_no, project_id, type, source, actor, payload_schema_version, payload_json, idempotency_key, occurred_at_ms, recorded_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		id, nullable(cmd.RunID), cmd.AttemptNo, nullable(cmd.ProjectID), cmd.Type, cmd.Source,
		nullable(cmd.Actor), string(cmd.PayloadJSON), nullable(cmd.IdempotencyKey), cmd.OccurredAtMS, cmd.RecordedAtMS)
	if err != nil {
		return "", fmt.Errorf("storage: append event: %w", err)
	}
	if cmd.IdempotencyKey != "" {
		if err := d.db.QueryRowContext(ctx, `SELECT id FROM events WHERE idempotency_key=?`, cmd.IdempotencyKey).Scan(&id); err != nil {
			return "", fmt.Errorf("storage: read inserted event: %w", err)
		}
	}
	return id, nil
}
