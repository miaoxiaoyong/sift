package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// EmitInterrupt is the sole creation port. Its transaction contains the Run
// transition, budget charge, Interrupt, audit event and forge-comment outbox.
func (d *DB) EmitInterrupt(ctx context.Context, cmd EmitInterruptCmd) (Interrupt, error) {
	if cmd.CalibrationID != "" {
		return Interrupt{}, fmt.Errorf("%w: calibration binding requires gate recorder", ErrInterruptRejected)
	}
	return d.emitInterrupt(ctx, cmd, nil)
}

// emitInterrupt keeps the M3 emission sequence in one transaction while
// allowing the Gate recorder to append its frozen evidence before the
// Interrupt is inserted. The callback must not perform external IO.
func (d *DB) emitInterrupt(ctx context.Context, cmd EmitInterruptCmd, before func(*sql.Tx) error) (Interrupt, error) {
	return d.emitInterruptHooks(ctx, cmd, before, nil, false)
}

func (d *DB) emitReportInterruptHooks(ctx context.Context, cmd EmitInterruptCmd, before func(*sql.Tx) error, after func(*sql.Tx, Interrupt) error) (Interrupt, error) {
	return d.emitInterruptHooks(ctx, cmd, before, after, true)
}

func (d *DB) emitInterruptHooks(ctx context.Context, cmd EmitInterruptCmd, before func(*sql.Tx) error, after func(*sql.Tx, Interrupt) error, reportOnly bool) (Interrupt, error) {
	prep, cmd, err := d.prepareInterruptEmission(ctx, cmd, reportOnly)
	if err != nil {
		return Interrupt{}, err
	}
	if existing, found, err := d.interruptByKey(ctx, prep.key); err != nil {
		return Interrupt{}, err
	} else if found {
		return existing, nil
	}
	if err := d.refineInterruptEmission(ctx, cmd, &prep); err != nil {
		return Interrupt{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, err
	}
	defer tx.Rollback()
	if before != nil {
		if err := before(tx); err != nil {
			return Interrupt{}, err
		}
	}
	if existing, found, err := interruptByKeyTx(ctx, tx, prep.key); err != nil {
		return Interrupt{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return Interrupt{}, err
		}
		return existing, nil
	}
	in, err := d.insertInterruptEmissionTx(ctx, tx, cmd, prep, after, reportOnly, true)
	if err != nil {
		return Interrupt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, err
	}
	d.wakeOutbox()
	return in, nil
}

// interruptEffectBinding is the immutable closed source/effect discriminator
// consumed by later command/expiry code. Its JSON is deliberately persisted,
// rather than reconstructing an arm from the current Run or configuration.
func interruptEffectBinding(cmd EmitInterruptCmd) ([]byte, string) {
	if cmd.FailureReviewVariant == FailureReviewReportQuota {
		b, _ := json.Marshal(map[string]any{
			"arm": "report_quota_failure_review", "run_id": cmd.RunID,
			"daily_bucket_start_ms": cmd.Generation.ReportDailyBucketStartMS,
			"daily_bucket_end_ms":   cmd.Generation.ReportDailyBucketEndMS,
			"security_event_id":     cmd.Generation.SecurityEventID,
		})
		return b, "failure_review"
	}
	if cmd.Reason == InterruptFailureReview {
		retryKind := cmd.FailureReviewRetryKind
		if retryKind == "" {
			retryKind = FailureReviewNewAttempt
		}
		fields := map[string]any{"arm": "failure_review_attempt", "run_id": cmd.RunID, "attempt_no": *cmd.AttemptNo, "generation": cmd.Generation.Generation, "retry_kind": retryKind}
		if retryKind == FailureReviewGateRecheck {
			fields["change_id"], fields["head_sha"] = cmd.Generation.ChangeID, cmd.Generation.HeadSHA
			fields["terminal_attempt_no"], fields["terminal_generation"] = nil, nil
		} else {
			fields["change_id"], fields["head_sha"] = nil, nil
			fields["terminal_attempt_no"], fields["terminal_generation"] = *cmd.AttemptNo, cmd.Generation.Generation
		}
		b, _ := json.Marshal(fields)
		return b, "failure_review"
	}
	fields := map[string]any{"arm": string(cmd.Reason), "run_id": cmd.RunID}
	switch cmd.Reason {
	case InterruptDesignApproval:
		fields["task_spec_snapshot_id"] = cmd.Generation.TaskSpecSnapshotID
	case InterruptGuardrailViolation:
		fields["head_sha"], fields["rule_id"], fields["matched_paths_digest"] = cmd.Generation.HeadSHA, cmd.Generation.ViolationCode, cmd.Generation.SubjectDigest
	case InterruptCodeReview:
		delete(fields, "run_id")
		fields["change_id"], fields["head_sha"], fields["review_policy_snapshot_digest"] = cmd.Generation.ChangeID, cmd.Generation.HeadSHA, cmd.Generation.PolicySnapshotID
	case InterruptAgentBlocked:
		fields["attempt_no"], fields["generation"], fields["report_id"] = cmd.Generation.AttemptNo, cmd.Generation.Generation, cmd.Generation.ReportID
	case InterruptMergeConflict:
		delete(fields, "run_id")
		fields["change_id"], fields["head_sha"], fields["conflict_digest"] = cmd.Generation.ChangeID, cmd.Generation.HeadSHA, cmd.Generation.ConflictDigest
	case InterruptStartupStall:
		fields["attempt_no"], fields["generation"] = cmd.Generation.AttemptNo, cmd.Generation.Generation
	}
	b, _ := json.Marshal(fields)
	return b, string(cmd.Reason)
}
