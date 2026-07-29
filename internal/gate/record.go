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
	v, record, err := evaluationRecord(in, cacheHit, features, nowMS)
	if err != nil || db == nil {
		if db == nil && err == nil {
			err = errors.New("gate: storage is required")
		}
		return v, storage.RecordedGateEvaluation{}, err
	}
	r, err := db.RecordGateEvaluation(ctx, record)
	return v, r, err
}

// EvaluateRecordAndEmitInterrupt is the production Gate HITL boundary. It
// evaluates exactly once, then commits the frozen snapshot, calibration and
// M3 Interrupt emission through the single atomic storage port.
func EvaluateRecordAndEmitInterrupt(ctx context.Context, db *storage.DB, in Input, cacheHit bool, features json.RawMessage, cmd storage.EmitInterruptCmd) (Verdict, storage.RecordedGateEvaluation, storage.Interrupt, error) {
	v, record, err := evaluationRecord(in, cacheHit, features, cmd.NowMS)
	if err != nil || db == nil {
		if db == nil && err == nil {
			err = errors.New("gate: storage is required")
		}
		return v, storage.RecordedGateEvaluation{}, storage.Interrupt{}, err
	}
	if v.Kind != "hitl" {
		return v, storage.RecordedGateEvaluation{}, storage.Interrupt{}, errors.New("gate: interrupt recording requires a HITL verdict")
	}
	r, interrupt, err := db.RecordGateEvaluationAndEmitInterrupt(ctx, record, cmd)
	return v, r, interrupt, err
}

func evaluationRecord(in Input, cacheHit bool, features json.RawMessage, nowMS int64) (Verdict, storage.GateEvaluationRecord, error) {
	snapshot, hash, err := CanonicalInput(in)
	if err != nil {
		return Verdict{}, storage.GateEvaluationRecord{}, err
	}
	v, err := Evaluate(in)
	if err != nil {
		return Verdict{}, storage.GateEvaluationRecord{}, err
	}
	verdict, err := canonical(v)
	if err != nil {
		return Verdict{}, storage.GateEvaluationRecord{}, err
	}
	vd, err := VerdictDigest(v)
	if err != nil {
		return Verdict{}, storage.GateEvaluationRecord{}, err
	}
	riskVersion := in.Risk.Source.Version
	if riskVersion == "" {
		riskVersion = in.Risk.Source.PromptVersion
	}
	if riskVersion == "" {
		return Verdict{}, storage.GateEvaluationRecord{}, errors.New("gate: risk source version is required for recording")
	}
	return v, storage.GateEvaluationRecord{RunID: in.Identity.RunID, GateInputHash: hash, GateVersion: Version, SnapshotSchemaVersion: in.SchemaVersion, SnapshotJSON: snapshot, VerdictJSON: verdict, HeadSHA: in.Change.HeadSHA, EffectivePolicyHash: in.EffectivePolicyHash, CertificationVersion: in.CertificationVersion, RiskSourceVersion: riskVersion, VerdictDigest: vd, ShadowDecision: ShadowDecision(v), FeaturesJSON: features, BrainInputLinks: brainInputLinks(in), CacheHit: cacheHit, NowMS: nowMS}, nil
}

func brainInputLinks(in Input) []storage.GateBrainInputLink {
	byID := map[string]string{}
	if source := in.Risk.Source; source.LogicalCallID != "" {
		byID[source.LogicalCallID] = "T3"
	}
	if in.Checks.Triage != nil {
		if source := in.Checks.Triage.Source; source.LogicalCallID != "" {
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
