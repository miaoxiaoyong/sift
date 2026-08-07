package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

var interruptTemplates = map[InterruptReason]interruptTemplate{
	InterruptDesignApproval:     {"需要批准后再开始", "text", []InterruptOption{{"approve", "批准并开始", "Run 进入可启动状态", "开始后会消耗执行资源"}, {"reject", "拒绝并停止", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"risk_summary", "recommended_action", "task_spec_ref"}, []string{"task_spec_ref"}, SeverityNormal, 24 * 3600 * 1000, ExpireHold},
	InterruptGuardrailViolation: {"软护栏需要人工裁决", "text", []InterruptOption{{"approve", "豁免本次规则", "继续当前 Run", "豁免会接受规则所述风险"}, {"reject", "拒绝并停止", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"rule_id", "impact_scope", "recommended_action", "policy_evidence_ref"}, []string{"policy_evidence_ref"}, SeverityHigh, 24 * 3600 * 1000, ExpireHold},
	InterruptCodeReview:         {"变更等待代码审阅", "visual", []InterruptOption{{"approve", "批准审阅", "继续后续流程", "批准变更的当前内容"}, {"reject", "拒绝变更", "Run 停止", "需重新修改后发起"}, {"hold", "暂缓审阅", "保持等待", "Change 继续待审"}}, []string{"change_ref", "head_sha", "review_requirement", "recommended_action", "diff_ref"}, []string{"change_ref", "diff_ref"}, SeverityNormal, 72 * 3600 * 1000, ExpireHold},
	InterruptAgentBlocked:       {"Agent 需要你澄清", "voice", []InterruptOption{{"ask", "提供澄清", "写入澄清内容", "澄清会改变后续执行方向"}, {"retry", "重试当前工作", "再次尝试执行", "未澄清时可能再次阻塞"}, {"reject", "停止 Run", "Run 停止", "已完成工作不会继续"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"blocker_summary", "attempted_summary", "recommended_action", "agent_log_ref"}, []string{"agent_log_ref"}, SeverityNormal, 8 * 3600 * 1000, ExpireEscalate},
	InterruptMergeConflict:      {"合并冲突需要处理", "voice", []InterruptOption{{"retry", "重新执行合并", "再次尝试合并", "冲突未变时会再次失败"}, {"reject", "停止 Run", "Run 停止", "Change 不会合并"}, {"hold", "暂缓决定", "保持等待", "Change 继续待处理"}}, []string{"change_ref", "head_sha", "conflict_summary", "recommended_action", "conflict_evidence_ref"}, []string{"change_ref", "conflict_evidence_ref"}, SeverityHigh, 8 * 3600 * 1000, ExpireEscalate},
	InterruptFailureReview:      {"失败需要人工决定", "voice", []InterruptOption{{"retry", "重试失败步骤", "再次执行", "相同故障可能再次发生"}, {"reject", "停止 Run", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持等待", "Run 继续占用待处理项"}}, []string{"failure_class", "failure_evidence_ref", "recommended_action"}, []string{"failure_evidence_ref"}, SeverityHigh, 24 * 3600 * 1000, ExpireAutoReject},
	InterruptStartupStall:       {"无法确认旧执行体已停止", "text", []InterruptOption{{"retry", "重新探测旧执行体", "请求受控终止再探测", "未确认消失时仍保持隔离"}, {"reject", "放弃此 Run", "停止处理并保持隔离", "不代表旧执行体已停止"}, {"hold", "继续等待", "保持等待和隔离", "旧执行体可能仍在运行"}}, []string{"attempt_no", "generation", "diagnostic_cause", "isolation_consequence", "recommended_action", "attempt_diagnostic_ref", "worktree_ref"}, []string{"attempt_diagnostic_ref", "worktree_ref"}, SeverityHigh, 3600 * 1000, ExpireEscalate},
}

func interruptTemplateFor(cmd EmitInterruptCmd) (interruptTemplate, bool) {
	t, ok := interruptTemplates[cmd.Reason]
	if !ok || cmd.Reason != InterruptFailureReview || cmd.FailureReviewVariant != FailureReviewReportQuota {
		return t, ok
	}
	return interruptTemplate{
		headline: "报告打扰额度已耗尽", modality: "voice",
		options: []InterruptOption{{"reject", "停止 Run", "Run 停止", "需人工重新发起"}, {"hold", "暂缓决定", "保持 Interrupt 人工 held", "Run 继续运行"}},
		facts:   []string{"failure_class", "failure_evidence_ref", "recommended_action"},
		links:   []string{"failure_evidence_ref"}, base: SeverityHigh, expires: t.expires, onExpire: t.onExpire,
	}, true
}

func validateFailureReviewVariant(cmd EmitInterruptCmd) error {
	if cmd.Reason != InterruptFailureReview {
		if cmd.FailureReviewVariant != "" {
			return fmt.Errorf("%w: failure_review variant on another reason", ErrInterruptRejected)
		}
		return nil
	}
	facts := cmd.Facts
	if len(facts) != 3 {
		return fmt.Errorf("%w: failure_review facts are not closed", ErrInterruptRejected)
	}
	switch cmd.FailureReviewVariant {
	case FailureReviewAttempt:
		if cmd.AttemptNo == nil || *cmd.AttemptNo < 1 || cmd.Generation.AttemptNo != *cmd.AttemptNo || cmd.Generation.Generation < 1 || cmd.Generation.ReportDailyBucketStartMS != 0 || cmd.Generation.ReportDailyBucketEndMS != 0 || cmd.Generation.SecurityEventID != "" || facts["failure_class"] == "" || facts["failure_class"] == "report_interrupt_quota_exhausted" || facts["recommended_action"] == "" || facts["failure_evidence_ref"] == "" || (cmd.FailureReviewRetryKind != "" && cmd.FailureReviewRetryKind != FailureReviewNewAttempt && cmd.FailureReviewRetryKind != FailureReviewGateRecheck) {
			return fmt.Errorf("%w: invalid failure_review attempt variant", ErrInterruptRejected)
		}
	case FailureReviewReportQuota:
		quotaDigest := sha256.Sum256([]byte(fmt.Sprintf(`{"daily_bucket_end_ms":%d,"daily_bucket_start_ms":%d,"failure_class":"report_interrupt_quota_exhausted","recommended_action":"hold","run_id":%q}`, cmd.Generation.ReportDailyBucketEndMS, cmd.Generation.ReportDailyBucketStartMS, cmd.RunID)))
		if cmd.AttemptNo != nil || cmd.Generation.AttemptNo != 0 || cmd.Generation.Generation != 0 || cmd.Generation.ReportDailyBucketStartMS <= 0 || cmd.Generation.ReportDailyBucketEndMS <= cmd.Generation.ReportDailyBucketStartMS || len(cmd.Generation.SecurityEventID) != 32 || !lowerHex(cmd.Generation.SecurityEventID) || cmd.Generation.FailureDigest != hex.EncodeToString(quotaDigest[:]) || facts["failure_class"] != "report_interrupt_quota_exhausted" || facts["recommended_action"] != "hold" || facts["failure_evidence_ref"] != "sift://event/"+cmd.Generation.SecurityEventID {
			return fmt.Errorf("%w: invalid failure_review report quota variant", ErrInterruptRejected)
		}
	case "":
		return fmt.Errorf("%w: failure_review variant is required", ErrInterruptRejected)
	default:
		return fmt.Errorf("%w: unknown failure_review variant", ErrInterruptRejected)
	}
	return nil
}

type interruptDispatch struct {
	severity                                                InterruptSeverity
	state, channelID, channelSnapshot, delivery, heldReason string
	suggestedDowngrade                                      bool
	nextDispatchAtMS                                        *int64
}

func admitInterruptT6(ctx context.Context, cmd EmitInterruptCmd, modality string, base InterruptSeverity, expiresAtMS int64) (interruptDispatch, error) {
	compatible := make([]InterruptChannel, 0, len(cmd.Channels))
	isolated := false
	for _, channel := range cmd.Channels {
		if channel.ID == "" || !containsString(channel.Capabilities, modality) {
			continue
		}
		if channel.Isolated {
			isolated = true
			continue
		}
		compatible = append(compatible, channel)
	}
	if len(compatible) == 0 {
		reason := "no_compatible_channel"
		if isolated {
			reason = "channel_isolated"
		}
		return interruptDispatch{severity: base, state: "held", delivery: "held", heldReason: reason}, nil
	}
	sort.Slice(compatible, func(i, j int) bool { return compatible[i].ID < compatible[j].ID })
	defaultChannel := compatible[0].ID
	for _, channel := range compatible {
		if channel.Default {
			defaultChannel = channel.ID
			break
		}
	}
	input := InterruptT6Input{RunID: cmd.RunID, Reason: string(cmd.Reason), AttemptNo: cmd.AttemptNo, Severity: base, MinModality: modality, ExpiresAtMS: expiresAtMS, FrozenAtMS: cmd.NowMS, DefaultChannelID: defaultChannel, NextWindowAtMS: cmd.NextWindowAtMS}
	for _, channel := range compatible {
		input.ChannelCandidates = append(input.ChannelCandidates, channel.ID)
	}
	out := InterruptT6Output{Delivery: "batch", ChannelID: defaultChannel}
	if base == SeverityHigh || base == SeverityCritical {
		out.Delivery = "immediate"
	}
	if cmd.T6 != nil {
		if advised, err := cmd.T6(ctx, input); err == nil && validT6Advice(advised, input) {
			out = advised
		}
	}
	// Final severity is the single Severity(...) entry: T6 advice can only
	// suggest a one-level downgrade, never set or upgrade the base.
	severity := Severity(base, out.SuggestedDowngrade)
	if severity == SeverityHigh || severity == SeverityCritical {
		out.Delivery = "immediate"
	}
	var selected InterruptChannel
	for _, channel := range compatible {
		if channel.ID == out.ChannelID {
			selected = channel
			break
		}
	}
	d := interruptDispatch{severity: severity, state: "ready", channelID: out.ChannelID, channelSnapshot: string(selected.snapshot()), delivery: out.Delivery, suggestedDowngrade: out.SuggestedDowngrade}
	switch out.Delivery {
	case "immediate":
		now := cmd.NowMS
		d.nextDispatchAtMS = &now
	case "batch":
		if cmd.BatchAtMS == nil || *cmd.BatchAtMS >= expiresAtMS {
			d.state, d.delivery, d.heldReason = "held", "held", "batch_after_expiry"
		} else {
			d.nextDispatchAtMS = cmd.BatchAtMS
		}
	case "next_window":
		if cmd.NextWindowAtMS == nil || *cmd.NextWindowAtMS <= cmd.NowMS || *cmd.NextWindowAtMS >= expiresAtMS {
			return interruptDispatch{}, fmt.Errorf("%w: invalid next_window", ErrInterruptRejected)
		}
		d.nextDispatchAtMS = cmd.NextWindowAtMS
	default:
		return interruptDispatch{}, fmt.Errorf("%w: invalid T6 delivery", ErrInterruptRejected)
	}
	return d, nil
}

func validT6Advice(out InterruptT6Output, in InterruptT6Input) bool {
	if !containsString(in.ChannelCandidates, out.ChannelID) || (out.Delivery != "immediate" && out.Delivery != "batch" && out.Delivery != "next_window") {
		return false
	}
	severity := Severity(in.Severity, out.SuggestedDowngrade)
	if (severity == SeverityHigh || severity == SeverityCritical) && out.Delivery != "immediate" {
		return false
	}
	return out.Delivery != "next_window" || in.NextWindowAtMS != nil && *in.NextWindowAtMS > in.FrozenAtMS && *in.NextWindowAtMS < in.ExpiresAtMS
}

// Severity is the sole deterministic final-severity algorithm (interrupt.md
// §4.2). It takes the already-promoted base and the accepted T6
// suggested_downgrade advice, and lowers severity by at most one level
// (clamped at low), never upgrading. It is the only path by which an LLM/T6
// suggestion can move severity off the promoted BaseSeverity result.
// EmitInterruptCmd carries no severity field, so callers cannot manufacture a
// more urgent Interrupt; InterruptT4Output has no severity field, and T6's
// output only carries suggested_downgrade. The emitter applies this once to
// the frozen base and persists the decision, so replay and escalation reuse it
// rather than lowering again.
func Severity(base InterruptSeverity, suggestedDowngrade bool) InterruptSeverity {
	if !suggestedDowngrade {
		return base
	}
	switch base {
	case SeverityCritical:
		return SeverityHigh
	case SeverityHigh:
		return SeverityNormal
	default:
		return SeverityLow
	}
}

func BaseSeverity(reason InterruptReason, phase GatePhase, guard GuardrailLevel, escalation, max int) (InterruptSeverity, error) {
	t, ok := interruptTemplates[reason]
	if !ok {
		return "", fmt.Errorf("%w: unknown reason %q", ErrInterruptRejected, reason)
	}
	if guard == GuardrailHard {
		return "", fmt.Errorf("%w: hard guardrails do not emit interrupts", ErrInterruptRejected)
	}
	if phase != GateNone && phase != GatePreStart && phase != GateReview && phase != GateMerge {
		return "", fmt.Errorf("%w: invalid gate phase", ErrInterruptRejected)
	}
	if guard != GuardrailNone && guard != GuardrailSoft {
		return "", fmt.Errorf("%w: invalid guardrail level", ErrInterruptRejected)
	}
	if escalation < 0 || max < 0 {
		return "", fmt.Errorf("%w: negative escalation", ErrInterruptRejected)
	}
	s := t.base
	if guard == GuardrailSoft && s == SeverityLow || guard == GuardrailSoft && s == SeverityNormal {
		s = SeverityHigh
	}
	if phase == GateMerge && reason == InterruptCodeReview && s == SeverityNormal {
		s = SeverityHigh
	}
	for i := 0; i < minInt(escalation, max); i++ {
		s = promoteSeverity(s)
	}
	return s, nil
}
