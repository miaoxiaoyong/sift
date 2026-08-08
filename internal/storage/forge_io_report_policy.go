package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

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
