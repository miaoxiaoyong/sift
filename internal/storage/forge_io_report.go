package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ReportSubmitCmd is the daemon-side production port for report.submit. The
// token is checked against the launch claim; callers never receive database access.
type ReportSubmitCmd struct {
	Token      string
	RunID      string
	AttemptNo  int
	Generation int
	ReportKey  string
	Kind       string
	Payload    map[string]any
	NowMS      int64
}
type ReportResult struct {
	Disposition string `json:"disposition"`
	ReceiptID   string `json:"receipt_id"`
	EventID     string `json:"event_id"`
}

// RetryPolicy is the closed not_ready backoff derived from the binding Run's
// frozen config snapshot (report.md §4). The CLI consumes it verbatim and
// recomputes each delay from these exact integer fields.
type RetryPolicy struct {
	InitialDelayMS   int `json:"initial_delay_ms"`
	MultiplierMicros int `json:"multiplier_micros"`
	MaxDelayMS       int `json:"max_delay_ms"`
	TotalTimeoutMS   int `json:"total_timeout_ms"`
}

// ReportNotReadyError signals a legal spawning window: the attempt is bound to
// the presented run token but claim.started has not linearized yet. It carries
// the closed retry_policy so the CLI never reads config.yaml.
type ReportNotReadyError struct {
	RetryPolicy RetryPolicy
}

func (e *ReportNotReadyError) Error() string { return "report: not ready" }
func (e *ReportNotReadyError) Unwrap() error { return ErrReportNotReady }

// Typed report errors let the control-plane gateway map each outcome to the
// closed error code set (control-plane.md §3.4) without parsing strings.
var (
	ErrReportInvalid         = errors.New("report: invalid")
	ErrReportUnauthorized    = errors.New("report: unauthorized")
	ErrReportStale           = errors.New("report: stale")
	ErrReportConflict        = errors.New("report: conflict")
	ErrReportRateLimited     = errors.New("report: rate limit exceeded")
	ErrReportQuotaExhausted  = errors.New("report: report_interrupt_quota_exhausted")
	ErrReportPayloadTooLarge = errors.New("report: payload too large")
	ErrReportNotReady        = errors.New("report: not ready")
)

func (d *DB) RecordReport(ctx context.Context, cmd ReportSubmitCmd) (ReportResult, error) {
	if cmd.RunID == "" || cmd.AttemptNo < 1 || cmd.Generation < 1 || cmd.NowMS <= 0 || len(cmd.ReportKey) != 32 || !lowerHex(cmd.ReportKey) || cmd.Token == "" {
		return ReportResult{}, fmt.Errorf("%w: request", ErrReportInvalid)
	}
	if cmd.Kind != "progress" && cmd.Kind != "goal" && cmd.Kind != "blocker" && cmd.Kind != "completed" {
		return ReportResult{}, fmt.Errorf("%w: kind", ErrReportInvalid)
	}
	if err := validateReportPayload(cmd.Kind, cmd.Payload); err != nil {
		return ReportResult{}, err
	}
	wrapped := map[string]any{"kind": cmd.Kind, "payload": cmd.Payload}
	canonical, _ := json.Marshal(wrapped)
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	payloadCanonical, _ := json.Marshal(cmd.Payload)
	// The binding pre-check authorizes the run token, generation and attempt
	// phase before any write transaction opens. not_ready in particular must
	// not consume a rate token or occupy a report key, so it returns here.
	binding, err := d.checkReportBinding(ctx, cmd)
	if err != nil {
		return ReportResult{}, err
	}
	if len(payloadCanonical) > binding.cfg.Report.MaxPayloadBytes {
		return ReportResult{}, ErrReportPayloadTooLarge
	}
	if cmd.Kind == "blocker" {
		return d.recordBlockerReport(ctx, cmd, digest, binding)
	}
	return d.recordSimpleReport(ctx, cmd, digest, binding)
}

