package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/command"
)

// Command write port (specs/command.md §4/§5). ApplyCommandEvent is the sole
// public command write port; its private transaction primitives own the
// Ledger, Run state and outbox. It never obtains *sql.Tx from a caller and
// never calls Forge, Brain, process checks or signals inside the transaction.

// ErrCommandEffectNotWired means the reason×action pair is canonical but its
// full deterministic effect (Gate re-evaluation operation, new attempt/claim/
// launch, attempt terminalization) is not yet wired in this bootstrap slice.
// The transaction rolls back; no receipt, event or ack is written.
var ErrCommandEffectNotWired = errors.New("storage: command effect not wired")

// ApplyCommandEventCmd is the single write-port input. Parsed is zero for an
// approval_label candidate. Allowlist is the resolved operator logins for the
// Run's forge platform, taken from the immutable config snapshot.
type ApplyCommandEventCmd struct {
	Envelope  command.CommandEventEnvelopeV1
	Parsed    command.ParsedCommand
	Allowlist []string
	NowMS     int64
}

// ApplyCommandEventResult is the persisted outcome. Ignored is true for a
// null/untrusted actor (no event or ack). For a retry request Outcome is
// retry_pending and AckOperationKey is empty (no ack before a final result).
type ApplyCommandEventResult struct {
	Outcome         command.CommandOutcome
	Ignored         bool
	FinalEventID    string
	AckOperationKey string
	NextNonce       string
	InterruptID     string
	RunID           string
}

