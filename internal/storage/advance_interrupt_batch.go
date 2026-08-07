package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// NextDailySummaryAt returns the next frozen local summary occurrence.
func NextDailySummaryAt(now int64, zone, clock string) (int64, bool) {
	return nextSummary(now, zone, clock)
}

func nextSummary(now int64, zone, clock string) (int64, bool) {
	loc := time.Local
	var err error
	if zone != "local" {
		loc, err = time.LoadLocation(zone)
	}
	if err != nil {
		return 0, false
	}
	var h, m int
	if _, err = fmt.Sscanf(clock, "%d:%d", &h, &m); err != nil || h > 23 || m > 59 {
		return 0, false
	}
	t := time.UnixMilli(now).In(loc)
	sameDay := time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, loc)
	// time.Date normalizes a nonexistent wall time to one side of a DST gap.
	// Locate the offset transition and return its first valid instant.
	if sameDay.In(loc).Year() == t.Year() && sameDay.In(loc).YearDay() == t.YearDay() && (sameDay.In(loc).Hour() != h || sameDay.In(loc).Minute() != m) {
		for probe := sameDay.Add(4 * time.Hour); probe.After(sameDay); probe = probe.Add(-time.Minute) {
			before := probe.Add(-time.Minute)
			_, beforeOffset := before.In(loc).Zone()
			_, probeOffset := probe.In(loc).Zone()
			if beforeOffset != probeOffset {
				if probe.After(t) {
					return probe.UnixMilli(), true
				}
				break
			}
		}
	}
	candidate := sameDay
	if !candidate.After(t) {
		candidate = time.Date(t.Year(), t.Month(), t.Day()+1, h, m, 0, 0, loc)
	}
	return candidate.UnixMilli(), true
}

func addDailyBatchMemberTx(ctx context.Context, tx *sql.Tx, id string, version int64, nonce string, now int64, channel, snapshot, zone, summary string) error {
	at, ok := nextSummary(now, zone, summary)
	if !ok {
		return fmt.Errorf("%w: invalid frozen summary", ErrInterruptRejected)
	}
	return addDailyBatchMemberAtTx(ctx, tx, id, version, nonce, at, now, channel, snapshot)
}

// addDailyBatchMemberAtTx joins the already-frozen summary occurrence. The
// dispatch path must not recalculate it at the later tick: doing so would move
// a member into the following day's batch when the tick lands exactly at due.
func addDailyBatchMemberAtTx(ctx context.Context, tx *sql.Tx, id string, version int64, nonce string, at, now int64, channel, snapshot string) error {
	if channel == "" || snapshot == "" {
		return fmt.Errorf("%w: batched interrupt lacks channel snapshot", ErrInterruptRejected)
	}
	if at <= 0 {
		return fmt.Errorf("%w: invalid frozen summary", ErrInterruptRejected)
	}
	var admission string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM attention_admissions WHERE admission_key=?`, id+":initial").Scan(&admission); err != nil {
		return err
	}
	return addBatchMemberTx(ctx, tx, "", "daily_summary", id, version, nonce, admission, channel, snapshot, at, now)
}
func addCriticalBatchMemberTx(ctx context.Context, tx *sql.Tx, id string, version int64, nonce, admissionScope, channel, snapshot string, now int64) error {
	if channel == "" || snapshot == "" {
		return fmt.Errorf("%w: fused interrupt lacks channel snapshot", ErrInterruptRejected)
	}
	parts := strings.Split(admissionScope, ":")
	if len(parts) != 2 || (parts[1] != "global" && parts[1] != "run") {
		return fmt.Errorf("%w: invalid critical fuse scope", ErrInterruptRejected)
	}
	admission, scope := parts[0], parts[1]
	var run string
	var window int64
	if err := tx.QueryRowContext(ctx, `SELECT run_id,critical_window_ms FROM interrupts WHERE id=?`, id).Scan(&run, &window); err != nil {
		return err
	}
	scopeID := "global"
	if scope == "run" {
		scopeID = run
	}
	var batch string
	_ = tx.QueryRowContext(ctx, `SELECT b.id FROM attention_batches b JOIN interrupts i ON i.id=? JOIN runs r ON r.id=i.run_id WHERE b.kind='critical_fuse' AND b.scope=? AND b.scope_id=? AND b.channel_id=? AND b.forge_kind=r.forge_kind AND b.forge_host=r.forge_host AND b.forge_project_key=r.forge_project_key AND b.target_kind=CASE WHEN r.issue_id IS NOT NULL THEN 'issue' ELSE r.discussion_target_kind END AND b.target_id=COALESCE(r.issue_id,r.discussion_target_id) AND b.state='collecting' ORDER BY b.created_at_ms LIMIT 1`, id, scope, scopeID, channel).Scan(&batch)
	if batch == "" {
		batch = "critical:" + scope + ":" + scopeID + ":" + admission
	}
	var due int64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(created_at_ms)+? FROM attention_admissions WHERE kind='critical_admitted' AND created_at_ms>? AND created_at_ms<=?`+map[bool]string{true: " AND run_id=?", false: ""}[scope == "run"], append([]any{window, now - window, now}, func() []any {
		if scope == "run" {
			return []any{run}
		}
		return nil
	}()...)...).Scan(&due); err != nil {
		return err
	}
	return addBatchMemberTx(ctx, tx, batch, "critical_fuse", id, version, nonce, admission, channel, snapshot, due, now)
}
func mustBatchZone(ctx context.Context, tx *sql.Tx, interruptID string) string {
	var zone string
	if err := tx.QueryRowContext(ctx, `SELECT day_timezone FROM interrupts WHERE id=?`, interruptID).Scan(&zone); err != nil || zone == "" {
		return "UTC"
	}
	return zone
}

