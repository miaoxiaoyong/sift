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

// Event is the read model of one events row. It is the authoritative timeline
// of a Run: append-only, ordered by seq, carrying the timestamps the M1
// skeleton P50 measurement (PRD §10.2 trigger→started) is computed from.
type Event struct {
	Seq          int64
	ID           string
	RunID        string
	AttemptNo    *int
	ProjectID    string
	Type         string
	Source       string
	Actor        string
	PayloadJSON  []byte
	OccurredAtMS int64
	RecordedAtMS int64
}

// RunEvents returns the events of one Run in seq order (storage.md §7.1
// append-only stream). It is the read port the M1 skeleton uses to locate the
// trigger-observed and agent-started events for the P50 computation.
func (d *DB) RunEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT seq, id, COALESCE(run_id,''), attempt_no, COALESCE(project_id,''),
		type, source, actor, payload_json, occurred_at_ms, recorded_at_ms
		FROM events WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("storage: run events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var actor sql.NullString
		var attemptNo sql.NullInt64
		if err := rows.Scan(&e.Seq, &e.ID, &e.RunID, &attemptNo, &e.ProjectID, &e.Type, &e.Source,
			&actor, &e.PayloadJSON, &e.OccurredAtMS, &e.RecordedAtMS); err != nil {
			return nil, err
		}
		if attemptNo.Valid {
			v := int(attemptNo.Int64)
			e.AttemptNo = &v
		}
		e.Actor = actor.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// FirstEventOfType returns the first event of one type for a Run, or false. The
// skeleton uses it to resolve the P50 anchors deterministically.
func (d *DB) FirstEventOfType(ctx context.Context, runID, eventType string) (Event, bool, error) {
	events, err := d.RunEvents(ctx, runID)
	if err != nil {
		return Event{}, false, err
	}
	for _, e := range events {
		if e.Type == eventType {
			return e, true, nil
		}
	}
	return Event{}, false, nil
}

// CountOperationsByKind reports how many outbox operations of one kind exist.
// The M1 skeleton test uses it to assert no create_change/merge_change
// operations were created (WBS M1 §1.6).
func (d *DB) CountOperationsByKind(ctx context.Context, kind OperationKind) (int, error) {
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_operations WHERE kind=?`, string(kind)).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count outbox by kind: %w", err)
	}
	return n, nil
}

// AppendEvent is the generic append-only event port (storage.md §7.1). The
// events table is append-only by trigger; this port lets the M1 skeleton record
// spine evidence that is not a side effect of another write port — the fake
// agent's completion evidence and the injected forge merge fact (WBS M1 §1.6).
// The stateful write ports (TransitionRun, CreateForgeRun, SetInitialTaskSpec)
// emit their own events as side effects; this port carries only standalone
// evidence events and never mutates a Run.
type EventCmd struct {
	RunID          string // optional
	AttemptNo      *int   // optional
	ProjectID      string // optional
	Type           string
	Source         EventSource
	Actor          string // optional
	PayloadJSON    []byte
	IdempotencyKey string // optional; when set, duplicate inserts are no-ops
	OccurredAtMS   int64
	RecordedAtMS   int64
}

// AppendEvent appends one evidence event. It validates the source enum and
// requires ordered timestamps; the append-only trigger (storage.md §13) makes
// UPDATE/DELETE impossible. An IdempotencyKey collision is a no-op.
func (d *DB) AppendEvent(ctx context.Context, cmd EventCmd) (string, error) {
	if !validSource(cmd.Source) {
		return "", fmt.Errorf("storage: append event source %q invalid", cmd.Source)
	}
	if cmd.Type == "" || cmd.OccurredAtMS <= 0 || cmd.RecordedAtMS < cmd.OccurredAtMS {
		return "", errors.New("storage: append event requires type and ordered timestamps")
	}
	if !json.Valid(cmd.PayloadJSON) {
		return "", errors.New("storage: append event payload must be valid JSON")
	}
	if cmd.IdempotencyKey != "" {
		var existing string
		err := d.db.QueryRowContext(ctx, `SELECT id FROM events WHERE idempotency_key=?`, cmd.IdempotencyKey).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if err != sql.ErrNoRows {
			return "", fmt.Errorf("storage: read idempotent event: %w", err)
		}
	}
	id := newID()
	_, err := d.db.ExecContext(ctx, `INSERT INTO events
		(id, run_id, attempt_no, project_id, type, source, actor, payload_schema_version, payload_json, idempotency_key, occurred_at_ms, recorded_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		id, nullable(cmd.RunID), cmd.AttemptNo, nullable(cmd.ProjectID), cmd.Type, cmd.Source,
		nullable(cmd.Actor), string(cmd.PayloadJSON), nullable(cmd.IdempotencyKey), cmd.OccurredAtMS, cmd.RecordedAtMS)
	if err != nil {
		return "", fmt.Errorf("storage: append event: %w", err)
	}
	if cmd.IdempotencyKey != "" {
		if err := d.db.QueryRowContext(ctx, `SELECT id FROM events WHERE idempotency_key=?`, cmd.IdempotencyKey).Scan(&id); err != nil {
			return "", fmt.Errorf("storage: read inserted event: %w", err)
		}
	}
	return id, nil
}

type OperationKind string

const (
	OperationForgeComment     OperationKind = "forge_comment"
	OperationForgeLabels      OperationKind = "forge_labels"
	OperationCreateChange     OperationKind = "create_change"
	OperationMergeChange      OperationKind = "merge_change"
	OperationRerunChecks      OperationKind = "rerun_checks"
	OperationChannelPublish   OperationKind = "channel_publish"
	OperationLaunchAgent      OperationKind = "launch_agent"
	OperationCommandAck       OperationKind = "command_ack"
	OperationGateReEvaluation OperationKind = "gate_re_evaluation"
	OperationForgeAlert       OperationKind = "forge_alert"
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
	// ChannelFailureAlertAfter is used by channel_publish projection updates.
	// Zero selects the V0 default of three consecutive failures.
	ChannelFailureAlertAfter int
	// MaxAttempts is the frozen effective outbox policy for this worker. Zero
	// retains the configured retry-forever semantics.
	MaxAttempts int
}

var ErrOperationConflict = errors.New("storage: operation key payload conflict")
var ErrRejectedStaleWorker = errors.New("storage: rejected stale outbox worker")
var ErrMissingCommandAckRoute = errors.New("storage: command ack route not found")
var ErrMissingRerunChecksRoute = errors.New("storage: rerun checks route not found")

// Stable operation-key constructors are the sole key vocabulary from
// specs/outbox.md §2. They use only frozen identities, never wall-clock data.
func CreateChangeOperationKey(runID, headSHA string) string {
	return "run:" + runID + ":create-change:" + headSHA
}
func MergeChangeOperationKey(runID, headSHA string) string {
	return "run:" + runID + ":merge:" + headSHA
}

// RerunChecksOperationKey is the frozen identity key for the Gate retry_checks/
// flaky_retry successor (storage.md §8.1, outbox.md §8). It is keyed by the
// frozen run, head, check and 1-based retry number, so a replayed or concurrent
// Gate evaluation cannot create a second rerun_checks operation for the same
// flaky retry (insertOperation dedupes by key).
func RerunChecksOperationKey(runID, headSHA, checkRunID string, retryNo int) string {
	return fmt.Sprintf("run:%s:checks-rerun:%s:%s:%d", runID, headSHA, checkRunID, retryNo)
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
func AlertOperationKey(kind, subjectID string, generation int) string {
	return fmt.Sprintf("alert:%s:%s:%d", kind, subjectID, generation)
}

// GateReEvaluationOperationKey is the frozen source identity key for a Gate
// re-evaluation enqueued by Command (storage.md §8.1). It is keyed by the
// frozen source Interrupt and the exact head from the immutable binding, so it
// is never reconstructed from the current Change or Run state.
func GateReEvaluationOperationKey(sourceInterruptID, headSHA string) string {
	return fmt.Sprintf("gate:%s:%s:reeval:1", sourceInterruptID, headSHA)
}

// CommandAckRoute is the frozen Forge routing a command_ack worker needs to
// publish an acknowledgement. It is resolved from the append-only command
// receipt and the project row, never reconstructed from the current Run,
// Change or Interrupt state (command.md §6.1: the ack target is the immutable
// envelope target).
type CommandAckRoute struct {
	ProjectID       string
	ForgeKind       string
	ForgeHost       string
	ForgeProjectKey string
	TargetKind      string
	TargetID        string
}

// ResolveCommandAckRouting returns the Forge target and project forge ref for a
// command ack from its append-only receipt. eventKey is the canonical command
// event key (command.md §1) carried by the ack operation key. A missing
// receipt fails closed as a contract violation: the worker must never post to
// a target it cannot prove from durable evidence.
func (d *DB) ResolveCommandAckRouting(ctx context.Context, eventKey string) (CommandAckRoute, error) {
	if eventKey == "" {
		return CommandAckRoute{}, errors.New("storage: event key is required")
	}
	var r CommandAckRoute
	err := d.db.QueryRowContext(ctx, `SELECT r.project_id, p.forge_kind, p.forge_host, p.forge_project_key, r.target_kind, r.target_id
		FROM command_receipts r JOIN projects p ON p.id = r.project_id
		WHERE r.event_key = ? LIMIT 1`, eventKey).
		Scan(&r.ProjectID, &r.ForgeKind, &r.ForgeHost, &r.ForgeProjectKey, &r.TargetKind, &r.TargetID)
	if errors.Is(err, sql.ErrNoRows) {
		return CommandAckRoute{}, ErrMissingCommandAckRoute
	}
	return r, err
}

// RerunChecksRoute is the immutable project routing for a Gate-created
// rerun_checks operation. The payload freezes run/change identity; routing is
// resolved from that Run's persisted project rather than caller input.
type RerunChecksRoute struct {
	ProjectID       string
	ForgeKind       string
	ForgeHost       string
	ForgeProjectKey string
}

func (d *DB) ResolveRerunChecksRouting(ctx context.Context, runID, changeID string) (RerunChecksRoute, error) {
	if runID == "" || changeID == "" {
		return RerunChecksRoute{}, ErrMissingRerunChecksRoute
	}
	var r RerunChecksRoute
	err := d.db.QueryRowContext(ctx, `SELECT r.project_id,p.forge_kind,p.forge_host,p.forge_project_key
		FROM runs r JOIN projects p ON p.id=r.project_id
		WHERE r.id=? AND r.change_id=?`, runID, changeID).
		Scan(&r.ProjectID, &r.ForgeKind, &r.ForgeHost, &r.ForgeProjectKey)
	if errors.Is(err, sql.ErrNoRows) {
		return RerunChecksRoute{}, ErrMissingRerunChecksRoute
	}
	return r, err
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

// CanonicalJSON returns the canonical (sorted-key, no HTML escape, no trailing
// newline) JSON encoding of v. It is the encoding used for every closed
// outbox/gate payload and digest, so workers build result bytes with it.
func CanonicalJSON(v any) ([]byte, error) { return canonicalJSON(v) }

// SHA256Hex returns the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string { return sha256Hex(b) }
func validOperationKind(k OperationKind) bool {
	switch k {
	case OperationForgeComment, OperationForgeLabels, OperationCreateChange, OperationMergeChange, OperationRerunChecks, OperationChannelPublish, OperationLaunchAgent, OperationCommandAck, OperationGateReEvaluation, OperationForgeAlert:
		return true
	}
	return false
}
func terminalOrRetry(s OperationState) bool {
	return s == OperationSucceeded || s == OperationRetryable || s == OperationFailed || s == OperationStale || s == OperationConflict
}