// ApplyCommandEvent is the only public command write port. It performs
// candidate dedup, allowlist auth, grammar, immutable-target/current-Interrupt/
// nonce/options validation, and the deterministic reason×action effect in one
// transaction, then emits exactly one command event and (for final outcomes)
// one ack operation. There is no second transition, Ledger or outbox path: it
// calls the private transition/recordHumanDecisionTx/insertOperation cores.
func (d *DB) ApplyCommandEvent(ctx context.Context, cmd ApplyCommandEventCmd) (ApplyCommandEventResult, error) {
	if cmd.NowMS <= 0 {
		return ApplyCommandEventResult{}, errors.New("storage: command requires NowMS")
	}
	if err := cmd.Envelope.Validate(); err != nil {
		return ApplyCommandEventResult{}, err
	}
	if !cmd.Envelope.VerifyEventKey() {
		return ApplyCommandEventResult{}, errors.New("storage: command event key mismatch")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	defer tx.Rollback()
	res, err := d.applyCommandEventTx(ctx, tx, cmd)
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

func (d *DB) applyCommandEventTx(ctx context.Context, tx *sql.Tx, cmd ApplyCommandEventCmd) (ApplyCommandEventResult, error) {
	env := cmd.Envelope

	// 1. Candidate dedup: a duplicate returns the stored outcome and creates no
	// event, Ledger entry, probe or ack.
	if stored, ok, err := lookupCommandReceiptTx(ctx, tx, env); err != nil {
		return ApplyCommandEventResult{}, err
	} else if ok {
		return stored, nil
	}

	// 2. Allowlist auth. A null/untrusted actor persists an ignored receipt and
	// a low-sensitivity security event; it creates no command event or ack.
	authorizer := command.NewAuthorizer(cmd.Allowlist)
	if !authorizer.Trusted(env.Actor) {
		disposition := "ignored_missing_actor"
		if env.Actor != nil && *env.Actor != "" {
			disposition = "ignored_untrusted_actor"
		}
		if err := writeIgnoredCommandReceiptTx(ctx, tx, env, disposition, cmd.NowMS); err != nil {
			return ApplyCommandEventResult{}, err
		}
		audit := map[string]string{"disposition": disposition, "target_kind": string(env.Target.Kind), "target_id": env.Target.ID}
		body, _ := json.Marshal(audit)
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?,?,'command.ignored','forge',1,?,?,?)`,
			newID(), env.ProjectID, string(body), cmd.NowMS, cmd.NowMS); err != nil {
			return ApplyCommandEventResult{}, err
		}
		return ApplyCommandEventResult{Ignored: true}, nil
	}

	// 3. Grammar (forge_comment only; approval_label compiles to approve).
	var parsed command.ParsedCommand
	outcome := command.OutcomeApplied
	if env.Source == command.SourceForgeComment {
		p, perr := command.ParseCommand(env.Comment.Body)
		if perr != nil {
			parsed = command.ParsedCommand{}
			outcome = command.OutcomeRejectedSyntax
		} else {
			parsed = p
		}
	}

	// 4. Immutable target / current Interrupt / nonce / options validation.
	row, bindingTarget, err := resolveCommandInterruptTx(ctx, tx, env)
	if err != nil {
		return ApplyCommandEventResult{}, err
	}
	var view *command.InterruptView
	if row != nil {
		view = interruptView(row)
	}
	var compiled command.CompiledCommandV1
	if outcome == command.OutcomeApplied {
		compileRes := command.Compile(env, parsed, view, bindingTarget)
		outcome = compileRes.Outcome
		compiled = compileRes.Compiled
	}

	// 5. Deterministic effect (only for applied / retry_pending outcomes).
	// action/runID are resolved before persistence so the command event can be
	// written before the effect: Gate re-evaluation operations and command_effects
	// facts reference the close event, and command_effects.event_id is a FK to
	// events, so the event row must exist inside the same transaction.
	action := compiled.Action
	if env.Source == command.SourceApprovalLabel {
		action = command.ActionApprove
	}
	if outcome == command.OutcomeRejectedSyntax || outcome == command.OutcomeRejectedTarget {
		action = ""
	}
	runID := ""
	if row != nil {
		runID = row.RunID
	}

	var nextNonce, eventID string
	if outcome == command.OutcomeApplied && row != nil && row.Reason == string(InterruptStartupStall) && compiled.Action == command.ActionRetry {
		// startup_stall retry is the two-phase request: it persists its own
		// initial retry event + pending outcome + probe (the event must exist
		// before the probe FK), so the common persist path is skipped.
		outcome = command.OutcomeRetryPending
		nextNonce, eventID, err = d.startupStallRetryRequestTx(ctx, tx, env, compiled, row, cmd.NowMS)
		if err != nil {
			return ApplyCommandEventResult{}, err
		}
	} else if outcome == command.OutcomeApplied {
		// hold is the only applied action that rotates the nonce; pre-compute it
		// so the event's next_nonce and the Interrupt CAS share one value.
		if compiled.Action == command.ActionHold {
			nextNonce = newToken()
		}
		eventID, err = persistCommandEventTx(ctx, tx, env, outcome, action, runID, compiled.InterruptID, nextNonce, "", cmd.NowMS, "")
		if err != nil {
			return ApplyCommandEventResult{}, err
		}
		if err = d.applyCommandEffectTx(ctx, tx, env, compiled, row, cmd.NowMS, eventID, nextNonce); err != nil {
			return ApplyCommandEventResult{}, err
		}
	} else {
		// Rejected candidates persist one final command event + outcome.
		eventID, err = persistCommandEventTx(ctx, tx, env, outcome, action, runID, compiled.InterruptID, "", "", cmd.NowMS, "")
		if err != nil {
			return ApplyCommandEventResult{}, err
		}
	}

	// 6. Outcome relation + receipt + ack operation.
	ackKey := ""
	if outcome != command.OutcomeRetryPending {
		ackKey = command.AckOperationKey(env.EventKey)
		if err := writeCommandAckOpTx(ctx, tx, env, outcome, action, eventID, compiled.InterruptID, runID, nextNonce, ackKey, cmd.NowMS); err != nil {
			return ApplyCommandEventResult{}, err
		}
	}
	if err := writeAcceptedCommandReceiptTx(ctx, tx, env, "accepted", eventID, cmd.NowMS); err != nil {
		return ApplyCommandEventResult{}, err
	}
	return ApplyCommandEventResult{
		Outcome:         outcome,
		FinalEventID:    eventID,
		AckOperationKey: ackKey,
		NextNonce:       nextNonce,
		InterruptID:     compiled.InterruptID,
		RunID:           runID,
	}, nil
}

// commandInterruptRow is the projection of the current open Interrupt needed
// for compilation and effect dispatch.
type commandInterruptRow struct {
	ID, RunID, Reason, Status, DispatchState, Nonce string
	Version, RunVersion                             int64
	AttemptNo, Generation                           int
	HoldMaxDurationMS                               int64
	Options                                         []string
	ApprovalLabelCutoffPosition                     sql.NullString
}

func resolveCommandInterruptTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1) (*commandInterruptRow, command.CommandTarget, error) {
	var row commandInterruptRow
	var reason, status, state, nonce, optionsJSON string
	var cutoff sql.NullString
	var attemptNo sql.NullInt64
	var targetKind, targetID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT i.id,i.run_id,i.reason,i.status,i.dispatch_state,i.nonce,i.version,r.version,i.attempt_no,i.hold_max_duration_ms,i.options_json,i.approval_label_cutoff_position,t.target_kind,t.target_id
		FROM interrupts i
		JOIN runs r ON r.id=i.run_id
		LEFT JOIN interrupt_command_targets t ON t.interrupt_id=i.id
		WHERE i.status='open' AND t.target_kind=? AND t.target_id=?
		ORDER BY i.created_at_ms DESC, i.id DESC LIMIT 1`, string(env.Target.Kind), env.Target.ID).
		Scan(&row.ID, &row.RunID, &reason, &status, &state, &nonce, &row.Version, &row.RunVersion, &attemptNo, &row.HoldMaxDurationMS, &optionsJSON, &cutoff, &targetKind, &targetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, command.CommandTarget{}, nil
	}
	if err != nil {
		return nil, command.CommandTarget{}, err
	}
	row.Reason, row.Status, row.DispatchState, row.Nonce = reason, status, state, nonce
	row.ApprovalLabelCutoffPosition = cutoff
	if attemptNo.Valid {
		row.AttemptNo = int(attemptNo.Int64)
	}
	if row.AttemptNo > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM attempts WHERE run_id=? AND attempt_no=?`, row.RunID, row.AttemptNo).Scan(&row.Generation); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, command.CommandTarget{}, err
		}
	}
	var opts []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return nil, command.CommandTarget{}, fmt.Errorf("storage: corrupt interrupt options: %w", err)
	}
	row.Options = make([]string, 0, len(opts))
	for _, o := range opts {
		row.Options = append(row.Options, o.ID)
	}
	binding := command.CommandTarget{}
	if targetKind.Valid && targetID.Valid {
		binding = command.CommandTarget{Kind: command.CommandTargetKind(targetKind.String), ID: targetID.String}
	}
	return &row, binding, nil
}

func interruptView(row *commandInterruptRow) *command.InterruptView {
	var cutoff *string
	if row.ApprovalLabelCutoffPosition.Valid {
		s := row.ApprovalLabelCutoffPosition.String
		cutoff = &s
	}
	return &command.InterruptView{
		ID:                          row.ID,
		RunID:                       row.RunID,
		Version:                     row.Version,
		RunVersion:                  row.RunVersion,
		Reason:                      command.InterruptReason(row.Reason),
		Status:                      command.InterruptStatus(row.Status),
		DispatchState:               command.DispatchState(row.DispatchState),
		Nonce:                       row.Nonce,
		Options:                     row.Options,
		HoldMaxDurationMS:           row.HoldMaxDurationMS,
		ApprovalLabelCutoffPosition: cutoff,
	}
}

// applyCommandEffectTx applies the deterministic reason×action effect for an
// applied (non-startup-retry) command. The command event is already persisted
// (eventID) so Gate re-evaluation operations and command_effects facts can
// reference the close event; nextNonce is the pre-computed hold nonce (empty
// for every other action). The Interrupt close is the terminal CAS for all
// applied actions.
func (d *DB) applyCommandEffectTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID, nextNonce string) error {
	if row == nil {
		return ErrRejectedStale
	}
	switch c.Action {
	case command.ActionReject:
		return d.commandRejectTx(ctx, tx, env, c, row, nowMS)
	case command.ActionHold:
		return d.commandHoldTx(ctx, tx, env, c, nowMS, nextNonce)
	case command.ActionAsk:
		return d.commandAskTx(ctx, tx, env, c, nowMS)
	case command.ActionApprove:
		return d.commandApproveTx(ctx, tx, env, c, row, nowMS, eventID)
	case command.ActionRetry:
		return d.commandRetryTx(ctx, tx, env, c, row, nowMS, eventID)
	}
	return ErrCommandEffectNotWired
}

func (d *DB) commandRejectTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64) error {
	// reject: close/responded; Run -> failed(human_reject). The bound attempt is
	// marked attempt_resolution=reject (write-once); for startup_stall the
	// isolation is retained until the execution body is proven absent, which is
	// owned by the probe/race path.
	if err := d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunFailed, Source: SourceOperator, Actor: actorName(env), FailureReason: "human_reject", OccurredAtMS: nowMS}); err != nil {
		return err
	}
	if row.AttemptNo > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET attempt_resolution='reject',resolution_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND attempt_resolution IS NULL`, nowMS, nowMS, row.RunID, row.AttemptNo); err != nil {
			return err
		}
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionReject, CommandEventID: env.EventKey, InterruptID: c.InterruptID,
		SemanticMaterial: c.RejectReason, NowMS: nowMS,
	}); err != nil {
		return err
	}
	return closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS)
}

