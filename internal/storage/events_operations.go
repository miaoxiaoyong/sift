package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

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
