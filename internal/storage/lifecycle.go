package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/forge"
)

// SetInitialTaskSpec is the T2 valid hitl=false commit port (storage.md §11).
// It idempotently inserts the initial Task Spec snapshot, writes the Run
// kind/agent/hitl + current_task_spec pointer and appends an event, in one
// transaction. The Run stays in (or is already in) the queued status — it is
// not exposed as launchable before the assignment is committed (brain.md §8.3).
//
// The full CommitT2Assignment (hitl=true → design_approval Interrupt in the same
// transaction) lands in M3 with the Interrupt emission core; this M1 port
// covers exactly the skeleton chain's hitl=false path.
type SetInitialTaskSpecCmd struct {
	RunID           string
	ExpectedVersion int64
	TaskSpecID      string
	CanonicalJSON   []byte
	ContentDigest   string
	Kind            string // feature|bug|chore|docs|refactor
	AgentID         string
	HITLBeforeStart bool
	// InitialAttempt admits the first launch in the same transaction as the
	// assignment. It is nil for assignments that must wait for a later command
	// (for example HITL design approval).
	InitialAttempt *InitialAttemptSpec
	SourceEventID  string // optional provenance event
	OccurredAtMS   int64
}

// SetInitialTaskSpec commits the initial Task Spec and Run assignment. When
// InitialAttempt is supplied, it also atomically admits the first launch. It is
// idempotent on (run_id, version=1): re-applying the same snapshot is a no-op.
func (d *DB) SetInitialTaskSpec(ctx context.Context, cmd SetInitialTaskSpecCmd) (Run, error) {
	if cmd.RunID == "" || cmd.ExpectedVersion < 1 {
		return Run{}, errors.New("storage: set initial task spec requires run id and expected version")
	}
	if cmd.TaskSpecID == "" || len(cmd.CanonicalJSON) == 0 || cmd.ContentDigest == "" {
		return Run{}, errors.New("storage: set initial task spec requires snapshot id/json/digest")
	}
	if !json.Valid(cmd.CanonicalJSON) {
		return Run{}, errors.New("storage: set initial task spec canonical json is not valid")
	}
	if cmd.OccurredAtMS <= 0 {
		return Run{}, errors.New("storage: set initial task spec requires occurred_at_ms")
	}
	if err := cmd.InitialAttempt.validate(); err != nil {
		return Run{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("storage: begin set initial task spec: %w", err)
	}
	defer tx.Rollback()

	var status string
	var version int64
	var forcedHITL int
	var snapshotJSON string
	if err := tx.QueryRowContext(ctx, `SELECT r.status, r.version, r.hitl_before_start, s.canonical_json
		FROM runs r JOIN config_snapshots s ON s.id=r.config_snapshot_id WHERE r.id=?`, cmd.RunID).Scan(&status, &version, &forcedHITL, &snapshotJSON); err != nil {
		return Run{}, err
	}
	if version != cmd.ExpectedVersion {
		return Run{}, ErrRejectedStale
	}
	// The assignment port only writes the kind/agent; a Run that already left
	// queued (waiting_human/running/...) was committed by a different path and
	// must not be silently re-assigned here.
	if status != string(RunQueued) {
		return Run{}, fmt.Errorf("%w: set initial task spec requires queued, got %s", ErrIllegalTransition, status)
	}

	sourceEvent := cmd.SourceEventID
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_spec_snapshots
		(id, run_id, version, schema_version, canonical_json, content_digest, source_event_id, created_at_ms)
		VALUES (?, ?, 1, 1, ?, ?, ?, ?)
		ON CONFLICT(run_id, version) DO NOTHING`,
		cmd.TaskSpecID, cmd.RunID, string(cmd.CanonicalJSON), cmd.ContentDigest, nullable(sourceEvent), cmd.OccurredAtMS); err != nil {
		return Run{}, fmt.Errorf("storage: insert initial task spec snapshot: %w", err)
	}

	hitlBeforeStart := cmd.HITLBeforeStart || forcedHITL != 0
	res, err := tx.ExecContext(ctx, `UPDATE runs
		SET kind=?, agent_id=?, hitl_before_start=?, current_task_spec_id=?, version=version+1, updated_at_ms=?
		WHERE id=? AND version=? AND status='queued'`,
		cmd.Kind, cmd.AgentID, boolInt(hitlBeforeStart), cmd.TaskSpecID, cmd.OccurredAtMS, cmd.RunID, cmd.ExpectedVersion)
	if err != nil {
		return Run{}, fmt.Errorf("storage: assign run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, err
	}
	if n != 1 {
		return Run{}, ErrRejectedStale
	}

	payload, _ := json.Marshal(map[string]any{
		"kind":              cmd.Kind,
		"agent":             cmd.AgentID,
		"hitl_before_start": hitlBeforeStart,
		"task_spec_id":      cmd.TaskSpecID,
	})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events
		(id, run_id, type, source, payload_schema_version, payload_json, occurred_at_ms, recorded_at_ms)
		VALUES (?, ?, 'run.assigned', 'system', 1, ?, ?, ?)`,
		newID(), cmd.RunID, string(payload), cmd.OccurredAtMS, cmd.OccurredAtMS); err != nil {
		return Run{}, fmt.Errorf("storage: insert assignment event: %w", err)
	}
	if cmd.InitialAttempt != nil {
		if hitlBeforeStart {
			return Run{}, fmt.Errorf("%w: HITL assignment cannot launch initial attempt", ErrIllegalTransition)
		}
		if err := insertInitialAttemptTx(ctx, tx, cmd.RunID, cmd.AgentID, cmd.TaskSpecID, snapshotJSON, cmd.InitialAttempt, cmd.OccurredAtMS); err != nil {
			return Run{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("storage: commit set initial task spec: %w", err)
	}
	if cmd.InitialAttempt != nil {
		d.wakeOutbox()
	}
	return d.Run(ctx, cmd.RunID)
}

// Recommendation is untrusted, advisory input (for example from Brain). It
// deliberately has no status or persistence fields. A caller must translate it
// into a deterministic DomainCommand before it can reach TransitionRun.
type Recommendation struct {
	Kind      string
	Rationale string
}

// DomainCommand is the deterministic request submitted to the Run state
// machine. It is intentionally separate from Recommendation: only commands
// name a state transition.
type DomainCommand struct {
	To            RunStatus
	Source        EventSource
	Actor         string
	FailureReason string
	ChangeID      string
	ChangeURL     string
	ChangeHeadSHA string
	GateBypassed  bool
	OccurredAtMS  int64
	// Operation, when present, is committed with the projection and event.
	// The worker may execute it only after this transaction commits.
	Operation *Operation
}

type RunStatus string

const (
	RunQueued       RunStatus = "queued"
	RunRunning      RunStatus = "running"
	RunWaitingHuman RunStatus = "waiting_human"
	RunDone         RunStatus = "done"
	RunFailed       RunStatus = "failed"
)

type EventSource string

const (
	SourceSystem   EventSource = "system"
	SourceForge    EventSource = "forge"
	SourceOperator EventSource = "operator"
	SourceAgent    EventSource = "agent"
	SourceRecovery EventSource = "recovery"
)

var (
	ErrRejectedStale     = errors.New("storage: rejected stale command")
	ErrIllegalTransition = errors.New("storage: illegal run transition")
)

type Run struct {
	ID              string
	Status          RunStatus
	Version         int64
	Kind            string
	AgentID         string
	HITLBeforeStart bool
	FailureReason   string
	ChangeID        string
	GateBypassed    bool
	UpdatedAtMS     int64
	CompletedAtMS   *int64
}

// Run returns the current Run projection. It is read-only and never exposes a
// transaction handle.
func (d *DB) Run(ctx context.Context, id string) (Run, error) {
	var r Run
	var status string
	var bypass int
	var completed sql.NullInt64
	var failure, change, kind, agent sql.NullString
	var hitl sql.NullInt64
	err := d.db.QueryRowContext(ctx, `SELECT id, status, version, kind, agent_id, hitl_before_start, failure_reason, change_id,
		gate_bypassed, updated_at_ms, completed_at_ms FROM runs WHERE id = ?`, id).Scan(
		&r.ID, &status, &r.Version, &kind, &agent, &hitl, &failure, &change, &bypass, &r.UpdatedAtMS, &completed)
	if err != nil {
		return Run{}, err
	}
	r.Status, r.FailureReason, r.ChangeID, r.GateBypassed = RunStatus(status), failure.String, change.String, bypass != 0
	r.Kind, r.AgentID = kind.String, agent.String
	if hitl.Valid {
		r.HITLBeforeStart = hitl.Int64 != 0
	}
	if completed.Valid {
		v := completed.Int64
		r.CompletedAtMS = &v
	}
	return r, nil
}

// TransitionRun is the only public state-writing port. It performs the CAS,
// appends the audit event, and creates any required outbox operation in one
// BEGIN IMMEDIATE transaction. There is deliberately no UpdateRun API.
func (d *DB) TransitionRun(ctx context.Context, runID string, expectedVersion int64, cmd DomainCommand) (Run, error) {
	if !validSource(cmd.Source) {
		return Run{}, fmt.Errorf("storage: invalid command source %q", cmd.Source)
	}
	if cmd.OccurredAtMS <= 0 {
		return Run{}, errors.New("storage: command OccurredAtMS is required")
	}
	if err := validateCommand(cmd); err != nil {
		return Run{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("storage: begin transition: %w", err)
	}
	defer tx.Rollback()

	var current string
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT status, version FROM runs WHERE id = ?`, runID).Scan(&current, &version); err != nil {
		return Run{}, err
	}
	if version != expectedVersion {
		return Run{}, ErrRejectedStale
	}
	if !legalTransition(RunStatus(current), cmd.To) {
		// This event is intentionally committed separately: illegal commands are
		// auditable but must not mutate the Run nor create side effects.
		if err := tx.Rollback(); err != nil {
			return Run{}, err
		}
		if err := d.auditIllegalTransition(ctx, runID, cmd, RunStatus(current), expectedVersion); err != nil {
			return Run{}, err
		}
		return Run{}, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, current, cmd.To)
	}
	if err := d.transition(ctx, tx, runID, expectedVersion, cmd); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("storage: commit transition: %w", err)
	}
	if cmd.Operation != nil {
		d.wakeOutbox()
	}
	return d.Run(ctx, runID)
}

// transition is the sole status writer. Keep it private: all callers enter via
// TransitionRun or a future restricted port that owns the entire transaction.
func (d *DB) transition(ctx context.Context, tx *sql.Tx, runID string, expectedVersion int64, cmd DomainCommand) error {
	completed := any(nil)
	failure := any(nil)
	changeID, changeURL, changeHead := any(nil), any(nil), any(nil)
	if cmd.To == RunDone || cmd.To == RunFailed {
		completed = cmd.OccurredAtMS
	}
	if cmd.To == RunFailed {
		failure = cmd.FailureReason
	}
	if cmd.To == RunDone {
		changeID, changeURL, changeHead = cmd.ChangeID, nullable(cmd.ChangeURL), nullable(cmd.ChangeHeadSHA)
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status=?, version=version+1, failure_reason=?,
		change_id=COALESCE(?, change_id), change_url=COALESCE(?, change_url),
		change_head_sha=COALESCE(?, change_head_sha), gate_bypassed=?, updated_at_ms=?, completed_at_ms=?
		WHERE id=? AND version=?`, cmd.To, failure, changeID, changeURL, changeHead, boolInt(cmd.GateBypassed), cmd.OccurredAtMS, completed, runID, expectedVersion)
	if err != nil {
		return fmt.Errorf("storage: transition run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRejectedStale
	}
	payload, _ := json.Marshal(map[string]any{"from_version": expectedVersion, "to": cmd.To, "failure_reason": cmd.FailureReason, "gate_bypassed": cmd.GateBypassed})
	eventID := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id, run_id, type, source, actor, payload_schema_version, payload_json, occurred_at_ms, recorded_at_ms)
		VALUES (?, ?, 'run.transitioned', ?, ?, 1, ?, ?, ?)`, eventID, runID, cmd.Source, nullable(cmd.Actor), string(payload), cmd.OccurredAtMS, cmd.OccurredAtMS); err != nil {
		return fmt.Errorf("storage: append transition event: %w", err)
	}
	if cmd.Operation != nil {
		if err := insertOperation(ctx, tx, *cmd.Operation, runID, eventID, cmd.OccurredAtMS); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) auditIllegalTransition(ctx context.Context, runID string, cmd DomainCommand, from RunStatus, version int64) error {
	payload, _ := json.Marshal(map[string]any{"from": from, "to": cmd.To, "expected_version": version})
	_, err := d.db.ExecContext(ctx, `INSERT INTO events (id, run_id, type, source, actor, payload_schema_version, payload_json, occurred_at_ms, recorded_at_ms)
		VALUES (?, ?, 'run.transition_rejected', ?, ?, 1, ?, ?, ?)`, newID(), runID, cmd.Source, nullable(cmd.Actor), string(payload), cmd.OccurredAtMS, cmd.OccurredAtMS)
	return err
}

func legalTransition(from, to RunStatus) bool {
	switch from {
	case RunQueued:
		return to == RunRunning || to == RunWaitingHuman || to == RunDone || to == RunFailed
	case RunRunning:
		return to == RunWaitingHuman || to == RunDone || to == RunFailed
	case RunWaitingHuman:
		return to == RunRunning || to == RunQueued || to == RunDone || to == RunFailed
	case RunFailed:
		return to == RunQueued
	default:
		return false
	}
}
func validSource(s EventSource) bool {
	return s == SourceSystem || s == SourceForge || s == SourceOperator || s == SourceAgent || s == SourceRecovery
}
func validateCommand(c DomainCommand) error {
	if !legalStatus(c.To) {
		return fmt.Errorf("storage: invalid destination status %q", c.To)
	}
	if c.To == RunDone && c.ChangeID == "" {
		return errors.New("storage: done transition requires ChangeID")
	}
	if c.To == RunFailed && c.FailureReason == "" {
		return errors.New("storage: failed transition requires FailureReason")
	}
	return nil
}
func legalStatus(s RunStatus) bool {
	return s == RunQueued || s == RunRunning || s == RunWaitingHuman || s == RunDone || s == RunFailed
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("storage: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// IsSiftMerge returns true only when the observed Change/head is backed by a
// succeeded merge_change operation carrying its immutable Gate evaluation ID.
// Reverse-sync uses this causal identity rather than inferring intent from a
// Run/head calibration.
func (d *DB) IsSiftMerge(ctx context.Context, runID, changeID, headSHA string) (bool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT payload_json FROM outbox_operations WHERE run_id=? AND kind='merge_change' AND state='succeeded'`, runID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var payload struct {
			ChangeID         string `json:"change_id"`
			GateEvaluationID string `json:"gate_evaluation_id"`
			ExpectedHeadSHA  string `json:"expected_head_sha"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.ChangeID == changeID && payload.ExpectedHeadSHA == headSHA && payload.GateEvaluationID != "" {
			return true, nil
		}
	}
	return false, rows.Err()
}

// UpdateProjectAutoMergeCapability records the startup CAS-capability proof.
// An absent or malformed projection is intentionally interpreted as disabled
// by AutoMergeEnabled, so restarts cannot recover an optimistic default.
func (d *DB) UpdateProjectAutoMergeCapability(ctx context.Context, projectID string, enabled bool, evidence string, nowMS int64) error {
	if projectID == "" || nowMS <= 0 {
		return errors.New("storage: auto-merge capability requires project and timestamp")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT capabilities_json FROM projects WHERE id=?`, projectID).Scan(&raw); err != nil {
		return err
	}
	capabilities := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &capabilities); err != nil {
		return fmt.Errorf("storage: invalid project capabilities: %w", err)
	}
	capabilities["auto_merge"] = enabled
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET capabilities_json=?,capabilities_checked_at_ms=?,updated_at_ms=? WHERE id=?`, string(encoded), nowMS, nowMS, projectID); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"auto_merge": enabled, "evidence": evidence})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?, 'project.capability_checked','system',1,?,?,?)`, newID(), projectID, string(payload), nowMS, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// AutoMergeEnabled implements forge.AutoMergeCapabilityReader using the
// durable project projection. Missing rows, malformed values, and absent keys
// are all unavailable rather than optimistic defaults.
func (d *DB) AutoMergeEnabled(ctx context.Context, ref forge.ProjectRef) (bool, error) {
	var raw string
	err := d.db.QueryRowContext(ctx, `SELECT capabilities_json FROM projects WHERE forge_kind=? AND forge_host=? AND forge_project_key=? AND enabled=1`, string(ref.Kind), ref.Host, ref.ProjectKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var capabilities map[string]any
	if err := json.Unmarshal([]byte(raw), &capabilities); err != nil {
		return false, nil
	}
	enabled, ok := capabilities["auto_merge"].(bool)
	return ok && enabled, nil
}

var _ forge.AutoMergeCapabilityReader = (*DB)(nil)
