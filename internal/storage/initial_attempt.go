package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// CreateInitialAttemptCmd contains the immutable execution-site facts for the
// first attempt. Backend selection is deliberately absent: it is recovered
// only from the Run's frozen config snapshot.
type CreateInitialAttemptCmd struct {
	RunID, WorktreePath, BranchName, BaseRef, BaseSHA string
	ExpectedRunVersion                                int64
	NowMS                                             int64
}

// CreateInitialAttempt creates the initial pending attempt, its claim, and its
// launch operation in one transaction. It reads the assigned Agent's concrete
// backend from the Run's immutable config snapshot, never current config.
func (d *DB) CreateInitialAttempt(ctx context.Context, cmd CreateInitialAttemptCmd) error {
	if cmd.RunID == "" || cmd.ExpectedRunVersion < 1 || cmd.NowMS <= 0 ||
		!filepath.IsAbs(cmd.WorktreePath) || cmd.BranchName == "" || cmd.BaseRef == "" || cmd.BaseSHA == "" {
		return errors.New("storage: invalid initial attempt")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var version int64
	var status, agentID, taskSpecID, snapshotJSON string
	if err := tx.QueryRowContext(ctx, `SELECT r.version,r.status,COALESCE(r.agent_id,''),COALESCE(r.current_task_spec_id,''),s.canonical_json
		FROM runs r JOIN config_snapshots s ON s.id=r.config_snapshot_id WHERE r.id=?`, cmd.RunID).
		Scan(&version, &status, &agentID, &taskSpecID, &snapshotJSON); err != nil {
		return err
	}
	if version != cmd.ExpectedRunVersion {
		return ErrRejectedStale
	}
	if status != string(RunQueued) || agentID == "" || taskSpecID == "" {
		return fmt.Errorf("%w: initial attempt requires assigned queued Run", ErrIllegalTransition)
	}
	backend, err := frozenAgentBackend([]byte(snapshotJSON), agentID)
	if err != nil {
		return err
	}

	key := LaunchOperationKey(cmd.RunID, 1, 1)
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempts
		(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms)
		VALUES (?,1,'pending',1,?,?,?,?,?,?,?,'none',?,?)`,
		cmd.RunID, backend, agentID, taskSpecID, cmd.WorktreePath, cmd.BranchName, cmd.BaseRef, cmd.BaseSHA, cmd.NowMS, cmd.NowMS); err != nil {
		return fmt.Errorf("storage: create initial attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_claims
		(run_id,attempt_no,generation,launch_operation_key,created_at_ms,updated_at_ms) VALUES (?,1,1,?,?,?)`,
		cmd.RunID, key, cmd.NowMS, cmd.NowMS); err != nil {
		return fmt.Errorf("storage: create initial attempt claim: %w", err)
	}
	if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationLaunchAgent, RunID: cmd.RunID, AttemptNo: intPtr(1), Payload: []byte(`{"schema_version":1}`)}, cmd.RunID, "", cmd.NowMS); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.wakeOutbox()
	return nil
}

func frozenAgentBackend(snapshot []byte, agentID string) (config.Backend, error) {
	var cfg struct {
		Agents []struct {
			ID      string         `json:"id"`
			Backend config.Backend `json:"backend"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(snapshot, &cfg); err != nil {
		return "", fmt.Errorf("storage: decode frozen config snapshot: %w", err)
	}
	for _, agent := range cfg.Agents {
		if agent.ID == agentID {
			if agent.Backend != config.BackendProcess && agent.Backend != config.BackendTmux {
				return "", fmt.Errorf("storage: frozen agent %q has invalid backend %q", agentID, agent.Backend)
			}
			return agent.Backend, nil
		}
	}
	return "", fmt.Errorf("storage: assigned agent %q is absent from frozen config snapshot", agentID)
}

// ConfigSnapshotID returns the immutable snapshot identity for a loaded
// configuration hash. It is a read-only lookup used when a Run is created.
func (d *DB) ConfigSnapshotID(ctx context.Context, configHash string) (string, error) {
	if configHash == "" {
		return "", errors.New("storage: config hash is required")
	}
	var id string
	if err := d.db.QueryRowContext(ctx, `SELECT id FROM config_snapshots WHERE config_hash=?`, configHash).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}
