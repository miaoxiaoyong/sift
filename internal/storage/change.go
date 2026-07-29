package storage

import (
	"context"
	"database/sql"
	"errors"
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
