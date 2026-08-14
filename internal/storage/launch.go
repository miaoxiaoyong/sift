package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/xsift/sift/internal/config"
)

// ClearLaunchQualification removes an invalidated qualification binding before
// any wrapper is spawned. Recovery consequently treats the attempt as
// unverified rather than authorizing absence from a stale verified row.
func (d *DB) ClearLaunchQualification(ctx context.Context, claim ClaimedOperation, qualificationKey string, nowMS int64) error {
	if claim.Kind != OperationLaunchAgent || claim.ID == "" || claim.LeaseOwner == "" || claim.LeaseExpiresAtMS <= 0 || qualificationKey == "" || nowMS <= 0 {
		return ErrRejectedStaleWorker
	}
	result, err := d.db.ExecContext(ctx, `UPDATE attempts SET topology_qualification_key=NULL,updated_at_ms=?
		WHERE run_id=(SELECT run_id FROM outbox_operations WHERE id=?)
		AND attempt_no=(SELECT attempt_no FROM outbox_operations WHERE id=?)
		AND topology_qualification_key=?
		AND EXISTS (SELECT 1 FROM outbox_operations WHERE id=? AND kind='launch_agent' AND state='executing' AND lease_owner=? AND lease_expires_at_ms=? AND lease_expires_at_ms>=?)`, nowMS, claim.ID, claim.ID, qualificationKey, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS, nowMS)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return ErrRejectedStaleWorker
	}
	return nil
}

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

// VerifyLaunchBinding rechecks the durable launch lease and its immutable
// attempt binding after an external host has observed an existing session.
// The tmux observation is not authority on its own: a reclaimed lease or
// changed dispatch must turn the old host call into a stale-worker error.
func (d *DB) VerifyLaunchBinding(ctx context.Context, operationID, leaseOwner string, leaseExpiresAtMS int64, runID string, attemptNo, generation int, dispatchID, backend string, nowMS int64) error {
	if operationID == "" || leaseOwner == "" || leaseExpiresAtMS <= 0 || runID == "" || attemptNo < 1 || generation < 1 || dispatchID == "" || backend == "" || nowMS <= 0 {
		return ErrRejectedStaleWorker
	}
	var one int
	err := d.db.QueryRowContext(ctx, `SELECT 1
		FROM outbox_operations o
		JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no
		JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no
		WHERE o.id=? AND o.kind='launch_agent' AND o.operation_key=? AND o.state='executing'
		  AND o.lease_owner=? AND o.lease_expires_at_ms=? AND o.lease_expires_at_ms>=?
		  AND a.run_id=? AND a.attempt_no=? AND a.generation=? AND a.backend=?
		  AND c.generation=? AND c.dispatch_id=?`, operationID, LaunchOperationKey(runID, attemptNo, generation), leaseOwner, leaseExpiresAtMS, nowMS, runID, attemptNo, generation, backend, generation, dispatchID).Scan(&one)
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
		WHERE dispatch_id=? AND (bootstrap_digest IS NULL OR bootstrap_digest=?)
		AND run_id=(SELECT run_id FROM outbox_operations WHERE id=?)
		AND attempt_no=(SELECT attempt_no FROM outbox_operations WHERE id=?)`, digest, nowMS, dispatchID, digest, claim.ID, claim.ID)
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
	Backend                                     string
	AgentID, TaskSpecID, WorktreePath           string
	TopologyQualificationKey                    string
	TaskSpec                                    []byte
}

// PrepareLaunchDispatch is the launch worker's fencing point. It verifies the
// exact outbox lease before making the dispatch visible; callers may write a
// bootstrap file only after this method succeeds.
func (d *DB) PrepareLaunchDispatch(ctx context.Context, claim ClaimedOperation, dispatchID, bootstrapNonce, runToken string, nowMS int64) (LaunchDispatch, error) {
	return d.PrepareLaunchDispatchWithQualification(ctx, claim, dispatchID, bootstrapNonce, runToken, "", nowMS)
}

// PrepareLaunchDispatchWithQualification atomically binds the current dispatch
// to the exact executable/version qualification key. A replay cannot replace a
// key, while each new attempt can bind a newly measured binary/version.
func (d *DB) PrepareLaunchDispatchWithQualification(ctx context.Context, claim ClaimedOperation, dispatchID, bootstrapNonce, runToken, qualificationKey string, nowMS int64) (LaunchDispatch, error) {
	if claim.Kind != OperationLaunchAgent || claim.ID == "" || claim.LeaseOwner == "" || claim.LeaseExpiresAtMS <= 0 || dispatchID == "" || !validHandoffSecret(bootstrapNonce) || !validHandoffSecret(runToken) || (qualificationKey != "" && !isLowerHex(qualificationKey)) || nowMS <= 0 {
		return LaunchDispatch{}, ErrRejectedStaleWorker
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LaunchDispatch{}, err
	}
	defer tx.Rollback()
	var out LaunchDispatch
	var operationKey string
	err = tx.QueryRowContext(ctx, `SELECT o.operation_key,a.run_id,a.attempt_no,a.generation,a.backend,a.agent_id,a.task_spec_snapshot_id,a.worktree_path,t.canonical_json,COALESCE(c.dispatch_id,''),COALESCE(a.topology_qualification_key,'')
		FROM outbox_operations o JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no
		JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no
		JOIN task_spec_snapshots t ON t.run_id=a.run_id AND t.id=a.task_spec_snapshot_id
		WHERE o.id=? AND o.kind='launch_agent' AND o.state='executing' AND o.lease_owner=? AND o.lease_expires_at_ms=?`, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS).
		Scan(&operationKey, &out.RunID, &out.AttemptNo, &out.Generation, &out.Backend, &out.AgentID, &out.TaskSpecID, &out.WorktreePath, &out.TaskSpec, &out.DispatchID, &out.TopologyQualificationKey)
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
	if qualificationKey != "" {
		if out.TopologyQualificationKey != "" && out.TopologyQualificationKey != qualificationKey {
			return LaunchDispatch{}, ErrRejectedStaleWorker
		}
		if _, err = tx.ExecContext(ctx, `UPDATE attempts SET topology_qualification_key=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND (topology_qualification_key IS NULL OR topology_qualification_key=?)`, qualificationKey, nowMS, out.RunID, out.AttemptNo, qualificationKey); err != nil {
			return LaunchDispatch{}, err
		}
		out.TopologyQualificationKey = qualificationKey
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempt_claims SET dispatch_id=?,bootstrap_nonce_hash=?,run_token_hash=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND dispatch_id IS NULL`, dispatchID, handoffHash(bootstrapNonce), handoffHash(runToken), nowMS, out.RunID, out.AttemptNo); err != nil {
		return LaunchDispatch{}, err
	}
	if err = tx.Commit(); err != nil {
		return LaunchDispatch{}, err
	}
	return out, nil
}

