package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// interruptEmissionPrepared holds EmitInterrupt facts derived before the write
// transaction. prepareInterruptEmission fixes the template, brief/links,
// generation key and escalation-based severity; refineInterruptEmission fills
// the T4 headline/brief, base severity and T6 dispatch only after a dedup miss,
// so a replay never invokes the advisory providers.
type interruptEmissionPrepared struct {
	template                          interruptTemplate
	headline, brief                   string
	links                             []InterruptLink
	key                               string
	baseSeverity, severity            InterruptSeverity
	dispatch                          interruptDispatch
}

func (d *DB) prepareInterruptEmission(ctx context.Context, cmd EmitInterruptCmd, reportOnly bool) (interruptEmissionPrepared, EmitInterruptCmd, error) {
	if reportOnly && (cmd.Reason != InterruptAgentBlocked || cmd.Source != SourceAgent) {
		return interruptEmissionPrepared{}, cmd, fmt.Errorf("%w: report-only transition is invalid", ErrInterruptRejected)
	}
	if err := validateFailureReviewVariant(cmd); err != nil {
		return interruptEmissionPrepared{}, cmd, err
	}
	t, ok := interruptTemplateFor(cmd)
	if !ok {
		return interruptEmissionPrepared{}, cmd, fmt.Errorf("%w: unknown reason", ErrInterruptRejected)
	}
	if cmd.RunID == "" || cmd.NowMS <= 0 || !validSource(cmd.Source) {
		return interruptEmissionPrepared{}, cmd, fmt.Errorf("%w: run, source and timestamp are required", ErrInterruptRejected)
	}
	if cmd.Reason == InterruptStartupStall && cmd.AttemptNo == nil {
		return interruptEmissionPrepared{}, cmd, fmt.Errorf("%w: startup_stall requires attempt", ErrInterruptRejected)
	}
	if cmd.ExpiresAfterMS == 0 {
		cmd.ExpiresAfterMS = t.expires
	}
	if cmd.OnExpire == "" {
		cmd.OnExpire = t.onExpire
	}
	if cmd.OnMaxEscalations == "" {
		cmd.OnMaxEscalations = ExpireHold
	}
	if cmd.ExpiresAfterMS <= 0 || (cmd.OnExpire != ExpireHold && cmd.OnExpire != ExpireEscalate && cmd.OnExpire != ExpireAutoReject) || (cmd.OnMaxEscalations != ExpireHold && cmd.OnMaxEscalations != ExpireAutoReject) || (cmd.Reason == InterruptStartupStall && (cmd.OnExpire == ExpireAutoReject || cmd.OnMaxEscalations == ExpireAutoReject)) {
		return interruptEmissionPrepared{}, cmd, fmt.Errorf("%w: invalid expiry policy", ErrInterruptRejected)
	}
	brief, links, err := renderInterrupt(t, cmd.Facts, cmd.Reason)
	if err != nil {
		return interruptEmissionPrepared{}, cmd, err
	}
	if !canonicalRecommendedAction(t.options, cmd.Facts["recommended_action"]) {
		return interruptEmissionPrepared{}, cmd, fmt.Errorf("%w: recommended_action is not a canonical option", ErrInterruptRejected)
	}
	severity, err := BaseSeverity(cmd.Reason, cmd.GatePhase, cmd.GuardrailLevel, cmd.EscalationCount, cmd.MaxEscalations)
	if err != nil {
		return interruptEmissionPrepared{}, cmd, err
	}
	key, err := interruptGenerationKeyFor(cmd)
	if err != nil {
		return interruptEmissionPrepared{}, cmd, err
	}
	return interruptEmissionPrepared{
		template: t, headline: t.headline, brief: brief, links: links, key: key,
		severity: severity,
	}, cmd, nil
}

