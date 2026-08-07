package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/command"
)

type RetryProbeResultCmd struct {
	InterruptID        string
	ProbeID            string
	Succeeded          bool
	ExpectedRunVersion int64
	// AbsenceEvidenceJSON is the proven-absence evidence (required on success).
	AbsenceEvidenceJSON json.RawMessage
	NowMS               int64
}

// ApplyRetryProbeResult is the sole finalizer of a startup_stall retry probe.
// On success it runs the ADR-013 transaction; on failure it rotates the nonce
// and emits the absence_unconfirmed ack. It is the only writer of the
// probe-succeeded / probe-failed final stage keys.
func (d *DB) ApplyRetryProbeResult(ctx context.Context, cmd RetryProbeResultCmd) (ApplyCommandEventResult, error) {
	if cmd.InterruptID == "" || cmd.ProbeID == "" || cmd.NowMS <= 0 {
		return ApplyCommandEventResult{}, errors.New("storage: probe result requires interrupt, probe and timestamp")
	}
	if cmd.Succeeded && (len(cmd.AbsenceEvidenceJSON) == 0 || !json.Valid(cmd.AbsenceEvidenceJSON)) {
		return ApplyCommandEventResult{}, errors.New("storage: probe success requires absence evidence")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	defer tx.Rollback()
	res, err := d.applyRetryProbeResultTx(ctx, tx, cmd)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApplyCommandEventResult{}, err
	}
	if res.AckOperationKey != "" {
		d.wakeOutbox()
	}
	return res, nil
}

func (d *DB) applyRetryProbeResultTx(ctx context.Context, tx *sql.Tx, cmd RetryProbeResultCmd) (ApplyCommandEventResult, error) {
	var runID, reason, status, nonce string
	var version, runVersion int64
	var attemptNo, generation, escalation, maxEscalations int
	err := tx.QueryRowContext(ctx, `SELECT i.run_id,i.reason,i.status,i.nonce,i.version,r.version,i.attempt_no,a.generation,i.escalation_count,i.max_escalations FROM interrupts i JOIN runs r ON r.id=i.run_id JOIN attempts a ON a.run_id=i.run_id AND a.attempt_no=i.attempt_no WHERE i.id=?`, cmd.InterruptID).
		Scan(&runID, &reason, &status, &nonce, &version, &runVersion, &attemptNo, &generation, &escalation, &maxEscalations)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	if reason != string(InterruptStartupStall) || status != "open" {
		return ApplyCommandEventResult{}, ErrRejectedStale
	}
	if cmd.ExpectedRunVersion != 0 && runVersion != cmd.ExpectedRunVersion {
		return ApplyCommandEventResult{}, ErrRejectedStale
	}
	// Resolve the initial retry event / event_key for the final stage key.
	var eventKey, initialEventID string
	if err := tx.QueryRowContext(ctx, `SELECT o.event_key,o.initial_event_id FROM command_event_outcomes o JOIN attempt_probes p ON p.interrupt_id=? WHERE p.id=? AND o.initial_event_id=p.requested_by_event_id`, cmd.InterruptID, cmd.ProbeID).Scan(&eventKey, &initialEventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApplyCommandEventResult{}, ErrRejectedStale
		}
		return ApplyCommandEventResult{}, err
	}

	if cmd.Succeeded {
		return d.probeSucceededTx(ctx, tx, cmd, runID, runVersion, nonce, version, attemptNo, generation, eventKey, initialEventID)
	}
	return d.probeFailedTx(ctx, tx, cmd, nonce, version, escalation, maxEscalations, eventKey, initialEventID)
}

