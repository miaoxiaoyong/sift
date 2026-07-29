package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// RecoveryAttempt is the immutable snapshot a recovery or operator-termination
// coordinator needs before it performs any process IO. Callers must return the
// resulting observation through RecordTerminationObservation; they must not
// mutate attempts directly.
type RecoveryAttempt struct {
	RunID              string
	RunVersion         int64
	AttemptNo          int
	Generation         int
	Phase              string
	AgentID            string
	WrapperPID         int
	WrapperStartedAtMS int64
	WrapperExecutable  string
	WrapperPGID        int
	ControlNonceHash   string
	HeartbeatAtMS      int64
	IsolationState     string
}

// RecoveryAttempts returns every nonterminal attempt, irrespective of Run
// state. This deliberately does not filter on runs.status: a failed Run can
// still own an isolated live execution body.
func (d *DB) RecoveryAttempts(ctx context.Context) ([]RecoveryAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT a.run_id,r.version,a.attempt_no,a.generation,a.phase,a.agent_id,
		COALESCE(a.wrapper_pid,0),COALESCE(a.wrapper_started_at_ms,0),COALESCE(a.wrapper_executable,''),COALESCE(a.wrapper_pgid,0),
		COALESCE(a.control_nonce_hash,''),COALESCE(a.heartbeat_at_ms,0),a.isolation_state
		FROM attempts a JOIN runs r ON r.id=a.run_id
		WHERE a.phase NOT IN ('finished','orphaned') ORDER BY a.run_id,a.attempt_no`)
	if err != nil {
		return nil, fmt.Errorf("storage: list recovery attempts: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryAttempt
	for rows.Next() {
		var a RecoveryAttempt
		if err := rows.Scan(&a.RunID, &a.RunVersion, &a.AttemptNo, &a.Generation, &a.Phase, &a.AgentID, &a.WrapperPID, &a.WrapperStartedAtMS, &a.WrapperExecutable, &a.WrapperPGID, &a.ControlNonceHash, &a.HeartbeatAtMS, &a.IsolationState); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

// RecoveryAttemptForRun returns the newest nonterminal attempt for an
// operator request. The requested Run version is checked later by the write
// port, preserving the command's CAS boundary.
// StaleHeartbeatAttempts returns running attempts whose persisted heartbeat is
// absent or older than cutoff. The caller must still use controlled
// termination before treating any of them as orphaned.
func (d *DB) StaleHeartbeatAttempts(ctx context.Context, cutoffMS int64) ([]RecoveryAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT a.run_id,r.version,a.attempt_no,a.generation,a.phase,a.agent_id,
		COALESCE(a.wrapper_pid,0),COALESCE(a.wrapper_started_at_ms,0),COALESCE(a.wrapper_executable,''),COALESCE(a.wrapper_pgid,0),
		COALESCE(a.control_nonce_hash,''),COALESCE(a.heartbeat_at_ms,0),a.isolation_state
		FROM attempts a JOIN runs r ON r.id=a.run_id
		WHERE a.phase='running' AND (a.heartbeat_at_ms IS NULL OR a.heartbeat_at_ms < ?) ORDER BY a.run_id,a.attempt_no`, cutoffMS)
	if err != nil {
		return nil, fmt.Errorf("storage: list stale heartbeats: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryAttempt
	for rows.Next() {
		var a RecoveryAttempt
		if err := rows.Scan(&a.RunID, &a.RunVersion, &a.AttemptNo, &a.Generation, &a.Phase, &a.AgentID, &a.WrapperPID, &a.WrapperStartedAtMS, &a.WrapperExecutable, &a.WrapperPGID, &a.ControlNonceHash, &a.HeartbeatAtMS, &a.IsolationState); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (d *DB) RecoveryAttemptForRun(ctx context.Context, runID string) (RecoveryAttempt, error) {
	var a RecoveryAttempt
	err := d.db.QueryRowContext(ctx, `SELECT a.run_id,r.version,a.attempt_no,a.generation,a.phase,a.agent_id,
		COALESCE(a.wrapper_pid,0),COALESCE(a.wrapper_started_at_ms,0),COALESCE(a.wrapper_executable,''),COALESCE(a.wrapper_pgid,0),
		COALESCE(a.control_nonce_hash,''),COALESCE(a.heartbeat_at_ms,0),a.isolation_state
		FROM attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.phase NOT IN ('finished','orphaned') ORDER BY a.attempt_no DESC LIMIT 1`, runID).
		Scan(&a.RunID, &a.RunVersion, &a.AttemptNo, &a.Generation, &a.Phase, &a.AgentID, &a.WrapperPID, &a.WrapperStartedAtMS, &a.WrapperExecutable, &a.WrapperPGID, &a.ControlNonceHash, &a.HeartbeatAtMS, &a.IsolationState)
	if err == sql.ErrNoRows {
		return RecoveryAttempt{}, ErrRejectedStale
	}
	if err != nil {
		return RecoveryAttempt{}, fmt.Errorf("storage: recovery attempt for run: %w", err)
	}
	return a, nil
}
