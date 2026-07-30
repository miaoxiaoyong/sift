package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type InterruptReason string
type InterruptSeverity string

type GatePhase string
type GuardrailLevel string

type ExpireAction string
type FailureReviewVariant string
type FailureReviewRetryKind string

const (
	InterruptDesignApproval     InterruptReason = "design_approval"
	InterruptGuardrailViolation InterruptReason = "guardrail_violation"
	InterruptCodeReview         InterruptReason = "code_review"
	InterruptAgentBlocked       InterruptReason = "agent_blocked"
	InterruptMergeConflict      InterruptReason = "merge_conflict"
	InterruptFailureReview      InterruptReason = "failure_review"
	InterruptStartupStall       InterruptReason = "startup_stall"

	SeverityLow      InterruptSeverity = "low"
	SeverityNormal   InterruptSeverity = "normal"
	SeverityHigh     InterruptSeverity = "high"
	SeverityCritical InterruptSeverity = "critical"

	GateNone         GatePhase      = "none"
	GatePreStart     GatePhase      = "pre_start"
	GateReview       GatePhase      = "review"
	GateMerge        GatePhase      = "merge"
	GuardrailNone    GuardrailLevel = "none"
	GuardrailSoft    GuardrailLevel = "soft"
	GuardrailHard    GuardrailLevel = "hard"
	ExpireHold       ExpireAction   = "hold"
	ExpireEscalate   ExpireAction   = "escalate"
	ExpireAutoReject ExpireAction   = "auto_reject"

	FailureReviewAttempt     FailureReviewVariant   = "attempt"
	FailureReviewReportQuota FailureReviewVariant   = "report_quota"
	FailureReviewNewAttempt  FailureReviewRetryKind = "new_attempt"
	FailureReviewGateRecheck FailureReviewRetryKind = "gate_recheck"
)

type InterruptOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
	Risk   string `json:"risk"`
}
type InterruptLink struct {
	Label  string `json:"label"`
	Target string `json:"target"`
}

func (o InterruptOption) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Effect string `json:"effect"`
		ID     string `json:"id"`
		Label  string `json:"label"`
		Risk   string `json:"risk"`
	}{o.Effect, o.ID, o.Label, o.Risk})
}

// ActiveInterruptReasons is the canonical active Interrupt reason set.
func ActiveInterruptReasons() []InterruptReason {
	return []InterruptReason{
		InterruptDesignApproval, InterruptGuardrailViolation, InterruptCodeReview,
		InterruptAgentBlocked, InterruptMergeConflict, InterruptFailureReview,
		InterruptStartupStall,
	}
}

type InterruptGeneration struct {
	TaskSpecSnapshotID, PolicySnapshotID, ViolationCode, SubjectDigest string
	ChangeID, HeadSHA, ReportID, ConflictDigest, FailureDigest         string
	ReportDailyBucketStartMS, ReportDailyBucketEndMS                   int64
	SecurityEventID                                                    string
	AttemptNo, Generation                                              int
}

type InterruptT4Input struct {
	RunID     string
	AttemptNo *int
	Reason    InterruptReason
	Severity  InterruptSeverity
	Modality  string
	Headline  string
	Brief     string
	Fragments []string
	Links     []InterruptLink
	Options   []InterruptOption
}

type InterruptT4Output struct {
	Headline            string
	Conclusion          string
	KeyPoints           []string
	Options             []string
	RecommendedOptionID string
}

// InterruptT4Caller runs outside the emission transaction. Errors and invalid
// outputs deterministically fall back to the canonical Interrupt renderer.
type InterruptT4Caller func(context.Context, InterruptT4Input) (InterruptT4Output, error)

type InterruptChannel struct {
	ID           string
	Type         string
	TargetRef    string
	Capabilities []string
	Renderer     string
	Default      bool
	Isolated     bool
}

