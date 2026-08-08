package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

func (d *DB) PrepareDueAttentionBatches(ctx context.Context, nowMS int64) error {
	rows, err := d.db.QueryContext(ctx, `SELECT id FROM attention_batches WHERE state='collecting' AND due_at_ms<=? ORDER BY due_at_ms,id`, nowMS)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := d.prepareAttentionBatch(ctx, id, nowMS); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) prepareAttentionBatch(ctx context.Context, batchID string, nowMS int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, project, channelJSON, forgeKind, host, forgeProject, targetKind, targetID, deliveryID, scope, scopeID string
	var due int64
	if err := tx.QueryRowContext(ctx, `SELECT kind,project_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,delivery_id,scope,scope_id,due_at_ms FROM attention_batches WHERE id=? AND state='collecting'`, batchID).Scan(&kind, &project, &channelJSON, &forgeKind, &host, &forgeProject, &targetKind, &targetID, &deliveryID, &scope, &scopeID, &due); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.delivery_id,m.interrupt_id,a.interrupt_version,a.nonce,a.headline,i.brief_markdown,a.reason,a.severity,a.links_json,a.options_json,i.run_id FROM attention_batch_members m JOIN attention_batch_member_authority a ON a.batch_id=m.batch_id AND a.interrupt_id=m.interrupt_id JOIN interrupts i ON i.id=m.interrupt_id WHERE m.batch_id=? AND m.excluded_at_ms IS NULL AND i.status='open' AND i.version=a.interrupt_version AND i.nonce=a.nonce ORDER BY m.interrupt_id`, batchID)
	if err != nil {
		return err
	}
	defer rows.Close()
	members := []map[string]any{}
	texts := []string{}
	for rows.Next() {
		var delivery, id, nonce, headline, brief, reason, severity, links, options, runID string
		var version int
		if err := rows.Scan(&delivery, &id, &version, &nonce, &headline, &brief, &reason, &severity, &links, &options, &runID); err != nil {
			return err
		}
		var l, o any
		if json.Unmarshal([]byte(links), &l) != nil || json.Unmarshal([]byte(options), &o) != nil {
			return fmt.Errorf("storage: corrupt batch member")
		}
		rendered, commandLines, err := renderChannelInterrupt(headline, brief, links, options, runID, nonce)
		if err != nil {
			return err
		}
		members = append(members, map[string]any{"delivery_id": delivery, "interrupt_id": id, "interrupt_version": version, "nonce": nonce, "headline": headline, "reason": reason, "severity": severity, "links": l, "options": o, "command_lines": commandLines})
		texts = append(texts, id+": "+rendered)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(members) == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE attention_batches SET state='cancelled',updated_at_ms=? WHERE id=? AND state='collecting'`, nowMS, batchID)
		if err != nil {
			return err
		}
		if kind == "critical_fuse" {
			if err := openCriticalSuccessorTx(ctx, tx, batchID, nowMS); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	var channel any
	if json.Unmarshal([]byte(channelJSON), &channel) != nil {
		return fmt.Errorf("storage: corrupt batch channel")
	}
	payloadKind := kind
	if kind == "critical_fuse" {
		payloadKind = "critical_fused"
	}
	payload, err := json.Marshal(map[string]any{"delivery_kind": "attention_batch", "batch_id": batchID, "delivery_id": deliveryID, "batch_kind": payloadKind, "channel": channel, "project_id": project, "forge_alert_target": map[string]any{"forge_kind": forgeKind, "forge_host": host, "forge_project_key": forgeProject, "target_kind": targetKind, "target_id": targetID}, "scope": scope, "scope_id": scopeID, "due_at_ms": due, "members": members, "rendered_text": joinBatchText(texts)})
	if err != nil {
		return err
	}
	key := "attention-batch:" + batchID + ":publish:1"
	if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationChannelPublish, Payload: payload}, "", "", nowMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO batch_deliveries(batch_id,delivery_id,operation_key,state,created_at_ms) VALUES(?,?,?,'pending',?)`, batchID, deliveryID, key, nowMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attention_batches SET state='sealed',operation_key=?,payload_json=?,payload_digest=?,sealed_at_ms=?,updated_at_ms=? WHERE id=? AND state='collecting'`, key, string(payload), digestJSON(payload), nowMS, nowMS, batchID); err != nil {
		return err
	}
	if kind == "critical_fuse" {
		if err := openCriticalSuccessorTx(ctx, tx, batchID, nowMS); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err == nil {
		d.wakeOutbox()
	}
	return err
}

