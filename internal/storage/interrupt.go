package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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
