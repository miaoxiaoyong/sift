package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (d *DB) EnqueueOperation(ctx context.Context, op Operation, nowMS int64) (Operation, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	if err := insertOperation(ctx, tx, op, op.RunID, "", nowMS); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	d.wakeOutbox()
	return op, nil
}
func insertOperation(ctx context.Context, tx *sql.Tx, op Operation, runID, _ string, nowMS int64) error {
	if op.ID == "" {
		op.ID = newID()
	}
	if op.RunID == "" {
		op.RunID = runID
	}
	if !validOperationKind(op.Kind) || op.Key == "" || len(op.Payload) == 0 || !json.Valid(op.Payload) {
		return errors.New("storage: invalid outbox operation")
	}
	if op.NextAttemptAtMS == 0 {
		op.NextAttemptAtMS = nowMS
	}
	digest := digestJSON(op.Payload)
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT payload_digest FROM outbox_operations WHERE operation_key=?`, op.Key).Scan(&existing)
	if err == nil {
		if existing != digest {
			return ErrOperationConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox_operations
		(id, operation_key, kind, run_id, attempt_no, interrupt_id, state, payload_schema_version, payload_json, payload_digest, next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', 1, ?, ?, ?, ?, ?)`, op.ID, op.Key, op.Kind, nullable(op.RunID), op.AttemptNo, nullable(op.InterruptID), string(op.Payload), digest, op.NextAttemptAtMS, nowMS, nowMS)
	return err
}

// ClaimOutboxOperation atomically leases one due operation. Expired leases are
// reclaimed in the same transaction, with an immutable lease_expired result
// written for the old attempt first.
func (d *DB) ClaimOutboxOperation(ctx context.Context, workerID string, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	// launch_agent has a stricter boot recovery barrier and must use
	// ClaimLaunchOperation instead.
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, "", "", "", "")
}

// ClaimLaunchOperation leases a launch only after recovery for bootID has
// completed. The barrier check and lease CAS share one transaction, so an
// expired launch lease cannot be reclaimed during recovery.
func (d *DB) ClaimLaunchOperation(ctx context.Context, bootID, workerID string, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	if bootID == "" {
		return nil, errors.New("storage: boot id is required for launch claim")
	}
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, OperationLaunchAgent, "", "", bootID)
}

// ClaimOutboxOperationKind leases only operations consumed by one worker kind.
// A worker must never claim another worker's operation and turn it into a
// contract failure.
func (d *DB) ClaimOutboxOperationKind(ctx context.Context, workerID string, kind OperationKind, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	return d.ClaimOutboxOperationKindProject(ctx, workerID, kind, "", nowMS, leaseMS)
}

// ClaimOutboxOperationKindPurpose prevents a specialized consumer from
// claiming another producer's payload within the same outbox kind.
func (d *DB) ClaimOutboxOperationKindPurpose(ctx context.Context, workerID string, kind OperationKind, purpose string, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	if purpose == "" {
		return nil, errors.New("storage: alert purpose is required")
	}
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, kind, "", purpose, "")
}

// ClaimOutboxOperationKindProject limits a worker to the project encoded in
// the operation payload, keeping per-project Forge adapters isolated.
func (d *DB) ClaimOutboxOperationKindProject(ctx context.Context, workerID string, kind OperationKind, projectID string, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	if !validOperationKind(kind) {
		return nil, errors.New("storage: invalid outbox operation kind")
	}
	if kind == OperationLaunchAgent {
		return nil, errors.New("storage: use ClaimLaunchOperation for launch_agent")
	}
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, kind, projectID, "", "")
}