func (d *DB) commandHoldTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, nowMS int64, nextNonce string) error {
	expires := nowMS + c.HoldDurationMS
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET nonce=?,version=version+1,dispatch_state='held',held_reason='manual',delivery='held',expires_at_ms=?,next_dispatch_at_ms=NULL,nonce_issued_at_ms=?,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`,
		nextNonce, expires, nowMS, nowMS, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRejectedStale
	}
	if err := excludeStaleBatchMembersTx(ctx, tx, c.InterruptID, nowMS); err != nil {
		return err
	}
	_, err = recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionHold, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	})
	return err
}

func (d *DB) commandAskTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, nowMS int64) error {
	// ask: same-tx task-layer clarification + Ledger semantic material. The Run
	// stays waiting_human; the clarification is recorded, not auto-promoted to
	// project/global Context.
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionAsk, CommandEventID: env.EventKey, InterruptID: c.InterruptID,
		SemanticMaterial: c.AskText, NowMS: nowMS,
	}); err != nil {
		return err
	}
	return closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS)
}

func (d *DB) commandApproveTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string) error {
	switch InterruptReason(row.Reason) {
	case InterruptDesignApproval:
		// approve: close/responded; waiting_human -> queued. The next pending
		// attempt/claim/launch is enqueued by the launch path on the queued Run.
		if err := d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunQueued, Source: SourceOperator, Actor: actorName(env), OccurredAtMS: nowMS}); err != nil {
			return err
		}
	case InterruptGuardrailViolation:
		// guardrail approve: consume the one-time exemption from the immutable
		// binding (command_effects row), close/responded, enqueue exactly one
		// Gate re-evaluation of that binding. Run stays waiting_human behind the
		// pending re-evaluation (storage.md §8.1).
		binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
		if err != nil {
			return err
		}
		return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{
			commandEffectKind: "one_time_exemption", decision: DecisionApprove,
		}, binding)
	case InterruptCodeReview:
		// code_review approve: insert one immutable human-review approval for
		// that binding, close/responded, enqueue exactly one Gate re-evaluation.
		binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
		if err != nil {
			return err
		}
		return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{
			commandEffectKind: "human_review_approval", decision: DecisionApprove,
		}, binding)
	default:
		// design_approval is the only approve-binding Run transition; every
		// other reason's approve remains honestly unwired until its successor
		// contract lands.
		return ErrCommandEffectNotWired
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionApprove, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return err
	}
	return closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS)
}

// gateReEvalEffect describes the optional immutable command_effects fact and
// the HumanDecision action for a Gate re-evaluation command. guardrail and
// code_review approve insert an effect fact; failure_review gate_recheck and
// merge_conflict retry do not.
type gateReEvalEffect struct {
	commandEffectKind string // "" | "one_time_exemption" | "human_review_approval"
	decision          HumanDecisionAction
}

// commandRetryTx wires the non-startup retry effects (command.md §4). It never
// guesses a Change or attempt outside the immutable binding: the binding arm
// selects between a Gate re-evaluation successor (failure_review gate_recheck,
// merge_conflict) and an attempt-terminalization + next-attempt successor
// (failure_review new_attempt, agent_blocked).
func (d *DB) commandRetryTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string) error {
	binding, err := loadInterruptEffectBindingTx(ctx, tx, c.InterruptID)
	if err != nil {
		return err
	}
	switch InterruptReason(row.Reason) {
	case InterruptFailureReview:
		if binding.Arm != "failure_review_attempt" {
			// report_quota_failure_review exposes only reject|hold; a retry never
			// reaches this port. Keep it honest rather than guessing a successor.
			return ErrCommandEffectNotWired
		}
		if binding.RetryKind == string(FailureReviewGateRecheck) {
			return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{decision: DecisionRetry}, binding)
		}
		return d.commandNewAttemptRetryTx(ctx, tx, env, c, row, binding, nowMS)
	case InterruptAgentBlocked:
		// agent_blocked retry terminalizes the bound blocked attempt and creates
		// the next attempt/claim/launch without a Task Spec change (the ask
		// contract's snapshot stays out of scope).
		return d.commandNewAttemptRetryTx(ctx, tx, env, c, row, binding, nowMS)
	case InterruptMergeConflict:
		return d.commandGateReEvalTx(ctx, tx, env, c, row, nowMS, eventID, gateReEvalEffect{decision: DecisionRetry}, binding)
	}
	return ErrCommandEffectNotWired
}

// commandGateReEvalTx is the shared core for guardrail/code_review approve and
// failure_review gate_recheck / merge_conflict retry. It optionally inserts one
// immutable command_effects fact from the frozen binding, records the human
// decision, closes the Interrupt responded and enqueues exactly one Gate
// re-evaluation of the frozen head. The Run stays waiting_human behind the
// pending re-evaluation (storage.md §8.1).
func (d *DB) commandGateReEvalTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64, eventID string, eff gateReEvalEffect, binding commandEffectBinding) error {
	if eff.commandEffectKind != "" {
		if err := insertCommandEffectTx(ctx, tx, c.InterruptID, eventID, row.RunID, eff.commandEffectKind, binding, nowMS); err != nil {
			return err
		}
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: eff.decision, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return err
	}
	if err := closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS); err != nil {
		return err
	}
	snap, err := loadInterruptGateSnapshotTx(ctx, tx, c.InterruptID)
	if err != nil {
		return err
	}
	return enqueueGateReEvaluationTx(ctx, tx, c.InterruptID, eventID, row, snap, binding, nowMS)
}

// commandNewAttemptRetryTx terminalizes the bound attempt and spawns the next
// pending attempt/claim/launch, then closes the Interrupt responded and queues
// the Run. It mirrors the retry-after-absence spawn without the absence proof:
// the human retry decision is recorded by the Ledger, not by attempt_resolution
// (storage.md §1.10 leaves that enum to reject|retry_after_absence).
func (d *DB) commandNewAttemptRetryTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, binding commandEffectBinding, nowMS int64) error {
	if binding.RunID == "" || binding.AttemptNo < 1 || binding.Generation < 1 {
		return ErrCommandEffectNotWired
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionRetry, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return err
	}
	if err := closeInterruptTx(ctx, tx, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce, "responded", nowMS); err != nil {
		return err
	}
	if _, err := d.spawnNextAttemptTx(ctx, tx, row.RunID, binding.AttemptNo, binding.Generation, nowMS); err != nil {
		return err
	}
	return d.transition(ctx, tx, row.RunID, row.RunVersion, DomainCommand{To: RunQueued, Source: SourceOperator, Actor: actorName(env), OccurredAtMS: nowMS})
}

// spawnNextAttemptTx moves the bound attempt out of the live set (so the
// single-live-phase index admits its successor) and creates the next pending
// attempt, its claim and its launch operation from the bound attempt's frozen
// assignment. It is the shared terminalize+spawn helper for human retry; the
// caller owns the Run transition and the human-decision Ledger entry.
func (d *DB) spawnNextAttemptTx(ctx context.Context, tx *sql.Tx, runID string, boundAttemptNo, boundGeneration int, nowMS int64) (int, error) {
	if boundAttemptNo < 1 || boundGeneration < 1 {
		return 0, ErrRejectedStale
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET phase='orphaned',updated_at_ms=? WHERE run_id=? AND attempt_no=? AND phase IN ('pending','starting','spawning','running')`, nowMS, runID, boundAttemptNo); err != nil {
		return 0, err
	}
	newNo := boundAttemptNo + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (run_id,attempt_no,phase,generation,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,isolation_state,created_at_ms,updated_at_ms) SELECT run_id,?,'pending',1,backend,agent_id,task_spec_snapshot_id,worktree_path,branch_name,base_ref,base_sha,'none',?,? FROM attempts WHERE run_id=? AND attempt_no=?`, newNo, nowMS, nowMS, runID, boundAttemptNo); err != nil {
		return 0, err
	}
	key := LaunchOperationKey(runID, newNo, 1)
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_claims (run_id,attempt_no,generation,launch_operation_key,created_at_ms,updated_at_ms) VALUES (?,?,1,?,?,?)`, runID, newNo, key, nowMS, nowMS); err != nil {
		return 0, err
	}
	if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationLaunchAgent, Payload: []byte(`{"schema_version":1}`), RunID: runID, AttemptNo: intPtr(newNo)}, runID, "", nowMS); err != nil {
		return 0, err
	}
	return newNo, nil
}

