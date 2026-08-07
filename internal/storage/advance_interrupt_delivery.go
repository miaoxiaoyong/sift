package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func enqueueInterruptChannelTx(ctx context.Context, tx *sql.Tx, id string, version int64, nonce string, escalation int, priority string, now int64) error {
	var channel, snapshot, headline, brief, links, options, runID, forgeKind, forgeHost, forgeProject, targetKind, targetID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(i.channel_id,''),COALESCE(i.channel_snapshot_json,''),i.headline,i.brief_markdown,i.links_json,i.options_json,i.run_id,r.forge_kind,r.forge_host,r.forge_project_key,CASE WHEN r.issue_id IS NOT NULL THEN 'issue' ELSE r.discussion_target_kind END,COALESCE(r.issue_id,r.discussion_target_id) FROM interrupts i JOIN runs r ON r.id=i.run_id WHERE i.id=?`, id).Scan(&channel, &snapshot, &headline, &brief, &links, &options, &runID, &forgeKind, &forgeHost, &forgeProject, &targetKind, &targetID); err != nil {
		return err
	}
	if channel == "" || snapshot == "" {
		return nil
	}
	deliveryID := fmt.Sprintf("interrupt:%s:%d:%s", id, escalation, channel)
	key := ChannelPublishOperationKey(id, escalation)
	var ch any
	if err := json.Unmarshal([]byte(snapshot), &ch); err != nil {
		return err
	}
	rendered, commandLines, err := renderChannelInterrupt(headline, brief, links, options, runID, nonce)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"delivery_kind": "interrupt", "delivery_id": deliveryID, "interrupt_id": id, "escalation_no": escalation, "priority": priority, "interrupt_version": version, "nonce": nonce, "channel": ch, "command_lines": commandLines, "rendered_text": rendered})
	if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationChannelPublish, Payload: payload, InterruptID: id}, "", "", now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO interrupt_deliveries(id,delivery_id,interrupt_id,surface,channel_id,channel_snapshot_json,interrupt_version,nonce,escalation_no,priority,operation_key,state,attempt_count,forge_kind,forge_host,forge_project_key,forge_alert_target_kind,forge_alert_target_id,created_at_ms) VALUES(?,?,?,'channel',?,?,?,?,?,?,?,'pending',0,?,?,?,?,?,?)`, newID(), deliveryID, id, channel, snapshot, version, nonce, escalation, priority, key, forgeKind, forgeHost, forgeProject, targetKind, targetID, now)
	return err
}

func admitCriticalTx(ctx context.Context, tx *sql.Tx, id string, finalSeverity InterruptSeverity, now int64, source string, window int64, total, perRun int) (bool, string, string, error) {
	var run, severity, charge, zone string
	if err := tx.QueryRowContext(ctx, `SELECT run_id,severity,COALESCE(charged_budget_entry_id,''),COALESCE(day_timezone,'UTC') FROM interrupts WHERE id=?`, id).Scan(&run, &severity, &charge, &zone); err != nil {
		return false, "", "", err
	}
	key := id + ":critical"
	var kind, existing string
	if err := tx.QueryRowContext(ctx, `SELECT id,kind FROM attention_admissions WHERE admission_key=?`, key).Scan(&existing, &kind); err == nil {
		if kind == "critical_admitted" {
			return true, existing, "", nil
		}
		var scope string
		if err := tx.QueryRowContext(ctx, `SELECT b.scope FROM attention_batch_members m JOIN attention_batches b ON b.id=m.batch_id WHERE m.admission_id=? AND b.kind='critical_fuse' ORDER BY b.created_at_ms LIMIT 1`, existing).Scan(&scope); err != nil {
			return false, existing, "", err
		}
		return false, existing, scope, nil
	} else if err != sql.ErrNoRows {
		return false, "", "", err
	}
	var global, local int
	// The window is (now-window, now], so evidence at the left boundary has
	// expired while same-millisecond committed evidence is counted.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_admissions WHERE kind='critical_admitted' AND created_at_ms>? AND created_at_ms<=?`, now-window, now).Scan(&global); err != nil {
		return false, "", "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_admissions WHERE kind='critical_admitted' AND run_id=? AND created_at_ms>? AND created_at_ms<=?`, run, now-window, now).Scan(&local); err != nil {
		return false, "", "", err
	}
	kind, scope := "critical_admitted", ""
	if global >= total {
		kind, scope = "critical_fused", "global"
	} else if local >= perRun {
		kind, scope = "critical_fused", "run"
	}
	admission := newID()
	_, err := tx.ExecContext(ctx, `INSERT INTO attention_admissions(id,interrupt_id,admission_key,kind,metric_identity,attention_charge_entry_id,severity,day_timezone,run_id,critical_source,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, admission, id, key, kind, id, nullable(charge), finalSeverity, zone, run, source, now)
	return kind == "critical_admitted", admission, scope, err
}
