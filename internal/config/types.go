// Package config implements the global configuration contract of
// specs/config.md: the closed schema for ~/.sift/config.yaml, SIFT_HOME path
// resolution, zero-config defaults, the sensitive-config fingerprint, the
// warn-only drift detector, the two-level startup probe framework and the
// scheduling hard guards.
//
// The package is the second consumer of the single decode gateway
// (internal/decode, DESIGN §5.2). The on-disk file is YAML; a strict YAML→JSON
// bridge converts it once, then [decode.Closed] enforces the closed contract
// (unknown fields, required fields, type and enum rules). Business code never
// reads YAML or JSON directly: the only entry point is [Load].
//
// V0 does not hot-reload global config (config.md §1.3). [Load] reads the file
// once at daemon startup, normalizes it, computes a canonical-JSON fingerprint
// and hands back an immutable in-memory snapshot. On-disk changes are observed
// only by the [DriftChecker], which appends one security event and a doctor
// warning but never applies the new content.

package config

import (
	"time"

	"github.com/miaoxiaoyong/sift/internal/contract"
)

// Version is the single supported global-config schema version (config.md §3).
const Version = 1

// This file holds the effective, materialized configuration: the normalized
// in-memory snapshot the runtime consumes. Every field is concrete (no
// pointers) because Normalize has already resolved every default, parsed every
// duration, validated every range and resolved every cross-field rule. The
// effective Config is immutable for the daemon lifetime (config.md §1.3).
//
// JSON tags match the on-disk field names so that [CanonicalJSON] can serialize
// the effective Config directly and then re-key it into dictionary order for
// the fingerprint.

// Config is the effective global configuration snapshot.
type Config struct {
	Version       int           `json:"version"`
	Operators     Operators     `json:"operators"`
	Agents        []Agent       `json:"agents"`
	Projects      []Project     `json:"projects"`
	Brain         Brain         `json:"brain"`
	Scheduler     Scheduler     `json:"scheduler"`
	Runtime       Runtime       `json:"runtime"`
	Outbox        Outbox        `json:"outbox"`
	Forge         Forge         `json:"forge"`
	Attention     Attention     `json:"attention"`
	Report        Report        `json:"report"`
	GateDefaults  GateDefaults  `json:"gate_defaults"`
	Certification Certification `json:"certification"`
	Metrics       Metrics       `json:"metrics"`
	Labels        Labels        `json:"labels"`
	Logging       Logging       `json:"logging"`
}

// Operators is the static forge actor allowlist (config.md §3.1). Lists are
// de-duplicated and sorted during normalization so the fingerprint is stable.
type Operators struct {
	GitHub []string `json:"github"`
	GitLab []string `json:"gitlab"`
}

// Agent is one resolved agent definition (config.md §3.2). MaxConcurrent is the
// effective per-agent cap: an agent that omits it inherits
// Runtime.DefaultAgentMaxConcurrent (C1).
type Agent struct {
	ID            string        `json:"id"`
	Executable    string        `json:"executable"`
	Args          []string      `json:"args"`
	TaskTransport TaskTransport `json:"task_transport"`
	Backend       Backend       `json:"backend"`
	MaxConcurrent int           `json:"max_concurrent"`
	VersionArgs   []string      `json:"version_args"`
}

// ForgeRef is the resolved project forge binding (config.md §3.3).
type ForgeRef struct {
	Kind    ForgeKind `json:"kind"`
	Project string    `json:"project"`
	Host    string    `json:"host"`
	CLI     string    `json:"cli,omitempty"`
}

// Project is one resolved project definition (config.md §3.3). Agents lists the
// resolved candidate agent ids; an empty list means all defined agents.
type Project struct {
	ID      string   `json:"id"`
	Repo    string   `json:"repo"`
	Forge   ForgeRef `json:"forge"`
	Enabled bool     `json:"enabled"`
	Agents  []string `json:"agents"`
}

// Brain is the resolved brain binding (config.md §3.4).
type Brain struct {
	Executable        string        `json:"executable,omitempty"`
	Args              []string      `json:"args"`
	Protocol          BrainProtocol `json:"protocol"`
	DailyTokenLimit   int           `json:"daily_token_limit"`
	CallTimeout       time.Duration `json:"call_timeout"`
	SchemaRetries     int           `json:"schema_retries"`
	MaxInputBytes     int           `json:"max_input_bytes"`
	MaxRawOutputBytes int           `json:"max_raw_output_bytes"`
	VersionArgs       []string      `json:"version_args"`
}

// Scheduler holds the resolved named-scheduler timing (config.md §3.5).
type Scheduler struct {
	IntakeIdleInterval       time.Duration `json:"intake_idle_interval"`
	IntakeActiveInterval     time.Duration `json:"intake_active_interval"`
	IntakeInterruptInterval  time.Duration `json:"intake_interrupt_interval"`
	IntakeInterruptBurst     time.Duration `json:"intake_interrupt_burst_duration"`
	SupervisorInterval       time.Duration `json:"supervisor_interval"`
	ConfigDriftCheckInterval time.Duration `json:"config_drift_check_interval"`
	PerClassTickLimit        int           `json:"per_class_tick_limit"`
}