func (c InterruptChannel) snapshot() []byte {
	type channelSnapshot struct {
		Capabilities []string `json:"capabilities"`
		ID           string   `json:"id"`
		Renderer     string   `json:"renderer"`
		TargetRef    string   `json:"target_ref"`
		Type         string   `json:"type"`
	}
	kind, renderer := c.Type, c.Renderer
	if kind == "" {
		kind = "webhook"
	}
	if renderer == "" {
		renderer = "plain-v1"
	}
	target := c.TargetRef
	if target == "" {
		target = "secret_ref:" + c.ID
	}
	caps := append([]string(nil), c.Capabilities...)
	sort.Strings(caps)
	b, _ := json.Marshal(channelSnapshot{caps, c.ID, renderer, target, kind})
	return b
}

type InterruptT6Input struct {
	RunID, Reason, MinModality string
	AttemptNo                  *int
	Severity                   InterruptSeverity
	ExpiresAtMS, FrozenAtMS    int64
	ChannelCandidates          []string
	DefaultChannelID           string
	NextWindowAtMS             *int64
}

type InterruptT6Output struct {
	SuggestedDowngrade bool
	Delivery           string
	ChannelID          string
}

// InterruptT6Caller runs outside the emission transaction. Invalid or failed
// advice deterministically uses the fallback delivery for the frozen channels.
type InterruptT6Caller func(context.Context, InterruptT6Input) (InterruptT6Output, error)

// EmitInterruptCmd carries only facts. Templates, severity and the generation
// key are derived here so callers cannot manufacture a more urgent or broader
// Interrupt.
type EmitInterruptCmd struct {
	RunID                           string
	ExpectedRunVersion              int64
	AttemptNo                       *int
	Reason                          InterruptReason
	FailureReviewVariant            FailureReviewVariant
	FailureReviewRetryKind          FailureReviewRetryKind
	Facts                           map[string]string
	Generation                      InterruptGeneration
	GatePhase                       GatePhase
	GuardrailLevel                  GuardrailLevel
	EscalationCount, MaxEscalations int
	ExpiresAfterMS                  int64
	OnExpire, OnMaxEscalations      ExpireAction
	AttentionDailyQuota             map[InterruptSeverity]int
	DayTimezone                     string
	Source                          EventSource
	// CalibrationID is set only by RecordGateEvaluationAndEmitInterrupt. It
	// binds a Gate HITL to the shadow prediction frozen in this transaction.
	CalibrationID string
	T6            InterruptT6Caller
	Channels      []InterruptChannel
	// NextWindowAtMS and BatchAtMS are availability/daily-summary instants
	// frozen before the external T6 call.
	NextWindowAtMS                          *int64
	BatchAtMS                               *int64
	DailySummaryAt                          string
	CriticalWindowMS                        int64
	CriticalTotalLimit, CriticalPerRunLimit int
	NowMS                                   int64
}

type Interrupt struct {
	ID, RunID, GenerationKey string
	AttemptNo                *int
	Reason                   InterruptReason
	Severity                 InterruptSeverity
	Headline, Brief          string
	Options                  []InterruptOption
	MinModality              string
	Links                    []InterruptLink
	ExpiresAtMS              int64
	OnExpire                 ExpireAction
	ChargedBudgetEntryID     string
	ChannelID, Delivery      string
	SuggestedDowngrade       bool
	NextDispatchAtMS         *int64
	HeldReason               string
}

var ErrInterruptRejected = errors.New("storage: interrupt rejected")
var ErrAttentionQuotaExceeded = errors.New("storage: attention quota exceeded")

type interruptTemplate struct {
	headline, modality string
	options            []InterruptOption
	facts, links       []string
	base               InterruptSeverity
	expires            int64
	onExpire           ExpireAction
}

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

// EmitInterrupt is the sole creation port. Its transaction contains the Run
// transition, budget charge, Interrupt, audit event and forge-comment outbox.
func (d *DB) EmitInterrupt(ctx context.Context, cmd EmitInterruptCmd) (Interrupt, error) {
	if cmd.CalibrationID != "" {
		return Interrupt{}, fmt.Errorf("%w: calibration binding requires gate recorder", ErrInterruptRejected)
	}
	return d.emitInterrupt(ctx, cmd, nil)
}