// FrozenLaunchAgent returns the Agent definition from the Run's immutable
// config snapshot, never the daemon's current configuration. It is only a
// preflight for qualification measurement; PrepareLaunchDispatchWithQualification
// repeats the lease fence in its binding transaction.
func (d *DB) FrozenLaunchAgent(ctx context.Context, claim ClaimedOperation, nowMS int64) (config.Agent, error) {
	if err := d.RevalidateLaunchLease(ctx, claim, nowMS); err != nil {
		return config.Agent{}, err
	}
	var agentID, canonical string
	err := d.db.QueryRowContext(ctx, `SELECT a.agent_id,s.canonical_json FROM outbox_operations o JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no JOIN runs r ON r.id=a.run_id JOIN config_snapshots s ON s.id=r.config_snapshot_id WHERE o.id=? AND o.kind='launch_agent'`, claim.ID).Scan(&agentID, &canonical)
	if err != nil {
		return config.Agent{}, ErrRejectedStaleWorker
	}
	var frozen config.Config
	if json.Unmarshal([]byte(canonical), &frozen) != nil {
		return config.Agent{}, errors.New("storage: invalid frozen launch configuration")
	}
	for _, agent := range frozen.Agents {
		if agent.ID == agentID {
			return agent, nil
		}
	}
	return config.Agent{}, errors.New("storage: frozen launch agent is unavailable")
}

// LaunchAgentID is retained solely for legacy synthetic test seeds. Production
// callers use FrozenLaunchAgent and reject a missing frozen definition.
func (d *DB) LaunchAgentID(ctx context.Context, claim ClaimedOperation, nowMS int64) (string, error) {
	if err := d.RevalidateLaunchLease(ctx, claim, nowMS); err != nil {
		return "", err
	}
	var agentID string
	if err := d.db.QueryRowContext(ctx, `SELECT a.agent_id FROM outbox_operations o JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no WHERE o.id=? AND o.kind='launch_agent'`, claim.ID).Scan(&agentID); err != nil {
		return "", ErrRejectedStaleWorker
	}
	return agentID, nil
}

// ValidatePreparedBootstrap verifies that an on-disk bootstrap belongs to the
// current pending claim without reconstructing its capabilities.
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
	err := d.db.QueryRowContext(ctx, `SELECT a.run_id,a.attempt_no,a.generation,a.backend,a.agent_id,a.task_spec_snapshot_id,a.worktree_path,t.canonical_json,COALESCE(a.topology_qualification_key,'')
		FROM outbox_operations o JOIN attempts a ON a.run_id=o.run_id AND a.attempt_no=o.attempt_no
		JOIN attempt_claims c ON c.run_id=a.run_id AND c.attempt_no=a.attempt_no
		JOIN task_spec_snapshots t ON t.run_id=a.run_id AND t.id=a.task_spec_snapshot_id
		WHERE o.id=? AND o.kind='launch_agent' AND o.state='executing' AND c.dispatch_id=?`, claim.ID, dispatchID).
		Scan(&out.RunID, &out.AttemptNo, &out.Generation, &out.Backend, &out.AgentID, &out.TaskSpecID, &out.WorktreePath, &out.TaskSpec, &out.TopologyQualificationKey)
	if err != nil {
		return LaunchDispatch{}, ErrRejectedStaleWorker
	}
	if err := d.ValidatePreparedBootstrap(ctx, out.RunID, out.AttemptNo, out.Generation, dispatchID, bootstrapNonce, runToken, digest); err != nil {
		return LaunchDispatch{}, err
	}
	out.DispatchID, out.BootstrapNonce, out.RunToken = dispatchID, bootstrapNonce, runToken
	return out, nil
}
