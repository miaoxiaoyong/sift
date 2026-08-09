package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// InitialAttemptSpec carries the immutable execution-site facts needed when a
// T2 assignment also admits its first launch. The assignment write port owns
// the transaction; callers cannot supply or override the effective backend.
type InitialAttemptSpec struct {
	WorktreePath string
	BranchName   string
	BaseRef      string
	BaseSHA      string
}

func (s *InitialAttemptSpec) validate() error {
	if s == nil {
		return nil
	}
	if !filepath.IsAbs(s.WorktreePath) || s.BranchName == "" || s.BaseRef == "" || s.BaseSHA == "" {
		return errors.New("storage: invalid initial attempt")
	}
	return nil
}

// insertInitialAttemptTx is called by the production assignment write port.
// Assignment, effective-backend resolution, attempt, claim, and launch
// operation are one transaction, so a queued Run can never expose an attempt
// whose backend was selected from current configuration.
func insertInitialAttemptTx(ctx context.Context, tx *sql.Tx, runID, agentID, taskSpecID, snapshotJSON string, spec *InitialAttemptSpec, nowMS int64) error {
	if spec == nil {
		return nil
	}
	if err := spec.validate(); err != nil {
		return err
	}
	backend, err := frozenAgentBackend([]byte(snapshotJSON), agentID)
	if err != nil {
		return err
	}
	key := LaunchOperationKey(runID, 1, 1)
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempts
		(run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms)
		VALUES (?,1,'pending',1,?,?,?,?,?,?,?,'none',?,?)`,
		runID, backend, agentID, taskSpecID, spec.WorktreePath, spec.BranchName, spec.BaseRef, spec.BaseSHA, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: create initial attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_claims
		(run_id,attempt_no,generation,launch_operation_key,created_at_ms,updated_at_ms) VALUES (?,1,1,?,?,?)`,
		runID, key, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: create initial attempt claim: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"schema_version": 1, "run_id": runID, "agent_id": agentID, "backend": backend})
	if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationLaunchAgent, RunID: runID, AttemptNo: intPtr(1), Payload: payload}, runID, "", nowMS); err != nil {
		return err
	}
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