// refineInterruptEmission runs the advisory T4 headline/brief refinement and
// the T6 severity/channel dispatch (both outside the five-write insert). It runs
// only after a dedup miss, so a replay — the standalone pre-tx lookup or a
// nested in-tx lookup that finds an existing Interrupt — never invokes the
// T4/T6 providers. baseSeverity is the zero-escalation origin persisted for
// later expiry re-derivation; severity becomes the T6-admitted final value.
func (d *DB) refineInterruptEmission(ctx context.Context, cmd EmitInterruptCmd, prep *interruptEmissionPrepared) error {
	t := prep.template
	if t4 := d.interruptT4Caller(); t4 != nil {
		candidate := InterruptT4Input{RunID: cmd.RunID, AttemptNo: cmd.AttemptNo, Reason: cmd.Reason, Severity: prep.severity, Modality: t.modality, Headline: t.headline, Brief: prep.brief, Fragments: interruptBriefFragments(t, cmd.Facts), Links: prep.links, Options: t.options}
		if out, callErr := t4(ctx, candidate); callErr == nil {
			if accepted, rendered := acceptInterruptT4(candidate, out); accepted {
				headline, brief := out.Headline, rendered
				prep.headline, prep.brief = headline, brief
			}
		}
	}
	baseSeverity, err := BaseSeverity(cmd.Reason, cmd.GatePhase, cmd.GuardrailLevel, 0, cmd.MaxEscalations)
	if err != nil {
		return err
	}
	prep.baseSeverity = baseSeverity
	if cmd.T6 == nil {
		cmd.T6 = d.interruptT6Caller()
	}
	dispatch, err := admitInterruptT6(ctx, cmd, t.modality, prep.severity, cmd.NowMS+cmd.ExpiresAfterMS)
	if err != nil {
		return err
	}
	prep.dispatch = dispatch
	prep.severity = dispatch.severity
	return nil
}

// emitInterruptInExistingTx performs the five EmitInterrupt writes inside a
// caller-owned transaction. Generation-key dedup is checked inside the tx first;
// T4/T6 run only on a miss, then the insert writes. A nested caller must not
// roll back the outer tx on unique-key races, so ownTx is false and such a race
// surfaces as an error to the caller.
func (d *DB) emitInterruptInExistingTx(ctx context.Context, tx *sql.Tx, cmd EmitInterruptCmd, reportOnly bool) (Interrupt, error) {
	prep, cmd, err := d.prepareInterruptEmission(ctx, cmd, reportOnly)
	if err != nil {
		return Interrupt{}, err
	}
	if existing, found, err := interruptByKeyTx(ctx, tx, prep.key); err != nil {
		return Interrupt{}, err
	} else if found {
		return existing, nil
	}
	if err := d.refineInterruptEmission(ctx, cmd, &prep); err != nil {
		return Interrupt{}, err
	}
	return d.insertInterruptEmissionTx(ctx, tx, cmd, prep, nil, reportOnly, false)
}

