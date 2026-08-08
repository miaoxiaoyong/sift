package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func (d *DB) RecordHandoffSecurityEvent(ctx context.Context, runID string, attemptNo int, method, disposition string, nowMS int64) error {
	if runID == "" || attemptNo < 1 || method == "" || disposition == "" || nowMS <= 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"method": method, "disposition": disposition})
	_, err := d.db.ExecContext(ctx, `INSERT INTO events
		(id,run_id,attempt_no,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms)
		SELECT ?,?,?, 'security.handoff_rejected','agent',1,?,?,?
		WHERE EXISTS (SELECT 1 FROM attempts WHERE run_id=? AND attempt_no=?)`,
		newID(), runID, attemptNo, string(payload), nowMS, nowMS, runID, attemptNo)
	return err
}

// Cross-package integration-test seeds. These are NOT domain write ports
// (§11): they exist so tests in other packages (brain shell, control plane)
// can satisfy foreign keys without raw SQL access. Production code must not
// call them.

// ExecForTest runs an arbitrary statement against the underlying handle. It is
// the escape hatch for cross-package tests that need bespoke fixture rows not
// covered by a named Seed helper; it is never a production write port.
func (d *DB) ExecForTest(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, query, args...)
}

// QueryRowForTest returns one row from the underlying handle. It is a
// read-only escape hatch for cross-package tests; production reads use the
// named projections.
func (d *DB) QueryRowForTest(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, query, args...)
}

