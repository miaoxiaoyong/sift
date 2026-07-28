package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// IntakeReplyCmd is a trusted, already-authenticated forge comment. The
// generation is carried by the comment operation payload, never inferred from
// its natural-language body.
type IntakeReplyCmd struct {
	IntakeID, EventID, Actor, RawDigest string
	Generation                          int
	Accept                              bool
	ObservedAtMS, NowMS                 int64
}

// ApplyIntakeReply records every reply. A reply from an old generation is
// intentionally audit-only; it cannot move the current projection.
func (d *DB) ApplyIntakeReply(ctx context.Context, cmd IntakeReplyCmd) error {
	if cmd.IntakeID == "" || cmd.EventID == "" || cmd.Actor == "" || cmd.Generation < 1 || cmd.ObservedAtMS <= 0 || cmd.NowMS <= 0 {
		return errors.New("storage: incomplete intake reply")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID, state string
	var generation, version int
	if err = tx.QueryRowContext(ctx, `SELECT project_id,state,version,clarification_generation FROM intake_items WHERE id=?`, cmd.IntakeID).Scan(&projectID, &state, &version, &generation); err != nil {
		return err
	}
	var existingReceipt string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM forge_event_receipts WHERE project_id=? AND forge_event_id=?`, projectID, cmd.EventID).Scan(&existingReceipt); err == nil {
		return tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"intake_id": cmd.IntakeID, "generation": cmd.Generation, "accepted": cmd.Accept, "forge_event_id": cmd.EventID})
	eventType := "intake.reply_ignored"
	if cmd.Generation == generation && cmd.Accept && (state == "awaiting_clarification" || state == "awaiting_duplicate_confirmation") {
		eventType = "intake.reply_accepted"
		if _, err = tx.ExecContext(ctx, `UPDATE intake_items SET state='pending_evaluation',version=version+1,updated_at_ms=? WHERE id=? AND version=?`, cmd.NowMS, cmd.IntakeID, version); err != nil {
			return err
		}
	}
	eventID := newID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,actor,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'forge',?,1,?,?,?)`, eventID, projectID, eventType, cmd.Actor, string(payload), cmd.ObservedAtMS, cmd.NowMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO forge_event_receipts(id,project_id,forge_event_id,event_kind,target_kind,target_id,actor,raw_digest,disposition,domain_event_id,observed_at_ms) VALUES(?,?,?,?,'issue',?,?,?, 'accepted',?,?)`, newID(), projectID, cmd.EventID, "issue_comment", cmd.IntakeID, cmd.Actor, cmd.RawDigest, eventID, cmd.ObservedAtMS); err != nil {
		return err
	}
	return tx.Commit()
}
