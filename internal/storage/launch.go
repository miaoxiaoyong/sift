package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LaunchDispatch is the durable, non-secret launch fact returned after the
// worker has atomically prepared its capabilities.
type LaunchDispatch struct {
	RunID, DispatchID, RunToken, BootstrapNonce string
	AttemptNo, Generation                       int
	AgentID, TaskSpecID, WorktreePath           string
	TaskSpec                                    []byte
}

// PrepareLaunchDispatch is the launch worker's fencing point. It verifies the
// exact outbox lease before making the dispatch visible; callers may write a
// bootstrap file only after this method succeeds.
func (d *DB) PrepareLaunchDispatch(ctx context.Context, claim ClaimedOperation, dispatchID, bootstrapNonce, runToken string, nowMS int64) (LaunchDispatch, error) {
	if claim.Kind != OperationLaunchAgent || claim.ID == "" || claim.LeaseOwner == "" || claim.LeaseExpiresAtMS <= 0 || dispatchID == "" || !validHandoffSecret(bootstrapNonce) || !validHandoffSecret(runToken) || nowMS <= 0 {
		return LaunchDispatch{}, ErrRejectedStaleWorker
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LaunchDispatch{}, err
	}
	defer tx.Rollback()
	var out LaunchDispatch
	var operationKey string
	err = tx.QueryRowContext(ctx, `SELECT o.operation_key,a.run_id,a.attempt_no,a.generation,a.agent_id,a.task_spec_snapshot_id,a.worktree_path,t.canonical_json,c.dispatch_id
		FROM outbox_operations o JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no
		JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no
		JOIN task_spec_snapshots t ON t.run_id=a.run_id AND t.id=a.task_spec_snapshot_id
		WHERE o.id=? AND o.kind='launch_agent' AND o.state='executing' AND o.lease_owner=? AND o.lease_expires_at_ms=?`, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS).
		Scan(&operationKey, &out.RunID, &out.AttemptNo, &out.Generation, &out.AgentID, &out.TaskSpecID, &out.WorktreePath, &out.TaskSpec, &out.DispatchID)
	if errors.Is(err, sql.ErrNoRows) {
		return LaunchDispatch{}, ErrRejectedStaleWorker
	}
	if err != nil {
		return LaunchDispatch{}, err
	}
	if operationKey != LaunchOperationKey(out.RunID, out.AttemptNo, out.Generation) {
		return LaunchDispatch{}, ErrRejectedStaleWorker
	}
	if out.DispatchID != "" {
		return LaunchDispatch{}, fmt.Errorf("%w: dispatch already prepared", ErrRejectedStaleWorker)
	}
	out.DispatchID, out.BootstrapNonce, out.RunToken = dispatchID, bootstrapNonce, runToken
	if _, err = tx.ExecContext(ctx, `UPDATE attempt_claims SET dispatch_id=?,bootstrap_nonce_hash=?,run_token_hash=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND dispatch_id IS NULL`, dispatchID, handoffHash(bootstrapNonce), handoffHash(runToken), nowMS, out.RunID, out.AttemptNo); err != nil {
		return LaunchDispatch{}, err
	}
	if err = tx.Commit(); err != nil {
		return LaunchDispatch{}, err
	}
	return out, nil
}