func (d *DB) claimOutboxOperation(ctx context.Context, workerID string, nowMS, leaseMS int64, filterKind OperationKind, projectID, purpose, bootID string) (*ClaimedOperation, error) {
	if workerID == "" || leaseMS <= 0 {
		return nil, errors.New("storage: worker id and positive lease required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if bootID != "" {
		var completed sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT recovery_completed_at_ms FROM daemon_boots WHERE id=?`, bootID).Scan(&completed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrRejectedStaleWorker
			}
			return nil, err
		}
		if !completed.Valid {
			return nil, nil
		}
	}
	query := `SELECT id, operation_key, kind, payload_json, run_id, attempt_no, interrupt_id, attempt_count, state
		FROM outbox_operations WHERE ((state IN ('pending','retryable') AND next_attempt_at_ms <= ?)
		OR (state='executing' AND lease_expires_at_ms <= ?))`
	args := []any{nowMS, nowMS}
	if bootID == "" && filterKind == "" {
		query += ` AND kind <> 'launch_agent'`
	}
	if filterKind != "" {
		query += ` AND kind=?`
		args = append(args, filterKind)
	}
	if filterKind == OperationLaunchAgent {
		// A pre-baseline project with Agent history is an upgrade boundary: do
		// not let an Agent run until an authenticated operator has explicitly
		// bootstrapped the current hooks observation. A fresh pending attempt
		// remains audit-only, including capture failures.
		query += ` AND (NOT EXISTS (SELECT 1 FROM attempts historical JOIN runs historical_run ON historical_run.id=historical.run_id WHERE historical_run.project_id=(SELECT project_id FROM runs WHERE id=outbox_operations.run_id) AND historical.phase <> 'pending') OR EXISTS (SELECT 1 FROM project_hook_baselines baseline JOIN runs run ON run.project_id=baseline.project_id WHERE run.id=outbox_operations.run_id))`
	}
	if projectID != "" {
		query += ` AND json_extract(payload_json, '$.project_id')=?`
		args = append(args, projectID)
	}
	if purpose != "" {
		query += ` AND json_extract(payload_json, '$.purpose')=?`
		args = append(args, purpose)
	}
	// Rotate across Run identities before considering another operation from a
	// hot Run. Operations without a Run are their own fairness identity.
	query += ` ORDER BY COALESCE((SELECT MAX(a.rowid) FROM outbox_attempts a JOIN outbox_operations prior ON prior.id=a.operation_id WHERE (outbox_operations.run_id IS NOT NULL AND prior.run_id=outbox_operations.run_id) OR (outbox_operations.run_id IS NULL AND prior.id=outbox_operations.id)),0),next_attempt_at_ms,id LIMIT 1`
	row := tx.QueryRowContext(ctx, query, args...)
	var c ClaimedOperation
	var kind, payload, state string
	var run, interrupt sql.NullString
	var attemptNo sql.NullInt64
	var oldCount int
	if err := row.Scan(&c.ID, &c.Key, &kind, &payload, &run, &attemptNo, &interrupt, &oldCount, &state); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	c.Kind, c.Payload, c.RunID, c.InterruptID = OperationKind(kind), json.RawMessage(payload), run.String, interrupt.String
	if attemptNo.Valid {
		n := int(attemptNo.Int64)
		c.AttemptNo = &n
	}
	terminalReclaim := false
	reclaimed := state == string(OperationExecuting)
	if state == string(OperationExecuting) {
		var oldAttempt string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM outbox_attempts WHERE operation_id=? AND attempt_no=?`, c.ID, oldCount).Scan(&oldAttempt); err != nil {
			return nil, err
		}
		if c.Kind == OperationRerunChecks {
			started, err := rerunRequestStartedTx(ctx, tx, oldAttempt)
			if err != nil {
				return nil, err
			}
			if started {
				if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_results (attempt_id,finished_at_ms,outcome,error_class,error_summary) VALUES (?,?,'conflict','semantic_conflict','request_started_lease_expired') ON CONFLICT(attempt_id) DO NOTHING`, oldAttempt, nowMS); err != nil {
					return nil, err
				}
				c.AttemptID, c.ClaimAttemptNo = oldAttempt, oldCount
				if err := d.applyRerunChecksConflictTx(ctx, tx, c, nowMS); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='conflict',lease_owner=NULL,lease_expires_at_ms=NULL,last_error_class='semantic_conflict',last_error_summary='request_started_lease_expired',completed_at_ms=?,updated_at_ms=?,version=version+1 WHERE id=? AND state='executing'`, nowMS, nowMS, c.ID); err != nil {
					return nil, err
				}
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				d.wakeOutbox()
				return nil, nil
			}
		}
		alertAfter, maxAttempts := d.channelPolicy()
		reclaim := CompleteOutcome{State: OperationRetryable, ErrorClass: ErrorTransient, ErrorSummary: "lease_expired", NowMS: nowMS, ChannelFailureAlertAfter: alertAfter, MaxAttempts: maxAttempts}
		// Channel's terminal projection is failed, but the expired attempt is
		// immutable evidence of a retryable transient lease expiry.
		if (c.Kind == OperationChannelPublish || c.Kind == OperationRerunChecks) && maxAttempts > 0 && oldCount >= maxAttempts {
			reclaim.State = OperationFailed
			terminalReclaim = true
		}
		result := "retry"
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_results (attempt_id, finished_at_ms, outcome, error_class, error_summary) VALUES (?, ?, ?, 'transient', 'lease_expired')`, oldAttempt, nowMS, result); err != nil {
			return nil, err
		}
		if c.Kind == OperationChannelPublish {
			if err := applyChannelOutcomeTx(ctx, tx, c, reclaim, true); err != nil {
				return nil, err
			}
		}
		if terminalReclaim {
			if _, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='failed',lease_owner=NULL,lease_expires_at_ms=NULL,completed_at_ms=?,updated_at_ms=? WHERE id=? AND state='executing'`, nowMS, nowMS, c.ID); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			d.wakeOutbox()
			return nil, nil
		}
	}
	c.ClaimAttemptNo, c.AttemptID, c.LeaseOwner, c.LeaseExpiresAtMS = oldCount+1, newID(), workerID, nowMS+leaseMS
	res, err := tx.ExecContext(ctx, `UPDATE outbox_operations SET state='executing', lease_owner=?, lease_expires_at_ms=?, attempt_count=?, version=version+1, updated_at_ms=?
		WHERE id=? AND ((state IN ('pending','retryable') AND next_attempt_at_ms <= ?) OR (state='executing' AND lease_expires_at_ms <= ?))`, workerID, c.LeaseExpiresAtMS, c.ClaimAttemptNo, nowMS, c.ID, nowMS, nowMS)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, ErrRejectedStaleWorker
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempts (id, operation_id, attempt_no, worker_id, started_at_ms) VALUES (?, ?, ?, ?, ?)`, c.AttemptID, c.ID, c.ClaimAttemptNo, workerID, nowMS); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Reclaim can cross the Channel failure threshold and create a
	// forge_alert while also leasing the next retryable attempt. Wake after
	// commit so the alert consumer observes the durable row.
	if reclaimed && c.Kind == OperationChannelPublish {
		d.wakeOutbox()
	}
	return &c, nil
}

// MarkOutboxAttemptRequestStarted commits the rerun_checks at-most-once
// boundary before any remote request. The current lease and immutable attempt
// must still match; replay of the same attempt is idempotent.
func (d *DB) MarkOutboxAttemptRequestStarted(ctx context.Context, claim ClaimedOperation, nowMS int64) error {
	if claim.Kind != OperationRerunChecks || claim.ID == "" || claim.AttemptID == "" || claim.LeaseOwner == "" || nowMS <= 0 {
		return errors.New("storage: invalid rerun checks request start")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM outbox_operations o JOIN outbox_attempts a ON a.operation_id=o.id
		WHERE o.id=? AND o.kind='rerun_checks' AND o.state='executing' AND o.lease_owner=? AND o.lease_expires_at_ms=? AND o.lease_expires_at_ms>=? AND a.id=?`,
		claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS, nowMS, claim.AttemptID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRejectedStaleWorker
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_request_starts (attempt_id,started_at_ms) VALUES (?,?) ON CONFLICT(attempt_id) DO NOTHING`, claim.AttemptID, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

func rerunRequestStartedTx(ctx context.Context, tx *sql.Tx, attemptID string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM outbox_attempt_request_starts WHERE attempt_id=?`, attemptID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type rerunChecksPayload struct {
	RunID              string `json:"run_id"`
	ChangeID           string `json:"change_id"`
	HeadSHA            string `json:"head_sha"`
	CheckRunID         string `json:"check_run_id"`
	RetryNo            int    `json:"retry_no"`
	TriageSourceDigest string `json:"triage_source_digest"`
	CreatedFromEventID string `json:"created_from_event_id"`
}