// emitInterrupt keeps the M3 emission sequence in one transaction while
// allowing the Gate recorder to append its frozen evidence before the
// Interrupt is inserted. The callback must not perform external IO.
func (d *DB) emitInterrupt(ctx context.Context, cmd EmitInterruptCmd, before func(*sql.Tx) error) (Interrupt, error) {
	return d.emitInterruptHooks(ctx, cmd, before, nil, false)
}

func (d *DB) emitReportInterruptHooks(ctx context.Context, cmd EmitInterruptCmd, before func(*sql.Tx) error, after func(*sql.Tx, Interrupt) error) (Interrupt, error) {
	return d.emitInterruptHooks(ctx, cmd, before, after, true)
}

func (d *DB) emitInterruptHooks(ctx context.Context, cmd EmitInterruptCmd, before func(*sql.Tx) error, after func(*sql.Tx, Interrupt) error, reportOnly bool) (Interrupt, error) {
	prep, cmd, err := d.prepareInterruptEmission(ctx, cmd, reportOnly)
	if err != nil {
		return Interrupt{}, err
	}
	if existing, found, err := d.interruptByKey(ctx, prep.key); err != nil {
		return Interrupt{}, err
	} else if found {
		return existing, nil
	}
	if err := d.refineInterruptEmission(ctx, cmd, &prep); err != nil {
		return Interrupt{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, err
	}
	defer tx.Rollback()
	if before != nil {
		if err := before(tx); err != nil {
			return Interrupt{}, err
		}
	}
	if existing, found, err := interruptByKeyTx(ctx, tx, prep.key); err != nil {
		return Interrupt{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return Interrupt{}, err
		}
		return existing, nil
	}
	in, err := d.insertInterruptEmissionTx(ctx, tx, cmd, prep, after, reportOnly, true)
	if err != nil {
		return Interrupt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, err
	}
	d.wakeOutbox()
	return in, nil
}

// interruptEffectBinding is the immutable closed source/effect discriminator
// consumed by later command/expiry code. Its JSON is deliberately persisted,
// rather than reconstructing an arm from the current Run or configuration.
func interruptEffectBinding(cmd EmitInterruptCmd) ([]byte, string) {
	if cmd.FailureReviewVariant == FailureReviewReportQuota {
		b, _ := json.Marshal(map[string]any{
			"arm": "report_quota_failure_review", "run_id": cmd.RunID,
			"daily_bucket_start_ms": cmd.Generation.ReportDailyBucketStartMS,
			"daily_bucket_end_ms":   cmd.Generation.ReportDailyBucketEndMS,
			"security_event_id":     cmd.Generation.SecurityEventID,
		})
		return b, "failure_review"
	}
	if cmd.Reason == InterruptFailureReview {
		retryKind := cmd.FailureReviewRetryKind
		if retryKind == "" {
			retryKind = FailureReviewNewAttempt
		}
		fields := map[string]any{"arm": "failure_review_attempt", "run_id": cmd.RunID, "attempt_no": *cmd.AttemptNo, "generation": cmd.Generation.Generation, "retry_kind": retryKind}
		if retryKind == FailureReviewGateRecheck {
			fields["change_id"], fields["head_sha"] = cmd.Generation.ChangeID, cmd.Generation.HeadSHA
			fields["terminal_attempt_no"], fields["terminal_generation"] = nil, nil
		} else {
			fields["change_id"], fields["head_sha"] = nil, nil
			fields["terminal_attempt_no"], fields["terminal_generation"] = *cmd.AttemptNo, cmd.Generation.Generation
		}
		b, _ := json.Marshal(fields)
		return b, "failure_review"
	}
	fields := map[string]any{"arm": string(cmd.Reason), "run_id": cmd.RunID}
	switch cmd.Reason {
	case InterruptDesignApproval:
		fields["task_spec_snapshot_id"] = cmd.Generation.TaskSpecSnapshotID
	case InterruptGuardrailViolation:
		fields["head_sha"], fields["rule_id"], fields["matched_paths_digest"] = cmd.Generation.HeadSHA, cmd.Generation.ViolationCode, cmd.Generation.SubjectDigest
	case InterruptCodeReview:
		delete(fields, "run_id")
		fields["change_id"], fields["head_sha"], fields["review_policy_snapshot_digest"] = cmd.Generation.ChangeID, cmd.Generation.HeadSHA, cmd.Generation.PolicySnapshotID
	case InterruptAgentBlocked:
		fields["attempt_no"], fields["generation"], fields["report_id"] = cmd.Generation.AttemptNo, cmd.Generation.Generation, cmd.Generation.ReportID
	case InterruptMergeConflict:
		delete(fields, "run_id")
		fields["change_id"], fields["head_sha"], fields["conflict_digest"] = cmd.Generation.ChangeID, cmd.Generation.HeadSHA, cmd.Generation.ConflictDigest
	case InterruptStartupStall:
		fields["attempt_no"], fields["generation"] = cmd.Generation.AttemptNo, cmd.Generation.Generation
	}
	b, _ := json.Marshal(fields)
	return b, string(cmd.Reason)
}

func interruptBriefFragments(t interruptTemplate, facts map[string]string) []string {
	if t.headline == "报告打扰额度已耗尽" {
		return []string{"请人工处理", "额度已耗尽"}
	}
	fragments := make([]string, 0, len(t.facts))
	for _, key := range t.facts {
		if value, ok := facts[key]; ok && safeT4Fragment(value) {
			fragments = append(fragments, value)
		}
	}
	sort.Strings(fragments)
	return slices.Compact(fragments)
}

func safeT4Fragment(value string) bool {
	return !strings.ContainsAny(value, "\r\n") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func canonicalRecommendedAction(options []InterruptOption, action string) bool {
	for _, option := range options {
		if action == option.ID {
			return true
		}
	}
	return false
}

func acceptInterruptT4(in InterruptT4Input, out InterruptT4Output) (bool, string) {
	if out.Headline != in.Headline || !containsString(in.Fragments, out.Conclusion) || len(out.KeyPoints) < 1 || len(out.KeyPoints) > 3 || len(out.Options) != len(in.Options) {
		return false, ""
	}
	for i, option := range in.Options {
		if out.Options[i] != option.ID {
			return false, ""
		}
	}
	seen := map[string]bool{}
	for _, point := range out.KeyPoints {
		if !containsString(in.Fragments, point) || seen[point] {
			return false, ""
		}
		seen[point] = true
	}
	label := ""
	for _, option := range in.Options {
		if option.ID == out.RecommendedOptionID {
			label = option.Label
			break
		}
	}
	if label == "" {
		return false, ""
	}
	points := make([]string, len(out.KeyPoints))
	for i, point := range out.KeyPoints {
		points[i] = escapeT4Text(point)
	}
	return true, "结论：" + escapeT4Text(out.Conclusion) + "；要点：" + strings.Join(points, "；") + "；建议：" + escapeT4Text(label) + "（" + out.RecommendedOptionID + "）"
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func escapeT4Text(value string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+", "-", "\\-", "!", "\\!", ">", "\\>", "<", "\\<", "&", "\\&").Replace(value)
}

func renderComment(in Interrupt) string { return in.Headline + "\n\n" + in.Brief }
func renderInterrupt(t interruptTemplate, facts map[string]string, reason InterruptReason) (string, []InterruptLink, error) {
	vals := make([]string, len(t.facts))
	for i, k := range t.facts {
		v, ok := facts[k]
		if !ok || v == "" {
			return "", nil, fmt.Errorf("%w: missing fact %s", ErrInterruptRejected, k)
		}
		e, err := escapeBrief(v)
		if err != nil {
			return "", nil, err
		}
		vals[i] = e
	}
	parts := make([]string, len(t.facts))
	for i, k := range t.facts {
		parts[i] = k + "=" + vals[i]
	}
	brief := "事实：" + strings.Join(parts, "；") + "。建议：" + vals[indexOf(t.facts, "recommended_action")]
	links := []InterruptLink{}
	for _, k := range t.links {
		v := facts[k]
		if !validLink(v) {
			return "", nil, fmt.Errorf("%w: invalid required link %s", ErrInterruptRejected, k)
		}
		links = append(links, InterruptLink{k, v})
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Target == links[j].Target {
			return links[i].Label < links[j].Label
		}
		return links[i].Target < links[j].Target
	})
	out := links[:0]
	for _, l := range links {
		if len(out) == 0 || out[len(out)-1] != l {
			out = append(out, l)
		}
	}
	return brief, out, nil
}
func escapeBrief(v string) (string, error) {
	if strings.Contains(v, "\r\n") {
		return "", fmt.Errorf("%w: interrupt_brief_crlf_rejected", ErrInterruptRejected)
	}
	if strings.Contains(v, "\r") {
		return "", fmt.Errorf("%w: interrupt_brief_cr_rejected", ErrInterruptRejected)
	}
	if strings.Contains(v, "\n") {
		return "", fmt.Errorf("%w: interrupt_brief_lf_rejected", ErrInterruptRejected)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: interrupt_brief_control_rejected", ErrInterruptRejected)
		}
	}
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+", "-", "\\-", "!", "\\!", ">", "\\>", "_", "\\_").Replace(v), nil
}
func timezoneOrUTC(v string) string {
	if v == "" || v == "local" {
		return "UTC"
	}
	return v
}
func summaryOrDefault(v string) string {
	if v == "" {
		return "09:00"
	}
	return v
}
func fuseWindowOrDefault(v int64) int64 {
	if v <= 0 {
		return 15 * 60 * 1000
	}
	return v
}
func fuseTotalOrDefault(v int) int {
	if v <= 0 {
		return 5
	}
	return v
}
func fuseRunOrDefault(v int) int {
	if v <= 0 {
		return 2
	}
	return v
}

func quotaDay(now int64, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return time.UnixMilli(now).In(loc).Format("2006-01-02")
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func validLink(v string) bool {
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, "https://") {
		return true
	}
	const prefix = "sift://event/"
	if !strings.HasPrefix(v, prefix) {
		return false
	}
	rest := v[len(prefix):]
	// Security event reference (report quota): sift://event/<32 lowercase hex>.
	if len(rest) == 32 && lowerHex(rest) {
		return true
	}
	// Terminal event reference (storage.md §8.1): sift://event/event:<K>, where
	// <K> is a server-allocated event key over [a-z0-9:_] (operation key plus a
	// closed suffix such as :failed or :verdict:<kind>:<code>).
	const terminalPrefix = "event:"
	if strings.HasPrefix(rest, terminalPrefix) {
		if k := rest[len(terminalPrefix):]; k != "" && validEventKey(k) {
			return true
		}
	}
	return false
}

func validEventKey(k string) bool {
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == ':' || c == '_') {
			return false
		}
	}
	return true
}
func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func promoteSeverity(s InterruptSeverity) InterruptSeverity {
	switch s {
	case SeverityLow:
		return SeverityNormal
	case SeverityNormal:
		return SeverityHigh
	default:
		return SeverityCritical
	}
}

