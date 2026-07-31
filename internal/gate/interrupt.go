package gate

import (
	"crypto/sha256"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func interruptCommand(c storage.GateCandidate, in Input, v Verdict, attention config.Attention, channels []storage.InterruptChannel, nowMS int64) (storage.EmitInterruptCmd, error) {
	cmd := storage.EmitInterruptCmd{RunID: c.RunID, ExpectedRunVersion: c.Version, Generation: storage.InterruptGeneration{ChangeID: in.Identity.ChangeID, HeadSHA: in.Change.HeadSHA, PolicySnapshotID: in.EffectivePolicyHash}, GatePhase: storage.GateReview, GuardrailLevel: storage.GuardrailNone, AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: attention.DailyQuota.Low, storage.SeverityNormal: attention.DailyQuota.Normal, storage.SeverityHigh: attention.DailyQuota.High}, DayTimezone: attention.DayTimezone, MaxEscalations: attention.MaxEscalations, Source: storage.SourceSystem, NowMS: nowMS, DailySummaryAt: attention.DailySummaryAt, CriticalWindowMS: attention.CriticalFuse.Window.Milliseconds(), CriticalTotalLimit: attention.CriticalFuse.TotalLimit, CriticalPerRunLimit: attention.CriticalFuse.PerRunLimit, Channels: channels}
	if c.AttemptNo > 0 {
		cmd.AttemptNo = &c.AttemptNo
	}
	if batchAt, ok := storage.NextDailySummaryAt(nowMS, attention.DayTimezone, attention.DailySummaryAt); ok {
		cmd.BatchAtMS = &batchAt
	}
	changeRef := "https://sift.invalid/change/" + in.Identity.ChangeID
	switch v.Code {
	case "guardrail_violation":
		cmd.Reason, cmd.GuardrailLevel = storage.InterruptGuardrailViolation, storage.GuardrailSoft
		cmd.Generation.ViolationCode, cmd.Generation.SubjectDigest = v.RuleID, v.MatchedPathsDigest
		cmd.Facts = map[string]string{"rule_id": v.RuleID, "impact_scope": v.MatchedPathsDigest, "recommended_action": "approve", "policy_evidence_ref": changeRef}
	case "code_review":
		cmd.Reason = storage.InterruptCodeReview
		cmd.Facts = map[string]string{"change_ref": changeRef, "head_sha": in.Change.HeadSHA, "review_requirement": v.ReviewPolicy, "recommended_action": "approve", "diff_ref": changeRef + "/diff"}
	case "merge_conflict":
		cmd.Reason, cmd.GatePhase = storage.InterruptMergeConflict, storage.GateMerge
		cmd.Generation.ConflictDigest = storage.MergeConflictDigest(in.Identity.ChangeID, in.Change.HeadSHA)
		cmd.Facts = map[string]string{"change_ref": changeRef, "head_sha": in.Change.HeadSHA, "conflict_summary": v.Code, "recommended_action": "retry", "conflict_evidence_ref": changeRef}
	case "mergeability_unknown":
		// Unknown mergeability is a failed gate recheck, not a conflict.  Keep
		// its durable successor and provenance distinct from merge_conflict.
		cmd.Reason, cmd.GatePhase = storage.InterruptFailureReview, storage.GateMerge
		cmd.FailureReviewVariant = storage.FailureReviewAttempt
		cmd.FailureReviewRetryKind = storage.FailureReviewGateRecheck
		cmd.AttemptNo = &c.AttemptNo
		cmd.Generation.AttemptNo, cmd.Generation.Generation = c.AttemptNo, c.Generation
		cmd.Generation.ChangeID, cmd.Generation.HeadSHA = in.Identity.ChangeID, in.Change.HeadSHA
		digest := sha256.Sum256([]byte(in.Change.HeadSHA + "\x00" + in.Checks.ExternalURL + "\x00" + v.Code))
		cmd.Generation.FailureDigest = fmt.Sprintf("%x", digest)
		cmd.Facts = map[string]string{"failure_class": v.Code, "failure_evidence_ref": changeRef, "recommended_action": "retry"}
	default:
		cmd.Reason = storage.InterruptFailureReview
		cmd.FailureReviewVariant = storage.FailureReviewAttempt
		cmd.FailureReviewRetryKind = storage.FailureReviewGateRecheck
		cmd.AttemptNo = &c.AttemptNo
		cmd.Generation.AttemptNo, cmd.Generation.Generation = c.AttemptNo, c.Generation
		digest := sha256.Sum256([]byte(in.Change.HeadSHA + "\x00" + in.Checks.ExternalURL + "\x00" + v.Code))
		cmd.Generation.FailureDigest = fmt.Sprintf("%x", digest)
		cmd.Facts = map[string]string{"failure_class": v.Code, "failure_evidence_ref": changeRef, "recommended_action": "retry"}
	}
	if cmd.Reason == "" {
		return storage.EmitInterruptCmd{}, fmt.Errorf("gate reconciler: no interrupt for %s", v.Code)
	}
	if d, ok := attention.ReasonDefaults[string(cmd.Reason)]; ok {
		cmd.ExpiresAfterMS = d.ExpiresAfterMS
		cmd.OnExpire = storage.ExpireAction(d.OnExpire)
		cmd.OnMaxEscalations = storage.ExpireAction(d.OnMaxEscalations)
	}
	return cmd, nil
}