// recordSimpleReport writes a non-blocker (progress/goal/completed) report. It
// never writes runs.status, a Report charge, or an Interrupt; only the
// append-only event and its immutable receipt share one transaction with the
// rate-token CAS (report.md §5.1, §7).
func (d *DB) recordSimpleReport(ctx context.Context, cmd ReportSubmitCmd, digest string, binding reportBinding) (ReportResult, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ReportResult{}, err
	}
	defer tx.Rollback()
	projectID, snapshotID, runVersion, err := assertReportBindingTx(ctx, tx, cmd)
	if err != nil {
		return ReportResult{}, err
	}
	_ = runVersion
	if dup, kind := lookupReportDuplicateTx(ctx, tx, cmd, digest, binding.cfg.Report.DedupeWindow); kind != dedupeNone {
		if kind == dedupeConflict {
			return ReportResult{}, ErrReportConflict
		}
		return dup, tx.Commit()
	}
	if err := consumeReportTokenTx(ctx, tx, cmd, snapshotID); err != nil {
		return ReportResult{}, err
	}
	eventID, receiptID := newID(), newID()
	payload, _ := json.Marshal(map[string]any{"report_key": cmd.ReportKey, "payload_digest": digest, "generation": cmd.Generation, "report": cmd.Payload})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,?,?, 'agent',1,?,?,?)`, eventID, cmd.RunID, cmd.AttemptNo, projectID, "report."+cmd.Kind, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
		return ReportResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,report_interrupt_charge_entry_id,received_at_ms) VALUES(?,?,?,?,?,?,?,?,?)`, receiptID, cmd.RunID, cmd.AttemptNo, cmd.ReportKey, cmd.Kind, digest, eventID, nullable(""), cmd.NowMS); err != nil {
		return ReportResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReportResult{}, err
	}
	return ReportResult{"accepted", receiptID, eventID}, nil
}

