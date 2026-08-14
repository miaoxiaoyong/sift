package config

import (
	"github.com/xsift/sift/internal/schema"
)

type RawConfig struct {
	schema.ClosedType `json:"-"`

	Version       *int              `json:"version" sift:"required"`
	Operators     *RawOperators     `json:"operators,omitempty"`
	Agents        []RawAgent        `json:"agents,omitempty"`
	Projects      []RawProject      `json:"projects,omitempty"`
	Brain         *RawBrain         `json:"brain,omitempty"`
	Scheduler     *RawScheduler     `json:"scheduler,omitempty"`
	Runtime       *RawRuntime       `json:"runtime,omitempty"`
	Outbox        *RawOutbox        `json:"outbox,omitempty"`
	Forge         *RawForge         `json:"forge,omitempty"`
	Attention     *RawAttention     `json:"attention,omitempty"`
	Report        *RawReport        `json:"report,omitempty"`
	GateDefaults  *RawGateDefaults  `json:"gate_defaults,omitempty"`
	Certification *RawCertification `json:"certification,omitempty"`
	Metrics       *RawMetrics       `json:"metrics,omitempty"`
	Labels        *RawLabels        `json:"labels,omitempty"`
	Logging       *RawLogging       `json:"logging,omitempty"`
}

// RawOperators is the static forge actor allowlist (config.md §3.1).
type RawOperators struct {
	GitHub []string `json:"github,omitempty"`
	GitLab []string `json:"gitlab,omitempty"`
}

// RawAgent is one agent definition (config.md §3.2).
type RawAgent struct {
	ID            *string        `json:"id" sift:"required"`
	Executable    *string        `json:"executable" sift:"required"`
	Args          []string       `json:"args,omitempty"`
	TaskTransport *TaskTransport `json:"task_transport,omitempty"`
	Backend       *Backend       `json:"backend,omitempty"`
	MaxConcurrent *int           `json:"max_concurrent,omitempty"`
	VersionArgs   []string       `json:"version_args,omitempty"`
}

// RawForgeRef is the project forge binding (config.md §3.3).
type RawForgeRef struct {
	Kind    *ForgeKind `json:"kind" sift:"required"`
	Project *string    `json:"project" sift:"required"`
	Host    *string    `json:"host,omitempty"`
	CLI     *string    `json:"cli,omitempty"`
}

// RawProject is one project definition (config.md §3.3).
type RawProject struct {
	ID      *string      `json:"id" sift:"required"`
	Repo    *string      `json:"repo" sift:"required"`
	Forge   *RawForgeRef `json:"forge" sift:"required"`
	Enabled *bool        `json:"enabled,omitempty"`
	Agents  []string     `json:"agents,omitempty"`
}

// RawBrain is the deterministic-vs-LLM brain binding (config.md §3.4).
type RawBrain struct {
	Executable        *string        `json:"executable,omitempty"`
	Args              []string       `json:"args,omitempty"`
	Protocol          *BrainProtocol `json:"protocol,omitempty"`
	DailyTokenLimit   *int           `json:"daily_token_limit,omitempty"`
	CallTimeout       *string        `json:"call_timeout,omitempty"`
	SchemaRetries     *int           `json:"schema_retries,omitempty"`
	MaxInputBytes     *int           `json:"max_input_bytes,omitempty"`
	MaxRawOutputBytes *int           `json:"max_raw_output_bytes,omitempty"`
	VersionArgs       []string       `json:"version_args,omitempty"`
}

// RawScheduler holds the named-scheduler timing knobs (config.md §3.5).
type RawScheduler struct {
	IntakeIdleInterval       *string `json:"intake_idle_interval,omitempty"`
	IntakeActiveInterval     *string `json:"intake_active_interval,omitempty"`
	IntakeInterruptInterval  *string `json:"intake_interrupt_interval,omitempty"`
	IntakeInterruptBurst     *string `json:"intake_interrupt_burst_duration,omitempty"`
	SupervisorInterval       *string `json:"supervisor_interval,omitempty"`
	ConfigDriftCheckInterval *string `json:"config_drift_check_interval,omitempty"`
	PerClassTickLimit        *int    `json:"per_class_tick_limit,omitempty"`
}

