package gate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/miaoxiaoyong/sift/internal/storage"
)

// EvaluateAndRecord is the Gate application boundary: callers receive the
// deterministic verdict only after its immutable snapshot and shadow sample
// have been committed. CacheHit describes the caller's cache lookup; it never
// suppresses a new evaluation/calibration row.
func EvaluateAndRecord(ctx context.Context, db *storage.DB, in Input, cacheHit bool, features json.RawMessage, nowMS int64) (Verdict, storage.RecordedGateEvaluation, error) {
	if db == nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, errors.New("gate: storage is required")
	}
	snapshot, hash, err := CanonicalInput(in)
	if err != nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, err
	}
	v, err := Evaluate(in)
	if err != nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, err
	}
	verdict, err := canonical(v)
	if err != nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, err
	}
	vd, err := VerdictDigest(v)
	if err != nil {
		return Verdict{}, storage.RecordedGateEvaluation{}, err
	}
	riskVersion := in.Risk.Source.Version
	if riskVersion == "" {
		riskVersion = in.Risk.Source.PromptVersion
	}
	if riskVersion == "" {
		return Verdict{}, storage.RecordedGateEvaluation{}, errors.New("gate: risk source version is required for recording")
	}
	r, err := db.RecordGateEvaluation(ctx, storage.GateEvaluationRecord{RunID: in.Identity.RunID, GateInputHash: hash, GateVersion: Version, SnapshotSchemaVersion: in.SchemaVersion, SnapshotJSON: snapshot, VerdictJSON: verdict, HeadSHA: in.Change.HeadSHA, EffectivePolicyHash: in.EffectivePolicyHash, CertificationVersion: in.CertificationVersion, RiskSourceVersion: riskVersion, VerdictDigest: vd, ShadowDecision: ShadowDecision(v), FeaturesJSON: features, CacheHit: cacheHit, NowMS: nowMS})
	return v, r, err
}
