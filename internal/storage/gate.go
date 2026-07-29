package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// GateEvaluationRecord is the durable result of one invocation of the pure
// Gate. SnapshotJSON and VerdictJSON are already canonicalized by gate; this
// package deliberately does not re-evaluate them.
type GateEvaluationRecord struct {
	RunID, GateInputHash, GateVersion string
	SnapshotSchemaVersion             int
	SnapshotJSON, VerdictJSON         json.RawMessage
	HeadSHA, EffectivePolicyHash      string
	CertificationVersion              string
	RiskSourceVersion                 string
	VerdictDigest, ShadowDecision     string
	FeaturesJSON                      json.RawMessage
	CacheHit                          bool
	NowMS                             int64
}

type RecordedGateEvaluation struct {
	SnapshotID, EvaluationID, CalibrationID, GateSampleEntryID string
}

func (r GateEvaluationRecord) validate() error {
	if r.RunID == "" || r.GateInputHash == "" || r.GateVersion == "" || r.SnapshotSchemaVersion < 1 ||
		r.HeadSHA == "" || r.EffectivePolicyHash == "" || r.CertificationVersion == "" || r.RiskSourceVersion == "" ||
		r.VerdictDigest == "" || r.NowMS <= 0 || !json.Valid(r.SnapshotJSON) || !json.Valid(r.VerdictJSON) || !json.Valid(r.FeaturesJSON) {
		return errors.New("storage: invalid gate evaluation record")
	}
	if r.ShadowDecision != "allow" && r.ShadowDecision != "block" && r.ShadowDecision != "inconclusive" {
		return errors.New("storage: invalid gate shadow decision")
	}
	return nil
}

// RecordGateEvaluation appends the snapshot/evaluation/calibration/gate sample
// for every Gate call, including cache hits. It has no optional "shadow off"
// path.
func (d *DB) RecordGateEvaluation(ctx context.Context, r GateEvaluationRecord) (RecordedGateEvaluation, error) {
	if err := r.validate(); err != nil {
		return RecordedGateEvaluation{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordedGateEvaluation{}, err
	}
	defer tx.Rollback()
	out, err := recordGateEvaluationTx(ctx, tx, r)
	if err != nil {
		return RecordedGateEvaluation{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecordedGateEvaluation{}, err
	}
	return out, nil
}

// RecordGateEvaluationAndEmitInterrupt is the only Gate HITL write port. It
// atomically freezes the prediction and executes all five M3 Interrupt writes;
// a failed publish operation or attention charge rolls back the calibration as
// well as the waiting_human transition.
func (d *DB) RecordGateEvaluationAndEmitInterrupt(ctx context.Context, r GateEvaluationRecord, cmd EmitInterruptCmd) (RecordedGateEvaluation, Interrupt, error) {
	if err := r.validate(); err != nil {
		return RecordedGateEvaluation{}, Interrupt{}, err
	}
	if cmd.RunID != r.RunID || cmd.CalibrationID != "" {
		return RecordedGateEvaluation{}, Interrupt{}, errors.New("storage: invalid gate interrupt binding")
	}
	var out RecordedGateEvaluation
	cmd.CalibrationID = newID()
	in, err := d.emitInterrupt(ctx, cmd, func(tx *sql.Tx) error {
		var e error
		out, e = recordGateEvaluationTxWithIDs(ctx, tx, r, RecordedGateEvaluation{CalibrationID: cmd.CalibrationID})
		return e
	})
	if err != nil {
		return RecordedGateEvaluation{}, Interrupt{}, err
	}
	return out, in, nil
}

func recordGateEvaluationTx(ctx context.Context, tx *sql.Tx, r GateEvaluationRecord) (RecordedGateEvaluation, error) {
	return recordGateEvaluationTxWithIDs(ctx, tx, r, RecordedGateEvaluation{})
}
func recordGateEvaluationTxWithIDs(ctx context.Context, tx *sql.Tx, r GateEvaluationRecord, out RecordedGateEvaluation) (RecordedGateEvaluation, error) {
	if out.SnapshotID == "" {
		out.SnapshotID = newID()
	}
	if out.EvaluationID == "" {
		out.EvaluationID = newID()
	}
	if out.CalibrationID == "" {
		out.CalibrationID = newID()
	}
	if out.GateSampleEntryID == "" {
		out.GateSampleEntryID = newID()
	}
	// Snapshots are content-addressed. Repeated calls share the immutable input
	// row but always receive fresh evaluation/calibration rows below.
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_input_snapshots (id,gate_input_hash,schema_version,canonical_json,head_sha,effective_policy_hash,certification_version,risk_source_version,created_at_ms) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(gate_input_hash) DO NOTHING`, out.SnapshotID, r.GateInputHash, r.SnapshotSchemaVersion, string(r.SnapshotJSON), r.HeadSHA, r.EffectivePolicyHash, r.CertificationVersion, r.RiskSourceVersion, r.NowMS); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM gate_input_snapshots WHERE gate_input_hash=?`, r.GateInputHash).Scan(&out.SnapshotID); err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_cache (gate_input_hash,gate_version,snapshot_id,verdict_json,verdict_digest,created_at_ms) VALUES (?,?,?,?,?,?) ON CONFLICT(gate_input_hash,gate_version) DO NOTHING`, r.GateInputHash, r.GateVersion, out.SnapshotID, string(r.VerdictJSON), r.VerdictDigest, r.NowMS); err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_evaluations (id,run_id,snapshot_id,gate_version,verdict_json,verdict_digest,cache_hit,created_at_ms) VALUES (?,?,?,?,?,?,?,?)`, out.EvaluationID, r.RunID, out.SnapshotID, r.GateVersion, string(r.VerdictJSON), r.VerdictDigest, gateBoolInt(r.CacheHit), r.NowMS); err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO calibration_entries (id,run_id,gate_evaluation_id,predicted_decision,features_json,predicted_at_ms) VALUES (?,?,?,?,?,?)`, out.CalibrationID, r.RunID, out.EvaluationID, r.ShadowDecision, string(r.FeaturesJSON), r.NowMS); err != nil {
		return out, err
	}
	features, err := json.Marshal(map[string]string{"calibration_id": out.CalibrationID, "gate_evaluation_id": out.EvaluationID})
	if err != nil {
		return out, fmt.Errorf("storage: gate sample features: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (id,run_id,entry_kind,features_schema_version,features_json,created_at_ms) VALUES (?,?,'gate_sample',1,?,?)`, out.GateSampleEntryID, r.RunID, string(features), r.NowMS); err != nil {
		return out, err
	}
	return out, nil
}
func gateBoolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
