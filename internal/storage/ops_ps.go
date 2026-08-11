package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type PSQuery struct {
	RunID           string // exact run filter; empty means list
	ProjectID       string
	Status          string // RunStatus; empty means any
	Limit           int
	AfterRunID      string         // keyset pagination cursor
	ConfiguredQuota map[string]int // optional live per-severity daily ceilings
}

// PSAttempt is the current-attempt projection shown by ops.ps.
type PSAttempt struct {
	AttemptNo      int    `json:"attempt_no"`
	Generation     int    `json:"generation"`
	Phase          string `json:"phase"`
	IsolationState string `json:"isolation_state"`
	HeartbeatAtMS  int64  `json:"heartbeat_at_ms"`
	AgentID        string `json:"agent_id"`
}

// PSRun is one row of an ops.ps listing.
type PSRun struct {
	RunID              string     `json:"run_id"`
	ProjectID          string     `json:"project_id"`
	Status             string     `json:"status"`
	Version            int64      `json:"version"`
	Attempt            *PSAttempt `json:"attempt"`
	OpenInterruptCount int        `json:"open_interrupt_count"`
	PendingOutboxCount int        `json:"pending_outbox_count"`
	GateBypassed       bool       `json:"gate_bypassed"`
	UpdatedAtMS        int64      `json:"updated_at_ms"`
}

// PSReport is the ops.ps result.
type PSReport struct {
	Runs               []PSRun        `json:"runs"`
	NextAfterRunID     string         `json:"next_after_run_id"`
	AttentionRemaining map[string]int `json:"attention_remaining"`
}

// RunPS lists Runs with their current attempt, open-Interrupt / pending-outbox
// counts and today's remaining attention quota per severity. It is keyset
// paginated by run_id ascending (control-plane.md §6.2).
func (d *DB) RunPS(ctx context.Context, q PSQuery) (PSReport, error) {
	if q.Limit < 1 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	report := PSReport{Runs: []PSRun{}, AttentionRemaining: map[string]int{}}

	where := ""
	args := []any{}
	if q.RunID != "" {
		where = "WHERE r.id=?"
		args = append(args, q.RunID)
	} else {
		conds := []string{}
		if q.ProjectID != "" {
			conds = append(conds, "r.project_id=?")
			args = append(args, q.ProjectID)
		}
		if q.Status != "" {
			conds = append(conds, "r.status=?")
			args = append(args, q.Status)
		}
		if q.AfterRunID != "" {
			conds = append(conds, "r.id>?")
			args = append(args, q.AfterRunID)
		}
		if len(conds) > 0 {
			where = "WHERE " + strings.Join(conds, " AND ")
		}
	}
	args = append(args, q.Limit)
	query := `SELECT r.id,r.project_id,r.status,r.version,r.gate_bypassed,r.updated_at_ms,
		a.attempt_no,a.agent_id,a.generation,a.phase,a.isolation_state,COALESCE(a.heartbeat_at_ms,0)
		FROM runs r
		LEFT JOIN attempts a ON a.run_id=r.id AND a.attempt_no=(SELECT MAX(attempt_no) FROM attempts WHERE run_id=r.id)
		` + where + ` ORDER BY r.id ASC LIMIT ?`
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return report, fmt.Errorf("storage: ps runs: %w", err)
	}
	type runRow struct {
		ps PSRun
	}
	var list []runRow
	for rows.Next() {
		var r PSRun
		var status string
		var bypass int
		var attemptNo sql.NullInt64
		var agentID sql.NullString
		var gen sql.NullInt64
		var phase, isolation sql.NullString
		var hb sql.NullInt64
		if err := rows.Scan(&r.RunID, &r.ProjectID, &status, &r.Version, &bypass, &r.UpdatedAtMS, &attemptNo, &agentID, &gen, &phase, &isolation, &hb); err != nil {
			rows.Close()
			return report, err
		}
		r.Status, r.GateBypassed = status, bypass != 0
		if attemptNo.Valid {
			r.Attempt = &PSAttempt{AttemptNo: int(attemptNo.Int64), AgentID: agentID.String, Generation: int(gen.Int64), Phase: phase.String, IsolationState: isolation.String, HeartbeatAtMS: hb.Int64}
		}
		list = append(list, runRow{ps: r})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()

	for _, row := range list {
		r := row.ps
		r.OpenInterruptCount, err = d.countWhere(ctx, "interrupts", "run_id=? AND status='open'", []any{r.RunID})
		if err != nil {
			return report, err
		}
		r.PendingOutboxCount, err = d.countWhere(ctx, "outbox_operations", "run_id=? AND state IN ('pending','executing','retryable')", []any{r.RunID})
		if err != nil {
			return report, err
		}
		report.Runs = append(report.Runs, r)
	}
	if q.RunID == "" && len(report.Runs) == q.Limit {
		report.NextAfterRunID = report.Runs[len(report.Runs)-1].RunID
	}

	// Today's remaining attention per severity = ceiling − consumed. The
	// ceiling is the live configured quota when supplied, else the limit
	// persisted on the latest bucket; consumed always comes from persisted
	// budget_counters so a restart never loses the count.
	report.AttentionRemaining = d.attentionRemaining(ctx, q.ConfiguredQuota)
	return report, nil
}

