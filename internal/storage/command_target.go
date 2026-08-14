package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xsift/sift/internal/command"
)

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