// Runtime holds the resolved runtime/launcher values (config.md §3.6).
type Runtime struct {
	Backend                   Backend       `json:"backend"`
	MaxConcurrentTotal        int           `json:"max_concurrent_total"`
	DefaultAgentMaxConcurrent int           `json:"default_agent_max_concurrent"`
	MaxAttempts               int           `json:"max_attempts"`
	AttemptTimeout            time.Duration `json:"attempt_timeout"`
	AgentSilenceTimeout       time.Duration `json:"agent_silence_timeout"`
	RetryInitialDelay         time.Duration `json:"retry_initial_delay"`
	RetryMaxDelay             time.Duration `json:"retry_max_delay"`
	RetryMultiplier           float64       `json:"retry_multiplier"`
	SpawnOperationLeaseTTL    time.Duration `json:"spawn_operation_lease_ttl"`
	StartingPermitTimeout     time.Duration `json:"starting_permit_timeout"`
	SpawningStartedTimeout    time.Duration `json:"spawning_started_timeout"`
	HeartbeatInterval         time.Duration `json:"heartbeat_interval"`
	HeartbeatStaleAfter       time.Duration `json:"heartbeat_stale_after"`
	TerminationTermGrace      time.Duration `json:"termination_term_grace"`
	TerminationKillGrace      time.Duration `json:"termination_kill_grace"`
	AbsenceRecheckCount       int           `json:"absence_recheck_count"`
	AbsenceRecheckInterval    time.Duration `json:"absence_recheck_interval"`
}

// Outbox holds the resolved transactional-outbox values (config.md §3.7).
type Outbox struct {
	LeaseTTL          time.Duration `json:"lease_ttl"`
	RetryInitialDelay time.Duration `json:"retry_initial_delay"`
	RetryMaxDelay     time.Duration `json:"retry_max_delay"`
	RetryMultiplier   float64       `json:"retry_multiplier"`
	MaxAttempts       int           `json:"max_attempts"`
	MaxAdvanceDelay   time.Duration `json:"max_advance_delay"`
	WorkerBatchSize   int           `json:"worker_batch_size"`
}

// Forge holds the resolved forge API budget (config.md §3.8).
type Forge struct {
	HourlyAPILimit   int           `json:"hourly_api_limit"`
	WarningRatio     float64       `json:"warning_ratio"`
	SlowPollInterval time.Duration `json:"slow_poll_interval"`
	CommandTimeout   time.Duration `json:"command_timeout"`
}

// CriticalFuse is the resolved critical-severity circuit breaker (config.md §3.9).
type CriticalFuse struct {
	Window      time.Duration `json:"window"`
	TotalLimit  int           `json:"total_limit"`
	PerRunLimit int           `json:"per_run_limit"`
}

// DailyQuota is the resolved per-severity daily attention quota (config.md §3.9).
type DailyQuota struct {
	Low    int `json:"low"`
	Normal int `json:"normal"`
	High   int `json:"high"`
}

// Attention holds the resolved attention/interrupt values (config.md §3.9).
type AttentionReasonDefault struct {
	ExpiresAfterMS   int64  `json:"expires_after_ms"`
	OnExpire         string `json:"on_expire"`
	OnMaxEscalations string `json:"on_max_escalations"`
}

type AttentionChannel struct {
	ID           string   `json:"id"`
	Enabled      bool     `json:"enabled"`
	Renderer     string   `json:"renderer"`
	Type         string   `json:"type"`
	TargetRef    string   `json:"target_ref"`
	Capabilities []string `json:"capabilities"`
	Default      bool     `json:"default"`
}

type Attention struct {
	Channels                 []AttentionChannel                `json:"channels"`
	DayTimezone              string                            `json:"day_timezone"`
	DailyQuota               DailyQuota                        `json:"daily_quota"`
	MaxEscalations           int                               `json:"max_escalations"`
	CriticalFuse             CriticalFuse                      `json:"critical_fuse"`
	DailySummaryAt           string                            `json:"daily_summary_at"`
	HoldMaxDuration          time.Duration                     `json:"hold_max_duration"`
	ChannelFailureAlertAfter int                               `json:"channel_failure_alert_after"`
	ReasonDefaults           map[string]AttentionReasonDefault `json:"reason_defaults"`
}

// Report holds the resolved run.sock report values (config.md §3.10).
type Report struct {
	EventsPerMinute            int                          `json:"events_per_minute"`
	Burst                      int                          `json:"burst"`
	DedupeWindow               time.Duration                `json:"dedupe_window"`
	MaxPayloadBytes            int                          `json:"max_payload_bytes"`
	NotReadyInitialDelay       time.Duration                `json:"not_ready_initial_delay"`
	NotReadyMaxDelay           time.Duration                `json:"not_ready_max_delay"`
	NotReadyTotalTimeout       time.Duration                `json:"not_ready_total_timeout"`
	InterruptsPerRunDailyQuota int                          `json:"interrupts_per_run_daily_quota"`
	OnInterruptQuotaExceeded   InterruptQuotaExceededAction `json:"on_interrupt_quota_exceeded"`
}