// recordBlockerReport makes the Report receipt/event, rate token, child quota,
// and agent_blocked Interrupt one SQLite transaction. EmitInterrupt invokes
// before only after T4/T6 have completed and rolls this transaction back on
// any structural or publish failure.
func (d *DB) recordBlockerReport(ctx context.Context, cmd ReportSubmitCmd, digest string, binding reportBinding) (ReportResult, error) {
	var result ReportResult
	var exhausted bool
	cfg := binding.cfg
	runVersion := binding.RunVersion
	n := cmd.AttemptNo
	receiptID := newID()
	var batchAt *int64
	if at, ok := nextSummary(cmd.NowMS, timezoneOrUTC(cfg.Attention.DayTimezone), summaryOrDefault(cfg.Attention.DailySummaryAt)); ok {
		batchAt = &at
	}
	facts := map[string]string{"blocker_summary": cmd.Payload["blocker_summary"].(string), "attempted_summary": cmd.Payload["attempted_summary"].(string), "recommended_action": cmd.Payload["recommended_action"].(string), "agent_log_ref": strings.TrimRight(binding.Worktree, "/") + "/agent.log"}
	err := func() error {
		_, emitErr := d.emitReportInterruptHooks(ctx, EmitInterruptCmd{RunID: cmd.RunID, ExpectedRunVersion: runVersion, AttemptNo: &n, Reason: InterruptAgentBlocked, Facts: facts, Generation: InterruptGeneration{AttemptNo: cmd.AttemptNo, Generation: cmd.Generation, ReportID: receiptID}, GatePhase: GateNone, GuardrailLevel: GuardrailNone, MaxEscalations: cfg.Attention.MaxEscalations, AttentionDailyQuota: map[InterruptSeverity]int{SeverityLow: cfg.Attention.DailyQuota.Low, SeverityNormal: cfg.Attention.DailyQuota.Normal, SeverityHigh: cfg.Attention.DailyQuota.High}, DayTimezone: cfg.Attention.DayTimezone, DailySummaryAt: cfg.Attention.DailySummaryAt, CriticalWindowMS: cfg.Attention.CriticalFuse.Window, CriticalTotalLimit: cfg.Attention.CriticalFuse.TotalLimit, CriticalPerRunLimit: cfg.Attention.CriticalFuse.PerRunLimit, Channels: reportChannels(cfg), BatchAtMS: batchAt, Source: SourceAgent, NowMS: cmd.NowMS}, func(tx *sql.Tx) error {
			projectID, snapshotID, _, err := assertReportBindingTx(ctx, tx, cmd)
			if err != nil {
				return err
			}
			if dup, kind := lookupReportDuplicateTx(ctx, tx, cmd, digest, cfg.Report.DedupeWindow); kind != dedupeNone {
				if kind == dedupeConflict {
					return ErrReportConflict
				}
				result = dup
				return errReportDuplicate
			}
			if err := consumeReportTokenTx(ctx, tx, cmd, snapshotID); err != nil {
				return err
			}
			start, end, err := reportDayBucket(cmd.NowMS, cfg.Attention.DayTimezone)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('report','run',?,?,?, ?,0,1,?) ON CONFLICT DO NOTHING`, cmd.RunID, start, end, cfg.Report.InterruptsPerRunDailyQuota, cmd.NowMS); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE budget_counters SET consumed_value=consumed_value+1,version=version+1,updated_at_ms=? WHERE kind='report' AND scope='run' AND scope_id=? AND bucket_start_ms=? AND consumed_value<limit_value`, cmd.NowMS, cmd.RunID, start)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				exhausted = true
				return errReportQuotaExhausted
			}
			eventID, chargeID := newID(), newID()
			payload, _ := json.Marshal(map[string]any{"report_key": cmd.ReportKey, "payload_digest": digest, "generation": cmd.Generation, "report": cmd.Payload})
			if _, err = tx.ExecContext(ctx, `INSERT INTO budget_entries(id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES(?,'report','run',?,?,1,'report_agent_blocked',?,?,?)`, chargeID, cmd.RunID, start, cmd.RunID, "report-interrupt-quota:"+receiptID, cmd.NowMS); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,attempt_no,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,?,?, 'agent',1,?,?,?)`, eventID, cmd.RunID, cmd.AttemptNo, projectID, "report.blocker", string(payload), cmd.NowMS, cmd.NowMS); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO report_receipts(id,run_id,attempt_no,report_key,report_kind,payload_digest,event_id,report_interrupt_charge_entry_id,received_at_ms) VALUES(?,?,?,?,?,?,?,?,?)`, receiptID, cmd.RunID, cmd.AttemptNo, cmd.ReportKey, cmd.Kind, digest, eventID, chargeID, cmd.NowMS); err != nil {
				return err
			}
			result = ReportResult{"accepted", receiptID, eventID}
			return nil
		}, func(tx *sql.Tx, in Interrupt) error {
			_, err := tx.ExecContext(ctx, `UPDATE report_receipts SET direct_interrupt_id=? WHERE id=? AND direct_interrupt_id IS NULL`, in.ID, receiptID)
			return err
		})
		return emitErr
	}()
	if errors.Is(err, errReportDuplicate) {
		return result, nil
	}
	if errors.Is(err, errReportQuotaExhausted) && exhausted {
		start, end, bucketErr := reportDayBucket(cmd.NowMS, cfg.Attention.DayTimezone)
		if bucketErr != nil {
			return ReportResult{}, bucketErr
		}
		if bucketErr = d.commitReportQuotaExhaustion(ctx, cmd, cfg, runVersion, start, end); bucketErr != nil {
			return ReportResult{}, bucketErr
		}
		_, emitErr := d.RecordReportQuotaExhaustion(ctx, reportQuotaCmd(cmd, runVersion, start, end, cfg))
		if emitErr != nil && !errors.Is(emitErr, ErrInterruptRejected) {
			return ReportResult{}, emitErr
		}
		return ReportResult{}, ErrReportQuotaExhausted
	}
	if err != nil {
		return ReportResult{}, err
	}
	return result, nil
}

var errReportDuplicate = errors.New("report duplicate")
var errReportQuotaExhausted = errors.New("report quota exhausted")

