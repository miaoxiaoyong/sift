package storage

import (
	"context"
	"database/sql"
	"errors"
)

// RevalidateLaunchLease fences every external launch-worker step. A lease that
// expired or moved to another worker is never authority to write a bootstrap
// file or start a wrapper.
func (d *DB) RevalidateLaunchLease(ctx context.Context, claim ClaimedOperation, nowMS int64) error {
	if claim.Kind != OperationLaunchAgent || claim.ID == "" || claim.LeaseOwner == "" || claim.LeaseExpiresAtMS <= 0 || nowMS <= 0 {
		return ErrRejectedStaleWorker
	}
	var one int
	err := d.db.QueryRowContext(ctx, `SELECT 1 FROM outbox_operations WHERE id=? AND kind='launch_agent' AND state='executing' AND lease_owner=? AND lease_expires_at_ms=? AND lease_expires_at_ms>=?`, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS, nowMS).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRejectedStaleWorker
	}
	return err
}

// RecordBootstrapDigest binds the durable dispatch to the exact bootstrap
// bytes written by its lease owner. Recovery must not infer a credential file
// from hashes alone.
func (d *DB) RecordBootstrapDigest(ctx context.Context, claim ClaimedOperation, dispatchID, digest string, nowMS int64) error {
	if dispatchID == "" || len(digest) != 64 || !isLowerHex(digest) {
		return ErrRejectedStaleWorker
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM outbox_operations WHERE id=? AND kind='launch_agent' AND state='executing' AND lease_owner=? AND lease_expires_at_ms=? AND lease_expires_at_ms>=?`, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS, nowMS).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRejectedStaleWorker
		}
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE attempt_claims SET bootstrap_digest=?,updated_at_ms=?
		WHERE dispatch_id=? AND bootstrap_digest IS NULL
		AND run_id=(SELECT run_id FROM outbox_operations WHERE id=?)
		AND attempt_no=(SELECT attempt_no FROM outbox_operations WHERE id=?)`, digest, nowMS, dispatchID, claim.ID, claim.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRejectedStaleWorker
	}
	return tx.Commit()
}

var ErrLaunchDispatchPrepared = errors.New("storage: launch dispatch already prepared")

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
		return LaunchDispatch{}, ErrLaunchDispatchPrepared
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

// ValidatePreparedBootstrap verifies that an on-disk bootstrap belongs to the
// current pending claim. A recorded digest is required when one exists; a
// crash between rename and digest recording is recovered by checking the
// dispatch secrets against their durable hashes.
func (d *DB) ValidatePreparedBootstrap(ctx context.Context, runID string, attemptNo, generation int, dispatchID, bootstrapNonce, runToken, digest string) error {
	var storedDigest, nonceHash, tokenHash string
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(bootstrap_digest,''),COALESCE(bootstrap_nonce_hash,''),COALESCE(run_token_hash,'')
		FROM attempt_claims WHERE run_id=? AND attempt_no=? AND generation=? AND dispatch_id=?`, runID, attemptNo, generation, dispatchID).Scan(&storedDigest, &nonceHash, &tokenHash)
	if err != nil || nonceHash != handoffHash(bootstrapNonce) || tokenHash != handoffHash(runToken) || (storedDigest != "" && storedDigest != digest) {
		return ErrRejectedStaleWorker
	}
	return nil
}

// ResumeLaunchDispatch returns the durable payload after a new lease owner has
// validated the existing bootstrap file. It never reconstructs capabilities:
// the caller must supply the values read from that file.
func (d *DB) ResumeLaunchDispatch(ctx context.Context, claim ClaimedOperation, dispatchID, bootstrapNonce, runToken, digest string, nowMS int64) (LaunchDispatch, error) {
	if err := d.RevalidateLaunchLease(ctx, claim, nowMS); err != nil {
		return LaunchDispatch{}, err
	}
	var out LaunchDispatch
	err := d.db.QueryRowContext(ctx, `SELECT a.run_id,a.attempt_no,a.generation,a.agent_id,a.task_spec_snapshot_id,a.worktree_path,t.canonical_json
		FROM outbox_operations o JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no
		JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no
		JOIN task_spec_snapshots t ON t.run_id=a.run_id AND t.id=a.task_spec_snapshot_id
		WHERE o.id=? AND o.kind='launch_agent' AND o.state='executing' AND c.dispatch_id=?`, claim.ID, dispatchID).
		Scan(&out.RunID, &out.AttemptNo, &out.Generation, &out.AgentID, &out.TaskSpecID, &out.WorktreePath, &out.TaskSpec)
	if err != nil {
		return LaunchDispatch{}, ErrRejectedStaleWorker
	}
	if err := d.ValidatePreparedBootstrap(ctx, out.RunID, out.AttemptNo, out.Generation, dispatchID, bootstrapNonce, runToken, digest); err != nil {
		return LaunchDispatch{}, err
	}
	out.DispatchID, out.BootstrapNonce, out.RunToken = dispatchID, bootstrapNonce, runToken
	return out, nil
}
