package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RecoveryAttempt is the immutable snapshot a recovery coordinator needs
// before it performs process IO. It deliberately includes attempts from failed
// Runs: Run status is not evidence that an execution body is gone.
type RecoveryAttempt struct {
	RunID                    string
	RunVersion               int64
	AttemptNo                int
	Generation               int
	Phase                    string
	Backend                  string
	AgentID                  string
	DispatchID               string
	TopologyQualificationKey string
	WrapperPID               int
	WrapperStartedAtMS       int64
	WrapperExecutable        string
	WrapperPGID              int
	ControlNonceHash         string
	HeartbeatAtMS            int64
	IsolationState           string
}

type RecoveryLaunchOperation struct {
	ID        string
	Version   int64
	RunID     string
	AttemptNo int
	State     OperationState
}

type StartupRecoveryAction struct {
	BootID                   string
	RunID                    string
	AttemptNo                int
	ExpectedGeneration       int
	OperationID              string
	ExpectedOperationVersion int64
	ObservationDigest        string
	Action                   string
	NowMS                    int64
}

const (
	startupRecoverySupervise  = "supervise"
	startupRecoveryRedispatch = "redispatch"
	startupRecoveryFreeze     = "frozen"
	startupRecoveryReuse      = "reuse_dispatch"
	startupRecoveryOperation  = "converge_operation"
)

