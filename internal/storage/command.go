package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

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
