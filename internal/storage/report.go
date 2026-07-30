package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
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

type reportRuntimeChannel struct {
	ID           string   `json:"id"`
	Enabled      bool     `json:"enabled"`
	Type         string   `json:"type"`
	TargetRef    string   `json:"target_ref"`
	Capabilities []string `json:"capabilities"`
	Renderer     string   `json:"renderer"`
	Default      bool     `json:"default"`
}

type reportRuntimeConfig struct {
	Runtime struct {
		RetryMultiplier float64 `json:"retry_multiplier"`
	} `json:"runtime"`
	Attention struct {
		DayTimezone string `json:"day_timezone"`
		DailyQuota  struct {
			Low    int `json:"low"`
			Normal int `json:"normal"`
			High   int `json:"high"`
		} `json:"daily_quota"`
		MaxEscalations int `json:"max_escalations"`
		CriticalFuse   struct {
			Window      int64 `json:"window"`
			TotalLimit  int   `json:"total_limit"`
			PerRunLimit int   `json:"per_run_limit"`
		} `json:"critical_fuse"`
		DailySummaryAt string                 `json:"daily_summary_at"`
		Channels       []reportRuntimeChannel `json:"channels"`
	} `json:"attention"`
	Report struct {
		EventsPerMinute            int           `json:"events_per_minute"`
		Burst                      int           `json:"burst"`
		DedupeWindow               time.Duration `json:"dedupe_window"`
		MaxPayloadBytes            int           `json:"max_payload_bytes"`
		NotReadyInitialDelay       time.Duration `json:"not_ready_initial_delay"`
		NotReadyMaxDelay           time.Duration `json:"not_ready_max_delay"`
		NotReadyTotalTimeout       time.Duration `json:"not_ready_total_timeout"`
		InterruptsPerRunDailyQuota int           `json:"interrupts_per_run_daily_quota"`
	} `json:"report"`
}

func reportDayBucket(nowMS int64, zone string) (int64, int64, error) {
	loc, err := time.LoadLocation(timezoneOrUTC(zone))
	if err != nil {
		return 0, 0, fmt.Errorf("report: invalid day timezone: %w", err)
	}
	now := time.UnixMilli(nowMS).In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli(), nil
}

func reportQuotaCmd(cmd ReportSubmitCmd, version, start, end int64, cfg reportRuntimeConfig) ReportQuotaExhaustionCmd {
	return ReportQuotaExhaustionCmd{RunID: cmd.RunID, ExpectedRunVersion: version, DailyBucketStartMS: start, DailyBucketEndMS: end, AttentionDailyQuota: map[InterruptSeverity]int{SeverityLow: cfg.Attention.DailyQuota.Low, SeverityNormal: cfg.Attention.DailyQuota.Normal, SeverityHigh: cfg.Attention.DailyQuota.High}, DayTimezone: cfg.Attention.DayTimezone, DailySummaryAt: cfg.Attention.DailySummaryAt, CriticalWindowMS: cfg.Attention.CriticalFuse.Window, CriticalTotalLimit: cfg.Attention.CriticalFuse.TotalLimit, CriticalPerRunLimit: cfg.Attention.CriticalFuse.PerRunLimit, Channels: reportChannels(cfg), NowMS: cmd.NowMS}
}

func reportChannels(cfg reportRuntimeConfig) []InterruptChannel {
	channels := make([]InterruptChannel, 0, len(cfg.Attention.Channels))
	for _, c := range cfg.Attention.Channels {
		channels = append(channels, InterruptChannel{ID: c.ID, Type: c.Type, TargetRef: c.TargetRef, Capabilities: c.Capabilities, Renderer: c.Renderer, Default: c.Default, Isolated: !c.Enabled})
	}
	return channels
}