// probeSucceededTx runs the ADR-013 single CAS (specs/command.md §5). Evidence,
// retry_after_absence, isolation release, close/responded, waiting_human ->
// queued, next attempt/claim/launch, final outcome + ack are all-or-nothing.
func (d *DB) probeSucceededTx(ctx context.Context, tx *sql.Tx, cmd RetryProbeResultCmd, runID string, runVersion int64, nonce string, version int64, attemptNo, generation int, eventKey, initialEventID string) (ApplyCommandEventResult, error) {
	// 1. Probe -> succeeded (one-time CAS) with the absence evidence.
	res, err := tx.ExecContext(ctx, `UPDATE attempt_probes SET state='succeeded',absence_evidence_json=?,absence_evidence_digest=?,finished_at_ms=? WHERE id=? AND state IN ('pending','running')`, string(cmd.AbsenceEvidenceJSON), digestJSON(cmd.AbsenceEvidenceJSON), cmd.NowMS, cmd.ProbeID)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ApplyCommandEventResult{}, ErrRejectedStale
	}
	// 2. End old attempt with retry_after_absence (isolation still frozen here;
	// released below once the release event exists).
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET attempt_resolution='retry_after_absence',resolution_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND attempt_resolution IS NULL`, cmd.NowMS, cmd.NowMS, runID, attemptNo); err != nil {
		return ApplyCommandEventResult{}, err
	}
	// 3. Close Interrupt responded.
	if err := closeInterruptTx(ctx, tx, cmd.InterruptID, version, nonce, "responded", cmd.NowMS); err != nil {
		return ApplyCommandEventResult{}, err
	}
	// 4. waiting_human -> queued.
	if err := d.transition(ctx, tx, runID, runVersion, DomainCommand{To: RunQueued, Source: SourceOperator, Actor: "startup_stall_probe", OccurredAtMS: cmd.NowMS}); err != nil {
		return ApplyCommandEventResult{}, err
	}
	// 5. Final event (probe-succeeded) + outcome CAS. Its id is the isolation
	// release evidence event.
	finalID, err := finalizeRetryOutcomeTx(ctx, tx, eventKey, initialEventID, command.OutcomeApplied, runID, cmd.InterruptID, "", cmd.NowMS, command.StageFinalProbeSucceeded)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	// 6. Release isolation, referencing the final event as proof.
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET isolation_state='none',isolation_released_at_ms=?,isolation_release_event_id=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND isolation_state='frozen'`, cmd.NowMS, finalID, cmd.NowMS, runID, attemptNo); err != nil {
		return ApplyCommandEventResult{}, err
	}
	// 7. Create exactly one pending successor attempt, claim and launch.
	if _, err := d.spawnNextAttemptTx(ctx, tx, runID, attemptNo, generation, cmd.NowMS, ""); err != nil {
		return ApplyCommandEventResult{}, err
	}
	// 8. Ack operation.
	ackKey := command.AckOperationKey(eventKey)
	if err := writeProbeAckOpTx(ctx, tx, eventKey, command.OutcomeApplied, finalID, cmd.InterruptID, runID, ackKey, cmd.NowMS); err != nil {
		return ApplyCommandEventResult{}, err
	}
	return ApplyCommandEventResult{Outcome: command.OutcomeApplied, FinalEventID: finalID, AckOperationKey: ackKey, InterruptID: cmd.InterruptID, RunID: runID}, nil
}

// probeFailedTx marks the probe failed, retains Interrupt/isolation, increments
// version, rotates nonce and emits the absence_unconfirmed ack (specs/command.md
// §5). When the Interrupt is already at its escalation cap the probe failure
// applies the frozen capped hold directly ("or applies frozen capped hold"),
// because startup_stall may never auto_reject: it stays open+held with no
// resolution write and no second advance/write port. Below the cap the probe
// failure reverts dispatch_state to the post-emit batched state so the unique
// AdvanceInterrupt/expiry path can still escalate (interrupt.md §8.2). The
// held/NULL state previously written here was both excluded from the expiry
// scan and rejected by AdvanceInterrupt, so escalation could never run after a
// probe failure; batched is escalate-able and, unlike ready, does not spuriously
// re-dispatch because next_dispatch_at_ms stays NULL.
func (d *DB) probeFailedTx(ctx context.Context, tx *sql.Tx, cmd RetryProbeResultCmd, nonce string, version int64, escalation, maxEscalations int, eventKey, initialEventID string) (ApplyCommandEventResult, error) {
	res, err := tx.ExecContext(ctx, `UPDATE attempt_probes SET state='failed',finished_at_ms=? WHERE id=? AND state IN ('pending','running')`, cmd.NowMS, cmd.ProbeID)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ApplyCommandEventResult{}, ErrRejectedStale
	}
	nextNonce := newToken()
	if escalation >= maxEscalations {
		// Frozen capped hold: startup_stall at cap must hold, never auto_reject.
		// This mirrors holdAdvance("max_escalations") without a second advance.
		if _, err := tx.ExecContext(ctx, `UPDATE interrupts SET nonce=?,version=version+1,dispatch_state='held',held_reason='max_escalations',delivery='held',next_dispatch_at_ms=NULL,nonce_issued_at_ms=?,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`, nextNonce, cmd.NowMS, cmd.NowMS, cmd.InterruptID, version, nonce); err != nil {
			return ApplyCommandEventResult{}, err
		}
	} else {
		// Below the cap: revert to the escalate-able post-emit batched state so
		// the expiry tick / AdvanceInterrupt can drive escalation_count forward.
		if _, err := tx.ExecContext(ctx, `UPDATE interrupts SET nonce=?,version=version+1,dispatch_state='batched',held_reason=NULL,nonce_issued_at_ms=?,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`, nextNonce, cmd.NowMS, cmd.NowMS, cmd.InterruptID, version, nonce); err != nil {
			return ApplyCommandEventResult{}, err
		}
	}
	finalID, err := finalizeRetryOutcomeTx(ctx, tx, eventKey, initialEventID, command.OutcomeAbsenceUnconfirmed, "", cmd.InterruptID, nextNonce, cmd.NowMS, command.StageFinalProbeFailed)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	ackKey := command.AckOperationKey(eventKey)
	if err := writeProbeAckOpTx(ctx, tx, eventKey, command.OutcomeAbsenceUnconfirmed, finalID, cmd.InterruptID, "", ackKey, cmd.NowMS); err != nil {
		return ApplyCommandEventResult{}, err
	}
	return ApplyCommandEventResult{Outcome: command.OutcomeAbsenceUnconfirmed, FinalEventID: finalID, AckOperationKey: ackKey, NextNonce: nextNonce, InterruptID: cmd.InterruptID}, nil
}

