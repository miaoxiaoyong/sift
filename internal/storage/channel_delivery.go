package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func (d *DB) ChannelDiagnostics(ctx context.Context) ([]map[string]any, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT d.delivery_id,d.channel_id,d.operation_key,d.state,d.attempt_count,COALESCE(d.last_error,''),d.created_at_ms,COALESCE(channel_op.next_attempt_at_ms,0),COALESCE(e.consecutive_failures,0),COALESCE(e.state,''),COALESCE(e.last_error_class,''),COALESCE(e.alert_operation_key,''),COALESCE(alert_op.state,'') FROM interrupt_deliveries d LEFT JOIN outbox_operations channel_op ON channel_op.operation_key=d.operation_key LEFT JOIN channel_failure_episodes e ON e.subject_id=d.delivery_id AND e.generation=1 LEFT JOIN outbox_operations alert_op ON alert_op.operation_key=e.alert_operation_key WHERE d.surface='channel' ORDER BY d.created_at_ms,d.delivery_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, channelID, key, state, last string
		var attempts, created, nextRetry, failures int64
		var episode, errorClass, alertKey, alertState string
		if err := rows.Scan(&id, &channelID, &key, &state, &attempts, &last, &created, &nextRetry, &failures, &episode, &errorClass, &alertKey, &alertState); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"delivery_id": id, "channel_id": channelID, "operation_key": key, "state": state, "attempt_count": attempts, "last_error": last, "created_at_ms": created, "next_attempt_at_ms": nextRetry, "consecutive_failures": failures, "episode_state": episode, "last_error_class": errorClass, "alert_operation_key": alertKey, "alert_state": alertState, "generated_not_delivered": state != "delivered"})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	batchRows, err := d.db.QueryContext(ctx, `SELECT d.delivery_id,b.channel_id,d.operation_key,d.state,d.attempt_count,COALESCE(d.last_error,''),d.created_at_ms,COALESCE(channel_op.next_attempt_at_ms,0),COALESCE(e.consecutive_failures,0),COALESCE(e.state,''),COALESCE(e.last_error_class,''),COALESCE(e.alert_operation_key,''),COALESCE(alert_op.state,'') FROM batch_deliveries d JOIN attention_batches b ON b.id=d.batch_id LEFT JOIN outbox_operations channel_op ON channel_op.operation_key=d.operation_key LEFT JOIN channel_failure_episodes e ON e.subject_id=d.delivery_id AND e.generation=1 LEFT JOIN outbox_operations alert_op ON alert_op.operation_key=e.alert_operation_key ORDER BY d.created_at_ms,d.delivery_id`)
	if err != nil {
		return nil, err
	}
	defer batchRows.Close()
	for batchRows.Next() {
		var id, channelID, key, state, last, episode, errorClass, alertKey, alertState string
		var attempts, created, nextRetry, failures int64
		if err := batchRows.Scan(&id, &channelID, &key, &state, &attempts, &last, &created, &nextRetry, &failures, &episode, &errorClass, &alertKey, &alertState); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"delivery_id": id, "channel_id": channelID, "operation_key": key, "state": state, "attempt_count": attempts, "last_error": last, "created_at_ms": created, "next_attempt_at_ms": nextRetry, "consecutive_failures": failures, "episode_state": episode, "last_error_class": errorClass, "alert_operation_key": alertKey, "alert_state": alertState, "generated_not_delivered": state != "delivered"})
	}
	return out, batchRows.Err()
}

