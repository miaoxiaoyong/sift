package storage

import (
	"context"
	"database/sql"
	"fmt"
)

type AdvanceKind string

const (
	AdvanceExpiry   AdvanceKind = "expiry"
	AdvanceDispatch AdvanceKind = "dispatch"
)

type AdvanceInterruptCmd struct {
	InterruptID     string
	ExpectedVersion int64
	ExpectedNonce   string
	Kind            AdvanceKind
	NowMS           int64
}

func (d *DB) AdvanceInterrupt(ctx context.Context, cmd AdvanceInterruptCmd) (bool, error) {
	if cmd.InterruptID == "" || cmd.ExpectedVersion < 1 || cmd.ExpectedNonce == "" || cmd.NowMS <= 0 || (cmd.Kind != AdvanceExpiry && cmd.Kind != AdvanceDispatch) {
		return false, fmt.Errorf("%w: invalid advance command", ErrInterruptRejected)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status, state, held, nonce, reason, base, onExpire, onMax, channel, snapshot, zone, summary, delivery string
	var version, expiresAt, expiresAfter, window int64
	var nextDispatch sql.NullInt64
	var escalation, max, total, perRun int
	var downgraded bool
	err = tx.QueryRowContext(ctx, `SELECT status,dispatch_state,COALESCE(held_reason,''),nonce,reason,base_severity,on_expire,on_max_escalations,COALESCE(channel_id,''),COALESCE(channel_snapshot_json,''),delivery,day_timezone,daily_summary_at,version,expires_at_ms,expires_after_ms,escalation_count,max_escalations,suggested_downgrade,critical_window_ms,critical_total_limit,critical_per_run_limit,next_dispatch_at_ms FROM interrupts WHERE id=?`, cmd.InterruptID).Scan(&status, &state, &held, &nonce, &reason, &base, &onExpire, &onMax, &channel, &snapshot, &delivery, &zone, &summary, &version, &expiresAt, &expiresAfter, &escalation, &max, &downgraded, &window, &total, &perRun, &nextDispatch)
	if err == sql.ErrNoRows || status != "open" || version != cmd.ExpectedVersion || nonce != cmd.ExpectedNonce {
		return false, ErrRejectedStale
	}
	if err != nil {
		return false, err
	}
	if cmd.Kind == AdvanceDispatch {
		if state != "ready" || expiresAt <= cmd.NowMS {
			return false, ErrRejectedStale
		}
		res, err := tx.ExecContext(ctx, `UPDATE interrupts SET dispatch_state='batched',next_dispatch_at_ms=NULL,version=version+1,updated_at_ms=? WHERE id=? AND status='open' AND dispatch_state='ready' AND version=? AND nonce=?`, cmd.NowMS, cmd.InterruptID, cmd.ExpectedVersion, cmd.ExpectedNonce)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, ErrRejectedStale
		}
		if delivery == "immediate" {
			priority := "normal"
			if escalation > 0 {
				priority = "strong"
			}
			if err := enqueueInterruptChannelTx(ctx, tx, cmd.InterruptID, cmd.ExpectedVersion+1, nonce, escalation, priority, cmd.NowMS); err != nil {
				return false, err
			}
		} else if !nextDispatch.Valid {
			return false, fmt.Errorf("%w: batched interrupt lacks frozen summary due", ErrInterruptRejected)
		} else if err := addDailyBatchMemberAtTx(ctx, tx, cmd.InterruptID, cmd.ExpectedVersion+1, nonce, nextDispatch.Int64, cmd.NowMS, channel, snapshot); err != nil {
			return false, err
		}
		return finishAdvance(ctx, tx, res, cmd, "interrupt.dispatched")
	}
	if state == "probe_in_progress" || (state == "held" && held != "manual") || expiresAt > cmd.NowMS {
		return false, ErrRejectedStale
	}
	if onExpire == string(ExpireHold) {
		return d.holdAdvance(ctx, tx, cmd, "expiry", "interrupt.expired")
	}
	if onExpire == string(ExpireAutoReject) {
		return d.closeExpiredInterrupt(ctx, tx, cmd)
	}
	if escalation >= max {
		if onMax == string(ExpireAutoReject) && reason != string(InterruptStartupStall) {
			return d.closeExpiredInterrupt(ctx, tx, cmd)
		}
		return d.holdAdvance(ctx, tx, cmd, "max_escalations", "interrupt.max_escalations")
	}
	next := InterruptSeverity(base)
	for i := 0; i <= escalation; i++ {
		next = promoteSeverity(next)
	}
	// Escalation reuses the frozen downgrade decision through the same
	// Severity(...) entry; it is applied once, never re-derived from T6.
	next = Severity(next, downgraded)
	newNonce := newID()
	nextState, delivery, heldReason := "ready", "batch", ""
	var due any
	if next == SeverityHigh || next == SeverityCritical {
		delivery = "immediate"
		due = cmd.NowMS
	} else if at, ok := nextSummary(cmd.NowMS, zone, summary); ok && at < cmd.NowMS+expiresAfter {
		due = at
	} else {
		nextState, delivery, heldReason = "held", "held", "batch_after_expiry"
	}
	var fusedAdmission string
	if next == SeverityCritical {
		admitted, admissionID, scope, err := admitCriticalTx(ctx, tx, cmd.InterruptID, next, cmd.NowMS, "escalation", window, total, perRun)
		if err != nil {
			return false, err
		}
		if !admitted {
			fusedAdmission = admissionID + ":" + scope
			nextState, delivery, heldReason, due = "batched", "batch", "", nil
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET severity=?,nonce=?,nonce_issued_at_ms=?,version=version+1,escalation_count=escalation_count+1,expires_at_ms=?,dispatch_state=?,delivery=?,held_reason=?,next_dispatch_at_ms=?,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`, next, newNonce, cmd.NowMS, cmd.NowMS+expiresAfter, nextState, delivery, nullable(heldReason), due, cmd.NowMS, cmd.InterruptID, cmd.ExpectedVersion, cmd.ExpectedNonce)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, ErrRejectedStale
	}
	if next == SeverityCritical && fusedAdmission != "" {
		if err := addCriticalBatchMemberTx(ctx, tx, cmd.InterruptID, cmd.ExpectedVersion+1, newNonce, fusedAdmission, channel, snapshot, cmd.NowMS); err != nil {
			return false, err
		}
	}
	if err := excludeStaleBatchMembersTx(ctx, tx, cmd.InterruptID, cmd.NowMS); err != nil {
		return false, err
	}
	return finishAdvance(ctx, tx, res, cmd, "interrupt.escalated")
}

func (d *DB) holdAdvance(ctx context.Context, tx *sql.Tx, cmd AdvanceInterruptCmd, reason, event string) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET dispatch_state='held',delivery='held',held_reason=?,next_dispatch_at_ms=NULL,version=version+1,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`, reason, cmd.NowMS, cmd.InterruptID, cmd.ExpectedVersion, cmd.ExpectedNonce)
	if err != nil {
		return false, err
	}
	if err := excludeStaleBatchMembersTx(ctx, tx, cmd.InterruptID, cmd.NowMS); err != nil {
		return false, err
	}
	return finishAdvance(ctx, tx, res, cmd, event)
}

func (d *DB) closeExpiredInterrupt(ctx context.Context, tx *sql.Tx, cmd AdvanceInterruptCmd) (bool, error) {
	var runID, status, reason, binding, bindingDigest string
	var runVersion, bindingSchema int64
	if err := tx.QueryRowContext(ctx, `SELECT r.id,r.status,r.version,i.reason,b.binding_schema_version,b.binding_json,b.binding_digest FROM interrupts i JOIN runs r ON r.id=i.run_id JOIN interrupt_command_effect_bindings b ON b.interrupt_id=i.id WHERE i.id=?`, cmd.InterruptID).Scan(&runID, &status, &runVersion, &reason, &bindingSchema, &binding, &bindingDigest); err != nil {
		return false, err
	}
	armName, err := validateEffectBinding(ctx, tx, cmd.InterruptID, reason, runID, bindingSchema, binding, bindingDigest)
	if err != nil {
		return false, err
	}
	noTransition := armName == "report_quota_failure_review"
	if !noTransition {
		if RunStatus(status) != RunWaitingHuman {
			return false, ErrRejectedStale
		}
		if err := d.transition(ctx, tx, runID, runVersion, DomainCommand{To: RunFailed, Source: SourceSystem, Actor: "advance_interrupt", FailureReason: "hitl_expired", OccurredAtMS: cmd.NowMS}); err != nil {
			return false, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE interrupts SET status='closed',close_reason='expired_auto_reject',closed_at_ms=?,version=version+1,updated_at_ms=? WHERE id=? AND status='open' AND version=? AND nonce=?`, cmd.NowMS, cmd.NowMS, cmd.InterruptID, cmd.ExpectedVersion, cmd.ExpectedNonce)
	if err != nil {
		return false, err
	}
	if err := excludeStaleBatchMembersTx(ctx, tx, cmd.InterruptID, cmd.NowMS); err != nil {
		return false, err
	}
	return finishAdvance(ctx, tx, res, cmd, "interrupt.expired_auto_reject")
}

func excludeStaleBatchMembersTx(ctx context.Context, tx *sql.Tx, interruptID string, nowMS int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE attention_batch_members AS m
		SET excluded_at_ms=?
		WHERE m.interrupt_id=? AND m.excluded_at_ms IS NULL
			AND EXISTS (SELECT 1 FROM attention_batches b WHERE b.id=m.batch_id AND b.state='collecting')
			AND NOT EXISTS (
				SELECT 1 FROM attention_batch_member_authority a
				JOIN interrupts i ON i.id=m.interrupt_id
				WHERE a.batch_id=m.batch_id AND a.interrupt_id=m.interrupt_id
					AND i.status='open' AND i.version=a.interrupt_version AND i.nonce=a.nonce
			)`, nowMS, interruptID)
	return err
}
