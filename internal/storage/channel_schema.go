package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

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