// ApplyStartupRecoveryAction applies a closed-set recovery action and records
// its boot receipt in the same transaction. A receipt is evidence of a safe
// postcondition, never a substitute for one.
func (d *DB) ApplyStartupRecoveryAction(ctx context.Context, cmd StartupRecoveryAction) error {
	if cmd.BootID == "" || cmd.ObservationDigest == "" || cmd.NowMS <= 0 {
		return errors.New("storage: invalid startup recovery action")
	}
	attemptTarget := cmd.RunID != "" || cmd.AttemptNo != 0 || cmd.ExpectedGeneration != 0
	operationTarget := cmd.OperationID != "" || cmd.ExpectedOperationVersion != 0
	if attemptTarget == operationTarget || (attemptTarget && (cmd.AttemptNo < 1 || cmd.ExpectedGeneration < 1)) || (operationTarget && (cmd.OperationID == "" || cmd.ExpectedOperationVersion < 1)) {
		return errors.New("storage: invalid startup recovery action target")
	}
	if attemptTarget && cmd.Action != startupRecoverySupervise && cmd.Action != startupRecoveryRedispatch && cmd.Action != startupRecoveryFreeze && cmd.Action != startupRecoveryReuse || operationTarget && cmd.Action != startupRecoveryOperation {
		return errors.New("storage: unknown startup recovery action")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var complete sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT recovery_completed_at_ms FROM daemon_boots WHERE id=?`, cmd.BootID).Scan(&complete); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRejectedStale
		}
		return err
	}
	if complete.Valid {
		return ErrRejectedStale
	}

	key := ""
	if attemptTarget {
		var phase, isolation, runStatus string
		var generation int
		if err := tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,a.isolation_state,r.status FROM attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=? AND a.phase NOT IN ('finished','orphaned')`, cmd.RunID, cmd.AttemptNo).Scan(&phase, &generation, &isolation, &runStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrRejectedStale
			}
			return err
		}
		if generation != cmd.ExpectedGeneration {
			return ErrRejectedStale
		}
		key = fmt.Sprintf("attempt:%s:%d", cmd.RunID, cmd.AttemptNo)
		if cmd.Action == startupRecoveryRedispatch {
			if phase != "pending" || isolation != "none" {
				return ErrRejectedStale
			}
			newGeneration := generation + 1
			newKey := LaunchOperationKey(cmd.RunID, cmd.AttemptNo, newGeneration)
			if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='stale',lease_owner=NULL,lease_expires_at_ms=NULL,completed_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND kind='launch_agent' AND state NOT IN ('succeeded','failed','stale','conflict')`, cmd.NowMS, cmd.NowMS, cmd.RunID, cmd.AttemptNo); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attempts SET generation=?,wrapper_pid=NULL,wrapper_started_at_ms=NULL,wrapper_executable=NULL,wrapper_pgid=NULL,wrapper_instance_id=NULL,agent_pid=NULL,agent_started_at_ms=NULL,agent_executable=NULL,control_nonce_hash=NULL,heartbeat_at_ms=NULL,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=? AND phase='pending'`, newGeneration, cmd.NowMS, cmd.RunID, cmd.AttemptNo, generation); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attempt_claims SET generation=?,launch_operation_key=?,dispatch_id=NULL,bootstrap_nonce_hash=NULL,run_token_hash=NULL,wrapper_instance_id=NULL,wrapper_session_hash=NULL,spawn_permit_hash=NULL,acquired_at_ms=NULL,permit_issued_at_ms=NULL,started_confirmed_at_ms=NULL,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND generation=?`, newGeneration, newKey, cmd.NowMS, cmd.RunID, cmd.AttemptNo, generation); err != nil {
				return err
			}
			if err := insertOperation(ctx, tx, Operation{Key: newKey, Kind: OperationLaunchAgent, Payload: []byte(`{"schema_version":1}`), RunID: cmd.RunID, AttemptNo: intPtr(cmd.AttemptNo)}, cmd.RunID, "", cmd.NowMS); err != nil {
				return err
			}
		}
		if cmd.Action == startupRecoveryFreeze {
			var visible int
			if isolation != "frozen" || runStatus != string(RunWaitingHuman) {
				return ErrRejectedStale
			}
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM interrupts WHERE run_id=? AND attempt_no=? AND reason='startup_stall' AND status='open'`, cmd.RunID, cmd.AttemptNo).Scan(&visible); err != nil {
				return err
			}
			if visible != 1 {
				return ErrRejectedStale
			}
		}
	} else {
		var version int64
		var operationKey string
		if err := tx.QueryRowContext(ctx, `SELECT version,operation_key FROM outbox_operations WHERE id=? AND kind='launch_agent' AND state NOT IN ('succeeded','failed','stale','conflict')`, cmd.OperationID).Scan(&version, &operationKey); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrRejectedStale
			}
			return err
		}
		if version != cmd.ExpectedOperationVersion {
			return ErrRejectedStale
		}
		key = fmt.Sprintf("operation:%s", cmd.OperationID)
		var safeRedispatch int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts a LEFT JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id=(SELECT run_id FROM outbox_operations WHERE id=?) AND a.attempt_no=(SELECT attempt_no FROM outbox_operations WHERE id=?) AND a.phase='pending' AND a.isolation_state='none' AND c.generation=a.generation AND c.launch_operation_key=?`, cmd.OperationID, cmd.OperationID, operationKey).Scan(&safeRedispatch); err != nil {
			return err
		}
		if safeRedispatch == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='stale',lease_owner=NULL,lease_expires_at_ms=NULL,completed_at_ms=?,updated_at_ms=? WHERE id=? AND version=?`, cmd.NowMS, cmd.NowMS, cmd.OperationID, cmd.ExpectedOperationVersion); err != nil {
				return err
			}
		}
	}
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT observation_digest FROM startup_recovery_actions WHERE boot_id=? AND candidate_key=?`, cmd.BootID, key).Scan(&digest)
	if err == nil {
		if digest != cmd.ObservationDigest {
			return ErrRejectedStale
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO startup_recovery_actions (boot_id,candidate_key,observation_digest,action,applied_at_ms) VALUES (?,?,?,?,?)`, cmd.BootID, key, cmd.ObservationDigest, cmd.Action, cmd.NowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// StartupRecoveryPending reports the attempt/unfinished-launch-operation
// union. Every unfinished operation is independently converged, including a
// newly fenced dispatch that is safe for the worker to claim.
func (d *DB) StartupRecoveryPending(ctx context.Context, bootID string) ([]RecoveryAttempt, []RecoveryLaunchOperation, error) {
	if bootID == "" {
		return nil, nil, errors.New("storage: boot id is required")
	}
	attempts, err := d.recoveryAttempts(ctx, bootID)
	if err != nil {
		return nil, nil, err
	}
	operations, err := d.recoveryLaunchOperations(ctx, bootID)
	if err != nil {
		return nil, nil, err
	}
	return attempts, operations, nil
}

