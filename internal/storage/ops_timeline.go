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
	// Keyset pagination cursor on (occurred_at_ms, seq). Both zero means the
	// first page; AfterSeq > 0 requires the matching AfterOccurredAtMS.
	AfterOccurredAtMS int64
	AfterSeq          int64
	Limit             int
}

// TimelineReport is the ops.timeline result.
type TimelineReport struct {
	Events           []Event `json:"events"`
	NextOccurredAtMS int64   `json:"next_occurred_at_ms"`
	NextSeq          int64   `json:"next_seq"`
	HasMore          bool    `json:"has_more"`
}

// RunTimeline returns a bounded, keyset-paginated slice of the append-only
// event stream (storage.md §7.1), globally ordered by occurred_at_ms
// descending with seq as the tie-breaker, so concatenating pages always
// yields the global newest-first order even when seq and occurred_at_ms are
// interleaved (replay/backfill). It never reconstructs events from memory.
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
	if q.AfterOccurredAtMS < 0 {
		q.AfterOccurredAtMS = 0
	}
	report := TimelineReport{Events: []Event{}}
	filters := []string{}
	filterArgs := []any{}
	if q.RunID != "" {
		filters = append(filters, "run_id=?")
		filterArgs = append(filterArgs, q.RunID)
	}
	if q.ProjectID != "" {
		filters = append(filters, "project_id=?")
		filterArgs = append(filterArgs, q.ProjectID)
	}
	if q.Type != "" {
		filters = append(filters, "type=?")
		filterArgs = append(filterArgs, q.Type)
	}
	conds := filters
	args := append([]any{}, filterArgs...)
	if q.AfterSeq > 0 {
		at := q.AfterOccurredAtMS
		if at == 0 {
			// Legacy cursor: an old ops.timeline caller sends only after_seq
			// (no after_occurred_at_ms). Resolve that seq's occurred_at_ms so
			// the (occurred_at_ms, seq) keyset pages correctly instead of
			// silently treating the missing time as 0, which would match no
			// events (every event has occurred_at_ms > 0) and return empty
			// or partial pages. A pruned/unresolvable seq keeps the keyset on
			// (0, seq): fail-closed empty page, never a wrong one.
			var seqAt int64
			err := d.db.QueryRowContext(ctx, `SELECT occurred_at_ms FROM events WHERE seq=?`, q.AfterSeq).Scan(&seqAt)
			if err == nil {
				at = seqAt
			} else if err != sql.ErrNoRows {
				return report, fmt.Errorf("storage: timeline: resolve after_seq: %w", err)
			}
		}
		conds = append(conds, "(occurred_at_ms<? OR (occurred_at_ms=? AND seq<?))")
		args = append(args, at, at, q.AfterSeq)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, q.Limit)
	rows, err := d.db.QueryContext(ctx, `SELECT seq, id, COALESCE(run_id,''), attempt_no, COALESCE(project_id,''),
		type, source, actor, payload_json, occurred_at_ms, recorded_at_ms
		FROM events`+where+` ORDER BY occurred_at_ms DESC, seq DESC LIMIT ?`, args...)
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
		last := report.Events[len(report.Events)-1]
		report.NextSeq = last.Seq
		report.NextOccurredAtMS = last.OccurredAtMS
		moreConds := append(append([]string{}, filters...), "(occurred_at_ms<? OR (occurred_at_ms=? AND seq<?))")
		moreArgs := append(append([]any{}, filterArgs...), last.OccurredAtMS, last.OccurredAtMS, last.Seq)
		var more int
		if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE `+strings.Join(moreConds, " AND "), moreArgs...).Scan(&more); err == nil {
			report.HasMore = more > 0
		}
	}
	return report, nil
}
