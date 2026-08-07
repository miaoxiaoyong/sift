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
	"time"
	"unicode/utf8"
)

func interruptGenerationKey(run string, reason InterruptReason, g InterruptGeneration) (string, error) {
	domain := "sift.interrupt.generation"
	if reason == InterruptFailureReview && g.AttemptNo == 0 && g.ReportDailyBucketStartMS > 0 {
		domain = "sift.interrupt.report-quota.generation"
	}
	fields := [][2]string{{"string:domain", domain}, {"uint:version", "1"}, {"string:run_id", run}, {"enum:reason", string(reason)}}
	add := func(t, n, v string) { fields = append(fields, [2]string{t + ":" + n, v}) }
	switch reason {
	case InterruptDesignApproval:
		add("string", "task_spec_snapshot_id", g.TaskSpecSnapshotID)
	case InterruptGuardrailViolation:
		add("string", "policy_snapshot_id", g.PolicySnapshotID)
		add("string", "violation_code", g.ViolationCode)
		add("sha256", "subject_digest", g.SubjectDigest)
	case InterruptCodeReview:
		add("string", "change_id", g.ChangeID)
		add("git_oid", "head_sha", g.HeadSHA)
	case InterruptAgentBlocked:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("string", "report_id", g.ReportID)
	case InterruptMergeConflict:
		add("string", "change_id", g.ChangeID)
		add("git_oid", "head_sha", g.HeadSHA)
		add("sha256", "conflict_digest", g.ConflictDigest)
	case InterruptFailureReview:
		if g.AttemptNo == 0 && g.ReportDailyBucketStartMS > 0 {
			add("uint", "day_bucket_start_ms", fmt.Sprint(g.ReportDailyBucketStartMS))
			add("sha256", "failure_digest", g.FailureDigest)
			break
		}
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("sha256", "failure_digest", g.FailureDigest)
	case InterruptStartupStall:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("enum", "cause", "startup_stall")
	default:
		return "", fmt.Errorf("%w: unknown reason", ErrInterruptRejected)
	}
	var b strings.Builder
	for _, f := range fields {
		if f[1] == "" || !utf8.ValidString(f[1]) || strings.ContainsRune(f[1], 0) {
			return "", fmt.Errorf("%w: invalid generation field", ErrInterruptRejected)
		}
		switch {
		case strings.HasPrefix(f[0], "uint:"):
			if f[1] == "0" || strings.HasPrefix(f[1], "-") || !decimal(f[1]) {
				return "", fmt.Errorf("%w: invalid uint", ErrInterruptRejected)
			}
		case strings.HasPrefix(f[0], "sha256:"):
			if len(f[1]) != 64 || !lowerHex(f[1]) {
				return "", fmt.Errorf("%w: invalid sha256", ErrInterruptRejected)
			}
		case strings.HasPrefix(f[0], "git_oid:"):
			if (len(f[1]) != 40 && len(f[1]) != 64) || !lowerHex(f[1]) {
				return "", fmt.Errorf("%w: invalid git oid", ErrInterruptRejected)
			}
		}
		b.WriteString(f[0])
		b.WriteByte(0)
		b.WriteString(f[1])
		b.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func chargeAttentionTx(ctx context.Context, tx *sql.Tx, cmd EmitInterruptCmd, s InterruptSeverity) (string, error) {
	if s == SeverityCritical {
		return "", nil
	}
	id := newID()
	key := "interrupt-charge:" + mustGenerationKey(cmd)
	var bucket int64
	{
		loc := time.Local
		if cmd.DayTimezone != "" && cmd.DayTimezone != "local" {
			var err error
			loc, err = time.LoadLocation(cmd.DayTimezone)
			if err != nil {
				return "", fmt.Errorf("%w: invalid day timezone", ErrInterruptRejected)
			}
		}
		t := time.UnixMilli(cmd.NowMS).In(loc)
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		bucket = start.UnixMilli()
		limit, ok := cmd.AttentionDailyQuota[s]
		if !ok {
			return "", fmt.Errorf("%w: attention quota missing", ErrInterruptRejected)
		}
		end := start.AddDate(0, 0, 1).UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_counters (kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES ('attention','severity',?,?,?, ?,0,1,?) ON CONFLICT DO NOTHING`, string(s), bucket, end, limit, cmd.NowMS); err != nil {
			return "", err
		}
		// A zero-row CAS is not, by itself, proof of exhaustion (config §3.9):
		// re-read the authority counter and only treat consumed+1>limit as a
		// quota_batched admission. A missing row or unreadable counter rolls the
		// emission back so a storage fault cannot masquerade as a batched result.
		const quotaCASRetries = 8
		for attempt := 0; ; attempt++ {
			res, err := tx.ExecContext(ctx, `UPDATE budget_counters SET consumed_value=consumed_value+1,version=version+1,updated_at_ms=? WHERE kind='attention' AND scope='severity' AND scope_id=? AND bucket_start_ms=? AND consumed_value<limit_value`, cmd.NowMS, string(s), bucket)
			if err != nil {
				return "", err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				break
			}
			var consumed, have int64
			if err := tx.QueryRowContext(ctx, `SELECT consumed_value,limit_value FROM budget_counters WHERE kind='attention' AND scope='severity' AND scope_id=? AND bucket_start_ms=?`, string(s), bucket).Scan(&consumed, &have); err != nil {
				return "", err
			}
			if consumed+1 <= have {
				if attempt >= quotaCASRetries {
					return "", fmt.Errorf("%w: attention quota CAS retry exhausted", ErrInterruptRejected)
				}
				continue
			}
			return "", ErrAttentionQuotaExceeded
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_entries (id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES (?,'attention','severity',?,?,1,?,?,?,?)`, id, string(s), bucket, string(cmd.Reason), cmd.RunID, key, cmd.NowMS); err != nil {
		return "", err
	}
	return id, nil
}
func decimal(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func lowerHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func interruptGenerationKeyFor(cmd EmitInterruptCmd) (string, error) {
	if err := validateFailureReviewVariant(cmd); err != nil {
		return "", err
	}
	return interruptGenerationKey(cmd.RunID, cmd.Reason, cmd.Generation)
}

func mustGenerationKey(cmd EmitInterruptCmd) string {
	k, err := interruptGenerationKeyFor(cmd)
	if err != nil {
		panic(err)
	}
	return k
}
func (d *DB) interruptByKey(ctx context.Context, key string) (Interrupt, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, false, err
	}
	defer tx.Rollback()
	in, found, err := interruptByKeyTx(ctx, tx, key)
	if err != nil {
		return Interrupt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, false, err
	}
	return in, found, nil
}

func interruptByKeyTx(ctx context.Context, tx *sql.Tx, key string) (Interrupt, bool, error) {
	var in Interrupt
	var n, next sql.NullInt64
	var opts, links string
	var reason, severity, on string
	var channelID, delivery, heldReason sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,run_id,attempt_no,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,channel_id,delivery,suggested_downgrade,next_dispatch_at_ms,held_reason,expires_at_ms,on_expire,COALESCE(charged_budget_entry_id,'') FROM interrupts WHERE generation_key=?`, key).Scan(&in.ID, &in.RunID, &n, &in.GenerationKey, &reason, &severity, &in.Headline, &in.Brief, &opts, &in.MinModality, &links, &channelID, &delivery, &in.SuggestedDowngrade, &next, &heldReason, &in.ExpiresAtMS, &on, &in.ChargedBudgetEntryID)
	if errors.Is(err, sql.ErrNoRows) {
		return Interrupt{}, false, nil
	}
	if err != nil {
		return Interrupt{}, false, err
	}
	if n.Valid {
		x := int(n.Int64)
		in.AttemptNo = &x
	}
	in.Reason = InterruptReason(reason)
	in.Severity = InterruptSeverity(severity)
	in.OnExpire = ExpireAction(on)
	in.ChannelID, in.Delivery, in.HeldReason = channelID.String, delivery.String, heldReason.String
	if next.Valid {
		in.NextDispatchAtMS = &next.Int64
	}
	if json.Unmarshal([]byte(opts), &in.Options) != nil || json.Unmarshal([]byte(links), &in.Links) != nil {
		return Interrupt{}, false, errors.New("storage: corrupt interrupt JSON")
	}
	return in, true, nil
}
