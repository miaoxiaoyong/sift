// Package config implements the global configuration contract of
// specs/config.md: the closed schema for ~/.sift/config.yaml, SIFT_HOME path
// resolution, zero-config defaults, the sensitive-config fingerprint, the
// warn-only drift detector, the two-level startup probe framework and the
// scheduling hard guards.
//
// The package is the second consumer of the single decode gateway
// (internal/schema, DESIGN §5.2). The on-disk file is YAML; a strict YAML→JSON
// bridge converts it once, then [schema.Closed] enforces the closed contract
// (unknown fields, required fields, type and enum rules). Business code never
// reads YAML or JSON directly: the only entry point is [Load].
//
// V0 does not hot-reload global config (config.md §1.3). [Load] reads the file
// once at daemon startup, normalizes it, computes a canonical-JSON fingerprint
// and hands back an immutable in-memory snapshot. On-disk changes are observed
// only by the [DriftChecker], which appends one security event and a doctor
// warning but never applies the new content.

package config

import "time"

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
// It embeds [schema.ClosedType] and is decoded with [schema.Closed], so any
// unknown field is rejected. The generated JSON Schema lives in
// internal/schema/artifacts/raw_config.schema.json.
