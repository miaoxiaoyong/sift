package config

import (
	"time"
)

type TaskTransport string

const (
	TaskTransportStdin TaskTransport = "stdin"
	TaskTransportFile  TaskTransport = "file"
)

// EnumValues satisfies [schema.Enumerated].
func (TaskTransport) EnumValues() []string {
	return []string{string(TaskTransportStdin), string(TaskTransportFile)}
}

// Backend selects the agent execution backend (config.md §3.2, §3.6).
type Backend string

const (
	BackendProcess Backend = "process"
	BackendTmux    Backend = "tmux"
)

// EnumValues satisfies [schema.Enumerated].
func (Backend) EnumValues() []string {
	return []string{string(BackendProcess), string(BackendTmux)}
}

// ForgeKind selects the forge platform adapter (config.md §3.3).
type ForgeKind string

const (
	ForgeKindGitHub ForgeKind = "github"
	ForgeKindGitLab ForgeKind = "gitlab"
)

// EnumValues satisfies [schema.Enumerated].
func (ForgeKind) EnumValues() []string {
	return []string{string(ForgeKindGitHub), string(ForgeKindGitLab)}
}

// defaultHost returns the platform's public host when a project omits
// forge.host (config.md §3.3).
func (k ForgeKind) defaultHost() string {
	switch k {
	case ForgeKindGitHub:
		return "github.com"
	case ForgeKindGitLab:
		return "gitlab.com"
	default:
		return ""
	}
}

// defaultCLI returns the platform's default CLI executable when a project
// omits forge.cli (config.md §3.3).
func (k ForgeKind) defaultCLI() string {
	switch k {
	case ForgeKindGitHub:
		return "gh"
	case ForgeKindGitLab:
		return "glab"
	default:
		return ""
	}
}

// BrainProtocol pins the Brain I/O protocol version (config.md §3.4). V0 admits
// only claude-json-v1; a protocol change must introduce a new value.
type BrainProtocol string

const (
	BrainProtocolClaudeJSONv1 BrainProtocol = "claude-json-v1"
)

// EnumValues satisfies [schema.Enumerated].
func (BrainProtocol) EnumValues() []string {
	return []string{string(BrainProtocolClaudeJSONv1)}
}

// ReviewPolicy is the project gate review default (config.md §3.11).
type ReviewPolicy string

const (
	ReviewPolicyAlways    ReviewPolicy = "always"
	ReviewPolicyRiskyOnly ReviewPolicy = "risky-only"
	ReviewPolicyNever     ReviewPolicy = "never"
)

// EnumValues satisfies [schema.Enumerated].
func (ReviewPolicy) EnumValues() []string {
	return []string{string(ReviewPolicyAlways), string(ReviewPolicyRiskyOnly), string(ReviewPolicyNever)}
}

// InterruptQuotaExceededAction pins the report quota-exceeded action
// (config.md §3.10). V0 admits only failure_review_once.
type InterruptQuotaExceededAction string

const (
	InterruptQuotaFailureReviewOnce InterruptQuotaExceededAction = "failure_review_once"
)

// EnumValues satisfies [schema.Enumerated].
func (InterruptQuotaExceededAction) EnumValues() []string {
	return []string{string(InterruptQuotaFailureReviewOnce)}
}

