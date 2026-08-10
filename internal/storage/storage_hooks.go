package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

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

type HookBaseline struct {
	ProjectID string
	Digest    string
}

// HookBaselineSnapshot is the immutable observation supplied by the
// application layer. Storage persists it but never performs filesystem IO.
type HookBaselineSnapshot struct {
	GitConfigDigest      string
	CoreHooksPathValue   *string
	EffectiveHooksPath   string
	HooksDirectoryDigest string
	Digest               string
}

type RecordHookBaselineCmd struct {
	ProjectID       string
	Snapshot        HookBaselineSnapshot
	ExpectedDigest  string
	SourceRunID     string
	SourceAttemptNo *int
	// TrustedBootstrap is set only by the authenticated operator bootstrap
	// path. It permits a legacy project with terminal history to establish its
	// first baseline; normal activation must never adopt that observation.
	TrustedBootstrap bool
	CapturedAtMS     int64
}

type HookRecheckReceipt struct {
	RunID     string
	AttemptNo int
	ProjectID string
}

// PendingHookRechecks returns terminal attempts whose audit receipt survived
// completion but has not yet been inspected.
func (d *DB) PendingHookRechecks(ctx context.Context) ([]HookRecheckReceipt, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT run_id,attempt_no,project_id FROM hook_recheck_receipts WHERE state='pending' ORDER BY created_at_ms,run_id,attempt_no`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookRecheckReceipt
	for rows.Next() {
		var receipt HookRecheckReceipt
		if err := rows.Scan(&receipt.RunID, &receipt.AttemptNo, &receipt.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, receipt)
	}
	return out, rows.Err()
}

// CompleteHookRecheck makes an audit result durable. It is idempotent so a
// replay after an already-recorded diagnostic never creates a second result.
func (d *DB) CompleteHookRecheck(ctx context.Context, runID string, attemptNo int, nowMS int64) error {
	if runID == "" || attemptNo < 1 || nowMS <= 0 {
		return errors.New("storage: invalid hook recheck receipt")
	}
	_, err := d.db.ExecContext(ctx, `UPDATE hook_recheck_receipts SET state='completed',completed_at_ms=COALESCE(completed_at_ms,?) WHERE run_id=? AND attempt_no=?`, nowMS, runID, attemptNo)
	return err
}

// RecordHookDiagnostic is the audit-only failure path for capture/persist
// observations. Its stable receipt key makes crash replay byte-stable.
func (d *DB) RecordHookDiagnostic(ctx context.Context, projectID, runID string, attemptNo int, kind, detail string, nowMS int64) error {
	if projectID == "" || kind == "" || nowMS <= 0 || (runID == "") != (attemptNo == 0) {
		return errors.New("storage: invalid hook diagnostic")
	}
	payload, _ := json.Marshal(map[string]any{"project_id": projectID, "run_id": runID, "attempt_no": attemptNo, "detail": detail})
	var attempt any
	if attemptNo != 0 {
		attempt = attemptNo
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO events(id,project_id,run_id,attempt_no,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms)
		VALUES(?,?,?,?,?,'system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), projectID, nullable(runID), attempt, kind, string(payload), "hook-diagnostic:"+kind+":"+projectID+":"+runID+":"+strconv.Itoa(attemptNo), nowMS, nowMS)
	return err
}

// HookProjectForRun resolves the immutable project/repository identity used
// by completion-time hook rechecks.
func (d *DB) HookProjectForRun(ctx context.Context, runID string) (string, string, error) {
	var projectID, repo string
	err := d.db.QueryRowContext(ctx, `SELECT r.project_id,p.repo_path FROM runs r JOIN projects p ON p.id=r.project_id WHERE r.id=?`, runID).Scan(&projectID, &repo)
	return projectID, repo, err
}

// RecordHookBaseline installs the trusted initial baseline, or rechecks the
// current baseline. A changed observation is recorded as an audit event and
// deliberately does not replace the trusted baseline.
func (d *DB) RecordHookBaseline(ctx context.Context, cmd RecordHookBaselineCmd) error {
	if cmd.ProjectID == "" || cmd.Snapshot.Digest == "" || cmd.Snapshot.GitConfigDigest == "" || cmd.Snapshot.EffectiveHooksPath == "" || cmd.Snapshot.HooksDirectoryDigest == "" || cmd.CapturedAtMS <= 0 {
		return errors.New("storage: invalid hook baseline")
	}
	if (cmd.SourceRunID == "") != (cmd.SourceAttemptNo == nil) {
		return errors.New("storage: hook baseline source must be paired")
	}
	if cmd.TrustedBootstrap && cmd.SourceRunID != "" {
		return errors.New("storage: trusted hook bootstrap cannot have an attempt source")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT baseline_digest FROM project_hook_baselines WHERE project_id=?`, cmd.ProjectID).Scan(&old)
	if err == sql.ErrNoRows {
		exists = false
	} else if err != nil {
		return err
	} else {
		exists = true
	}
	if cmd.ExpectedDigest != "" && ((!exists) || old != cmd.ExpectedDigest) {
		return ErrRejectedStale
	}
	if !exists && cmd.SourceRunID != "" {
		payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "observed_digest": cmd.Snapshot.Digest, "source_run_id": cmd.SourceRunID, "source_attempt_no": cmd.SourceAttemptNo})
		_, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_baseline_missing','system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-baseline-missing:"+cmd.ProjectID, cmd.CapturedAtMS, cmd.CapturedAtMS)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if !exists {
		var completed int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts a JOIN runs r ON r.id=a.run_id WHERE r.project_id=? AND a.phase <> 'pending'`, cmd.ProjectID).Scan(&completed); err != nil {
			return err
		}
		if completed != 0 && !cmd.TrustedBootstrap {
			payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "observed_digest": cmd.Snapshot.Digest})
			_, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_baseline_activation_missing','system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-baseline-activation-missing:"+cmd.ProjectID, cmd.CapturedAtMS, cmd.CapturedAtMS)
			if err != nil {
				return err
			}
			return tx.Commit()
		}
		if completed != 0 {
			payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "baseline_digest": cmd.Snapshot.Digest})
			if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_baseline_bootstrapped','operator',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-baseline-bootstrap:"+cmd.ProjectID, cmd.CapturedAtMS, cmd.CapturedAtMS); err != nil {
				return err
			}
		}
	}
	if exists {
		if old != cmd.Snapshot.Digest {
			payload, _ := json.Marshal(map[string]any{"project_id": cmd.ProjectID, "baseline_digest": old, "observed_digest": cmd.Snapshot.Digest, "git_config_digest": cmd.Snapshot.GitConfigDigest, "core_hooks_path": cmd.Snapshot.CoreHooksPathValue, "effective_hooks_path": cmd.Snapshot.EffectiveHooksPath, "hooks_directory_digest": cmd.Snapshot.HooksDirectoryDigest, "source_run_id": cmd.SourceRunID, "source_attempt_no": cmd.SourceAttemptNo})
			_, err = tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'hooks_drift_detected','system',1,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, newID(), cmd.ProjectID, string(payload), "hooks-drift:"+cmd.ProjectID+":"+cmd.Snapshot.Digest, cmd.CapturedAtMS, cmd.CapturedAtMS)
			if err != nil {
				return err
			}
			return tx.Commit()
		}
		_, err = tx.ExecContext(ctx, `UPDATE project_hook_baselines SET updated_at_ms=? WHERE project_id=?`, cmd.CapturedAtMS, cmd.ProjectID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_hook_baselines(project_id,git_config_digest,core_hooks_path_value,effective_hooks_path,hooks_directory_digest,baseline_digest,source_run_id,source_attempt_no,captured_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?)`, cmd.ProjectID, cmd.Snapshot.GitConfigDigest, cmd.Snapshot.CoreHooksPathValue, cmd.Snapshot.EffectiveHooksPath, cmd.Snapshot.HooksDirectoryDigest, cmd.Snapshot.Digest, nullable(cmd.SourceRunID), cmd.SourceAttemptNo, cmd.CapturedAtMS, cmd.CapturedAtMS)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type DoctorAttempt struct {
	RunID          string
	AttemptNo      int
	Phase          string
	IsolationState string
	WorktreePath   string
	AgentID        string
}