// GateDefaults holds the resolved project-policy defaults (config.md §3.11).
type GateDefaults struct {
	ReviewPolicy         ReviewPolicy  `json:"review_policy"`
	RiskyReviewThreshold int           `json:"risky_review_threshold"`
	AutoMerge            bool          `json:"auto_merge"`
	ChecksPendingTimeout time.Duration `json:"checks_pending_timeout"`
	FlakyRetryLimit      int           `json:"flaky_retry_limit"`
}

// Certification holds the resolved global certification thresholds
// (config.md §3.12). CertificationVersion is the derived cache key: any
// threshold or window change must alter it and so invalidate Gate caches.
type Certification struct {
	TotalSamplesMin    int           `json:"total_samples_min"`
	NegativeSamplesMin int           `json:"negative_samples_min"`
	LeakRateMax        float64       `json:"leak_rate_max"`
	FalseBlockRateMax  float64       `json:"false_block_rate_max"`
	Window             time.Duration `json:"window"`
}

// Metrics holds the resolved north-star reason weights (config.md §3.13).
type Metrics struct {
	DesignApproval     float64 `json:"design_approval"`
	GuardrailViolation float64 `json:"guardrail_violation"`
	CodeReview         float64 `json:"code_review"`
	AgentBlocked       float64 `json:"agent_blocked"`
	MergeConflict      float64 `json:"merge_conflict"`
	FailureReview      float64 `json:"failure_review"`
	StartupStall       float64 `json:"startup_stall"`
}

// Labels holds the resolved label projections (config.md §3.14).
type Labels struct {
	Trigger      string `json:"trigger"`
	Approved     string `json:"approved"`
	Queued       string `json:"queued"`
	Running      string `json:"running"`
	WaitingHuman string `json:"waiting_human"`
	Done         string `json:"done"`
	Failed       string `json:"failed"`
}

// Logging holds the resolved log rotation values (config.md §3.15).
type Logging struct {
	SystemMaxBytes int `json:"system_max_bytes"`
	AgentMaxBytes  int `json:"agent_max_bytes"`
	RetainedFiles  int `json:"retained_files"`
}

// RawConfig is the closed-contract decode target for ~/.sift/config.yaml
// (config.md §3). Every field is a pointer so "absent" is distinguishable from
// a present zero value: outbox.max_attempts defaults to 0 (retry forever) and
// 0 is also a valid explicit value, so the gateway cannot infer intent from a
// zero. Only Version is required (and only when the file is present); the
// absent-file path constructs a RawConfig directly and bypasses the gateway.
//
// It embeds [contract.ClosedType] and is decoded with [decode.Closed], so any
// unknown field is rejected. The generated JSON Schema lives in
// internal/contract/schema/raw_config.schema.json.
type RawConfig struct {
	contract.ClosedType `json:"-"`

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
	contract.ClosedType `json:"-"`
	SecretRef           string `json:"secret_ref"`
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
// type implements [decode.Enumerated]; EnumValues is the single source for both
// the runtime membership check (run by the decode gateway) and the generated
// JSON Schema. V0 pins several enums to a single allowed value: any other
// value is rejected fail-closed rather than silently tolerated.

// TaskTransport is the closed set of agent task delivery channels
// (config.md §3.2).
type TaskTransport string

const (
	TaskTransportStdin TaskTransport = "stdin"
	TaskTransportFile  TaskTransport = "file"
)

// EnumValues satisfies [decode.Enumerated].
func (TaskTransport) EnumValues() []string {
	return []string{string(TaskTransportStdin), string(TaskTransportFile)}
}

// Backend selects the agent execution backend (config.md §3.2, §3.6).
type Backend string

const (
	BackendProcess Backend = "process"
	BackendTmux    Backend = "tmux"
)

// EnumValues satisfies [decode.Enumerated].
func (Backend) EnumValues() []string {
	return []string{string(BackendProcess), string(BackendTmux)}
}

// ForgeKind selects the forge platform adapter (config.md §3.3).
type ForgeKind string

const (
	ForgeKindGitHub ForgeKind = "github"
	ForgeKindGitLab ForgeKind = "gitlab"
)

// EnumValues satisfies [decode.Enumerated].
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

// EnumValues satisfies [decode.Enumerated].
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

// EnumValues satisfies [decode.Enumerated].
func (ReviewPolicy) EnumValues() []string {
	return []string{string(ReviewPolicyAlways), string(ReviewPolicyRiskyOnly), string(ReviewPolicyNever)}
}

// InterruptQuotaExceededAction pins the report quota-exceeded action
// (config.md §3.10). V0 admits only failure_review_once.
type InterruptQuotaExceededAction string

const (
	InterruptQuotaFailureReviewOnce InterruptQuotaExceededAction = "failure_review_once"
)

// EnumValues satisfies [decode.Enumerated].
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
