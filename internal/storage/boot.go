package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// StartDaemonBoot records a new daemon lifetime with its recovery barrier
// closed. A boot never reuses a prior completion marker: every restart must
// reclassify recovery candidates before it can dispatch an agent.
func (d *DB) StartDaemonBoot(ctx context.Context, configHash, binaryVersion string, protocolMajor, pid int, nowMS int64) (string, error) {
	if configHash == "" || binaryVersion == "" || protocolMajor < 1 || pid < 1 || nowMS <= 0 {
		return "", errors.New("storage: invalid daemon boot")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var configID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM config_snapshots WHERE config_hash=?`, configHash).Scan(&configID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("storage: daemon boot config snapshot: %w", err)
		}
		return "", err
	}
	id := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO daemon_boots
		(id,config_snapshot_id,pid,binary_version,protocol_major,started_at_ms)
		VALUES (?,?,?,?,?,?)`, id, configID, pid, binaryVersion, protocolMajor, nowMS); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// CompleteStartupRecovery opens this boot's launch-agent claim barrier. It is
// deliberately separate from recovery writes so a crash before this commit
// leaves the next boot closed as well.
func (d *DB) CompleteStartupRecovery(ctx context.Context, bootID string, nowMS int64) error {
	if bootID == "" || nowMS <= 0 {
		return errors.New("storage: invalid startup recovery completion")
	}
	// The final query is part of the barrier write, not a best-effort check in
	// the coordinator. A crash or a newly visible candidate cannot open the
	// launch gate until it has a boot-scoped classification receipt.
	result, err := d.db.ExecContext(ctx, `UPDATE daemon_boots
		SET recovery_completed_at_ms=?
		WHERE id=? AND recovery_completed_at_ms IS NULL
		AND NOT EXISTS (
			SELECT 1 FROM attempts a WHERE a.phase NOT IN ('finished','orphaned')
			AND NOT EXISTS (SELECT 1 FROM startup_recovery_actions s WHERE s.boot_id=?
				AND s.candidate_key='attempt:' || a.run_id || ':' || a.attempt_no)
		)
		AND NOT EXISTS (
			SELECT 1 FROM outbox_operations o WHERE o.kind='launch_agent' AND o.state NOT IN ('succeeded','failed','stale','conflict')
			AND NOT EXISTS (SELECT 1 FROM startup_recovery_actions s WHERE s.boot_id=?
				AND s.candidate_key='operation:' || o.id)
		)`, nowMS, bootID, bootID, bootID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRejectedStale
	}
	return nil
}