// commandEffectBinding is the parsed immutable effect binding for the current
// Interrupt (storage.md §6.4). Only the fields consumed by Command effects are
// projected; the frozen binding_json/digest are retained for the Gate
// re-evaluation payload.
type commandEffectBinding struct {
	Arm                        string `json:"arm"`
	RunID                      string `json:"run_id"`
	AttemptNo                  int    `json:"attempt_no"`
	Generation                 int    `json:"generation"`
	ChangeID                   string `json:"change_id"`
	HeadSHA                    string `json:"head_sha"`
	RuleID                     string `json:"rule_id"`
	MatchedPathsDigest         string `json:"matched_paths_digest"`
	ReviewPolicySnapshotDigest string `json:"review_policy_snapshot_digest"`
	ConflictDigest             string `json:"conflict_digest"`
	RetryKind                  string `json:"retry_kind"`
	JSON                       []byte
	Digest                     string
}

// loadInterruptEffectBindingTx reads the immutable one-to-one effect binding
// frozen at emission. Command never reconstructs an arm from the current Run,
// Change, attempt or Forge state.
func loadInterruptEffectBindingTx(ctx context.Context, tx *sql.Tx, interruptID string) (commandEffectBinding, error) {
	var b commandEffectBinding
	var bindingJSON string
	err := tx.QueryRowContext(ctx, `SELECT binding_json,binding_digest FROM interrupt_command_effect_bindings WHERE interrupt_id=?`, interruptID).Scan(&bindingJSON, &b.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return commandEffectBinding{}, ErrCommandEffectNotWired
	}
	if err != nil {
		return commandEffectBinding{}, err
	}
	b.JSON = []byte(bindingJSON)
	if err := json.Unmarshal(b.JSON, &b); err != nil {
		return commandEffectBinding{}, fmt.Errorf("storage: corrupt command effect binding: %w", err)
	}
	return b, nil
}

