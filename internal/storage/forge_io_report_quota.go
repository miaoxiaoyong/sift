package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ReportQuotaExhaustionCmd is the committed Report quota fact that may produce
// the Report-only failure_review Interrupt. Callers invoke it only after the
// Report rate-token transaction has consumed its token.
type ReportQuotaExhaustionCmd struct {
	RunID                                   string
	ExpectedRunVersion                      int64
	DailyBucketStartMS                      int64
	DailyBucketEndMS                        int64
	AttentionDailyQuota                     map[InterruptSeverity]int
	DayTimezone                             string
	CriticalWindowMS                        int64
	CriticalTotalLimit, CriticalPerRunLimit int
	DailySummaryAt                          string
	Channels                                []InterruptChannel
	NowMS                                   int64
}

// RecordReportQuotaExhaustion is the production owner for the Report quota
// exhaustion fact. It commits the system security event and its unique
// exhaustion identity before attempting the best-effort EmitInterrupt step.
// A structural emission rejection never rolls back the durable quota fact.
func (d *DB) RecordReportQuotaExhaustion(ctx context.Context, cmd ReportQuotaExhaustionCmd) (Interrupt, error) {
	if cmd.RunID == "" || cmd.ExpectedRunVersion < 1 || cmd.DailyBucketStartMS <= 0 || cmd.DailyBucketEndMS <= cmd.DailyBucketStartMS || cmd.NowMS <= 0 {
		return Interrupt{}, fmt.Errorf("%w: invalid report quota exhaustion", ErrInterruptRejected)
	}
	digest := reportQuotaFailureDigest(cmd.RunID, cmd.DailyBucketStartMS, cmd.DailyBucketEndMS)
	generation := InterruptGeneration{ReportDailyBucketStartMS: cmd.DailyBucketStartMS, ReportDailyBucketEndMS: cmd.DailyBucketEndMS, FailureDigest: digest}
	key, err := interruptGenerationKey(cmd.RunID, InterruptFailureReview, generation)
	if err != nil {
		return Interrupt{}, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, err
	}
	defer tx.Rollback()
	var eventID string
	if err := tx.QueryRowContext(ctx, `SELECT security_event_id FROM report_quota_exhaustions WHERE run_id=? AND daily_bucket_start_ms=?`, cmd.RunID, cmd.DailyBucketStartMS).Scan(&eventID); err != nil && err != sql.ErrNoRows {
		return Interrupt{}, err
	} else if err == sql.ErrNoRows {
		var status string
		var version int64
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT status,version,project_id FROM runs WHERE id=?`, cmd.RunID).Scan(&status, &version, &projectID); err != nil {
			return Interrupt{}, err
		}
		if RunStatus(status) != RunRunning || version != cmd.ExpectedRunVersion {
			return Interrupt{}, ErrRejectedStale
		}
		eventID = newID()
		payload, _ := json.Marshal(map[string]any{"daily_bucket_end_ms": cmd.DailyBucketEndMS, "daily_bucket_start_ms": cmd.DailyBucketStartMS, "failure_class": "report_interrupt_quota_exhausted", "failure_digest": digest, "generation_key": key})
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'security.report_quota_exhausted','system',1,?,?,?)`, eventID, cmd.RunID, projectID, string(payload), cmd.NowMS, cmd.NowMS); err != nil {
			return Interrupt{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_quota_exhaustions(run_id,daily_bucket_start_ms,daily_bucket_end_ms,security_event_id,failure_digest,generation_key,created_at_ms) VALUES(?,?,?,?,?,?,?)`, cmd.RunID, cmd.DailyBucketStartMS, cmd.DailyBucketEndMS, eventID, digest, key, cmd.NowMS); err != nil {
			return Interrupt{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, err
	}

	generation.SecurityEventID = eventID
	emit := EmitInterruptCmd{
		RunID: cmd.RunID, ExpectedRunVersion: cmd.ExpectedRunVersion,
		Reason: InterruptFailureReview, FailureReviewVariant: FailureReviewReportQuota,
		Facts:      map[string]string{"failure_class": "report_interrupt_quota_exhausted", "failure_evidence_ref": "sift://event/" + eventID, "recommended_action": "hold"},
		Generation: generation, GatePhase: GateNone, GuardrailLevel: GuardrailNone,
		AttentionDailyQuota: cmd.AttentionDailyQuota, DayTimezone: cmd.DayTimezone,
		CriticalWindowMS: cmd.CriticalWindowMS, CriticalTotalLimit: cmd.CriticalTotalLimit, CriticalPerRunLimit: cmd.CriticalPerRunLimit,
		DailySummaryAt: cmd.DailySummaryAt, Channels: cmd.Channels,
		Source: SourceSystem, NowMS: cmd.NowMS,
	}
	if cmd.DailySummaryAt != "" {
		if batchAt, ok := NextDailySummaryAt(cmd.NowMS, cmd.DayTimezone, cmd.DailySummaryAt); ok {
			emit.BatchAtMS = &batchAt
		}
	}
	interrupt, err := d.EmitInterrupt(ctx, emit)
	if errors.Is(err, ErrInterruptRejected) {
		if diagnosticErr := d.recordReportEmissionDiagnostic(ctx, cmd.RunID, eventID, key, cmd.NowMS); diagnosticErr != nil {
			return Interrupt{}, diagnosticErr
		}
	}
	return interrupt, err
}

// recordReportEmissionDiagnostic records the post-exhaustion structural
// rejection under the same generation key as its best-effort emission.
func (d *DB) recordReportEmissionDiagnostic(ctx context.Context, runID, securityEventID, generationKey string, nowMS int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT event_id FROM report_emission_diagnostics WHERE generation_key=?`, generationKey).Scan(&existing); err == nil {
		return tx.Commit()
	} else if err != sql.ErrNoRows {
		return err
	}
	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id=?`, runID).Scan(&projectID); err != nil {
		return err
	}
	eventID := newID()
	payload, _ := json.Marshal(map[string]string{"disposition": "structural_rejected", "generation_key": generationKey, "security_event_id": securityEventID})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,project_id,type,source,payload_schema_version,payload_json,occurred_at_ms,recorded_at_ms) VALUES(?,?,?,'security.report_interrupt_rejected','system',1,?,?,?)`, eventID, runID, projectID, string(payload), nowMS, nowMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO report_emission_diagnostics(generation_key,run_id,security_event_id,event_id,disposition,created_at_ms) VALUES(?,?,?,?, 'structural_rejected',?)`, generationKey, runID, securityEventID, eventID, nowMS); err != nil {
		if lookupErr := tx.QueryRowContext(ctx, `SELECT event_id FROM report_emission_diagnostics WHERE generation_key=?`, generationKey).Scan(&existing); lookupErr == nil {
			return tx.Commit()
		}
		return err
	}
	return tx.Commit()
}

func reportQuotaFailureDigest(runID string, start, end int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"daily_bucket_end_ms":%d,"daily_bucket_start_ms":%d,"failure_class":"report_interrupt_quota_exhausted","recommended_action":"hold","run_id":%q}`, end, start, runID)))
	return hex.EncodeToString(sum[:])
}