// attentionRemaining returns today's remaining attention per severity. It
// merges the live configured ceilings with persisted consumption: a severity
// that has never been bucketed reports its configured ceiling as fully
// remaining, while a consumed bucket decrements from the same ceiling.
func (d *DB) attentionRemaining(ctx context.Context, configured map[string]int) map[string]int {
	buckets, _ := d.attentionConsumed(ctx)
	out := map[string]int{}
	for _, sev := range []string{"low", "normal", "high"} {
		limit, ok := configured[sev]
		if !ok {
			limit = 0
		}
		consumed := 0
		if b, ok := buckets[sev]; ok {
			// A persisted bucket carries its own frozen limit; prefer the live
			// configured ceiling when supplied so a same-day quota raise shows.
			if limit == 0 {
				limit = b.limit
			}
			consumed = b.consumed
		}
		remaining := limit - consumed
		if remaining < 0 {
			remaining = 0
		}
		out[sev] = remaining
	}
	return out
}

type attentionBucket struct{ consumed, limit int }

// attentionConsumed reads the latest daily attention bucket per severity from
// persisted budget_counters — the current/today bucket as the daemon wrote it.
func (d *DB) attentionConsumed(ctx context.Context) (map[string]attentionBucket, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT scope_id, consumed_value, limit_value FROM budget_counters WHERE kind='attention' AND scope='severity' AND (scope_id, bucket_start_ms) IN (SELECT scope_id, MAX(bucket_start_ms) FROM budget_counters WHERE kind='attention' AND scope='severity' GROUP BY scope_id)`)
	if err != nil {
		return nil, fmt.Errorf("storage: attention consumed: %w", err)
	}
	defer rows.Close()
	out := map[string]attentionBucket{}
	for rows.Next() {
		var sev string
		var consumed, limit int
		if err := rows.Scan(&sev, &consumed, &limit); err != nil {
			return nil, err
		}
		out[sev] = attentionBucket{consumed: consumed, limit: limit}
	}
	return out, rows.Err()
}

// MaxAttemptNo returns the highest attempt number for a Run, or 0 if none. It
// resolves the attempt scope for ops.logs when the caller omits attempt_no.
func (d *DB) MaxAttemptNo(ctx context.Context, runID string) (int, error) {
	var n sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT MAX(attempt_no) FROM attempts WHERE run_id=?`, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: max attempt no: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

func (d *DB) countWhere(ctx context.Context, table, clause string, args []any) (int, error) {
	var n int
	if err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, table, clause), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count %s: %w", table, err)
	}
	return n, nil
}

// TimelineQuery scopes an event-timeline read.