// interruptGateSnapshot is the frozen previous Gate evaluation identity sourced
// through the Interrupt's calibration. The Gate re-evaluation operation payload
// carries this frozen source identity (storage.md §8.1); Complete alone
// allocates the re-observed successor snapshot.
type interruptGateSnapshot struct {
	SnapshotID, InputHash, GateVersion string
	ChangeID, HeadSHA                  string
}

// loadInterruptGateSnapshotTx resolves the frozen Gate input/evaluation for the
// Interrupt's calibration. It is the sole source of the re-evaluation's
// gate_input_snapshot_id/gate_input_hash/gate_version and exact head.
func loadInterruptGateSnapshotTx(ctx context.Context, tx *sql.Tx, interruptID string) (interruptGateSnapshot, error) {
	var s interruptGateSnapshot
	err := tx.QueryRowContext(ctx, `SELECT s.id,s.gate_input_hash,e.gate_version,json_extract(s.canonical_json,'$.identity.change_id'),s.head_sha
		FROM interrupts i
		JOIN calibration_entries c ON c.id=i.calibration_id
		JOIN gate_evaluations e ON e.id=c.gate_evaluation_id
		JOIN gate_input_snapshots s ON s.id=e.snapshot_id
		WHERE i.id=?`, interruptID).Scan(&s.SnapshotID, &s.InputHash, &s.GateVersion, &s.ChangeID, &s.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return interruptGateSnapshot{}, ErrCommandEffectNotWired
	}
	if err != nil {
		return interruptGateSnapshot{}, err
	}
	return s, nil
}