func interruptGenerationKey(run string, reason InterruptReason, g InterruptGeneration) (string, error) {
	domain := "sift.interrupt.generation"
	if reason == InterruptFailureReview && g.AttemptNo == 0 && g.ReportDailyBucketStartMS > 0 {
		domain = "sift.interrupt.report-quota.generation"
	}
	fields := [][2]string{{"string:domain", domain}, {"uint:version", "1"}, {"string:run_id", run}, {"enum:reason", string(reason)}}
	add := func(t, n, v string) { fields = append(fields, [2]string{t + ":" + n, v}) }
	switch reason {
	case InterruptDesignApproval:
		add("string", "task_spec_snapshot_id", g.TaskSpecSnapshotID)
	case InterruptGuardrailViolation:
		add("string", "policy_snapshot_id", g.PolicySnapshotID)
		add("string", "violation_code", g.ViolationCode)
		add("sha256", "subject_digest", g.SubjectDigest)
	case InterruptCodeReview:
		add("string", "change_id", g.ChangeID)
		add("git_oid", "head_sha", g.HeadSHA)
	case InterruptAgentBlocked:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("string", "report_id", g.ReportID)
	case InterruptMergeConflict:
		add("string", "change_id", g.ChangeID)
		add("git_oid", "head_sha", g.HeadSHA)
		add("sha256", "conflict_digest", g.ConflictDigest)
	case InterruptFailureReview:
		if g.AttemptNo == 0 && g.ReportDailyBucketStartMS > 0 {
			add("uint", "day_bucket_start_ms", fmt.Sprint(g.ReportDailyBucketStartMS))
			add("sha256", "failure_digest", g.FailureDigest)
			break
		}
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("sha256", "failure_digest", g.FailureDigest)
	case InterruptStartupStall:
		add("uint", "attempt_no", fmt.Sprint(g.AttemptNo))
		add("uint", "generation", fmt.Sprint(g.Generation))
		add("enum", "cause", "startup_stall")
	default:
		return "", fmt.Errorf("%w: unknown reason", ErrInterruptRejected)
	}
	var b strings.Builder
	for _, f := range fields {
		if f[1] == "" || !utf8.ValidString(f[1]) || strings.ContainsRune(f[1], 0) {
			return "", fmt.Errorf("%w: invalid generation field", ErrInterruptRejected)
		}
		switch {
		case strings.HasPrefix(f[0], "uint:"):
			if f[1] == "0" || strings.HasPrefix(f[1], "-") || !decimal(f[1]) {
				return "", fmt.Errorf("%w: invalid uint", ErrInterruptRejected)
			}
		case strings.HasPrefix(f[0], "sha256:"):
			if len(f[1]) != 64 || !lowerHex(f[1]) {
				return "", fmt.Errorf("%w: invalid sha256", ErrInterruptRejected)
			}
		case strings.HasPrefix(f[0], "git_oid:"):
			if (len(f[1]) != 40 && len(f[1]) != 64) || !lowerHex(f[1]) {
				return "", fmt.Errorf("%w: invalid git oid", ErrInterruptRejected)
			}
		}
		b.WriteString(f[0])
		b.WriteByte(0)
		b.WriteString(f[1])
		b.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func chargeAttentionTx(ctx context.Context, tx *sql.Tx, cmd EmitInterruptCmd, s InterruptSeverity) (string, error) {
	if s == SeverityCritical {
		return "", nil
	}
	id := newID()
	key := "interrupt-charge:" + mustGenerationKey(cmd)
	bucket := cmd.NowMS
	{
		loc := time.Local
		if cmd.DayTimezone != "" && cmd.DayTimezone != "local" {
			var err error
			loc, err = time.LoadLocation(cmd.DayTimezone)
			if err != nil {
				return "", fmt.Errorf("%w: invalid day timezone", ErrInterruptRejected)
			}
		}
		t := time.UnixMilli(cmd.NowMS).In(loc)
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		bucket = start.UnixMilli()
		limit, ok := cmd.AttentionDailyQuota[s]
		if !ok {
			return "", fmt.Errorf("%w: attention quota missing", ErrInterruptRejected)
		}
		end := start.AddDate(0, 0, 1).UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_counters (kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES ('attention','severity',?,?,?, ?,0,1,?) ON CONFLICT DO NOTHING`, string(s), bucket, end, limit, cmd.NowMS); err != nil {
			return "", err
		}
		// A zero-row CAS is not, by itself, proof of exhaustion (config §3.9):
		// re-read the authority counter and only treat consumed+1>limit as a
		// quota_batched admission. A missing row or unreadable counter rolls the
		// emission back so a storage fault cannot masquerade as a batched result.
		const quotaCASRetries = 8
		for attempt := 0; ; attempt++ {
			res, err := tx.ExecContext(ctx, `UPDATE budget_counters SET consumed_value=consumed_value+1,version=version+1,updated_at_ms=? WHERE kind='attention' AND scope='severity' AND scope_id=? AND bucket_start_ms=? AND consumed_value<limit_value`, cmd.NowMS, string(s), bucket)
			if err != nil {
				return "", err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				break
			}
			var consumed, have int64
			if err := tx.QueryRowContext(ctx, `SELECT consumed_value,limit_value FROM budget_counters WHERE kind='attention' AND scope='severity' AND scope_id=? AND bucket_start_ms=?`, string(s), bucket).Scan(&consumed, &have); err != nil {
				return "", err
			}
			if consumed+1 <= have {
				if attempt >= quotaCASRetries {
					return "", fmt.Errorf("%w: attention quota CAS retry exhausted", ErrInterruptRejected)
				}
				continue
			}
			return "", ErrAttentionQuotaExceeded
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_entries (id,kind,scope,scope_id,bucket_start_ms,amount,reason,run_id,operation_key,created_at_ms) VALUES (?,'attention','severity',?,?,1,?,?,?,?)`, id, string(s), bucket, string(cmd.Reason), cmd.RunID, key, cmd.NowMS); err != nil {
		return "", err
	}
	return id, nil
}
func decimal(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func lowerHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func interruptGenerationKeyFor(cmd EmitInterruptCmd) (string, error) {
	if err := validateFailureReviewVariant(cmd); err != nil {
		return "", err
	}
	return interruptGenerationKey(cmd.RunID, cmd.Reason, cmd.Generation)
}

func mustGenerationKey(cmd EmitInterruptCmd) string {
	k, err := interruptGenerationKeyFor(cmd)
	if err != nil {
		panic(err)
	}
	return k
}
func (d *DB) interruptByKey(ctx context.Context, key string) (Interrupt, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return Interrupt{}, false, err
	}
	defer tx.Rollback()
	in, found, err := interruptByKeyTx(ctx, tx, key)
	if err != nil {
		return Interrupt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Interrupt{}, false, err
	}
	return in, found, nil
}

func interruptByKeyTx(ctx context.Context, tx *sql.Tx, key string) (Interrupt, bool, error) {
	var in Interrupt
	var n, next sql.NullInt64
	var opts, links string
	var reason, severity, on string
	var channelID, delivery, heldReason sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,run_id,attempt_no,generation_key,reason,severity,headline,brief_markdown,options_json,min_modality,links_json,channel_id,delivery,suggested_downgrade,next_dispatch_at_ms,held_reason,expires_at_ms,on_expire,COALESCE(charged_budget_entry_id,'') FROM interrupts WHERE generation_key=?`, key).Scan(&in.ID, &in.RunID, &n, &in.GenerationKey, &reason, &severity, &in.Headline, &in.Brief, &opts, &in.MinModality, &links, &channelID, &delivery, &in.SuggestedDowngrade, &next, &heldReason, &in.ExpiresAtMS, &on, &in.ChargedBudgetEntryID)
	if errors.Is(err, sql.ErrNoRows) {
		return Interrupt{}, false, nil
	}
	if err != nil {
		return Interrupt{}, false, err
	}
	if n.Valid {
		x := int(n.Int64)
		in.AttemptNo = &x
	}
	in.Reason = InterruptReason(reason)
	in.Severity = InterruptSeverity(severity)
	in.OnExpire = ExpireAction(on)
	in.ChannelID, in.Delivery, in.HeldReason = channelID.String, delivery.String, heldReason.String
	if next.Valid {
		in.NextDispatchAtMS = &next.Int64
	}
	if json.Unmarshal([]byte(opts), &in.Options) != nil || json.Unmarshal([]byte(links), &in.Links) != nil {
		return Interrupt{}, false, errors.New("storage: corrupt interrupt JSON")
	}
	return in, true, nil
}
