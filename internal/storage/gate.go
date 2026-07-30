package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// GateEvaluationRecord is the durable result of one invocation of the pure
// Gate. SnapshotJSON and VerdictJSON are already canonicalized by gate; this
// package deliberately does not re-evaluate them.
type GateBrainInputLink struct {
	LogicalCallID string
	Touchpoint    string
}

type GateEvaluationRecord struct {
	RunID, GateInputHash, GateVersion string
	SnapshotSchemaVersion             int
	SnapshotJSON, VerdictJSON         json.RawMessage
	HeadSHA, EffectivePolicyHash      string
	CertificationVersion              string
	RiskSourceVersion                 string
	VerdictDigest, ShadowDecision     string
	ConflictDigest                    string
	ReviewPolicySnapshotDigest        string
	FeaturesJSON                      json.RawMessage
	BrainInputLinks                   []GateBrainInputLink
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
	seen := make(map[string]bool, len(r.BrainInputLinks))
	for _, link := range r.BrainInputLinks {
		if link.LogicalCallID == "" || (link.Touchpoint != "T3" && link.Touchpoint != "T5") || seen[link.LogicalCallID] {
			return errors.New("storage: invalid gate brain input links")
		}
		seen[link.LogicalCallID] = true
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
	if cmd.Reason == InterruptMergeConflict {
		if cmd.Generation.ConflictDigest != MergeConflictDigest(cmd.Generation.ChangeID, cmd.Generation.HeadSHA) {
			return RecordedGateEvaluation{}, Interrupt{}, errors.New("storage: invalid merge conflict digest")
		}
		r.ConflictDigest = cmd.Generation.ConflictDigest
	}
	if cmd.Reason == InterruptCodeReview {
		if cmd.Generation.PolicySnapshotID != "" && cmd.Generation.PolicySnapshotID != r.EffectivePolicyHash {
			return RecordedGateEvaluation{}, Interrupt{}, errors.New("storage: code review policy snapshot does not match gate record")
		}
		cmd.Generation.PolicySnapshotID = r.EffectivePolicyHash
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_input_snapshots (id,gate_input_hash,schema_version,canonical_json,head_sha,effective_policy_hash,certification_version,risk_source_version,conflict_digest,created_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(gate_input_hash) DO NOTHING`, out.SnapshotID, r.GateInputHash, r.SnapshotSchemaVersion, string(r.SnapshotJSON), r.HeadSHA, r.EffectivePolicyHash, r.CertificationVersion, r.RiskSourceVersion, nullable(r.ConflictDigest), r.NowMS); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM gate_input_snapshots WHERE gate_input_hash=?`, r.GateInputHash).Scan(&out.SnapshotID); err != nil {
		return out, err
	}
	for _, link := range r.BrainInputLinks {
		var touchpoint, status string
		if err := tx.QueryRowContext(ctx, `SELECT touchpoint, status FROM brain_calls WHERE id=?`, link.LogicalCallID).Scan(&touchpoint, &status); err != nil {
			return out, err
		}
		if touchpoint != link.Touchpoint || status == BrainCallRunning {
			return out, errors.New("storage: gate brain input link must reference a terminal matching call")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO brain_gate_input_links (logical_call_id,gate_input_snapshot_id,touchpoint,created_at_ms) VALUES (?,?,?,?) ON CONFLICT(logical_call_id,gate_input_snapshot_id) DO NOTHING`, link.LogicalCallID, out.SnapshotID, link.Touchpoint, r.NowMS); err != nil {
			return out, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_cache (gate_input_hash,gate_version,snapshot_id,verdict_json,verdict_digest,created_at_ms) VALUES (?,?,?,?,?,?) ON CONFLICT(gate_input_hash,gate_version) DO NOTHING`, r.GateInputHash, r.GateVersion, out.SnapshotID, string(r.VerdictJSON), r.VerdictDigest, r.NowMS); err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gate_evaluations (id,run_id,snapshot_id,gate_version,verdict_json,verdict_digest,cache_hit,created_at_ms,review_policy_snapshot_digest) VALUES (?,?,?,?,?,?,?,?,?)`, out.EvaluationID, r.RunID, out.SnapshotID, r.GateVersion, string(r.VerdictJSON), r.VerdictDigest, gateBoolInt(r.CacheHit), r.NowMS, nullable(r.ReviewPolicySnapshotDigest)); err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO calibration_entries (id,run_id,gate_evaluation_id,predicted_decision,features_json,gate_sample_entry_id,predicted_at_ms) VALUES (?,?,?,?,?,?,?)`, out.CalibrationID, r.RunID, out.EvaluationID, r.ShadowDecision, string(r.FeaturesJSON), out.GateSampleEntryID, r.NowMS); err != nil {
		return out, err
	}
	features := r.FeaturesJSON
	digest := sha256Hex(features)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_entries (id,run_id,entry_kind,features_schema_version,features_json,features_digest,created_at_ms) VALUES (?,?,'gate_sample',1,?,?,?)`, out.GateSampleEntryID, r.RunID, string(features), digest, r.NowMS); err != nil {
		return out, err
	}
	return out, nil
}
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func gateBoolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// GateCandidate is the immutable Run identity plus the latest execution refs
// needed to freeze a Gate input after create_change has converged.
type GateCandidate struct {
	RunID, ProjectID, TaskKind, ChangeID, BaseRef, HeadRef string
	Version                                                int64
	AttemptNo, Generation                                  int
}

// GateReevaluationSource resolves the frozen Run identity (project, task
// kind, change, branch refs) for a gate_re_evaluation worker. It mirrors the
// GateCandidate projection but for a single run, so the worker can route to
// the matching Forge adapter and Gate reconciler without reconstructing these
// refs from mutable state. A run missing its change or branch refs is not a
// valid re-evaluation source.
func (d *DB) GateReevaluationSource(ctx context.Context, runID string) (GateCandidate, error) {
	if runID == "" {
		return GateCandidate{}, errors.New("storage: run id is required")
	}
	var c GateCandidate
	var changeID sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT r.id,r.project_id,COALESCE(r.kind,''),r.change_id,r.version,
		COALESCE((SELECT a.base_ref FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),''),
		COALESCE((SELECT a.branch_name FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),''),
		COALESCE((SELECT a.attempt_no FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),0),
		COALESCE((SELECT a.generation FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),0)
		FROM runs r WHERE r.id=?`, runID).Scan(&c.RunID, &c.ProjectID, &c.TaskKind, &changeID, &c.Version, &c.BaseRef, &c.HeadRef, &c.AttemptNo, &c.Generation)
	if err != nil {
		return GateCandidate{}, err
	}
	c.ChangeID = changeID.String
	return c, nil
}

// FreezeGateChangeHead records the exact Change head that Gate is about to
// evaluate. It returns the current Run version, advancing it only on head drift.
func (d *DB) FreezeGateChangeHead(ctx context.Context, runID, changeID, headSHA string, expectedVersion, nowMS int64) (int64, error) {
	if runID == "" || changeID == "" || headSHA == "" || expectedVersion < 1 || nowMS < 1 {
		return 0, errors.New("storage: invalid gate change identity")
	}
	result, err := d.db.ExecContext(ctx, `UPDATE runs SET change_head_sha=?,version=version+1,updated_at_ms=? WHERE id=? AND change_id=? AND version=? AND (change_head_sha IS NULL OR change_head_sha<>?)`, headSHA, nowMS, runID, changeID, expectedVersion, headSHA)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed == 1 {
		return expectedVersion + 1, nil
	}
	var version int64
	var currentChange, currentHead string
	if err := d.db.QueryRowContext(ctx, `SELECT version,change_id,COALESCE(change_head_sha,'') FROM runs WHERE id=?`, runID).Scan(&version, &currentChange, &currentHead); err != nil {
		return 0, err
	}
	if version != expectedVersion || currentChange != changeID || currentHead != headSHA {
		return 0, ErrRejectedStale
	}
	return version, nil
}

func (d *DB) GateCandidates(ctx context.Context, projectID string) ([]GateCandidate, error) {
	if projectID == "" {
		return nil, errors.New("storage: gate candidates require project")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT r.id,r.project_id,COALESCE(r.kind,''),r.change_id,r.version,
		COALESCE((SELECT a.base_ref FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),''),
		COALESCE((SELECT a.branch_name FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),''),
		COALESCE((SELECT a.attempt_no FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),0),
		COALESCE((SELECT a.generation FROM attempts a WHERE a.run_id=r.id ORDER BY a.attempt_no DESC LIMIT 1),0)
		FROM runs r WHERE r.project_id=? AND r.change_id IS NOT NULL
		AND r.status IN ('queued','running','waiting_human') ORDER BY r.updated_at_ms,r.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateCandidate
	for rows.Next() {
		var c GateCandidate
		if err := rows.Scan(&c.RunID, &c.ProjectID, &c.TaskKind, &c.ChangeID, &c.Version, &c.BaseRef, &c.HeadRef, &c.AttemptNo, &c.Generation); err != nil {
			return nil, err
		}
		if c.TaskKind == "" || c.BaseRef == "" || c.HeadRef == "" {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