// enqueueGateReEvaluationTx inserts exactly one pending gate_re_evaluation
// outbox operation (storage.md §8.1) built only from the frozen source
// Interrupt, its Gate snapshot and the immutable effect binding. The operation
// key is keyed by the source Interrupt and the exact head, so it can never be
// reconstructed from the current Change.
func enqueueGateReEvaluationTx(ctx context.Context, tx *sql.Tx, sourceInterruptID, sourceCommandEventID string, row *commandInterruptRow, snap interruptGateSnapshot, binding commandEffectBinding, nowMS int64) error {
	opKey := GateReEvaluationOperationKey(sourceInterruptID, snap.HeadSHA)
	payload, err := canonicalJSON(map[string]any{
		"source_interrupt_id":     sourceInterruptID,
		"source_command_event_id": sourceCommandEventID,
		"source_run_version":      row.RunVersion,
		"run_id":                  row.RunID,
		"attempt_no":              row.AttemptNo,
		"generation":              row.Generation,
		"change_id":               snap.ChangeID,
		"head_sha":                snap.HeadSHA,
		"gate_input_snapshot_id":  snap.SnapshotID,
		"gate_input_hash":         snap.InputHash,
		"gate_version":            snap.GateVersion,
		"effect_binding_digest":   binding.Digest,
		"operation_key":           opKey,
	})
	if err != nil {
		return err
	}
	return insertOperation(ctx, tx, Operation{
		Key: opKey, Kind: OperationGateReEvaluation, Payload: payload,
		RunID: row.RunID, AttemptNo: intPtr(row.AttemptNo), InterruptID: sourceInterruptID,
	}, row.RunID, sourceCommandEventID, nowMS)
}