// RawRuntime holds the runtime/launcher knobs (config.md §3.6).
type RawRuntime struct {
	Backend                   *Backend `json:"backend,omitempty"`
	MaxConcurrentTotal        *int     `json:"max_concurrent_total,omitempty"`
	DefaultAgentMaxConcurrent *int     `json:"default_agent_max_concurrent,omitempty"`
	MaxAttempts               *int     `json:"max_attempts,omitempty"`
	AttemptTimeout            *string  `json:"attempt_timeout,omitempty"`
	AgentSilenceTimeout       *string  `json:"agent_silence_timeout,omitempty"`
	RetryInitialDelay         *string  `json:"retry_initial_delay,omitempty"`
	RetryMaxDelay             *string  `json:"retry_max_delay,omitempty"`
	RetryMultiplier           *float64 `json:"retry_multiplier,omitempty"`
	SpawnOperationLeaseTTL    *string  `json:"spawn_operation_lease_ttl,omitempty"`
	StartingPermitTimeout     *string  `json:"starting_permit_timeout,omitempty"`
	SpawningStartedTimeout    *string  `json:"spawning_started_timeout,omitempty"`
	HeartbeatInterval         *string  `json:"heartbeat_interval,omitempty"`
	HeartbeatStaleAfter       *string  `json:"heartbeat_stale_after,omitempty"`
	TerminationTermGrace      *string  `json:"termination_term_grace,omitempty"`
	TerminationKillGrace      *string  `json:"termination_kill_grace,omitempty"`
	AbsenceRecheckCount       *int     `json:"absence_recheck_count,omitempty"`
	AbsenceRecheckInterval    *string  `json:"absence_recheck_interval,omitempty"`
}

// RawOutbox holds the transactional-outbox knobs (config.md §3.7).
type RawOutbox struct {
	LeaseTTL          *string  `json:"lease_ttl,omitempty"`
	RetryInitialDelay *string  `json:"retry_initial_delay,omitempty"`
	RetryMaxDelay     *string  `json:"retry_max_delay,omitempty"`
	RetryMultiplier   *float64 `json:"retry_multiplier,omitempty"`
	MaxAttempts       *int     `json:"max_attempts,omitempty"`
	MaxAdvanceDelay   *string  `json:"max_advance_delay,omitempty"`
	WorkerBatchSize   *int     `json:"worker_batch_size,omitempty"`
}

// RawForge holds the forge API budget knobs (config.md §3.8).
type RawForge struct {
	HourlyAPILimit   *int     `json:"hourly_api_limit,omitempty"`
	WarningRatio     *float64 `json:"warning_ratio,omitempty"`
	SlowPollInterval *string  `json:"slow_poll_interval,omitempty"`
	CommandTimeout   *string  `json:"command_timeout,omitempty"`
}

// RawCriticalFuse is the critical-severity circuit breaker (config.md §3.9).
type RawCriticalFuse struct {
	Window      *string `json:"window,omitempty"`
	TotalLimit  *int    `json:"total_limit,omitempty"`
	PerRunLimit *int    `json:"per_run_limit,omitempty"`
}

// RawDailyQuota is the per-severity daily attention quota (config.md §3.9). It
// is a fixed-shape struct, not a map, so the closed contract rejects stray
// severities.
type RawDailyQuota struct {
	Low    *int `json:"low,omitempty"`
	Normal *int `json:"normal,omitempty"`
	High   *int `json:"high,omitempty"`
}

// RawAttention holds the attention/interrupt knobs (config.md §3.9).
type RawAttentionReasonDefault struct {
	ExpiresAfter     *string `json:"expires_after,omitempty"`
	OnExpire         *string `json:"on_expire,omitempty"`
	OnMaxEscalations *string `json:"on_max_escalations,omitempty"`
}

type RawAttentionChannelTarget struct {
	schema.ClosedType `json:"-"`
	SecretRef         string `json:"secret_ref"`
}

type RawAttentionChannel struct {
	ID           string                    `json:"id"`
	Enabled      *bool                     `json:"enabled,omitempty"`
	Renderer     string                    `json:"renderer"`
	Type         string                    `json:"type"`
	Target       RawAttentionChannelTarget `json:"target"`
	Capabilities []string                  `json:"capabilities"`
	Default      bool                      `json:"default"`
}