type DoctorOutbox struct {
	Pending int
	Failed  []DoctorOutboxFailure
}

type DoctorOutboxFailure struct {
	ID           string
	Kind         string
	AttemptCount int
	ErrorClass   string
	ErrorSummary string
}

type DoctorDaemonVersion struct {
	BinaryVersion string
	ProtocolMajor int
}

// ReadDoctorState reads only diagnostic projections from an existing database.
// It never creates, migrates, or mutates the database.
func ReadDoctorState(ctx context.Context, path string) ([]HookBaseline, []DoctorAttempt, error) {
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, nil, fmt.Errorf("storage: open doctor database: %w", err)
	}
	defer pool.Close()
	hooks := []HookBaseline{}
	rows, err := pool.QueryContext(ctx, `SELECT project_id,baseline_digest FROM project_hook_baselines`)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read hook baselines: %w", err)
	}
	for rows.Next() {
		var h HookBaseline
		if err := rows.Scan(&h.ProjectID, &h.Digest); err != nil {
			rows.Close()
			return nil, nil, err
		}
		hooks = append(hooks, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	attempts := []DoctorAttempt{}
	rows, err = pool.QueryContext(ctx, `SELECT run_id,attempt_no,phase,isolation_state,worktree_path,agent_id FROM attempts WHERE isolation_state='frozen' OR phase NOT IN ('finished','orphaned') ORDER BY run_id,attempt_no`)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read attempt state: %w", err)
	}
	for rows.Next() {
		var a DoctorAttempt
		if err := rows.Scan(&a.RunID, &a.AttemptNo, &a.Phase, &a.IsolationState, &a.WorktreePath, &a.AgentID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		attempts = append(attempts, a)
	}
	return hooks, attempts, rows.Err()
}

// ReadDoctorOutbox reads pending delivery pressure and terminal delivery
// failures without opening a write connection.
func ReadDoctorOutbox(ctx context.Context, path string) (DoctorOutbox, error) {
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return DoctorOutbox{}, fmt.Errorf("storage: open doctor database: %w", err)
	}
	defer pool.Close()
	var result DoctorOutbox
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_operations WHERE state IN ('pending','executing','retryable')`).Scan(&result.Pending); err != nil {
		return DoctorOutbox{}, fmt.Errorf("storage: read outbox backlog: %w", err)
	}
	rows, err := pool.QueryContext(ctx, `SELECT id,kind,attempt_count,COALESCE(last_error_class,''),COALESCE(last_error_summary,'') FROM outbox_operations WHERE state='failed' ORDER BY updated_at_ms,id`)
	if err != nil {
		return DoctorOutbox{}, fmt.Errorf("storage: read outbox failures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var failure DoctorOutboxFailure
		if err := rows.Scan(&failure.ID, &failure.Kind, &failure.AttemptCount, &failure.ErrorClass, &failure.ErrorSummary); err != nil {
			return DoctorOutbox{}, err
		}
		result.Failed = append(result.Failed, failure)
	}
	return result, rows.Err()
}

// ReadDoctorDaemonVersion returns the active daemon's version handshake when
// one is recorded. A stopped historical boot is deliberately ignored.
func ReadDoctorDaemonVersion(ctx context.Context, path string) (DoctorDaemonVersion, bool, error) {
	pool, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return DoctorDaemonVersion{}, false, fmt.Errorf("storage: open doctor database: %w", err)
	}
	defer pool.Close()
	var version DoctorDaemonVersion
	err = pool.QueryRowContext(ctx, `SELECT binary_version,protocol_major FROM daemon_boots WHERE stopped_at_ms IS NULL ORDER BY started_at_ms DESC,id DESC LIMIT 1`).Scan(&version.BinaryVersion, &version.ProtocolMajor)
	if errors.Is(err, sql.ErrNoRows) {
		return DoctorDaemonVersion{}, false, nil
	}
	if err != nil {
		return DoctorDaemonVersion{}, false, fmt.Errorf("storage: read active daemon version: %w", err)
	}
	return version, true, nil
}

// Migration refusal sentinels (specs/storage.md §3.1). Callers match with
// errors.Is; the wrapped message carries the versions involved.
