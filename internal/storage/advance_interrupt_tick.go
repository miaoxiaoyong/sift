package storage

import (
	"context"
	"database/sql"
	"encoding/json"
)

func finishAdvance(ctx context.Context, tx *sql.Tx, res sql.Result, cmd AdvanceInterruptCmd, event string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, ErrRejectedStale
	}
	payload, _ := json.Marshal(map[string]any{"interrupt_id": cmd.InterruptID, "advance": cmd.Kind})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,'system',1,?,?,?)`, newID(), event, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DB) SupervisorInterruptTick(ctx context.Context, now int64) error {
	rows, err := d.db.QueryContext(ctx, `SELECT id,version,nonce,'expiry' FROM interrupts WHERE status='open' AND dispatch_state!='probe_in_progress' AND (dispatch_state!='held' OR held_reason='manual') AND expires_at_ms<=? UNION ALL SELECT id,version,nonce,'dispatch' FROM interrupts WHERE status='open' AND dispatch_state='ready' AND next_dispatch_at_ms<=?`, now, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	var cmds []AdvanceInterruptCmd
	for rows.Next() {
		var c AdvanceInterruptCmd
		var kind string
		if err := rows.Scan(&c.InterruptID, &c.ExpectedVersion, &c.ExpectedNonce, &kind); err != nil {
			return err
		}
		c.Kind, c.NowMS = AdvanceKind(kind), now
		cmds = append(cmds, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cmds {
		if _, err := d.AdvanceInterrupt(ctx, c); err != nil && err != ErrRejectedStale {
			return err
		}
	}
	return d.PrepareDueAttentionBatches(ctx, now)
}