type RawAttention struct {
	Channels                 *[]RawAttentionChannel               `json:"channels,omitempty"`
	DayTimezone              *string                              `json:"day_timezone,omitempty"`
	DailyQuota               *RawDailyQuota                       `json:"daily_quota,omitempty"`
	MaxEscalations           *int                                 `json:"max_escalations,omitempty"`
	CriticalFuse             *RawCriticalFuse                     `json:"critical_fuse,omitempty"`
	DailySummaryAt           *string                              `json:"daily_summary_at,omitempty"`
	HoldMaxDuration          *string                              `json:"hold_max_duration,omitempty"`
	ChannelFailureAlertAfter *int                                 `json:"channel_failure_alert_after,omitempty"`
	ReasonDefaults           map[string]RawAttentionReasonDefault `json:"reason_defaults,omitempty"`
}

// RawReport holds the run.sock report knobs (config.md §3.10).
type RawReport struct {
	EventsPerMinute            *int                          `json:"events_per_minute,omitempty"`
	Burst                      *int                          `json:"burst,omitempty"`
	DedupeWindow               *string                       `json:"dedupe_window,omitempty"`
	MaxPayloadBytes            *int                          `json:"max_payload_bytes,omitempty"`
	NotReadyInitialDelay       *string                       `json:"not_ready_initial_delay,omitempty"`
	NotReadyMaxDelay           *string                       `json:"not_ready_max_delay,omitempty"`
	NotReadyTotalTimeout       *string                       `json:"not_ready_total_timeout,omitempty"`
	InterruptsPerRunDailyQuota *int                          `json:"interrupts_per_run_daily_quota,omitempty"`
	OnInterruptQuotaExceeded   *InterruptQuotaExceededAction `json:"on_interrupt_quota_exceeded,omitempty"`
}

// RawGateDefaults holds the project-policy defaults (config.md §3.11).
type RawGateDefaults struct {
	ReviewPolicy         *ReviewPolicy `json:"review_policy,omitempty"`
	RiskyReviewThreshold *int          `json:"risky_review_threshold,omitempty"`
	AutoMerge            *bool         `json:"auto_merge,omitempty"`
	ChecksPendingTimeout *string       `json:"checks_pending_timeout,omitempty"`
	FlakyRetryLimit      *int          `json:"flaky_retry_limit,omitempty"`
}

// RawCertification holds the global certification thresholds (config.md §3.12).
// These are a global专属 section; project policy cannot override them.
type RawCertification struct {
	TotalSamplesMin    *int     `json:"total_samples_min,omitempty"`
	NegativeSamplesMin *int     `json:"negative_samples_min,omitempty"`
	LeakRateMax        *float64 `json:"leak_rate_max,omitempty"`
	FalseBlockRateMax  *float64 `json:"false_block_rate_max,omitempty"`
	Window             *string  `json:"window,omitempty"`
}

// RawMetrics holds the north-star reason weights (config.md §3.13). Fixed
// shape so unknown reasons are rejected.
type RawMetrics struct {
	DesignApproval     *float64 `json:"design_approval,omitempty"`
	GuardrailViolation *float64 `json:"guardrail_violation,omitempty"`
	CodeReview         *float64 `json:"code_review,omitempty"`
	AgentBlocked       *float64 `json:"agent_blocked,omitempty"`
	MergeConflict      *float64 `json:"merge_conflict,omitempty"`
	FailureReview      *float64 `json:"failure_review,omitempty"`
	StartupStall       *float64 `json:"startup_stall,omitempty"`
}

// RawLabels holds the label projections (config.md §3.14).
type RawLabels struct {
	Trigger      *string `json:"trigger,omitempty"`
	Approved     *string `json:"approved,omitempty"`
	Queued       *string `json:"queued,omitempty"`
	Running      *string `json:"running,omitempty"`
	WaitingHuman *string `json:"waiting_human,omitempty"`
	Done         *string `json:"done,omitempty"`
	Failed       *string `json:"failed,omitempty"`
}

// RawLogging holds the log rotation knobs (config.md §3.15).
type RawLogging struct {
	SystemMaxBytes *int `json:"system_max_bytes,omitempty"`
	AgentMaxBytes  *int `json:"agent_max_bytes,omitempty"`
	RetainedFiles  *int `json:"retained_files,omitempty"`
}

// This file holds the closed-set enums of the global config. Each named string
// type implements [schema.Enumerated]; EnumValues is the single source for both
// the runtime membership check (run by the decode gateway) and the generated
// JSON Schema. V0 pins several enums to a single allowed value: any other
// value is rejected fail-closed rather than silently tolerated.

// TaskTransport is the closed set of agent task delivery channels
// (config.md §3.2).