// applyRerunChecksConflictTx creates the unique failure_review successor in
// the same transaction that makes an ambiguous rerun terminal. Its calibration
// is tied to the Gate evaluation which created the rerun operation.
func (d *DB) applyRerunChecksConflictTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, nowMS int64) error {
	var p rerunChecksPayload
	if err := json.Unmarshal(claim.Payload, &p); err != nil || p.RunID == "" || p.ChangeID == "" || p.HeadSHA == "" || p.CheckRunID == "" || p.RetryNo < 1 || p.CreatedFromEventID == "" {
		return errors.New("storage: invalid rerun checks conflict payload")
	}
	var eventPayload string
	if err := tx.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE id=? AND run_id=?`, p.CreatedFromEventID, p.RunID).Scan(&eventPayload); err != nil {
		return fmt.Errorf("storage: rerun checks source event: %w", err)
	}
	var source struct {
		GateEvaluationID string `json:"gate_evaluation_id"`
	}
	if err := json.Unmarshal([]byte(eventPayload), &source); err != nil || source.GateEvaluationID == "" {
		return errors.New("storage: rerun checks source evaluation missing")
	}
	var predicted, features string
	if err := tx.QueryRowContext(ctx, `SELECT predicted_decision,features_json FROM calibration_entries WHERE gate_evaluation_id=? ORDER BY predicted_at_ms,id LIMIT 1`, source.GateEvaluationID).Scan(&predicted, &features); err != nil {
		return fmt.Errorf("storage: rerun checks calibration: %w", err)
	}
	calibrationID := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO calibration_entries (id,run_id,gate_evaluation_id,predicted_decision,features_json,gate_sample_entry_id,predicted_at_ms) VALUES (?,?,?,?,?,NULL,?)`, calibrationID, p.RunID, source.GateEvaluationID, predicted, features, nowMS); err != nil {
		return err
	}
	var runVersion int64
	var changeID, headSHA string
	if err := tx.QueryRowContext(ctx, `SELECT version,change_id,change_head_sha FROM runs WHERE id=?`, p.RunID).Scan(&runVersion, &changeID, &headSHA); err != nil {
		return err
	}
	if changeID != p.ChangeID || headSHA != p.HeadSHA {
		return errors.New("storage: rerun checks conflict identity drift")
	}
	var attemptNo, generation int
	if err := tx.QueryRowContext(ctx, `SELECT attempt_no,generation FROM attempts WHERE run_id=? ORDER BY attempt_no DESC LIMIT 1`, p.RunID).Scan(&attemptNo, &generation); err != nil {
		return err
	}
	facts := map[string]string{
		"failure_class":        "checks_rerun_ambiguous",
		"failure_evidence_ref": "sift://event/" + p.CreatedFromEventID,
		"recommended_action":   "retry",
	}
	factsJSON, err := canonicalJSON(facts)
	if err != nil {
		return err
	}
	cfg := d.gateReEvalInterruptEmission()
	if cfg.AttentionDailyQuota == nil {
		return errors.New("storage: rerun checks interrupt emission not configured")
	}
	cmd := EmitInterruptCmd{
		RunID: p.RunID, ExpectedRunVersion: runVersion, AttemptNo: &attemptNo,
		Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewAttempt,
		FailureReviewRetryKind: FailureReviewGateRecheck, Facts: facts,
		Generation: InterruptGeneration{AttemptNo: attemptNo, Generation: generation, ChangeID: p.ChangeID, HeadSHA: p.HeadSHA, FailureDigest: sha256Hex(factsJSON)},
		GatePhase:  GateReview, GuardrailLevel: GuardrailNone,
		AttentionDailyQuota: cfg.AttentionDailyQuota, DayTimezone: cfg.DayTimezone,
		DailySummaryAt: cfg.DailySummaryAt, MaxEscalations: cfg.MaxEscalations,
		CriticalWindowMS: cfg.CriticalWindowMS, CriticalTotalLimit: cfg.CriticalTotalLimit,
		CriticalPerRunLimit: cfg.CriticalPerRunLimit, Channels: cfg.Channels,
		CalibrationID: calibrationID, Source: SourceSystem, NowMS: nowMS,
	}
	_, err = d.emitInterruptInExistingTx(ctx, tx, cmd, false)
	return err
}

