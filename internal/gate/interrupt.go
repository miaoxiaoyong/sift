package gate

import (
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func interruptCommand(c storage.GateCandidate, in Input, v Verdict, attention config.Attention, nowMS int64) (storage.EmitInterruptCmd, error) {
	cmd := storage.EmitInterruptCmd{RunID: c.RunID, ExpectedRunVersion: c.Version, Generation: storage.InterruptGeneration{ChangeID: in.Identity.ChangeID, HeadSHA: in.Change.HeadSHA}, GatePhase: storage.GateReview, GuardrailLevel: storage.GuardrailNone, AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: attention.DailyQuota.Low, storage.SeverityNormal: attention.DailyQuota.Normal, storage.SeverityHigh: attention.DailyQuota.High}, DayTimezone: attention.DayTimezone, MaxEscalations: attention.MaxEscalations, Source: storage.SourceSystem, NowMS: nowMS}
	changeRef := "https://sift.invalid/change/" + in.Identity.ChangeID
	switch v.Code {
	case "guardrail_violation":
		cmd.Reason, cmd.GuardrailLevel = storage.InterruptGuardrailViolation, storage.GuardrailSoft
		cmd.Generation.ViolationCode, cmd.Generation.SubjectDigest = v.RuleID, v.MatchedPathsDigest
		cmd.Facts = map[string]string{"rule_id": v.RuleID, "impact_scope": v.MatchedPathsDigest, "recommended_action": "approve", "policy_evidence_ref": changeRef}
	case "code_review":
		cmd.Reason = storage.InterruptCodeReview
		cmd.Facts = map[string]string{"change_ref": changeRef, "head_sha": in.Change.HeadSHA, "review_requirement": v.ReviewPolicy, "recommended_action": "approve", "diff_ref": changeRef + "/diff"}
	case "merge_conflict", "mergeability_unknown":
		cmd.Reason, cmd.GatePhase = storage.InterruptMergeConflict, storage.GateMerge
		cmd.Generation.ConflictDigest = in.Change.HeadSHA
		cmd.Facts = map[string]string{"change_ref": changeRef, "head_sha": in.Change.HeadSHA, "conflict_summary": v.Code, "recommended_action": "retry", "conflict_evidence_ref": changeRef}
	default:
		cmd.Reason = storage.InterruptFailureReview
		cmd.Facts = map[string]string{"failure_class": v.Code, "failure_evidence_ref": changeRef, "recommended_action": "retry"}
	}
	if cmd.Reason == "" {
		return storage.EmitInterruptCmd{}, fmt.Errorf("gate reconciler: no interrupt for %s", v.Code)
	}
	return cmd, nil
}