// finalizeRetryOutcomeTx is the sole finalizer of a retry outcome. It inserts
// the final event (referencing the initial by final_for_event_id) and CAS-
// completes the outcome relation from pending to final. The final event's
// source identity is taken from the initial event payload, which is itself a
// CommandEventV1.
func finalizeRetryOutcomeTx(ctx context.Context, tx *sql.Tx, eventKey, initialEventID string, outcome command.CommandOutcome, runID, interruptID, nextNonce string, nowMS int64, stage string) (string, error) {
	var initialBytes []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE id=?`, initialEventID).Scan(&initialBytes); err != nil {
		return "", err
	}
	var initial command.CommandEventV1
	if err := json.Unmarshal(initialBytes, &initial); err != nil {
		return "", fmt.Errorf("storage: corrupt initial command event: %w", err)
	}
	finalEvent := command.NewEvent(command.CommandEventEnvelopeV1{
		SchemaVersion: 1, EventKey: initial.EventKey, ProjectID: "", Source: initial.Source, RemoteEventID: initial.RemoteEventID,
	}, outcome, command.ActionRetry, runID, interruptID, nextNonce, initialEventID)
	// Preserve the project id (the initial event stored it; the final carries it).
	var projectID string
	_ = tx.QueryRowContext(ctx, `SELECT project_id FROM events WHERE id=?`, initialEventID).Scan(&projectID)
	body, err := finalEvent.CanonicalBytes()
	if err != nil {
		return "", err
	}
	finalID := newID()
	idem := command.EventStageKey(eventKey, stage)
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,run_id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,'command.event','forge',1,?,?,?,?)`,
		finalID, nullable(runID), nullable(projectID), string(body), idem, nowMS, nowMS); err != nil {
		return "", err
	}
	// Single finalizer CAS: pending -> final with this event id.
	res, err := tx.ExecContext(ctx, `UPDATE command_event_outcomes SET final_event_id=?,state='final',finalized_at_ms=? WHERE event_key=? AND state='pending' AND final_event_id IS NULL AND finalized_at_ms IS NULL`, finalID, nowMS, eventKey)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", fmt.Errorf("storage: retry outcome already finalized")
	}
	return finalID, nil
}

func actionPtr(a command.CommandAction) *command.CommandAction { return &a }

func writeProbeAckOpTx(ctx context.Context, tx *sql.Tx, eventKey string, outcome command.CommandOutcome, finalEventID, interruptID, runID, ackKey string, nowMS int64) error {
	ack := command.CommandAckV1{
		SchemaVersion:  1,
		CommandEventID: finalEventID,
		Action:         actionPtr(command.ActionRetry),
		Disposition:    outcome,
	}
	if runID != "" {
		r := runID
		ack.RunID = &r
	}
	if interruptID != "" {
		i := interruptID
		ack.InterruptID = &i
	}
	body, err := ack.CanonicalBytes()
	if err != nil {
		return err
	}
	op := Operation{Key: ackKey, Kind: OperationCommandAck, Payload: body, InterruptID: interruptID, RunID: runID}
	return insertOperation(ctx, tx, op, runID, interruptID, nowMS)
}

