package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

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
