package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type TimelineQuery struct {
	RunID     string
	ProjectID string
	Type      string // event type filter; empty means any
	AfterSeq  int64  // keyset pagination cursor by seq
	Limit     int
}

// TimelineReport is the ops.timeline result.
type TimelineReport struct {
	Events  []Event `json:"events"`
	NextSeq int64   `json:"next_seq"`
	HasMore bool    `json:"has_more"`
}

// RunTimeline returns a bounded, keyset-paginated slice of the append-only
// event stream (storage.md §7.1). It never reconstructs events from memory.
func (d *DB) RunTimeline(ctx context.Context, q TimelineQuery) (TimelineReport, error) {
	if q.Limit < 1 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	if q.AfterSeq < 0 {
		q.AfterSeq = 0
	}
	report := TimelineReport{Events: []Event{}}
	conds := []string{"seq>?"}
	args := []any{q.AfterSeq}
	if q.RunID != "" {
		conds = append(conds, "run_id=?")
		args = append(args, q.RunID)
	}
	if q.ProjectID != "" {
		conds = append(conds, "project_id=?")
		args = append(args, q.ProjectID)
	}
	if q.Type != "" {
		conds = append(conds, "type=?")
		args = append(args, q.Type)
	}
	args = append(args, q.Limit)
	rows, err := d.db.QueryContext(ctx, `SELECT seq, id, COALESCE(run_id,''), attempt_no, COALESCE(project_id,''),
		type, source, actor, payload_json, occurred_at_ms, recorded_at_ms
		FROM events WHERE `+strings.Join(conds, " AND ")+` ORDER BY seq ASC LIMIT ?`, args...)
	if err != nil {
		return report, fmt.Errorf("storage: timeline: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var actor sql.NullString
		var attemptNo sql.NullInt64
		if err := rows.Scan(&e.Seq, &e.ID, &e.RunID, &attemptNo, &e.ProjectID, &e.Type, &e.Source,
			&actor, &e.PayloadJSON, &e.OccurredAtMS, &e.RecordedAtMS); err != nil {
			return report, err
		}
		if attemptNo.Valid {
			v := int(attemptNo.Int64)
			e.AttemptNo = &v
		}
		e.Actor = actor.String
		report.Events = append(report.Events, e)
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	if len(report.Events) > 0 {
		last := report.Events[len(report.Events)-1].Seq
		report.NextSeq = last
		var more int
		if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE seq>?`, last).Scan(&more); err == nil {
			report.HasMore = more > 0
		}
	}
	return report, nil
}