func validateReportPayload(kind string, p map[string]any) error {
	if p == nil {
		return fmt.Errorf("%w: payload object", ErrReportInvalid)
	}
	want := map[string]string{"progress": "message", "goal": "goal", "blocker": "blocker_summary,attempted_summary,recommended_action", "completed": "summary"}[kind]
	keys := strings.Split(want, ",")
	if len(p) != len(keys) {
		return fmt.Errorf("%w: payload not closed", ErrReportInvalid)
	}
	for _, k := range keys {
		v, ok := p[k].(string)
		if !ok || v == "" {
			return fmt.Errorf("%w: payload field %q", ErrReportInvalid, k)
		}
		// report.md §3: reject empty strings, NUL and every Unicode Cc control
		// code point so the event is safe to project onto a single-line timeline.
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%w: payload field %q", ErrReportInvalid, k)
			}
		}
	}
	return nil
}

// reportBinding is the frozen attempt/claim/run state plus the Run's resolved
// config snapshot, read once before any write transaction opens. Every Report
// outcome derives its authorization, phase and policy values from this struct.
type reportBinding struct {
	Phase      string
	Generation int
	TokenHash  string
	ProjectID  string
	SnapshotID string
	RunVersion int64
	Worktree   string
	cfg        reportRuntimeConfig
}

// checkReportBinding authorizes the run token, generation and attempt phase in
// a single read. It is the not_ready fast path: a legal spawning window returns
// ReportNotReadyError before any transaction or rate token is touched.
func (d *DB) checkReportBinding(ctx context.Context, cmd ReportSubmitCmd) (reportBinding, error) {
	var b reportBinding
	err := d.db.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.run_token_hash,r.project_id,r.config_snapshot_id,r.version,a.worktree_path FROM attempts a JOIN attempt_claims c USING(run_id,attempt_no) JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).Scan(&b.Phase, &b.Generation, &b.TokenHash, &b.ProjectID, &b.SnapshotID, &b.RunVersion, &b.Worktree)
	if err != nil {
		return reportBinding{}, ErrReportUnauthorized
	}
	if b.TokenHash != handoffHash(cmd.Token) {
		return reportBinding{}, ErrReportUnauthorized
	}
	if b.Generation != cmd.Generation {
		return reportBinding{}, ErrReportStale
	}
	raw, err := reportSnapshotJSON(ctx, d.db, b.SnapshotID)
	if err != nil {
		return reportBinding{}, err
	}
	if err := json.Unmarshal(raw, &b.cfg); err != nil {
		return reportBinding{}, errors.New("report: invalid snapshot")
	}
	if b.cfg.Report.MaxPayloadBytes < 1 || b.cfg.Report.EventsPerMinute < 1 || b.cfg.Report.Burst < 1 || b.cfg.Report.InterruptsPerRunDailyQuota < 1 {
		return reportBinding{}, errors.New("report: invalid snapshot")
	}
	switch b.Phase {
	case "running":
		return b, nil
	case "spawning":
		policy, perr := reportRetryPolicy(b.cfg)
		if perr != nil {
			return reportBinding{}, perr
		}
		return reportBinding{}, &ReportNotReadyError{RetryPolicy: policy}
	default:
		// pending/starting/finished/orphaned: phase already passed or not reached.
		return reportBinding{}, ErrReportConflict
	}
}

// assertReportBindingTx is the authoritative in-transaction re-check of the
// binding established by checkReportBinding. It guards against concurrent
// state mutation between the read-only pre-check and the write transaction.
func assertReportBindingTx(ctx context.Context, tx *sql.Tx, cmd ReportSubmitCmd) (string, string, int64, error) {
	var phase string
	var generation int
	var tokenHash, projectID, snapshotID string
	var runVersion int64
	err := tx.QueryRowContext(ctx, `SELECT a.phase,a.generation,c.run_token_hash,r.project_id,r.config_snapshot_id,r.version FROM attempts a JOIN attempt_claims c USING(run_id,attempt_no) JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.attempt_no=?`, cmd.RunID, cmd.AttemptNo).Scan(&phase, &generation, &tokenHash, &projectID, &snapshotID, &runVersion)
	if err != nil {
		return "", "", 0, ErrReportUnauthorized
	}
	if tokenHash != handoffHash(cmd.Token) {
		return "", "", 0, ErrReportUnauthorized
	}
	if generation != cmd.Generation {
		return "", "", 0, ErrReportStale
	}
	if phase != "running" {
		return "", "", 0, ErrReportConflict
	}
	return projectID, snapshotID, runVersion, nil
}

