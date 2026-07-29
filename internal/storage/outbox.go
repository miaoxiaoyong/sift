package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

type OperationKind string

const (
	OperationForgeComment   OperationKind = "forge_comment"
	OperationForgeLabels    OperationKind = "forge_labels"
	OperationCreateChange   OperationKind = "create_change"
	OperationMergeChange    OperationKind = "merge_change"
	OperationChannelPublish OperationKind = "channel_publish"
	OperationLaunchAgent    OperationKind = "launch_agent"
	OperationCommandAck     OperationKind = "command_ack"
	OperationForgeAlert     OperationKind = "forge_alert"
)

type OperationState string

const (
	OperationPending   OperationState = "pending"
	OperationExecuting OperationState = "executing"
	OperationRetryable OperationState = "retryable"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
	OperationStale     OperationState = "stale"
	OperationConflict  OperationState = "conflict"
)

type ErrorClass string

const (
	ErrorTransient        ErrorClass = "transient"
	ErrorRateLimited      ErrorClass = "rate_limited"
	ErrorAuthCapability   ErrorClass = "auth_or_capability"
	ErrorContract         ErrorClass = "contract_violation"
	ErrorSemanticConflict ErrorClass = "semantic_conflict"
)

type Operation struct {
	ID              string
	Key             string
	Kind            OperationKind
	Payload         json.RawMessage
	RunID           string
	AttemptNo       *int
	InterruptID     string
	NextAttemptAtMS int64
}
type ClaimedOperation struct {
	Operation
	LeaseOwner       string
	LeaseExpiresAtMS int64
	AttemptID        string
	ClaimAttemptNo   int
}
type CompleteOutcome struct {
	State        OperationState
	ErrorClass   ErrorClass
	ErrorSummary string
	Evidence     json.RawMessage
	NowMS        int64
	RetryAfterMS int64
	Backoff      BackoffPolicy
}

var ErrOperationConflict = errors.New("storage: operation key payload conflict")
var ErrRejectedStaleWorker = errors.New("storage: rejected stale outbox worker")

// Stable operation-key constructors are the sole key vocabulary from
// specs/outbox.md §2. They use only frozen identities, never wall-clock data.
func CreateChangeOperationKey(runID, headSHA string) string {
	return "run:" + runID + ":create-change:" + headSHA
}
func MergeChangeOperationKey(runID, headSHA string) string {
	return "run:" + runID + ":merge:" + headSHA
}
func LaunchOperationKey(runID string, attemptNo, generation int) string {
	return fmt.Sprintf("run:%s:attempt:%d:generation:%d:launch", runID, attemptNo, generation)
}
func ChannelPublishOperationKey(interruptID string, escalation int) string {
	return fmt.Sprintf("interrupt:%s:publish:%d", interruptID, escalation)
}
func CommentOperationKey(purpose, subjectID string, generation int) string {
	return fmt.Sprintf("comment:%s:%s:%d", purpose, subjectID, generation)
}
func CommandAckOperationKey(forgeEventID string) string { return "command:" + forgeEventID + ":ack" }
func LabelsOperationKey(subjectKind, subjectID string, version int) string {
	return fmt.Sprintf("labels:%s:%s:%d", subjectKind, subjectID, version)
}
func AlertOperationKey(kind, subjectID string, generation int) string {
	return fmt.Sprintf("alert:%s:%s:%d", kind, subjectID, generation)
}

// EnqueueOperation is for operations without a Run state transition. Stateful
// effects should normally be attached to DomainCommand.Operation instead.
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
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, "", "", "")
}

// ClaimLaunchOperation leases a launch only after recovery for bootID has
// completed. The barrier check and lease CAS share one transaction, so an
// expired launch lease cannot be reclaimed during recovery.
func (d *DB) ClaimLaunchOperation(ctx context.Context, bootID, workerID string, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	if bootID == "" {
		return nil, errors.New("storage: boot id is required for launch claim")
	}
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, OperationLaunchAgent, "", bootID)
}

// ClaimOutboxOperationKind leases only operations consumed by one worker kind.
// A worker must never claim another worker's operation and turn it into a
// contract failure.
func (d *DB) ClaimOutboxOperationKind(ctx context.Context, workerID string, kind OperationKind, nowMS, leaseMS int64) (*ClaimedOperation, error) {
	return d.ClaimOutboxOperationKindProject(ctx, workerID, kind, "", nowMS, leaseMS)
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
	return d.claimOutboxOperation(ctx, workerID, nowMS, leaseMS, kind, projectID, "")
}

func (d *DB) claimOutboxOperation(ctx context.Context, workerID string, nowMS, leaseMS int64, filterKind OperationKind, projectID, bootID string) (*ClaimedOperation, error) {
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
	if projectID != "" {
		query += ` AND json_extract(payload_json, '$.project_id')=?`
		args = append(args, projectID)
	}
	query += ` ORDER BY next_attempt_at_ms, id LIMIT 1`
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
	if state == string(OperationExecuting) {
		var oldAttempt string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM outbox_attempts WHERE operation_id=? AND attempt_no=?`, c.ID, oldCount).Scan(&oldAttempt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_attempt_results (attempt_id, finished_at_ms, outcome, error_class, error_summary) VALUES (?, ?, 'retry', 'transient', 'lease_expired')`, oldAttempt, nowMS); err != nil {
			return nil, err
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
	return &c, nil
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
	_, err = tx.ExecContext(ctx, `UPDATE outbox_operations SET state=?, lease_owner=NULL, lease_expires_at_ms=NULL, next_attempt_at_ms=?, remote_evidence_json=?, remote_evidence_digest=?, last_error_class=?, last_error_summary=?, version=version+1, updated_at_ms=?, completed_at_ms=? WHERE id=? AND lease_owner=? AND lease_expires_at_ms=?`, outcome.State, next, nullable(string(outcome.Evidence)), nullable(digestJSON(outcome.Evidence)), nullable(string(outcome.ErrorClass)), nullable(outcome.ErrorSummary), outcome.NowMS, completed, claim.ID, claim.LeaseOwner, claim.LeaseExpiresAtMS)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type BackoffPolicy struct {
	InitialDelayMS, MaxDelayMS int64
	Multiplier                 float64
}

func (p BackoffPolicy) DelayMS(attempt int) int64 {
	if p.InitialDelayMS <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	m := p.Multiplier
	if m < 1 {
		m = 1
	}
	v := int64(math.Ceil(float64(p.InitialDelayMS) * math.Pow(m, float64(attempt-1))))
	if p.MaxDelayMS > 0 && v > p.MaxDelayMS {
		return p.MaxDelayMS
	}
	return v
}
func digestJSON(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func validOperationKind(k OperationKind) bool {
	switch k {
	case OperationForgeComment, OperationForgeLabels, OperationCreateChange, OperationMergeChange, OperationChannelPublish, OperationLaunchAgent, OperationCommandAck, OperationForgeAlert:
		return true
	}
	return false
}
func terminalOrRetry(s OperationState) bool {
	return s == OperationSucceeded || s == OperationRetryable || s == OperationFailed || s == OperationStale || s == OperationConflict
}