func addBatchMemberTx(ctx context.Context, tx *sql.Tx, batch, kind, id string, version int64, nonce, admission, channel, snapshot string, due, now int64) error {
	var project, forgeKind, host, forgeProject, targetKind, targetID, headline, reason, severity, links, opts string
	var criticalWindow int64
	var criticalTotal, criticalPerRun int
	if err := tx.QueryRowContext(ctx, `SELECT r.project_id,r.forge_kind,r.forge_host,r.forge_project_key,CASE WHEN r.issue_id IS NOT NULL THEN 'issue' ELSE r.discussion_target_kind END,COALESCE(r.issue_id,r.discussion_target_id),i.headline,i.reason,i.severity,i.links_json,i.options_json,i.critical_window_ms,i.critical_total_limit,i.critical_per_run_limit FROM interrupts i JOIN runs r ON r.id=i.run_id WHERE i.id=?`, id).Scan(&project, &forgeKind, &host, &forgeProject, &targetKind, &targetID, &headline, &reason, &severity, &links, &opts, &criticalWindow, &criticalTotal, &criticalPerRun); err != nil {
		return err
	}
	enc := base64.RawURLEncoding.EncodeToString
	if kind == "daily_summary" {
		batch = fmt.Sprintf("daily:%s:%s:%d:%s:%s:%s:%s:%s:%s", project, mustBatchZone(ctx, tx, id), due, channel, forgeKind, enc([]byte(host)), enc([]byte(forgeProject)), targetKind, enc([]byte(targetID)))
	} else {
		if batch == "" {
			batch = "critical:global:global:" + admission
		}
		// Existing collecting batches already contain their complete immutable
		// target identity; append the target suffix only for a new episode.
		if strings.Count(batch, ":") < 8 {
			batch += fmt.Sprintf(":%s:%s:%s:%s:%s", channel, forgeKind, enc([]byte(host)), enc([]byte(forgeProject)), targetKind+":"+enc([]byte(targetID)))
		}
	}
	deliveryID := batch + ":publish:1"
	scope, scopeID := "global", "global"
	if kind == "daily_summary" {
		scope, scopeID = "day", mustBatchZone(ctx, tx, id)+":"+fmt.Sprint(due)
	} else {
		parts := strings.Split(batch, ":")
		if len(parts) >= 4 {
			scope, scopeID = parts[1], parts[2]
		}
	}
	episode := ""
	if kind == "critical_fuse" {
		parts := strings.Split(batch, ":")
		if len(parts) < 4 {
			return fmt.Errorf("%w: invalid critical batch identity", ErrInterruptRejected)
		}
		episode = parts[3]
	}
	batchWindowValue, batchTotalValue, batchPerRunValue := any(criticalWindow), any(criticalTotal), any(criticalPerRun)
	if kind == "daily_summary" {
		// Daily batches do not own critical-fuse limits. Keep these columns NULL
		// for compatibility with databases created before the columns existed.
		batchWindowValue, batchTotalValue, batchPerRunValue = nil, nil, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_batches(id,state,project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,kind,delivery_id,scope,scope_id,episode_admission_id,due_at_ms,critical_window_ms,critical_total_limit,critical_per_run_limit,created_at_ms,updated_at_ms) VALUES(?,'collecting',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, batch, project, channel, snapshot, forgeKind, host, forgeProject, targetKind, targetID, kind, deliveryID, scope, scopeID, nullable(episode), due, batchWindowValue, batchTotalValue, batchPerRunValue, now, now); err != nil {
		return fmt.Errorf("create attention batch: %w", err)
	}
	var batchProject, batchChannel, batchSnapshot, batchForgeKind, batchHost, batchForgeProject, batchTargetKind, batchTargetID, batchKind, batchDelivery, batchScope, batchScopeID string
	var batchEpisode sql.NullString
	var batchDue int64
	var batchWindow, batchTotal, batchPerRun sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,kind,delivery_id,scope,scope_id,episode_admission_id,due_at_ms,critical_window_ms,critical_total_limit,critical_per_run_limit FROM attention_batches WHERE id=?`, batch).Scan(&batchProject, &batchChannel, &batchSnapshot, &batchForgeKind, &batchHost, &batchForgeProject, &batchTargetKind, &batchTargetID, &batchKind, &batchDelivery, &batchScope, &batchScopeID, &batchEpisode, &batchDue, &batchWindow, &batchTotal, &batchPerRun); err != nil {
		return err
	}
	if batchProject != project || batchChannel != channel || batchSnapshot != snapshot || batchForgeKind != forgeKind || batchHost != host || batchForgeProject != forgeProject || batchTargetKind != targetKind || batchTargetID != targetID || batchKind != kind || batchDelivery != deliveryID || batchScope != scope || batchScopeID != scopeID || batchEpisode.String != episode || batchEpisode.Valid != (episode != "") || batchDue != due {
		return fmt.Errorf("%w: attention batch identity collision", ErrInterruptRejected)
	}
	// Critical limits are part of critical-fuse authority only. Legacy daily
	// batches legitimately contain NULL in these columns.
	if kind == "critical_fuse" && (!batchWindow.Valid || !batchTotal.Valid || !batchPerRun.Valid || batchWindow.Int64 != criticalWindow || batchTotal.Int64 != int64(criticalTotal) || batchPerRun.Int64 != int64(criticalPerRun)) {
		return fmt.Errorf("%w: attention batch identity collision", ErrInterruptRejected)
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_batch_members(batch_id,interrupt_id,admission_id,member_key,channel_id,channel_snapshot_json,delivery_id,interrupt_version,nonce,headline,reason,severity,links_json,options_json,joined_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, batch, id, admission, batch+":"+id, channel, snapshot, batch+":"+id, version, nonce, headline, reason, severity, links, opts, now)
	if err != nil {
		return fmt.Errorf("add attention batch member: %w", err)
	}
	var gotAdmission, gotKey, gotChannel, gotSnapshot, gotDelivery, gotNonce, gotHeadline, gotReason, gotSeverity, gotLinks, gotOpts string
	var gotVersion int64
	var gotJoined int64
	if err := tx.QueryRowContext(ctx, `SELECT admission_id,member_key,channel_id,channel_snapshot_json,delivery_id,interrupt_version,nonce,headline,reason,severity,links_json,options_json,joined_at_ms FROM attention_batch_members WHERE batch_id=? AND interrupt_id=?`, batch, id).Scan(&gotAdmission, &gotKey, &gotChannel, &gotSnapshot, &gotDelivery, &gotVersion, &gotNonce, &gotHeadline, &gotReason, &gotSeverity, &gotLinks, &gotOpts, &gotJoined); err != nil {
		return err
	}
	if gotAdmission != admission || gotKey != batch+":"+id || gotChannel != channel || gotSnapshot != snapshot || gotDelivery != batch+":"+id || gotHeadline != headline || gotReason != reason || gotLinks != links || gotOpts != opts || gotJoined > now {
		return fmt.Errorf("%w: batch member identity collision", ErrInterruptRejected)
	}
	// The member row is immutable history.  Its separate authority projection is
	// refreshed on every replay so a repeated fuse carries the current nonce.
	_, err = tx.ExecContext(ctx, `INSERT INTO attention_batch_member_authority(batch_id,interrupt_id,interrupt_version,nonce,headline,reason,severity,links_json,options_json,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(batch_id,interrupt_id) DO UPDATE SET interrupt_version=excluded.interrupt_version,nonce=excluded.nonce,headline=excluded.headline,reason=excluded.reason,severity=excluded.severity,links_json=excluded.links_json,options_json=excluded.options_json,updated_at_ms=excluded.updated_at_ms WHERE EXISTS (SELECT 1 FROM attention_batches WHERE id=excluded.batch_id AND state='collecting')`, batch, id, version, nonce, headline, reason, severity, links, opts, now)
	return err
}