func (d *DB) commitReportQuotaExhaustion(ctx context.Context, cmd ReportSubmitCmd, cfg reportRuntimeConfig, runVersion, start, end int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase, tokenHash, snapshotID string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.run_token_hash,r.config_snapshot_id FROM attempts a JOIN attempt_claims c USING(run_id,attempt_no) JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).Scan(&phase, &generation, &tokenHash, &snapshotID); err != nil || phase != "running" || generation != cmd.Generation || tokenHash != handoffHash(cmd.Token) {
		return ErrReportUnauthorized
	}
	// The exhaustion row is the rate-token linearization point. Replays and
	// later blockers reuse it without consuming another token.
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT security_event_id FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, start).Scan(&existing); err == nil {
		return tx.Commit()
	} else if err != sql.ErrNoRows {
		return err
	}
	if err := consumeReportTokenTx(ctx, tx, cmd, snapshotID); err != nil {
		return err
	}
	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id=? AND version=?`, cmd.RunID, runVersion).Scan(&projectID); err != nil {
		return err
	}
	eventID := newID()
	digest := reportQuotaFailureDigest(cmd.RunID, start, end)
	key, err := interruptGenerationKey(cmd.RunID, InterruptFailureReview, InterruptGeneration{ReportDailyBucketStartMS: start, ReportDailyBucketEndMS: end, FailureDigest: digest})
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"daily_bucket_end_ms": end, "daily_bucket_start_ms": start, "failure_class": "report_interrupt_quota_exhausted", "failure_digest": digest, "generation_key": key})
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'security.report_quota_exhausted','system',1,?,?,?)`, eventID, cmd.RunID, projectID, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO report_quota_exhaustions(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id,failure_digest,generation_key,created_at_ms) VALUES(?,?,?,?,?,?,?)`, cmd.RunID, start, end, eventID, digest, key, cmd.NowMS); err != nil {
		// Another writer may have linearized this day after our initial read.
		// Roll back the tentative token/event, then reuse the durable winner.
		_ = tx.Rollback()
		var winner string
		if lookupErr := d.db.QueryRowContext(ctx, `SELECT security_event_id FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, start).Scan(&winner); lookupErr == nil {
			return nil
		}
		return err
	}
	return tx.Commit()
}

func consumeReportTokenTx(ctx context.Context, tx *sql.Tx, cmd ReportSubmitCmd, snapshotID string) error {
	var capacity, available, numerator, period, remainder, last int64
	scope := "run:" + cmd.RunID + ":attempt:" + fmt.Sprint(cmd.AttemptNo)
	err := tx.QueryRowContext(ctx, `SELECT capacity_units,available_units,refill_numerator,refill_period_ms,refill_remainder,last_refill_at_ms FROM rate_limit_buckets WHERE kind='report' AND scope_id=?`, scope).Scan(&capacity, &available, &numerator, &period, &remainder, &last)
	if err == sql.ErrNoRows {
		var raw string
		if err = tx.QueryRowContext(ctx, `SELECT canonical_json FROM config_snapshots WHERE id=?`, snapshotID).Scan(&raw); err != nil {
			return err
		}
		var c struct {
			Report struct {
				EventsPerMinute int `json:"events_per_minute"`
				Burst           int `json:"burst"`
			} `json:"report"`
		}
		if json.Unmarshal([]byte(raw), &c) != nil || c.Report.Burst < 1 || c.Report.EventsPerMinute < 1 {
			return errors.New("report: invalid snapshot")
		}
		capacity, available, numerator, period, last = int64(c.Report.Burst), int64(c.Report.Burst), int64(c.Report.EventsPerMinute), 60000, cmd.NowMS
		if _, err = tx.ExecContext(ctx, `INSERT INTO rate_limit_buckets(kind,scope_id,capacity_units,available_units,refill_numerator,refill_period_ms,refill_remainder,last_refill_at_ms,version) VALUES('report',?,?,?,?,?,?,?,1)`, scope, capacity, available, numerator, period, 0, last); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if cmd.NowMS > last {
		add := (cmd.NowMS - last) * numerator / period
		if add > 0 {
			available += add
			if available > capacity {
				available = capacity
			}
			last = cmd.NowMS
		}
	}
	if available < 1 {
		return ErrReportRateLimited
	}
	available--
	_, err = tx.ExecContext(ctx, `UPDATE rate_limit_buckets SET available_units=?,last_refill_at_ms=?,version=version+1 WHERE kind='report' AND scope_id=?`, available, last, scope)
	return err
}