// insertCommandEffectTx inserts the immutable one-time exemption or
// human-review approval fact consumed by the next Gate snapshot (storage.md
// §6.4). The binding identity guarantees the run/head/rule/path or
// change/head/policy digest; Command never derives these from mutable state.
func insertCommandEffectTx(ctx context.Context, tx *sql.Tx, interruptID, eventID, runID, kind string, binding commandEffectBinding, nowMS int64) error {
	var changeID, headSHA, ruleID, matchedPathsDigest, reviewPolicySnapshotDigest string
	switch kind {
	case "one_time_exemption":
		headSHA, ruleID, matchedPathsDigest = binding.HeadSHA, binding.RuleID, binding.MatchedPathsDigest
	case "human_review_approval":
		changeID, headSHA, reviewPolicySnapshotDigest = binding.ChangeID, binding.HeadSHA, binding.ReviewPolicySnapshotDigest
	default:
		return fmt.Errorf("storage: invalid command effect kind %q", kind)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO command_effects (id,interrupt_id,event_id,effect_kind,run_id,change_id,head_sha,rule_id,matched_paths_digest,review_policy_snapshot_digest,created_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), interruptID, eventID, kind, runID, nullable(changeID), nullable(headSHA), nullable(ruleID), nullable(matchedPathsDigest), nullable(reviewPolicySnapshotDigest), nowMS)
	return err
}

// startupStallRetryRequestTx is the startup_stall retry request (§5). It does
// not close the Interrupt, create an attempt, release isolation, write a
// resolution or ack. It CAS-rotates the nonce, writes the initial retry event
// + pending outcome relation and a pending probe (referencing that event), and
// records the retry HumanDecision. Returns nextNonce and initialEventID.
func (d *DB) startupStallRetryRequestTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, c command.CompiledCommandV1, row *commandInterruptRow, nowMS int64) (string, string, error) {
	if row == nil || row.AttemptNo < 1 || row.Generation < 1 {
		return "", "", ErrRejectedStale
	}
	nextNonce := newToken()
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET nonce=?,version=version+1,dispatch_state='probe_in_progress',held_reason=NULL,nonce_issued_at_ms=?,updated_at_ms=? WHERE id=? AND status='open' AND dispatch_state<>'probe_in_progress' AND version=? AND nonce=? AND reason='startup_stall'`,
		nextNonce, nowMS, nowMS, c.InterruptID, c.ExpectedInterruptVersion, c.Nonce)
	if err != nil {
		return "", "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", "", ErrRejectedStale
	}
	// Initial retry event (outcome=retry_pending). It is both the initial
	// command event and the anchor for the pending probe and outcome relation.
	initialEventID, err := persistCommandEventTx(ctx, tx, env, command.OutcomeRetryPending, command.ActionRetry, row.RunID, c.InterruptID, nextNonce, "", nowMS, "")
	if err != nil {
		return "", "", err
	}
	// Pending probe referencing the initial event. The worker runs after commit.
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_probes (id,run_id,attempt_no,interrupt_id,state,expected_run_version,expected_generation,requested_by_event_id,created_at_ms) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		newID(), row.RunID, row.AttemptNo, c.InterruptID, row.RunVersion, row.Generation, initialEventID, nowMS); err != nil {
		return "", "", err
	}
	if _, err := recordHumanDecisionTx(ctx, tx, RecordHumanDecisionCmd{
		Action: DecisionRetry, CommandEventID: env.EventKey, InterruptID: c.InterruptID, NowMS: nowMS,
	}); err != nil {
		return "", "", err
	}
	return nextNonce, initialEventID, nil
}

// --- persistence helpers ---

func persistCommandEventTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, outcome command.CommandOutcome, action command.CommandAction, runID, interruptID, nextNonce, finalForEventID string, nowMS int64, eventID string) (string, error) {
	ev := command.NewEvent(env, outcome, action, runID, interruptID, nextNonce, finalForEventID)
	body, err := ev.CanonicalBytes()
	if err != nil {
		return "", err
	}
	// eventID is pre-generated by the caller when a deterministic effect (Gate
	// re-evaluation operation, command_effects fact) must reference the close
	// event inside the same transaction; otherwise allocate here.
	if eventID == "" {
		eventID = newID()
	}
	idem := command.EventStageKey(env.EventKey, command.StageInitial)
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,run_id,project_id,type,source,payload_schema_version,payload_json,idempotency_key,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,'command.event','forge',1,?,?,?,?)`,
		eventID, nullable(runID), env.ProjectID, string(body), idem, nowMS, nowMS); err != nil {
		return "", err
	}
	if outcome == command.OutcomeRetryPending {
		if _, err := tx.ExecContext(ctx, `INSERT INTO command_event_outcomes (id,event_key,initial_event_id,final_event_id,state,created_at_ms,finalized_at_ms) VALUES (?,?,?,NULL,'pending',?,NULL)`,
			newID(), env.EventKey, eventID, nowMS); err != nil {
			return "", err
		}
		return eventID, nil
	}
	// final in one transaction (non-retry: initial == final).
	if _, err := tx.ExecContext(ctx, `INSERT INTO command_event_outcomes (id,event_key,initial_event_id,final_event_id,state,created_at_ms,finalized_at_ms) VALUES (?,?,?,?, 'final', ?, ?)`,
		newID(), env.EventKey, eventID, eventID, nowMS, nowMS); err != nil {
		return "", err
	}
	return eventID, nil
}

func writeCommandAckOpTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, outcome command.CommandOutcome, action command.CommandAction, finalEventID, interruptID, runID, nextNonce, ackKey string, nowMS int64) error {
	ev := command.NewEvent(env, outcome, action, runID, interruptID, nextNonce, "")
	ack := command.NewAck(finalEventID, ev)
	body, err := ack.CanonicalBytes()
	if err != nil {
		return err
	}
	op := Operation{Key: ackKey, Kind: OperationCommandAck, Payload: body, InterruptID: interruptID, RunID: runID}
	return insertOperation(ctx, tx, op, runID, interruptID, nowMS)
}

func closeInterruptTx(ctx context.Context, tx *sql.Tx, interruptID string, expectedVersion int64, expectedNonce, closeReason string, nowMS int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET status='closed',close_reason=?,closed_at_ms=?,version=version+1,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`, closeReason, nowMS, nowMS, interruptID, expectedVersion, expectedNonce)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRejectedStale
	}
	return excludeStaleBatchMembersTx(ctx, tx, interruptID, nowMS)
}

func actorName(env command.CommandEventEnvelopeV1) string {
	if env.Actor == nil {
		return ""
	}
	return *env.Actor
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("storage: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func lookupCommandReceiptTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1) (ApplyCommandEventResult, bool, error) {
	var disposition, domainEventID string
	var outcomeID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT disposition,domain_event_id,command_outcome_id FROM command_receipts WHERE project_id=? AND event_kind=? AND remote_event_id=?`, env.ProjectID, string(env.Source), env.RemoteEventID).
		Scan(&disposition, &domainEventID, &outcomeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ApplyCommandEventResult{}, false, nil
	}
	if err != nil {
		return ApplyCommandEventResult{}, false, err
	}
	if disposition != "accepted" {
		return ApplyCommandEventResult{Ignored: true}, true, nil
	}
	res := ApplyCommandEventResult{FinalEventID: domainEventID}
	if outcomeID.Valid {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM command_event_outcomes WHERE id=?`, outcomeID.String).Scan(&state); err == nil && state == "pending" {
			res.Outcome = command.OutcomeRetryPending
			return res, true, nil
		}
	}
	res.Outcome = command.OutcomeApplied
	return res, true, nil
}

func writeIgnoredCommandReceiptTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, disposition string, nowMS int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO command_receipts (id,project_id,event_kind,remote_event_id,event_key,target_kind,target_id,actor,raw_digest,disposition,domain_event_id,command_outcome_id,observed_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,NULL,?)`,
		newID(), env.ProjectID, string(env.Source), env.RemoteEventID, env.EventKey, string(env.Target.Kind), env.Target.ID, nullablePtr(env.Actor), env.RawDigest, disposition, nowMS)
	return err
}

func writeAcceptedCommandReceiptTx(ctx context.Context, tx *sql.Tx, env command.CommandEventEnvelopeV1, disposition, domainEventID string, nowMS int64) error {
	var outcomeID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id FROM command_event_outcomes WHERE event_key=?`, env.EventKey).Scan(&outcomeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO command_receipts (id,project_id,event_kind,remote_event_id,event_key,target_kind,target_id,actor,raw_digest,disposition,domain_event_id,command_outcome_id,observed_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), env.ProjectID, string(env.Source), env.RemoteEventID, env.EventKey, string(env.Target.Kind), env.Target.ID, nullablePtr(env.Actor), env.RawDigest, disposition, nullable(domainEventID), nullableNullString(outcomeID), nowMS)
	return err
}

func nullablePtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
func nullableNullString(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}