// dedupeKind classifies a two-layer dedupe lookup (report.md §5.2).
const (
	dedupeNone = iota
	dedupeDuplicate
	dedupeConflict
)

// lookupReportDuplicateTx applies both dedupe layers before any token or
// quota is consumed. Layer 1 is the idempotency key (run, attempt, report_key):
// same digest returns the original receipt as a duplicate, a different digest
// is a permanent conflict. Layer 2 is the semantic window: a new key with the
// same (kind, digest) accepted inside dedupe_window also returns the original.
func lookupReportDuplicateTx(ctx context.Context, tx *sql.Tx, cmd ReportSubmitCmd, digest string, dedupeWindow time.Duration) (ReportResult, int) {
	var id, oldDigest, oldEvent string
	err := tx.QueryRowContext(ctx, `SELECT id,payload_digest,event_id FROM report_receipts WHERE run_id=? AND attempt_no=? AND report_key=?`, cmd.RunID, cmd.AttemptNo, cmd.ReportKey).Scan(&id, &oldDigest, &oldEvent)
	if err == nil {
		if oldDigest == digest {
			return ReportResult{Disposition: "duplicate", ReceiptID: id, EventID: oldEvent}, dedupeDuplicate
		}
		return ReportResult{}, dedupeConflict
	}
	if err != sql.ErrNoRows {
		return ReportResult{}, dedupeConflict
	}
	if dedupeWindow > 0 {
		cutoff := cmd.NowMS - dedupeWindow.Milliseconds()
		err = tx.QueryRowContext(ctx, `SELECT id,event_id FROM report_receipts WHERE run_id=? AND attempt_no=? AND report_kind=? AND payload_digest=? AND received_at_ms>=? ORDER BY received_at_ms ASC LIMIT 1`, cmd.RunID, cmd.AttemptNo, cmd.Kind, digest, cutoff).Scan(&id, &oldEvent)
		if err == nil {
			return ReportResult{Disposition: "duplicate", ReceiptID: id, EventID: oldEvent}, dedupeDuplicate
		}
	}
	return ReportResult{}, dedupeNone
}

// reportRetryPolicy derives the closed not_ready policy from the Run's frozen
// config snapshot. The config loader already rejected unrepresentable values,
// so this only fail-closes on a malformed or tampered snapshot.
func reportRetryPolicy(cfg reportRuntimeConfig) (RetryPolicy, error) {
	initial := int(cfg.Report.NotReadyInitialDelay / time.Millisecond)
	maxDelay := int(cfg.Report.NotReadyMaxDelay / time.Millisecond)
	total := int(cfg.Report.NotReadyTotalTimeout / time.Millisecond)
	micros := int(math.Round(cfg.Runtime.RetryMultiplier * 1000000))
	if initial < 1 || maxDelay < initial || total < maxDelay || micros < 1000000 || micros > 10000000 {
		return RetryPolicy{}, errors.New("report: invalid retry policy")
	}
	return RetryPolicy{InitialDelayMS: initial, MultiplierMicros: micros, MaxDelayMS: maxDelay, TotalTimeoutMS: total}, nil
}

func reportSnapshotJSON(ctx context.Context, db *sql.DB, snapshotID string) ([]byte, error) {
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT canonical_json FROM config_snapshots WHERE id=?`, snapshotID).Scan(&raw); err != nil {
		return nil, err
	}
	return []byte(raw), nil
}
