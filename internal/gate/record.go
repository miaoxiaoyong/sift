package gate

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

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
	links := brainInputLinks(in)
	riskVersion := in.Risk.Source.Version
	if riskVersion == "" {
		riskVersion = in.Risk.Source.PromptVersion
	}
	if riskVersion == "" {
		return Verdict{}, storage.RecordedGateEvaluation{}, errors.New("gate: risk source version is required for recording")
	}
	r, err := db.RecordGateEvaluation(ctx, storage.GateEvaluationRecord{RunID: in.Identity.RunID, GateInputHash: hash, GateVersion: Version, SnapshotSchemaVersion: in.SchemaVersion, SnapshotJSON: snapshot, VerdictJSON: verdict, HeadSHA: in.Change.HeadSHA, EffectivePolicyHash: in.EffectivePolicyHash, CertificationVersion: in.CertificationVersion, RiskSourceVersion: riskVersion, VerdictDigest: vd, ShadowDecision: ShadowDecision(v), FeaturesJSON: features, BrainInputLinks: links, CacheHit: cacheHit, NowMS: nowMS})
	return v, r, err
}

func brainInputLinks(in Input) []storage.GateBrainInputLink {
	byID := map[string]string{}
	if source := in.Risk.Source; source.Kind == "brain" && source.LogicalCallID != "" {
		byID[source.LogicalCallID] = "T3"
	}
	if in.Checks.Triage != nil {
		if source := in.Checks.Triage.Source; source.Kind == "brain" && source.LogicalCallID != "" {
			byID[source.LogicalCallID] = "T5"
		}
	}
	links := make([]storage.GateBrainInputLink, 0, len(byID))
	for id, touchpoint := range byID {
		links = append(links, storage.GateBrainInputLink{LogicalCallID: id, Touchpoint: touchpoint})
	}
	// The canonical Gate input requires deterministic collection ordering.
	sort.Slice(links, func(i, j int) bool { return links[i].LogicalCallID < links[j].LogicalCallID })
	return links
}