// finalizeProbeFactWinsTx is the X15 fact-wins writer (specs/command.md §5,
// docs/testing/runtime-matrix.md X15). It runs inside the same transaction
// that closes the startup_stall Interrupt as superseded_by_fact, marks the
// pending|running retry probe superseded, writes the final command event
// (stage=final:fact-wins), completes the outcome CAS pending->final, and
// enqueues exactly one superseded_by_fact command ack. It is a no-op when
// no probe is in flight.
//
// Replay safety: a duplicate ResolveAttemptRace call is rejected by the
// events.idempotency_key CAS before reaching this helper, so the probe and
// outcome CAS guards below only matter when two transactions race; the
// attempt_probes UPDATE state IN (pending,running) and the
// command_event_outcomes UPDATE state='pending' are both single-shot, and
// the ack operation key is unique on (operation_key). Even if the helper
// were called twice in the same transaction, the second invocation finds
// no pending|running probe and short-circuits.
func (d *DB) finalizeProbeFactWinsTx(ctx context.Context, tx *sql.Tx, runID string, attemptNo int, nowMS int64) error {
	var probeID, initialEventID, eventKey, interruptID string
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, o.initial_event_id, o.event_key, p.interrupt_id
		FROM attempt_probes p
		JOIN command_event_outcomes o ON o.initial_event_id = p.requested_by_event_id
		WHERE p.run_id = ? AND p.attempt_no = ?
		  AND p.state IN ('pending', 'running')
		ORDER BY p.created_at_ms DESC, p.id DESC
		LIMIT 1`, runID, attemptNo).Scan(&probeID, &initialEventID, &eventKey, &interruptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// 1. Mark the probe superseded (single-shot CAS: state IN pending|running).
	res, err := tx.ExecContext(ctx, `UPDATE attempt_probes SET state='superseded', finished_at_ms=? WHERE id=? AND state IN ('pending','running')`, nowMS, probeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Concurrent fact/race already finalized the probe; nothing more to write.
		return nil
	}
	// 2. Final command event (stage final:fact-wins) + outcome CAS pending->final.
	finalEventID, err := finalizeRetryOutcomeTx(ctx, tx, eventKey, initialEventID, command.OutcomeSupersededByFact, runID, interruptID, "", nowMS, command.StageFinalFactWins)
	if err != nil {
		return err
	}
	// 3. Exactly one superseded_by_fact command ack.
	ackKey := command.AckOperationKey(eventKey)
	return writeProbeAckOpTx(ctx, tx, eventKey, command.OutcomeSupersededByFact, finalEventID, interruptID, runID, ackKey, nowMS)
}

// RetryProbeCandidate is the immutable snapshot a probe process-check
// coordinator needs before it performs process IO outside any transaction. It
// joins attempt_probes with the bound attempt's recorded wrapper identity
// (specs/storage.md §5.5). ControlPath is not persisted; the coordinator
// derives it from its control root.
type RetryProbeCandidate struct {
	ProbeID            string
	RunID              string
	AttemptNo          int
	InterruptID        string
	ExpectedRunVersion int64
	ExpectedGeneration int
	AgentID            string
	WrapperPID         int
	WrapperStartedAtMS int64
	WrapperExecutable  string
	WrapperPGID        int
	ControlNonceHash   string
}

// PendingRetryProbes returns attempt_probes still in pending or running state,
// joined with the bound attempt's recorded wrapper identity. The probe
// process-check worker drives each to the unique ApplyRetryProbeResult
// finalizer (specs/command.md §5, specs/storage.md §5.5). Process observation
// and idempotent finalization happen outside this read; a probe superseded by a
// late fact or already finalized is excluded by its state.
func (d *DB) PendingRetryProbes(ctx context.Context) ([]RetryProbeCandidate, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT p.id,p.run_id,p.attempt_no,p.interrupt_id,p.expected_run_version,p.expected_generation,
		COALESCE(a.agent_id,''),COALESCE(a.wrapper_pid,0),COALESCE(a.wrapper_started_at_ms,0),COALESCE(a.wrapper_executable,''),COALESCE(a.wrapper_pgid,0),COALESCE(a.control_nonce_hash,'')
		FROM attempt_probes p JOIN attempts a ON a.run_id=p.run_id AND a.attempt_no=p.attempt_no
		WHERE p.state IN ('pending','running') ORDER BY p.created_at_ms, p.id`)
	if err != nil {
		return nil, fmt.Errorf("storage: list pending retry probes: %w", err)
	}
	defer rows.Close()
	var out []RetryProbeCandidate
	for rows.Next() {
		var c RetryProbeCandidate
		if err := rows.Scan(&c.ProbeID, &c.RunID, &c.AttemptNo, &c.InterruptID, &c.ExpectedRunVersion, &c.ExpectedGeneration, &c.AgentID, &c.WrapperPID, &c.WrapperStartedAtMS, &c.WrapperExecutable, &c.WrapperPGID, &c.ControlNonceHash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimRetryProbe performs the pending->running CAS so a crashed tick can be
// resumed (specs/storage.md §5.5: "崩溃后从 pending/running 继续"). started_at_ms
// is set once and preserved across replay. This is best-effort progress
// tracking; the unique ApplyRetryProbeResult finalizer's own pending|running CAS
// is the real at-most-once guard, so a failed/empty claim (an already-running
// probe, or one finalized by a concurrent tick/fact) is not an error.
func (d *DB) ClaimRetryProbe(ctx context.Context, probeID string, nowMS int64) (bool, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE attempt_probes SET state='running',started_at_ms=COALESCE(started_at_ms,?) WHERE id=? AND state='pending'`, nowMS, probeID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