func (d *DB) CompleteOutboxAttempt(ctx context.Context, claim ClaimedOperation, outcome CompleteOutcome) error {
	if outcome.NowMS <= 0 || !terminalOrRetry(outcome.State) {
		return errors.New("storage: invalid outbox completion")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM outbox_operations WHERE id=? AND state='executing' AND lease_owner=? AND lease_expires_at_ms=?`, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRejectedStaleWorker
		}
		return err
	}
	rerunStarted := false
	if claim.Kind == OperationRerunChecks {
		rerunStarted, err = rerunRequestStartedTx(ctx, tx, claim.AttemptID)
		if err != nil {
			return err
		}
		if rerunStarted && outcome.State != OperationSucceeded && outcome.State != OperationConflict {
			return errors.New("storage: started rerun checks attempt must complete success or conflict")
		}
		if !rerunStarted && outcome.State == OperationSucceeded {
			return errors.New("storage: rerun checks success requires request start")
		}
	}
	// A retryable completion at the frozen attempt limit is terminal. Decide
	// this before writing its immutable result and Channel projections so they
	// all describe the same outcome.
	if claim.Kind == OperationChannelPublish && outcome.State == OperationRetryable && outcome.MaxAttempts > 0 && claim.ClaimAttemptNo >= outcome.MaxAttempts {
		outcome.State = OperationFailed
	}
	result := map[OperationState]string{OperationSucceeded: "success", OperationRetryable: "retry", OperationFailed: "failed", OperationStale: "stale", OperationConflict: "conflict"}[outcome.State]
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_results (attempt_id, finished_at_ms, outcome, error_class, error_summary, evidence_digest) VALUES (?, ?, ?, ?, ?, ?)`, claim.AttemptID, outcome.NowMS, result, nullable(string(outcome.ErrorClass)), nullable(outcome.ErrorSummary), nullable(digestJSON(outcome.Evidence))); err != nil {
		return err
	}
	next := outcome.NowMS
	if outcome.State == OperationRetryable {
		next += outcome.Backoff.DelayMS(claim.ClaimAttemptNo)
		if outcome.RetryAfterMS > next-outcome.NowMS {
			next = outcome.NowMS + outcome.RetryAfterMS
		}
	}
	completed := any(nil)
	if outcome.State != OperationRetryable {
		completed = outcome.NowMS
	}
	if claim.Kind == OperationChannelPublish {
		if err := applyChannelOutcomeTx(ctx, tx, claim, outcome, false); err != nil {
			return err
		}
	}
	if claim.Kind == OperationRerunChecks && rerunStarted && outcome.State == OperationConflict {
		if err := d.applyRerunChecksConflictTx(ctx, tx, claim, outcome.NowMS); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE outbox_operations SET state=?, lease_owner=NULL, lease_expires_at_ms=NULL, next_attempt_at_ms=?, remote_evidence_json=?, remote_evidence_digest=?, last_error_class=?, last_error_summary=?, version=version+1, updated_at_ms=?, completed_at_ms=? WHERE id=? AND lease_owner=? AND lease_expires_at_ms=?`, outcome.State, next, nullable(string(outcome.Evidence)), nullable(digestJSON(outcome.Evidence)), nullable(string(outcome.ErrorClass)), nullable(outcome.ErrorSummary), outcome.NowMS, completed, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// A Channel completion may have atomically created its threshold alert.
	// Wake the dedicated consumer only after that transaction is durable.
	if claim.Kind == OperationChannelPublish {
		d.wakeOutbox()
	}
	return nil
}