// openCriticalSuccessorTx redecides a due fuse episode from the durable
// admitted-evidence window. A successor has a new immutable episode identity;
// the sealed batch is never retimed or reused.
func openCriticalSuccessorTx(ctx context.Context, tx *sql.Tx, batchID string, nowMS int64) error {
	var project, channel, snapshot, forgeKind, host, forgeProject, targetKind, targetID, scope, scopeID string
	var window int64
	var limitTotal, limitRun int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,scope,scope_id,critical_window_ms,critical_total_limit,critical_per_run_limit FROM attention_batches WHERE id=?`, batchID).Scan(&project, &channel, &snapshot, &forgeKind, &host, &forgeProject, &targetKind, &targetID, &scope, &scopeID, &window, &limitTotal, &limitRun); err != nil {
		return err
	}
	where, args := `a.created_at_ms>? AND a.created_at_ms<=?`, []any{nowMS - window, nowMS}
	if scope == "run" {
		where += " AND a.run_id=?"
		args = append(args, scopeID)
	}
	var count, limit int
	query := `SELECT COUNT(*) FROM attention_admissions a WHERE a.kind='critical_admitted' AND ` + where
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if scope == "global" {
		limit = limitTotal
	} else {
		limit = limitRun
	}
	if count < limit {
		return nil
	}
	var episode string
	if err := tx.QueryRowContext(ctx, `SELECT a.id FROM attention_admissions a WHERE a.kind='critical_admitted' AND `+where+` ORDER BY a.created_at_ms,a.id LIMIT 1`, args...).Scan(&episode); err != nil {
		return err
	}
	var due int64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(a.created_at_ms)+? FROM attention_admissions a WHERE a.kind='critical_admitted' AND `+where, append([]any{window}, args...)...).Scan(&due); err != nil {
		return err
	}
	enc := base64.RawURLEncoding.EncodeToString
	id := fmt.Sprintf("critical:%s:%s:%s:%s:%s:%s:%s:%s:%s", scope, scopeID, episode, channel, forgeKind, enc([]byte(host)), enc([]byte(forgeProject)), targetKind, enc([]byte(targetID)))
	deliveryID := id + ":publish:1"
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_batches(id,state,project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,kind,delivery_id,scope,scope_id,episode_admission_id,due_at_ms,critical_window_ms,critical_total_limit,critical_per_run_limit,created_at_ms,updated_at_ms) VALUES(?,'collecting',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, project, channel, snapshot, forgeKind, host, forgeProject, targetKind, targetID, "critical_fuse", deliveryID, scope, scopeID, episode, due, window, limitTotal, limitRun, nowMS, nowMS); err != nil {
		return err
	}
	var gotProject, gotChannel, gotSnapshot, gotForgeKind, gotHost, gotForgeProject, gotTargetKind, gotTargetID, gotKind, gotDelivery, gotScope, gotScopeID, gotEpisode string
	var gotDue, gotWindow int64
	var gotTotal, gotRun int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,kind,delivery_id,scope,scope_id,episode_admission_id,due_at_ms,critical_window_ms,critical_total_limit,critical_per_run_limit FROM attention_batches WHERE id=?`, id).Scan(&gotProject, &gotChannel, &gotSnapshot, &gotForgeKind, &gotHost, &gotForgeProject, &gotTargetKind, &gotTargetID, &gotKind, &gotDelivery, &gotScope, &gotScopeID, &gotEpisode, &gotDue, &gotWindow, &gotTotal, &gotRun); err != nil {
		return err
	}
	if gotProject != project || gotChannel != channel || gotSnapshot != snapshot || gotForgeKind != forgeKind || gotHost != host || gotForgeProject != forgeProject || gotTargetKind != targetKind || gotTargetID != targetID || gotKind != "critical_fuse" || gotDelivery != deliveryID || gotScope != scope || gotScopeID != scopeID || gotEpisode != episode || gotDue != due || gotWindow != window || gotTotal != limitTotal || gotRun != limitRun {
		return fmt.Errorf("storage: critical successor identity collision")
	}
	return nil
}

func joinBatchText(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "；"
		}
		out += p
	}
	return out
}

// renderChannelInterrupt is the sole deterministic Channel renderer. Command
// lines always carry the delivery's current Run and nonce; a summary never
// creates a batch-wide command.
func renderChannelInterrupt(headline, brief, linksJSON, optionsJSON, runID, nonce string) (string, []string, error) {
	var links []InterruptLink
	var options []InterruptOption
	if err := json.Unmarshal([]byte(linksJSON), &links); err != nil {
		return "", nil, fmt.Errorf("storage: corrupt interrupt links")
	}
	if err := json.Unmarshal([]byte(optionsJSON), &options); err != nil {
		return "", nil, fmt.Errorf("storage: corrupt interrupt options")
	}
	lines := []string{headline}
	if brief != "" {
		lines = append(lines, brief)
	}
	for _, link := range links {
		lines = append(lines, link.Label+": "+link.Target)
	}
	commands := make([]string, 0, len(options))
	for _, option := range options {
		lines = append(lines, option.Label+"（"+option.ID+"）："+option.Effect+"；风险："+option.Risk)
		command := "/sift " + option.ID + " " + runID + " " + nonce
		switch option.ID {
		case "reject":
			command += " [<reason>]"
		case "hold":
			command += " 1h"
		case "ask":
			command += " <text>"
		}
		commands = append(commands, command)
		lines = append(lines, command)
	}
	return joinBatchText(lines), commands, nil
}