const recoveryAttemptColumns = `a.run_id,r.version,a.attempt_no,a.generation,a.phase,a.backend,a.agent_id,
	COALESCE(c.dispatch_id,''),COALESCE(a.topology_qualification_key,''),COALESCE(a.wrapper_pid,0),COALESCE(a.wrapper_started_at_ms,0),COALESCE(a.wrapper_executable,''),COALESCE(a.wrapper_pgid,0),
	COALESCE(a.control_nonce_hash,''),COALESCE(a.heartbeat_at_ms,0),a.isolation_state`

func scanRecoveryAttempt(rows *sql.Rows) (RecoveryAttempt, error) {
	var a RecoveryAttempt
	err := rows.Scan(&a.RunID, &a.RunVersion, &a.AttemptNo, &a.Generation, &a.Phase, &a.Backend, &a.AgentID, &a.DispatchID, &a.TopologyQualificationKey, &a.WrapperPID, &a.WrapperStartedAtMS, &a.WrapperExecutable, &a.WrapperPGID, &a.ControlNonceHash, &a.HeartbeatAtMS, &a.IsolationState)
	return a, err
}

func (d *DB) recoveryAttempts(ctx context.Context, bootID string) ([]RecoveryAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+recoveryAttemptColumns+` FROM attempts a JOIN runs r ON r.id=a.run_id LEFT JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.phase NOT IN ('finished','orphaned') AND NOT EXISTS (SELECT 1 FROM startup_recovery_actions s WHERE s.boot_id=? AND s.candidate_key='attempt:' || a.run_id || ':' || a.attempt_no) ORDER BY a.run_id,a.attempt_no`, bootID)
	if err != nil {
		return nil, fmt.Errorf("storage: list recovery attempts: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryAttempt
	for rows.Next() {
		a, err := scanRecoveryAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (d *DB) recoveryLaunchOperations(ctx context.Context, bootID string) ([]RecoveryLaunchOperation, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT o.id,o.version,COALESCE(o.run_id,''),COALESCE(o.attempt_no,0),o.state FROM outbox_operations o WHERE o.kind='launch_agent' AND o.state NOT IN ('succeeded','failed','stale','conflict') AND NOT EXISTS (SELECT 1 FROM startup_recovery_actions s WHERE s.boot_id=? AND s.candidate_key='operation:' || o.id) ORDER BY o.id`, bootID)
	if err != nil {
		return nil, fmt.Errorf("storage: list recovery launch operations: %w", err)
	}
	defer rows.Close()
	var operations []RecoveryLaunchOperation
	for rows.Next() {
		var o RecoveryLaunchOperation
		var state string
		if err := rows.Scan(&o.ID, &o.Version, &o.RunID, &o.AttemptNo, &state); err != nil {
			return nil, err
		}
		o.State = OperationState(state)
		operations = append(operations, o)
	}
	return operations, rows.Err()
}

func (d *DB) RecoveryAttempts(ctx context.Context) ([]RecoveryAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+recoveryAttemptColumns+` FROM attempts a JOIN runs r ON r.id=a.run_id LEFT JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.phase NOT IN ('finished','orphaned') ORDER BY a.run_id,a.attempt_no`)
	if err != nil {
		return nil, fmt.Errorf("storage: list recovery attempts: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryAttempt
	for rows.Next() {
		a, err := scanRecoveryAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (d *DB) StaleHeartbeatAttempts(ctx context.Context, cutoffMS int64) ([]RecoveryAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+recoveryAttemptColumns+` FROM attempts a JOIN runs r ON r.id=a.run_id LEFT JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.phase='running' AND (a.heartbeat_at_ms IS NULL OR a.heartbeat_at_ms < ?) ORDER BY a.run_id,a.attempt_no`, cutoffMS)
	if err != nil {
		return nil, fmt.Errorf("storage: list stale heartbeats: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryAttempt
	for rows.Next() {
		a, err := scanRecoveryAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (d *DB) RecoveryAttemptForRun(ctx context.Context, runID string) (RecoveryAttempt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+recoveryAttemptColumns+` FROM attempts a JOIN runs r ON r.id=a.run_id LEFT JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no WHERE a.run_id=? AND a.phase NOT IN ('finished','orphaned') ORDER BY a.attempt_no DESC LIMIT 1`, runID)
	if err != nil {
		return RecoveryAttempt{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return RecoveryAttempt{}, ErrRejectedStale
	}
	return scanRecoveryAttempt(rows)
}