func (d *DB) insertInterruptEmissionTx(ctx context.Context, tx *sql.Tx, cmd EmitInterruptCmd, prep interruptEmissionPrepared, after func(*sql.Tx, Interrupt) error, reportOnly, ownTx bool) (Interrupt, error) {
	t := prep.template
	key := prep.key
	headline, brief, links := prep.headline, prep.brief, prep.links
	severity, baseSeverity, dispatch := prep.severity, prep.baseSeverity, prep.dispatch

	var status string
	var version int64
	var projectID, forgeKind, forgeHost, forgeProject string
	var issueID, issueURL, changeID, targetKind, targetID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status,version,project_id,forge_kind,forge_host,forge_project_key,issue_id,issue_url,change_id,discussion_target_kind,discussion_target_id FROM runs WHERE id=?`, cmd.RunID).Scan(&status, &version, &projectID, &forgeKind, &forgeHost, &forgeProject, &issueID, &issueURL, &changeID, &targetKind, &targetID); err != nil {
		return Interrupt{}, err
	}
	if version != cmd.ExpectedRunVersion {
		return Interrupt{}, ErrRejectedStale
	}
	if cmd.FailureReviewVariant == FailureReviewReportQuota {
		if RunStatus(status) != RunRunning {
			return Interrupt{}, fmt.Errorf("%w: report quota run is not running", ErrInterruptRejected)
		}
		var endMS int64
		var eventID string
		var digest, generationKey, source string
		if err := tx.QueryRowContext(ctx, `SELECT daily_bucket_end_ms,security_event_id,failure_digest,generation_key FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, cmd.Generation.ReportDailyBucketStartMS).Scan(&endMS, &eventID, &digest, &generationKey); err != nil {
			return Interrupt{}, fmt.Errorf("%w: report quota exhaustion binding: %v", ErrInterruptRejected, err)
		}
		if endMS != cmd.Generation.ReportDailyBucketEndMS || eventID != cmd.Generation.SecurityEventID || digest != cmd.Generation.FailureDigest || generationKey != key {
			return Interrupt{}, fmt.Errorf("%w: report quota exhaustion binding mismatch", ErrInterruptRejected)
		}
		if err := tx.QueryRowContext(ctx, `SELECT source FROM events WHERE id=?`, eventID).Scan(&source); err != nil || source != string(SourceSystem) {
			return Interrupt{}, fmt.Errorf("%w: report quota security event binding", ErrInterruptRejected)
		}
	} else if cmd.FailureReviewVariant == FailureReviewAttempt {
		var phase string
		var exitCode sql.NullInt64
		var signal sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT phase,result_exit_code,result_signal FROM attempts WHERE run_id=? AND attempt_no=? AND generation=?`, cmd.RunID, *cmd.AttemptNo, cmd.Generation.Generation).Scan(&phase, &exitCode, &signal); err != nil {
			return Interrupt{}, fmt.Errorf("%w: failure_review attempt binding: %v", ErrInterruptRejected, err)
		}
		if cmd.FailureReviewRetryKind != FailureReviewGateRecheck && (phase != "finished" || (!exitCode.Valid || exitCode.Int64 == 0) && !signal.Valid) {
			return Interrupt{}, fmt.Errorf("%w: failure_review requires failed attempt generation", ErrInterruptRejected)
		}
		if RunStatus(status) != RunWaitingHuman {
			if !legalTransition(RunStatus(status), RunWaitingHuman) {
				return Interrupt{}, fmt.Errorf("%w: %s cannot wait for human", ErrInterruptRejected, status)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='waiting_human',version=version+1,updated_at_ms=? WHERE id=? AND version=?`, cmd.NowMS, cmd.RunID, version); err != nil {
				return Interrupt{}, err
			}
		}
	} else if !reportOnly && RunStatus(status) != RunWaitingHuman {
		if !legalTransition(RunStatus(status), RunWaitingHuman) {
			return Interrupt{}, fmt.Errorf("%w: %s cannot wait for human", ErrInterruptRejected, status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='waiting_human',version=version+1,updated_at_ms=? WHERE id=? AND version=?`, cmd.NowMS, cmd.RunID, version); err != nil {
			return Interrupt{}, err
		}
	}
	if cmd.Reason == InterruptStartupStall {
		if cmd.Generation.AttemptNo != *cmd.AttemptNo || cmd.Generation.Generation < 1 {
			return Interrupt{}, fmt.Errorf("%w: startup_stall attempt identity mismatch", ErrInterruptRejected)
		}
		diagnostic := cmd.Facts["diagnostic_cause"]
		if diagnostic != "process_identity_unknown" && diagnostic != "termination_unconfirmed" && diagnostic != "process_group_unverified" {
			return Interrupt{}, fmt.Errorf("%w: invalid startup_stall diagnostic cause", ErrInterruptRejected)
		}
		var generation int
		var isolation string
		if err := tx.QueryRowContext(ctx, `SELECT generation,isolation_state FROM attempts WHERE run_id=? AND attempt_no=?`, cmd.RunID, *cmd.AttemptNo).Scan(&generation, &isolation); err != nil {
			return Interrupt{}, err
		}
		if generation != cmd.Generation.Generation {
			return Interrupt{}, ErrRejectedStale
		}
		if isolation == "none" {
			if _, err := tx.ExecContext(ctx, `UPDATE attempts SET isolation_state='frozen',isolation_reason=?,isolated_at_ms=?,updated_at_ms=? WHERE run_id=? AND attempt_no=? AND isolation_state='none'`, diagnostic, cmd.NowMS, cmd.NowMS, cmd.RunID, *cmd.AttemptNo); err != nil {
				return Interrupt{}, err
			}
		} else if isolation != "frozen" {
			return Interrupt{}, fmt.Errorf("%w: invalid attempt isolation state", ErrInterruptRejected)
		}
	}
	kind, id := "issue", issueID.String
	if id == "" {
		kind, id = "change", changeID.String
	}
	if id == "" {
		kind, id = targetKind.String, targetID.String
	}
	if id == "" {
		return Interrupt{}, fmt.Errorf("%w: interrupt_publish_target_missing", ErrInterruptRejected)
	}
	if issueURL.Valid && validLink(issueURL.String) {
		links = append(links, InterruptLink{Label: "Issue", Target: issueURL.String})
		sort.Slice(links, func(i, j int) bool {
			if links[i].Target == links[j].Target {
				return links[i].Label < links[j].Label
			}
			return links[i].Target < links[j].Target
		})
	}
	entryID, err := chargeAttentionTx(ctx, tx, cmd, severity)
	quotaBatched := errors.Is(err, ErrAttentionQuotaExceeded)
	if err != nil && !quotaBatched {
		return Interrupt{}, err
	}
	if quotaBatched {
		if dispatch.channelID == "" || dispatch.channelSnapshot == "" {
			return Interrupt{}, fmt.Errorf("%w: quota batch lacks channel", ErrInterruptRejected)
		}
		at, ok := nextSummary(cmd.NowMS, timezoneOrUTC(cmd.DayTimezone), summaryOrDefault(cmd.DailySummaryAt))
		if !ok || at >= cmd.NowMS+cmd.ExpiresAfterMS {
			return Interrupt{}, fmt.Errorf("%w: quota batch after expiry", ErrInterruptRejected)
		}
		dispatch.state, dispatch.delivery, dispatch.heldReason, dispatch.nextDispatchAtMS = "batched", "batch", "", nil
	}
	in := Interrupt{ID: newID(), RunID: cmd.RunID, AttemptNo: cmd.AttemptNo, GenerationKey: key, Reason: cmd.Reason, Severity: severity, Headline: headline, Brief: brief, Options: t.options, MinModality: t.modality, Links: links, ExpiresAtMS: cmd.NowMS + cmd.ExpiresAfterMS, OnExpire: cmd.OnExpire, ChargedBudgetEntryID: entryID, ChannelID: dispatch.channelID, Delivery: dispatch.delivery, SuggestedDowngrade: dispatch.suggestedDowngrade, NextDispatchAtMS: dispatch.nextDispatchAtMS, HeldReason: dispatch.heldReason}
	optionsJSON, _ := json.Marshal(in.Options)
	linksJSON, _ := json.Marshal(in.Links)
	nonce := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO interrupts (id,run_id,attempt_no,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,nonce,version,status,dispatch_state,channel_id,channel_snapshot_json,delivery,suggested_downgrade,next_dispatch_at_ms,held_reason,expires_at_ms,on_expire,escalation_count,max_escalations,charged_budget_entry_id,calibration_id,created_at_ms,updated_at_ms,expires_after_ms,on_max_escalations,base_severity,nonce_issued_at_ms,day_timezone,daily_summary_at,critical_window_ms,critical_total_limit,critical_per_run_limit) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1,'open',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.ID, in.RunID, in.AttemptNo, in.GenerationKey, in.Reason, in.Severity, in.Headline, in.Brief, string(optionsJSON), in.MinModality, string(linksJSON), nonce, dispatch.state, nullable(in.ChannelID), nullable(dispatch.channelSnapshot), nullable(in.Delivery), in.SuggestedDowngrade, nullableInt64(in.NextDispatchAtMS), nullable(in.HeldReason), in.ExpiresAtMS, in.OnExpire, cmd.EscalationCount, cmd.MaxEscalations, nullable(in.ChargedBudgetEntryID), nullable(cmd.CalibrationID), cmd.NowMS, cmd.NowMS, cmd.ExpiresAfterMS, cmd.OnMaxEscalations, baseSeverity, cmd.NowMS, timezoneOrUTC(cmd.DayTimezone), summaryOrDefault(cmd.DailySummaryAt), fuseWindowOrDefault(cmd.CriticalWindowMS), fuseTotalOrDefault(cmd.CriticalTotalLimit), fuseRunOrDefault(cmd.CriticalPerRunLimit)); err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			if existing, found, lookupErr := interruptByKeyTx(ctx, tx, key); lookupErr == nil && found {
				return existing, nil
			}
			if ownTx {
				_ = tx.Rollback()
				if existing, found, lookupErr := d.interruptByKey(ctx, key); lookupErr == nil && found {
					return existing, nil
				}
			}
		}
		return Interrupt{}, err
	}
	if in.Severity != SeverityCritical {
		admissionKind := "quota_charged"
		if quotaBatched {
			admissionKind = "quota_batched"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attention_admissions(id,interrupt_id,admission_key,kind,metric_identity,attention_charge_entry_id,severity,quota_day,day_timezone,run_id,critical_source,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,NULL,?)`, newID(), in.ID, in.ID+":initial", admissionKind, in.ID, nullable(in.ChargedBudgetEntryID), in.Severity, quotaDay(cmd.NowMS, timezoneOrUTC(cmd.DayTimezone)), timezoneOrUTC(cmd.DayTimezone), in.RunID, cmd.NowMS); err != nil {
			return Interrupt{}, err
		}
		if quotaBatched {
			if err := addDailyBatchMemberTx(ctx, tx, in.ID, 1, nonce, cmd.NowMS, dispatch.channelID, dispatch.channelSnapshot, timezoneOrUTC(cmd.DayTimezone), summaryOrDefault(cmd.DailySummaryAt)); err != nil {
				return Interrupt{}, err
			}
		}
	} else {
		admitted, admissionID, scope, err := admitCriticalTx(ctx, tx, in.ID, in.Severity, cmd.NowMS, "initial", fuseWindowOrDefault(cmd.CriticalWindowMS), fuseTotalOrDefault(cmd.CriticalTotalLimit), fuseRunOrDefault(cmd.CriticalPerRunLimit))
		if err != nil {
			return Interrupt{}, err
		}
		if !admitted {
			if err := addCriticalBatchMemberTx(ctx, tx, in.ID, 1, nonce, admissionID+":"+scope, dispatch.channelID, dispatch.channelSnapshot, cmd.NowMS); err != nil {
				return Interrupt{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE interrupts SET dispatch_state='batched',delivery='batch',held_reason=NULL,next_dispatch_at_ms=NULL WHERE id=?`, in.ID); err != nil {
				return Interrupt{}, err
			}
		}
	}
	binding, bindingReason := interruptEffectBinding(cmd)
	bindingDigest := sha256.Sum256(binding)
	if _, err := tx.ExecContext(ctx, `INSERT INTO interrupt_command_effect_bindings(interrupt_id,reason,binding_schema_version,binding_json,binding_digest,created_at_ms) VALUES(?,?,1,?,?,?)`, in.ID, bindingReason, string(binding), hex.EncodeToString(bindingDigest[:]), cmd.NowMS); err != nil {
		return Interrupt{}, fmt.Errorf("%w: interrupt effect binding: %v", ErrInterruptRejected, err)
	}
	eventID := newID()
	eventPayload, _ := json.Marshal(map[string]any{"interrupt_id": in.ID, "reason": in.Reason, "generation_key": in.GenerationKey})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES (?,?,?,?, 'interrupt.emitted',?,1,?,?,?)`, eventID, in.RunID, in.AttemptNo, projectID, cmd.Source, string(eventPayload), cmd.NowMS, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	markdown := renderComment(in)
	payload, _ := json.Marshal(map[string]any{"project_id": projectID, "forge_kind": forgeKind, "forge_host": forgeHost, "forge_project_key": forgeProject, "target_kind": kind, "target_id": id, "purpose": "interrupt", "markdown": markdown})
	opKey := CommentOperationKey("interrupt", in.ID, 1)
	if err := insertOperation(ctx, tx, Operation{Key: opKey, Kind: OperationForgeComment, Payload: payload, RunID: in.RunID, AttemptNo: in.AttemptNo, InterruptID: in.ID}, in.RunID, eventID, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO interrupt_deliveries (id,interrupt_id,surface,priority,operation_key,state,attempt_count,created_at_ms) VALUES (?,?,'forge_comment','normal',?,'pending',0,?)`, newID(), in.ID, opKey, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	var publishOpID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM outbox_operations WHERE operation_key=?`, opKey).Scan(&publishOpID); err != nil {
		return Interrupt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO interrupt_command_targets (interrupt_id,publish_operation_id,target_kind,target_id,created_at_ms) VALUES (?,?,?,?,?)`, in.ID, publishOpID, kind, id, cmd.NowMS); err != nil {
		return Interrupt{}, err
	}
	if dispatch.delivery == "immediate" {
		if err := enqueueInterruptChannelTx(ctx, tx, in.ID, 1, nonce, 0, "normal", cmd.NowMS); err != nil {
			return Interrupt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE interrupts SET dispatch_state='batched',next_dispatch_at_ms=NULL WHERE id=?`, in.ID); err != nil {
			return Interrupt{}, err
		}
	}
	if after != nil {
		if err := after(tx, in); err != nil {
			return Interrupt{}, err
		}
	}
	return in, nil
}
