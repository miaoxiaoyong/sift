package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

func (d *DB) EnqueueChannelPublish(ctx context.Context, op Operation, deliveryID string, nowMS int64) error {
	if op.Kind != OperationChannelPublish || deliveryID == "" {
		return fmt.Errorf("storage: invalid channel publish")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertOperation(ctx, tx, op, op.RunID, "", nowMS); err != nil {
		return err
	}
	var p channelPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return err
	}
	if p.DeliveryID != deliveryID {
		return fmt.Errorf("storage: channel delivery identity mismatch")
	}
	if p.DeliveryKind == "attention_batch" {
		if p.BatchID == "" || deliveryID != p.BatchID+":publish:1" || p.ProjectID == "" || p.ForgeAlertTarget == nil {
			return fmt.Errorf("storage: invalid batch identity")
		}
		var channel struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			TargetRef string `json:"target_ref"`
			Renderer  string `json:"renderer"`
		}
		if json.Unmarshal(p.Channel, &channel) != nil || channel.ID == "" || channel.Type != "webhook" || channel.TargetRef == "" || channel.Renderer != "plain-v1" {
			return fmt.Errorf("storage: invalid batch channel snapshot")
		}
		kind := p.BatchKind
		if kind == "critical_fused" {
			kind = "critical_fuse"
		}
		if (kind != "daily_summary" && kind != "critical_fuse") || p.BatchID == "" || p.Scope == "" || p.ScopeID == "" || p.DueAtMS <= 0 {
			return fmt.Errorf("storage: incomplete batch authority")
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_batches(id,state,project_id,channel_id,channel_snapshot_json,forge_kind,forge_host,forge_project_key,target_kind,target_id,kind,delivery_id,scope,scope_id,due_at_ms,operation_key,payload_json,payload_digest,created_at_ms,sealed_at_ms,updated_at_ms) VALUES(?,'sealed',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.BatchID, p.ProjectID, channel.ID, string(p.Channel), p.ForgeAlertTarget.ForgeKind, p.ForgeAlertTarget.ForgeHost, p.ForgeAlertTarget.ForgeProjectKey, p.ForgeAlertTarget.TargetKind, p.ForgeAlertTarget.TargetID, kind, deliveryID, p.Scope, p.ScopeID, p.DueAtMS, op.Key, string(op.Payload), digestJSON(op.Payload), nowMS, nowMS, nowMS); err != nil {
			return err
		}
		var existingKey, existingPayload string
		if err = tx.QueryRowContext(ctx, `SELECT operation_key,payload_json FROM attention_batches WHERE id=?`, p.BatchID).Scan(&existingKey, &existingPayload); err != nil {
			return err
		}
		if existingKey != op.Key || existingPayload != string(op.Payload) {
			return ErrOperationConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO batch_deliveries(batch_id,delivery_id,operation_key,state,created_at_ms) VALUES(?,?,?,'pending',?)`, p.BatchID, deliveryID, op.Key, nowMS)
		if err == nil {
			var existingDelivery, existingKey string
			err = tx.QueryRowContext(ctx, `SELECT delivery_id,operation_key FROM batch_deliveries WHERE batch_id=?`, p.BatchID).Scan(&existingDelivery, &existingKey)
			if err == nil && (existingDelivery != deliveryID || existingKey != op.Key) {
				err = ErrOperationConflict
			}
		}
	} else {
		if p.InterruptID == "" || p.InterruptVersion < 1 || p.Nonce == "" {
			return fmt.Errorf("storage: invalid interrupt channel delivery")
		}
		var channel struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(p.Channel, &channel) != nil || channel.ID == "" {
			return fmt.Errorf("storage: invalid channel snapshot")
		}
		var forgeKind, forgeHost, forgeProject, targetKind, targetID string
		if err := tx.QueryRowContext(ctx, `SELECT r.forge_kind,r.forge_host,r.forge_project_key,CASE WHEN r.issue_id IS NOT NULL THEN 'issue' ELSE r.discussion_target_kind END,COALESCE(r.issue_id,r.discussion_target_id) FROM interrupts i JOIN runs r ON r.id=i.run_id WHERE i.id=?`, p.InterruptID).Scan(&forgeKind, &forgeHost, &forgeProject, &targetKind, &targetID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO interrupt_deliveries(id,delivery_id,interrupt_id,surface,channel_id,channel_snapshot_json,interrupt_version,nonce,escalation_no,priority,operation_key,state,attempt_count,forge_kind,forge_host,forge_project_key,forge_alert_target_kind,forge_alert_target_id,created_at_ms) VALUES(?,?,?,'channel',?,?,?,?,?,'normal',?,'pending',0,?,?,?,?,?,?)`, newID(), deliveryID, p.InterruptID, channel.ID, string(p.Channel), p.InterruptVersion, p.Nonce, p.EscalationNo, op.Key, forgeKind, forgeHost, forgeProject, targetKind, targetID, nowMS)
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		d.wakeOutbox()
	}
	return err
}

// PrepareDueAttentionBatches is the sole batch sealing port. It freezes the
// surviving member snapshots and the channel operation in one transaction.
