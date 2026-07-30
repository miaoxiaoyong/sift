package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ensureChannelSchema is kept separate from the historical M1 migration so
// opening databases created by older binaries remains safe. The tables are
// additive projections and are deliberately not used as a second domain write
// port.
func ensureChannelSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS attention_batches (
 id TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('collecting','sealed','delivered','failed','cancelled')),
 project_id TEXT NOT NULL, channel_id TEXT NOT NULL, channel_snapshot_json TEXT NOT NULL,
 forge_kind TEXT NOT NULL, forge_host TEXT NOT NULL, forge_project_key TEXT NOT NULL,
 target_kind TEXT NOT NULL, target_id TEXT NOT NULL, operation_key TEXT, updated_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS batch_deliveries (
 batch_id TEXT NOT NULL PRIMARY KEY, delivery_id TEXT NOT NULL UNIQUE,
 operation_key TEXT NOT NULL UNIQUE, state TEXT NOT NULL CHECK(state IN ('pending','delivered','failed')),
 attempt_count INTEGER NOT NULL DEFAULT 0, remote_ref TEXT, last_error TEXT,
 created_at_ms INTEGER NOT NULL, delivered_at_ms INTEGER);
CREATE TABLE IF NOT EXISTS attention_admissions (
 id TEXT NOT NULL PRIMARY KEY, interrupt_id TEXT NOT NULL REFERENCES interrupts(id),
 admission_key TEXT NOT NULL UNIQUE, kind TEXT NOT NULL CHECK(kind IN ('critical_admitted','critical_fused')),
 metric_identity TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES runs(id),
 critical_source TEXT NOT NULL CHECK(critical_source IN ('initial','escalation')), created_at_ms INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS attention_admissions_interrupt_critical ON attention_admissions(interrupt_id);
CREATE TABLE IF NOT EXISTS channel_failure_episodes (
 subject_id TEXT NOT NULL, generation INTEGER NOT NULL CHECK(generation=1),
 consecutive_failures INTEGER NOT NULL CHECK(consecutive_failures>=0),
 state TEXT NOT NULL CHECK(state IN ('open','alerted','ended_delivered','ended_failed')),
 last_error_class TEXT, alert_operation_key TEXT UNIQUE, created_at_ms INTEGER NOT NULL,
 updated_at_ms INTEGER NOT NULL, ended_at_ms INTEGER, PRIMARY KEY(subject_id,generation));
CREATE UNIQUE INDEX IF NOT EXISTS attention_batches_identity ON attention_batches(project_id,kind,channel_id,scope,scope_id,forge_kind,forge_host,forge_project_key,target_kind,target_id);
CREATE INDEX IF NOT EXISTS channel_failure_episodes_state ON channel_failure_episodes(state,updated_at_ms);
CREATE INDEX IF NOT EXISTS interrupt_deliveries_channel_state ON interrupt_deliveries(surface,state,created_at_ms);
CREATE TRIGGER IF NOT EXISTS channel_delivery_target_required
BEFORE INSERT ON interrupt_deliveries
WHEN NEW.surface='channel' AND NEW.delivery_id IS NOT NULL AND (NEW.channel_id IS NULL OR NEW.channel_snapshot_json IS NULL OR NEW.forge_kind IS NULL OR NEW.forge_host IS NULL OR NEW.forge_project_key IS NULL OR NEW.forge_alert_target_kind IS NULL OR NEW.forge_alert_target_id IS NULL)
BEGIN SELECT RAISE(ABORT,'channel delivery requires frozen target'); END;
CREATE TRIGGER IF NOT EXISTS attention_batch_sealed_immutable
BEFORE UPDATE ON attention_batches
WHEN OLD.state IN ('sealed','delivered','cancelled') AND (NEW.payload_json IS NOT OLD.payload_json OR NEW.payload_digest IS NOT OLD.payload_digest OR NEW.operation_key IS NOT OLD.operation_key OR NEW.delivery_id IS NOT OLD.delivery_id)
BEGIN SELECT RAISE(ABORT,'sealed attention batch is immutable'); END;`)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT i.id,i.reason,i.run_id,i.created_at_ms FROM interrupts i LEFT JOIN interrupt_command_effect_bindings b ON b.interrupt_id=i.id WHERE b.interrupt_id IS NULL`)
	if err != nil {
		return err
	}
	type legacyBinding struct {
		id, reason, runID string
		created           int64
	}
	var missing []legacyBinding
	for rows.Next() {
		var row legacyBinding
		if err := rows.Scan(&row.id, &row.reason, &row.runID, &row.created); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range missing {
		// Historical rows did not retain every arm-specific identity. Keep the
		// binding explicit and unique rather than fabricating run_transition.
		binding, _ := json.Marshal(map[string]any{"arm": row.reason, "legacy_interrupt_id": row.id, "run_id": row.runID})
		sum := sha256.Sum256(binding)
		if _, err := db.ExecContext(ctx, `INSERT INTO interrupt_command_effect_bindings(interrupt_id,reason,binding_schema_version,binding_json,binding_digest,created_at_ms) VALUES(?,?,1,?,?,?)`, row.id, row.reason, string(binding), hex.EncodeToString(sum[:]), row.created); err != nil {
			return err
		}
	}
	return nil
}

type channelPayload struct {
	DeliveryKind     string            `json:"delivery_kind"`
	DeliveryID       string            `json:"delivery_id"`
	InterruptID      string            `json:"interrupt_id"`
	InterruptVersion int               `json:"interrupt_version"`
	Nonce            string            `json:"nonce"`
	EscalationNo     int               `json:"escalation_no"`
	BatchID          string            `json:"batch_id"`
	BatchKind        string            `json:"batch_kind"`
	Scope            string            `json:"scope"`
	ScopeID          string            `json:"scope_id"`
	DueAtMS          int64             `json:"due_at_ms"`
	Channel          json.RawMessage   `json:"channel"`
	ProjectID        string            `json:"project_id"`
	ForgeAlertTarget *forgeAlertTarget `json:"forge_alert_target"`
}
type forgeAlertTarget struct {
	ForgeKind       string `json:"forge_kind"`
	ForgeHost       string `json:"forge_host"`
	ForgeProjectKey string `json:"forge_project_key"`
	TargetKind      string `json:"target_kind"`
	TargetID        string `json:"target_id"`
}

func channelSubject(p channelPayload) string { return p.DeliveryID }

// ChannelDiagnostics reads the durable Channel projections used by operator
// views. It intentionally does not infer state from in-memory workers.
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