// DefaultConfig returns the effective configuration produced from an absent
// config file (config.md §6 scenario 1): empty operators/agents/projects and
// every documented default filled in. V12 asserts that this table is complete
// — a missing default is a failure, not a silent zero.
//
// Values mirror the default columns of config.md §3 verbatim. Durations use Go
// duration literals; ratios are floats in [0,1] or [1,10] as the spec bounds.
func DefaultConfig() *Config {
	return &Config{
		Version:   Version,
		Operators: Operators{GitHub: []string{}, GitLab: []string{}},
		Agents:    []Agent{},
		Projects:  []Project{},
		Brain: Brain{
			Args:              []string{},
			Protocol:          BrainProtocolClaudeJSONv1,
			DailyTokenLimit:   1000000,
			CallTimeout:       2 * time.Minute,
			SchemaRetries:     1,
			MaxInputBytes:     262144,
			MaxRawOutputBytes: 1048576,
			VersionArgs:       []string{"--version"},
		},
		Scheduler: Scheduler{
			IntakeIdleInterval:       60 * time.Second,
			IntakeActiveInterval:     15 * time.Second,
			IntakeInterruptInterval:  10 * time.Second,
			IntakeInterruptBurst:     5 * time.Minute,
			SupervisorInterval:       1 * time.Second,
			ConfigDriftCheckInterval: 30 * time.Second,
			PerClassTickLimit:        100,
		},
		Runtime: Runtime{
			Backend:                   BackendProcess,
			MaxConcurrentTotal:        3,
			DefaultAgentMaxConcurrent: 1,
			MaxAttempts:               3,
			AttemptTimeout:            2 * time.Hour,
			AgentSilenceTimeout:       30 * time.Minute,
			RetryInitialDelay:         30 * time.Second,
			RetryMaxDelay:             5 * time.Minute,
			RetryMultiplier:           2.0,
			SpawnOperationLeaseTTL:    30 * time.Second,
			StartingPermitTimeout:     30 * time.Second,
			SpawningStartedTimeout:    30 * time.Second,
			HeartbeatInterval:         5 * time.Second,
			HeartbeatStaleAfter:       15 * time.Second,
			TerminationTermGrace:      10 * time.Second,
			TerminationKillGrace:      5 * time.Second,
			AbsenceRecheckCount:       3,
			AbsenceRecheckInterval:    1 * time.Second,
		},
		Outbox: Outbox{
			LeaseTTL:          30 * time.Second,
			RetryInitialDelay: 1 * time.Second,
			RetryMaxDelay:     5 * time.Minute,
			RetryMultiplier:   2.0,
			MaxAttempts:       0,
			MaxAdvanceDelay:   2 * time.Second,
			WorkerBatchSize:   50,
		},
		Forge: Forge{
			HourlyAPILimit:   1000,
			WarningRatio:     0.8,
			SlowPollInterval: 5 * time.Minute,
			CommandTimeout:   30 * time.Second,
		},
		Attention: Attention{
			DayTimezone: "local",
			DailyQuota: DailyQuota{
				Low:    3,
				Normal: 5,
				High:   5,
			},
			MaxEscalations: 2,
			CriticalFuse: CriticalFuse{
				Window:      15 * time.Minute,
				TotalLimit:  5,
				PerRunLimit: 2,
			},
			DailySummaryAt:           "09:00",
			HoldMaxDuration:          720 * time.Hour,
			ChannelFailureAlertAfter: 3,
			ReasonDefaults: map[string]AttentionReasonDefault{
				"design_approval": {86400000, "hold", "hold"}, "guardrail_violation": {86400000, "hold", "hold"},
				"code_review": {259200000, "hold", "hold"}, "agent_blocked": {28800000, "escalate", "auto_reject"},
				"merge_conflict": {28800000, "escalate", "auto_reject"}, "failure_review": {86400000, "auto_reject", "auto_reject"},
				"startup_stall": {3600000, "escalate", "hold"},
			},
		},
		Report: Report{
			EventsPerMinute:            12,
			Burst:                      4,
			DedupeWindow:               30 * time.Second,
			MaxPayloadBytes:            65536,
			NotReadyInitialDelay:       100 * time.Millisecond,
			NotReadyMaxDelay:           1 * time.Second,
			NotReadyTotalTimeout:       10 * time.Second,
			InterruptsPerRunDailyQuota: 4,
			OnInterruptQuotaExceeded:   InterruptQuotaFailureReviewOnce,
		},
		GateDefaults: GateDefaults{
			ReviewPolicy:         ReviewPolicyAlways,
			RiskyReviewThreshold: 1,
			AutoMerge:            false,
			ChecksPendingTimeout: 1 * time.Hour,
			FlakyRetryLimit:      1,
		},
		Certification: Certification{
			TotalSamplesMin:    100,
			NegativeSamplesMin: 30,
			LeakRateMax:        0.0,
			FalseBlockRateMax:  0.2,
			Window:             4320 * time.Hour,
		},
		Metrics: Metrics{
			DesignApproval:     10,
			GuardrailViolation: 5,
			CodeReview:         15,
			AgentBlocked:       5,
			MergeConflict:      3,
			FailureReview:      5,
			StartupStall:       5,
		},
		Labels: Labels{
			Trigger:      "sift:run",
			Approved:     "sift:approved",
			Queued:       "sift:queued",
			Running:      "sift:running",
			WaitingHuman: "sift:waiting-human",
			Done:         "sift:done",
			Failed:       "sift:failed",
		},
		Logging: Logging{
			SystemMaxBytes: 10485760,
			AgentMaxBytes:  52428800,
			RetainedFiles:  5,
		},
	}
}