func applyChannelOutcomeTx(ctx context.Context, tx *sql.Tx, claim ClaimedOperation, outcome CompleteOutcome, _ bool) error {
	var p channelPayload
	if err := json.Unmarshal(claim.Payload, &p); err != nil || p.DeliveryID == "" {
		return fmt.Errorf("storage: invalid channel payload")
	}
	subject := channelSubject(p)
	batch := p.DeliveryKind == "attention_batch"
	var remote struct {
		RemoteRef string `json:"remote_ref"`
	}
	_ = json.Unmarshal(outcome.Evidence, &remote)
	var res sql.Result
	var err error
	if batch {
		res, err = tx.ExecContext(ctx, `UPDATE batch_deliveries SET attempt_count=attempt_count+1, state=?, remote_ref=CASE WHEN ?='delivered' THEN ? ELSE remote_ref END, last_error=?, delivered_at_ms=CASE WHEN ?='delivered' THEN ? ELSE delivered_at_ms END WHERE delivery_id=? AND operation_key=?`, channelDeliveryState(outcome), channelDeliveryState(outcome), nullable(remote.RemoteRef), nullable(outcome.ErrorSummary), channelDeliveryState(outcome), outcome.NowMS, subject, claim.Key)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE interrupt_deliveries SET attempt_count=attempt_count+1, state=?, remote_ref=CASE WHEN ?='delivered' THEN ? ELSE remote_ref END, last_error=?, delivered_at_ms=CASE WHEN ?='delivered' THEN ? ELSE delivered_at_ms END WHERE delivery_id=? AND operation_key=?`, channelDeliveryState(outcome), channelDeliveryState(outcome), nullable(remote.RemoteRef), nullable(outcome.ErrorSummary), channelDeliveryState(outcome), outcome.NowMS, subject, claim.Key)
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("storage: missing channel delivery projection")
	}
	if batch && outcome.State != OperationSucceeded {
		// The batch is immutable once sealed; delivery/episode projections carry
		// failure state instead of inventing a second batch terminal state.
		if _, err := tx.ExecContext(ctx, `UPDATE attention_batches SET updated_at_ms=? WHERE id=? AND state='sealed'`, outcome.NowMS, p.BatchID); err != nil {
			return err
		}
	}
	var old int
	var oldAlert sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures,alert_operation_key FROM channel_failure_episodes WHERE subject_id=? AND generation=1`, subject).Scan(&old, &oldAlert)
	if err == sql.ErrNoRows {
		if _, err = tx.ExecContext(ctx, `INSERT INTO channel_failure_episodes(subject_id,generation,consecutive_failures,state,last_error_class,created_at_ms,updated_at_ms) VALUES(?,1,0,'open',NULL,?,?)`, subject, outcome.NowMS, outcome.NowMS); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if outcome.State == OperationSucceeded {
		if batch {
			_, err = tx.ExecContext(ctx, `UPDATE attention_batches SET state='delivered',delivered_at_ms=?,updated_at_ms=? WHERE id=? AND state='sealed'`, outcome.NowMS, outcome.NowMS, p.BatchID)
			if err != nil {
				return err
			}
			// Delivery evidence is a Ledger fact for every frozen member. The
			// deterministic id makes completion/replay idempotent.
			var members []struct{ ID, RunID string }
			rows, qerr := tx.QueryContext(ctx, `SELECT m.interrupt_id,i.run_id FROM attention_batch_members m JOIN interrupts i ON i.id=m.interrupt_id WHERE m.batch_id=? AND m.excluded_at_ms IS NULL`, p.BatchID)
			if qerr != nil {
				return qerr
			}
			for rows.Next() {
				var m struct{ ID, RunID string }
				if err := rows.Scan(&m.ID, &m.RunID); err != nil {
					rows.Close()
					return err
				}
				members = append(members, m)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			for _, m := range members {
				ledgerID := "channel_delivery:" + subject + ":" + m.ID
				features := `{"surface":"channel","delivery_state":"delivered"}`
				if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ledger_entries(id,run_id,interrupt_id,entry_kind,features_schema_version,features_json,created_at_ms) VALUES(?,?,?,'attention_delivery',1,?,?)`, ledgerID, m.RunID, m.ID, features, outcome.NowMS); err != nil {
					return err
				}
				var gotRun, gotInterrupt, gotKind, gotFeatures string
				var gotVersion int
				if err = tx.QueryRowContext(ctx, `SELECT run_id,interrupt_id,entry_kind,features_schema_version,features_json FROM ledger_entries WHERE id=?`, ledgerID).Scan(&gotRun, &gotInterrupt, &gotKind, &gotVersion, &gotFeatures); err != nil {
					return err
				}
				if gotRun != m.RunID || gotInterrupt != m.ID || gotKind != "attention_delivery" || gotVersion != 1 || gotFeatures != features {
					return ErrOperationConflict
				}
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE channel_failure_episodes SET consecutive_failures=0,state='ended_delivered',last_error_class=NULL,updated_at_ms=?,ended_at_ms=? WHERE subject_id=? AND generation=1 AND state NOT LIKE 'ended_%'`, outcome.NowMS, outcome.NowMS, subject)
		return err
	}
	count := old + 1
	threshold := outcome.ChannelFailureAlertAfter
	if threshold <= 0 {
		threshold = 3
	}
	terminal := outcome.State != OperationRetryable
	state := "open"
	if terminal {
		state = "ended_failed"
	} else if oldAlert.Valid {
		state = "alerted"
	} else if count >= threshold {
		state = "alerted"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE channel_failure_episodes SET consecutive_failures=?,state=?,last_error_class=?,updated_at_ms=?,ended_at_ms=CASE WHEN ? THEN ? ELSE ended_at_ms END WHERE subject_id=? AND generation=1 AND state NOT LIKE 'ended_%'`, count, state, nullable(string(outcome.ErrorClass)), outcome.NowMS, terminal, outcome.NowMS, subject); err != nil {
		return err
	}
	if old < threshold && count >= threshold && !oldAlert.Valid {
		target := p.ForgeAlertTarget
		if target == nil && !batch {
			target = &forgeAlertTarget{}
			err := tx.QueryRowContext(ctx, `SELECT forge_kind,forge_host,forge_project_key,forge_alert_target_kind,forge_alert_target_id FROM interrupt_deliveries WHERE delivery_id=? AND operation_key=?`, subject, claim.Key).Scan(&target.ForgeKind, &target.ForgeHost, &target.ForgeProjectKey, &target.TargetKind, &target.TargetID)
			if err != nil || target.ForgeKind == "" || target.ForgeHost == "" || target.ForgeProjectKey == "" || target.TargetKind == "" || target.TargetID == "" {
				return fmt.Errorf("storage: missing frozen channel alert target")
			}
		}
		if target == nil {
			return fmt.Errorf("storage: missing batch channel alert target")
		}
		key := AlertOperationKey("channel_failure", subject, 1)
		markdown := fmt.Sprintf("[sift alert:channel_failure:%s:1]\nChannel operation: %s\nEpisode generation: 1\nConsecutive failures: %d\nLatest error class: %s\nStatus: generated_not_delivered\nDiagnostics: sift ps; sift doctor", subject, claim.Key, count, outcome.ErrorClass)
		payload, _ := json.Marshal(map[string]any{"forge_kind": target.ForgeKind, "forge_host": target.ForgeHost, "forge_project_key": target.ForgeProjectKey, "target_kind": target.TargetKind, "target_id": target.TargetID, "purpose": "channel_failure", "markdown": markdown})
		if err := insertOperation(ctx, tx, Operation{Key: key, Kind: OperationForgeAlert, Payload: payload}, "", "", outcome.NowMS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channel_failure_episodes SET alert_operation_key=? WHERE subject_id=? AND generation=1`, key, subject); err != nil {
			return err
		}
	}
	return nil
}

func channelDeliveryState(o CompleteOutcome) string {
	if o.State == OperationSucceeded {
		return "delivered"
	}
	if o.State == OperationRetryable {
		return "pending"
	}
	return "failed"
}

// EnqueueChannelPublish creates the durable delivery projection and immutable
// operation together. Callers pass already sealed channel_publish bytes.