// AdvanceAttachIdentityForTest atomically simulates recovery superseding an
// active attach binding. It is only for cross-package attach race fixtures.
func (d *DB) AdvanceAttachIdentityForTest(ctx context.Context, runID string, attemptNo, generation int, dispatchID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET generation=? WHERE run_id=? AND attempt_no=?`, generation, runID, attemptNo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempt_claims SET generation=?, dispatch_id=? WHERE run_id=? AND attempt_no=?`, generation, dispatchID, runID, attemptNo); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedProjectForTest inserts a config snapshot and project with minimal
// valid rows.
func (d *DB) SeedProjectForTest(ctx context.Context, cfgID, projectID string, nowMS int64) error {
	if _, err := d.db.ExecContext(ctx, `INSERT INTO config_snapshots
		(id, config_hash, schema_version, canonical_json, source_present, source_mtime_ms, loaded_at_ms, binary_version)
		VALUES (?, ?, 1, '{}', 1, NULL, ?, 'test-binary')`, cfgID, "hash-"+cfgID, nowMS); err != nil {
		return fmt.Errorf("storage: seed config snapshot: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO projects
		(id, config_snapshot_id, forge_kind, forge_host, forge_project_key, repo_path,
		 enabled, health, isolation_reason, capabilities_json, capabilities_checked_at_ms,
		 created_at_ms, updated_at_ms)
		VALUES (?, ?, 'github', 'github.com', ?, ?, 1, 'active', NULL, '{}', NULL, ?, ?)`,
		projectID, cfgID, "org/repo-"+projectID, "/repo/"+projectID, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed project: %w", err)
	}
	return nil
}

// SeedForgeRunForTest inserts a forge-sourced queued run with minimal valid
// fields.
func (d *DB) SeedForgeRunForTest(ctx context.Context, runID, projectID, cfgID, issueID string, nowMS int64) error {
	return d.SeedReverseSyncRunForTest(ctx, runID, projectID, cfgID, issueID, "", "queued", nowMS)
}

// SeedLaunchRunForTest creates the smallest fully assigned pending launch used
// by end-to-end runtime tests. It deliberately remains a test-only seed: the
// production assignment and transition ports are still the only domain writes.
func (d *DB) SeedLaunchRunForTest(ctx context.Context, runID, projectID, cfgID string, nowMS int64, worktree string) error {
	if err := d.SeedForgeRunForTest(ctx, runID, projectID, cfgID, "issue-1", nowMS); err != nil {
		return err
	}
	taskID := "task-" + runID
	if _, err := d.db.ExecContext(ctx, `INSERT INTO task_spec_snapshots
		(id, run_id, version, schema_version, canonical_json, content_digest, created_at_ms)
		VALUES (?, ?, 1, 1, '{"title":"crash-suite"}', 'task-digest', ?)
	`, taskID, runID, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch task: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE runs SET kind='bug', agent_id='agent', current_task_spec_id=?, version=2, updated_at_ms=? WHERE id=?`, taskID, nowMS, runID); err != nil {
		return fmt.Errorf("storage: seed launch assignment: %w", err)
	}
	key := LaunchOperationKey(runID, 1, 1)
	if _, err := d.db.ExecContext(ctx, `INSERT INTO attempts
		(run_id, attempt_no, phase, generation, backend, agent_id, task_spec_snapshot_id,
		 worktree_path, branch_name, base_ref, base_sha, isolation_state, created_at_ms, updated_at_ms)
		VALUES (?, 1, 'pending', 1, 'process', 'agent', ?, ?, 'main', 'main', 'base', 'none', ?, ?)`, runID, taskID, worktree, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch attempt: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO attempt_claims
		(run_id, attempt_no, generation, launch_operation_key, created_at_ms, updated_at_ms)
		VALUES (?, 1, 1, ?, ?, ?)`, runID, key, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch claim: %w", err)
	}
	if _, err := d.EnqueueOperation(ctx, Operation{Key: key, Kind: OperationLaunchAgent, RunID: runID, AttemptNo: intPtr(1), Payload: []byte(`{"schema_version":1}`)}, nowMS); err != nil {
		return fmt.Errorf("storage: seed launch operation: %w", err)
	}
	return nil
}

// SeedAttachRunForTest creates a fully dispatched active attempt for attach
// observer tests. It is test-only; production dispatch still flows through the
// launch worker's claim and prepare ports.
func (d *DB) SeedAttachRunForTest(ctx context.Context, runID, projectID, cfgID, backend string, nowMS int64, worktree string) error {
	if backend != "process" && backend != "tmux" {
		return fmt.Errorf("storage: invalid attach test backend %q", backend)
	}
	if err := d.SeedLaunchRunForTest(ctx, runID, projectID, cfgID, nowMS, worktree); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE attempts SET backend=? WHERE run_id=? AND attempt_no=1`, backend, runID); err != nil {
		return fmt.Errorf("storage: seed attach backend: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE attempt_claims
		SET dispatch_id='dispatch-' || ?, bootstrap_nonce_hash=x'01', run_token_hash=x'02'
		WHERE run_id=? AND attempt_no=1`, runID, runID); err != nil {
		return fmt.Errorf("storage: seed attach dispatch: %w", err)
	}
	return nil
}

// SeedGateCandidateForTest creates the persisted Change and attempt identity
// required by the production Gate reconciler.
func (d *DB) SeedGateCandidateForTest(ctx context.Context, runID, projectID, cfgID, changeID string, nowMS int64) error {
	if err := d.SeedLaunchRunForTest(ctx, runID, projectID, cfgID, nowMS, "/work"); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE runs SET kind='feature', change_id=?, version=1 WHERE id=?`, changeID, runID); err != nil {
		return fmt.Errorf("storage: seed Gate run: %w", err)
	}
	return nil
}

// SeedFailedAttemptForTest gives cross-package tests the failed attempt binding
// required by failure_review's new_attempt arm.
func (d *DB) SeedFailedAttemptForTest(ctx context.Context, runID string, attemptNo int, nowMS int64) error {
	_, err := d.db.ExecContext(ctx, `UPDATE attempts SET phase='finished',result_exit_code=1,result_digest='failed',result_observed_at_ms=?,finished_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=?`, nowMS, nowMS, nowMS, runID, attemptNo)
	return err
}

// SetRunChangeHeadForTest completes the immutable Change identity used by Gate fixtures.
func (d *DB) SetRunChangeHeadForTest(ctx context.Context, runID, changeID, headSHA string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE runs SET change_id=?, change_head_sha=? WHERE id=?`, changeID, headSHA, runID)
	return err
}

// SeedCertificationForTest installs a current certified projection for Gate tests.
func (d *DB) SeedCertificationForTest(ctx context.Context, kind, version string, nowMS int64) error {
	if _, err := d.db.ExecContext(ctx, `INSERT INTO certifications
		(task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms)
		VALUES (?,?,0,0,0,0,1,'test',?,?,0,?)`, kind, version, nowMS, version, nowMS); err != nil {
		return fmt.Errorf("storage: seed certification: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO certification_current(task_kind,certification_version,version,updated_at_ms) VALUES(?,?,1,?)`, kind, version, nowMS); err != nil {
		return fmt.Errorf("storage: seed current certification: %w", err)
	}
	return nil
}

// SeedReverseSyncRunForTest inserts an active forge run with the remote
// identity needed by the reverse-sync integration tests.
func (d *DB) SeedReverseSyncRunForTest(ctx context.Context, runID, projectID, cfgID, issueID, changeID, status string, nowMS int64) error {
	if _, err := d.db.ExecContext(ctx, `INSERT INTO runs
		(id, source_kind, project_id, config_snapshot_id, forge_kind, forge_host, forge_project_key,
		 issue_id, change_id, status, max_attempts, created_at_ms, updated_at_ms)
		VALUES (?, 'forge', ?, ?, 'github', 'github.com', ?, ?, NULLIF(?, ''), ?, 3, ?, ?)`,
		runID, projectID, cfgID, "org/repo-"+projectID, issueID, changeID, status, nowMS, nowMS); err != nil {
		return fmt.Errorf("storage: seed reverse-sync run: %w", err)
	}
	return nil
}
