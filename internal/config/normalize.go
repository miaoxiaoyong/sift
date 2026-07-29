package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ConfigError carries the dotted field path of a normalization failure so
// callers (startup probe, doctor) can point at the offending line.
type ConfigError struct {
	Field string
	Msg   string
}

func (e *ConfigError) Error() string {
	if e.Field == "" {
		return "config: " + e.Msg
	}
	return "config: " + e.Field + ": " + e.Msg
}

func configError(field, format string, args ...any) error {
	return &ConfigError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// agentIDRe is the id grammar shared by agents and projects (config.md §3.2/§3.3).
var agentIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

const taskFileToken = "{task_file}"

// Normalize materializes an effective [Config] from a decoded [RawConfig]:
// it overlays the specified values onto [DefaultConfig], parses every duration,
// resolves defaults, de-duplicates set-typed lists and enforces every range and
// cross-field rule in config.md §3. The returned snapshot is immutable for the
// daemon lifetime.
//
// A nil raw yields the pure zero-config default (config.md §6 scenario 1).
func Normalize(raw *RawConfig) (*Config, error) {
	if raw == nil {
		raw = &RawConfig{}
	}
	cfg := DefaultConfig()

	if raw.Version != nil {
		if *raw.Version != Version {
			return nil, configError("version", "must be %d, got %d", Version, *raw.Version)
		}
	}
	cfg.Version = Version

	if raw.Operators != nil {
		gh, err := dedupAllowlist("operators.github", raw.Operators.GitHub)
		if err != nil {
			return nil, err
		}
		gl, err := dedupAllowlist("operators.gitlab", raw.Operators.GitLab)
		if err != nil {
			return nil, err
		}
		cfg.Operators.GitHub = gh
		cfg.Operators.GitLab = gl
	}

	defined, err := normalizeAgents(raw.Agents, cfg.Runtime.DefaultAgentMaxConcurrent)
	if err != nil {
		return nil, err
	}
	cfg.Agents = defined

	if raw.Runtime != nil {
		if err := normalizeRuntime(raw.Runtime, cfg); err != nil {
			return nil, err
		}
		// Agent max_concurrent inherits the resolved runtime default (C1):
		// re-resolve any agent that took the default before runtime was known.
		for i := range cfg.Agents {
			if raw.Agents[i].MaxConcurrent == nil {
				cfg.Agents[i].MaxConcurrent = cfg.Runtime.DefaultAgentMaxConcurrent
			}
		}
	}

	projects, err := normalizeProjects(raw.Projects, cfg.Agents)
	if err != nil {
		return nil, err
	}
	cfg.Projects = projects

	if raw.Brain != nil {
		if err := normalizeBrain(raw.Brain, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Scheduler != nil {
		if err := normalizeScheduler(raw.Scheduler, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Outbox != nil {
		if err := normalizeOutbox(raw.Outbox, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Forge != nil {
		if err := normalizeForge(raw.Forge, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Attention != nil {
		if err := normalizeAttention(raw.Attention, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Report != nil {
		if err := normalizeReport(raw.Report, cfg); err != nil {
			return nil, err
		}
	}
	if raw.GateDefaults != nil {
		if err := normalizeGateDefaults(raw.GateDefaults, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Certification != nil {
		if err := normalizeCertification(raw.Certification, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Metrics != nil {
		if err := normalizeMetrics(raw.Metrics, cfg); err != nil {
			return nil, err
		}
	}
	if raw.Labels != nil {
		normalizeLabels(raw.Labels, cfg)
	}
	if raw.Logging != nil {
		if err := normalizeLogging(raw.Logging, cfg); err != nil {
			return nil, err
		}
	}

	// Cross-section guards that depend on resolved values.
	if err := checkHeartbeat(cfg); err != nil {
		return nil, err
	}
	if err := checkSlowPoll(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// dedupAllowlist de-duplicates and sorts a forge operator allowlist, rejecting
// empty strings (config.md §3.1).
func dedupAllowlist(field string, in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			return nil, configError(field, "contains empty login")
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeAgents(raw []RawAgent, defaultConc int) ([]Agent, error) {
	out := make([]Agent, 0, len(raw))
	ids := make(map[string]int)
	for i, a := range raw {
		f := fmt.Sprintf("agents[%d]", i)
		if a.ID == nil || *a.ID == "" {
			return nil, configError(f+".id", "required")
		}
		if !agentIDRe.MatchString(*a.ID) {
			return nil, configError(f+".id", "must match ^[a-z][a-z0-9-]{0,62}$")
		}
		if prev, dup := ids[*a.ID]; dup {
			return nil, configError(f+".id", "duplicate id %q (first at agents[%d])", *a.ID, prev)
		}
		ids[*a.ID] = i
		if a.Executable == nil || *a.Executable == "" {
			return nil, configError(f+".executable", "required and must be non-empty")
		}

		transport := TaskTransportStdin
		if a.TaskTransport != nil {
			transport = *a.TaskTransport
		}
		backend := BackendProcess
		if a.Backend != nil {
			backend = *a.Backend
		}
		if err := validateArgs(f+".args", a.Args, transport); err != nil {
			return nil, err
		}

		maxConc := defaultConc
		if a.MaxConcurrent != nil {
			maxConc = *a.MaxConcurrent
			if err := checkIntRange(f+".max_concurrent", maxConc, 1, 32); err != nil {
				return nil, err
			}
		}

		versionArgs := []string{"--version"}
		if a.VersionArgs != nil {
			versionArgs = append([]string(nil), a.VersionArgs...)
		}

		out = append(out, Agent{
			ID:            *a.ID,
			Executable:    *a.Executable,
			Args:          append([]string(nil), a.Args...),
			TaskTransport: transport,
			Backend:       backend,
			MaxConcurrent: maxConc,
			VersionArgs:   versionArgs,
		})
	}
	return out, nil
}

// validateArgs enforces the argv contract (config.md §3.2): no NUL bytes, the
// only permitted placeholder is the whole token {task_file}, stdin forbids it
// and file requires exactly one.
func validateArgs(field string, args []string, transport TaskTransport) error {
	placeholders := 0
	for i, a := range args {
		if strings.ContainsRune(a, '\x00') {
			return configError(fmt.Sprintf("%s[%d]", field, i), "contains NUL byte")
		}
		if strings.Contains(a, taskFileToken) {
			if a != taskFileToken {
				return configError(fmt.Sprintf("%s[%d]", field, i), "{task_file} must be a whole argv token, not a substring")
			}
			placeholders++
		}
	}
	switch transport {
	case TaskTransportStdin:
		if placeholders != 0 {
			return configError(field, "task_transport=stdin forbids {task_file}")
		}
	case TaskTransportFile:
		if placeholders != 1 {
			return configError(field, "task_transport=file requires exactly one {task_file} argv token")
		}
	}
	return nil
}

func normalizeProjects(raw []RawProject, agents []Agent) ([]Project, error) {
	agentIDs := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentIDs[a.ID] = true
	}

	out := make([]Project, 0, len(raw))
	ids := make(map[string]int)
	repoSeen := make(map[string]string) // cleaned repo -> project id
	for i, p := range raw {
		f := fmt.Sprintf("projects[%d]", i)
		if p.ID == nil || *p.ID == "" {
			return nil, configError(f+".id", "required")
		}
		if !agentIDRe.MatchString(*p.ID) {
			return nil, configError(f+".id", "must match ^[a-z][a-z0-9-]{0,62}$")
		}
		if prev, dup := ids[*p.ID]; dup {
			return nil, configError(f+".id", "duplicate id %q (first at projects[%d])", *p.ID, prev)
		}
		ids[*p.ID] = i
		if p.Repo == nil || *p.Repo == "" {
			return nil, configError(f+".repo", "required")
		}
		if !isAbsPath(*p.Repo) {
			return nil, configError(f+".repo", "must be an absolute path")
		}
		repo := cleanPath(*p.Repo)
		if p.Forge == nil {
			return nil, configError(f+".forge", "required")
		}
		if p.Forge.Kind == nil {
			return nil, configError(f+".forge.kind", "required")
		}
		if p.Forge.Project == nil || *p.Forge.Project == "" {
			return nil, configError(f+".forge.project", "required")
		}

		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		if enabled {
			if other, dup := repoSeen[repo]; dup {
				return nil, configError(f+".repo", "duplicate enabled repo %q (first used by project %q)", repo, other)
			}
			repoSeen[repo] = *p.ID
		}

		// Resolve per-project agent candidates. Empty means all defined agents.
		candidates := make([]string, 0, len(agents))
		if len(p.Agents) == 0 {
			for _, a := range agents {
				candidates = append(candidates, a.ID)
			}
		} else {
			seen := make(map[string]bool)
			for j, ref := range p.Agents {
				if !agentIDs[ref] {
					return nil, configError(fmt.Sprintf("%s.agents[%d]", f, j), "references unknown agent %q", ref)
				}
				if seen[ref] {
					continue
				}
				seen[ref] = true
				candidates = append(candidates, ref)
			}
		}

		host := p.Forge.Kind.defaultHost()
		if p.Forge.Host != nil && *p.Forge.Host != "" {
			host = *p.Forge.Host
		}
		cli := p.Forge.Kind.defaultCLI()
		if p.Forge.CLI != nil && *p.Forge.CLI != "" {
			cli = *p.Forge.CLI
		}

		out = append(out, Project{
			ID:      *p.ID,
			Repo:    repo,
			Enabled: enabled,
			Agents:  candidates,
			Forge: ForgeRef{
				Kind:    *p.Forge.Kind,
				Project: *p.Forge.Project,
				Host:    host,
				CLI:     cli,
			},
		})
	}
	return out, nil
}

func normalizeBrain(raw *RawBrain, cfg *Config) error {
	exe := ""
	if raw.Executable != nil {
		exe = *raw.Executable
	}
	// executable == "" is deterministic mode and is not an error (config.md §3.4).
	if exe != "" {
		if err := validateArgs("brain.args", raw.Args, TaskTransportStdin); err != nil {
			return err
		}
	}
	cfg.Brain.Executable = exe
	if raw.Args != nil {
		cfg.Brain.Args = append([]string(nil), raw.Args...)
	}
	if raw.Protocol != nil {
		cfg.Brain.Protocol = *raw.Protocol
	}
	if raw.DailyTokenLimit != nil {
		v := *raw.DailyTokenLimit
		switch {
		case v == 0:
		case v >= 1000:
		default:
			return configError("brain.daily_token_limit", "must be 0 (forbid LLM) or >= 1000, got %d", v)
		}
		cfg.Brain.DailyTokenLimit = v
	}
	d, err := parseDuration("brain.call_timeout", raw.CallTimeout, cfg.Brain.CallTimeout)
	if err != nil {
		return err
	}
	if err := checkRange("brain.call_timeout", d, time.Second, 30*time.Minute); err != nil {
		return err
	}
	cfg.Brain.CallTimeout = d
	if raw.SchemaRetries != nil {
		if *raw.SchemaRetries != 1 {
			return configError("brain.schema_retries", "V0 only allows 1, got %d", *raw.SchemaRetries)
		}
		cfg.Brain.SchemaRetries = *raw.SchemaRetries
	}
	if raw.MaxInputBytes != nil {
		if err := checkIntRange("brain.max_input_bytes", *raw.MaxInputBytes, 4096, 16777216); err != nil {
			return err
		}
		cfg.Brain.MaxInputBytes = *raw.MaxInputBytes
	}
	if raw.MaxRawOutputBytes != nil {
		if err := checkIntRange("brain.max_raw_output_bytes", *raw.MaxRawOutputBytes, 4096, 16777216); err != nil {
			return err
		}
		cfg.Brain.MaxRawOutputBytes = *raw.MaxRawOutputBytes
	}
	if raw.VersionArgs != nil {
		cfg.Brain.VersionArgs = append([]string(nil), raw.VersionArgs...)
	}
	return nil
}

func normalizeScheduler(raw *RawScheduler, cfg *Config) error {
	type dField struct {
		name     string
		src      *string
		dst      *time.Duration
		min, max time.Duration
	}
	intFields := []struct {
		name     string
		src      *int
		dst      *int
		min, max int
	}{
		{"scheduler.per_class_tick_limit", raw.PerClassTickLimit, &cfg.Scheduler.PerClassTickLimit, 1, 10000},
	}
	for _, f := range intFields {
		if err := overlayInt(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	durFields := []dField{
		{"scheduler.intake_idle_interval", raw.IntakeIdleInterval, &cfg.Scheduler.IntakeIdleInterval, 5 * time.Second, time.Hour},
		{"scheduler.intake_active_interval", raw.IntakeActiveInterval, &cfg.Scheduler.IntakeActiveInterval, 2 * time.Second, 10 * time.Minute},
		{"scheduler.intake_interrupt_interval", raw.IntakeInterruptInterval, &cfg.Scheduler.IntakeInterruptInterval, 2 * time.Second, 10 * time.Minute},
		{"scheduler.intake_interrupt_burst_duration", raw.IntakeInterruptBurst, &cfg.Scheduler.IntakeInterruptBurst, 10 * time.Second, time.Hour},
		{"scheduler.supervisor_interval", raw.SupervisorInterval, &cfg.Scheduler.SupervisorInterval, 100 * time.Millisecond, 30 * time.Second},
		{"scheduler.config_drift_check_interval", raw.ConfigDriftCheckInterval, &cfg.Scheduler.ConfigDriftCheckInterval, time.Second, time.Hour},
	}
	for _, f := range durFields {
		if err := overlayDuration(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	return nil
}

func normalizeRuntime(raw *RawRuntime, cfg *Config) error {
	if raw.Backend != nil {
		cfg.Runtime.Backend = *raw.Backend
	}
	intFields := []struct {
		name     string
		src      *int
		dst      *int
		min, max int
	}{
		{"runtime.max_concurrent_total", raw.MaxConcurrentTotal, &cfg.Runtime.MaxConcurrentTotal, 1, 32},
		{"runtime.default_agent_max_concurrent", raw.DefaultAgentMaxConcurrent, &cfg.Runtime.DefaultAgentMaxConcurrent, 1, 32},
		{"runtime.max_attempts", raw.MaxAttempts, &cfg.Runtime.MaxAttempts, 1, 20},
		{"runtime.absence_recheck_count", raw.AbsenceRecheckCount, &cfg.Runtime.AbsenceRecheckCount, 1, 20},
	}
	for _, f := range intFields {
		if err := overlayInt(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	if raw.RetryMultiplier != nil {
		if err := checkFloatRange("runtime.retry_multiplier", *raw.RetryMultiplier, 1, 10); err != nil {
			return err
		}
		cfg.Runtime.RetryMultiplier = *raw.RetryMultiplier
	}
	durFields := []struct {
		name     string
		src      *string
		dst      *time.Duration
		min, max time.Duration
	}{
		{"runtime.attempt_timeout", raw.AttemptTimeout, &cfg.Runtime.AttemptTimeout, time.Minute, 24 * time.Hour},
		{"runtime.agent_silence_timeout", raw.AgentSilenceTimeout, &cfg.Runtime.AgentSilenceTimeout, time.Minute, 24 * time.Hour},
		{"runtime.retry_initial_delay", raw.RetryInitialDelay, &cfg.Runtime.RetryInitialDelay, 0, time.Hour},
		{"runtime.spawn_operation_lease_ttl", raw.SpawnOperationLeaseTTL, &cfg.Runtime.SpawnOperationLeaseTTL, 5 * time.Second, 10 * time.Minute},
		{"runtime.starting_permit_timeout", raw.StartingPermitTimeout, &cfg.Runtime.StartingPermitTimeout, time.Second, 10 * time.Minute},
		{"runtime.spawning_started_timeout", raw.SpawningStartedTimeout, &cfg.Runtime.SpawningStartedTimeout, time.Second, 10 * time.Minute},
		{"runtime.heartbeat_interval", raw.HeartbeatInterval, &cfg.Runtime.HeartbeatInterval, 500 * time.Millisecond, time.Minute},
		{"runtime.termination_term_grace", raw.TerminationTermGrace, &cfg.Runtime.TerminationTermGrace, 0, 10 * time.Minute},
		{"runtime.termination_kill_grace", raw.TerminationKillGrace, &cfg.Runtime.TerminationKillGrace, 0, 10 * time.Minute},
		{"runtime.absence_recheck_interval", raw.AbsenceRecheckInterval, &cfg.Runtime.AbsenceRecheckInterval, 100 * time.Millisecond, time.Minute},
	}
	for _, f := range durFields {
		if err := overlayDuration(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	// retry_max_delay >= retry_initial_delay; heartbeat_stale_after checked in
	// checkHeartbeat (it bounds against heartbeat_interval).
	if err := overlayDuration("runtime.retry_max_delay", raw.RetryMaxDelay, &cfg.Runtime.RetryMaxDelay, 0, 0); err != nil {
		return err
	}
	if cfg.Runtime.RetryMaxDelay < cfg.Runtime.RetryInitialDelay {
		return configError("runtime.retry_max_delay", "%s < retry_initial_delay %s", cfg.Runtime.RetryMaxDelay, cfg.Runtime.RetryInitialDelay)
	}
	// heartbeat_stale_after max 10m; min is cross-checked against interval.
	if err := overlayDuration("runtime.heartbeat_stale_after", raw.HeartbeatStaleAfter, &cfg.Runtime.HeartbeatStaleAfter, 0, 10*time.Minute); err != nil {
		return err
	}
	return nil
}

func normalizeOutbox(raw *RawOutbox, cfg *Config) error {
	if raw.MaxAttempts != nil {
		v := *raw.MaxAttempts
		if v != 0 {
			if err := checkIntRange("outbox.max_attempts", v, 1, 1000); err != nil {
				return err
			}
		}
		cfg.Outbox.MaxAttempts = v
	}
	if raw.RetryMultiplier != nil {
		if err := checkFloatRange("outbox.retry_multiplier", *raw.RetryMultiplier, 1, 10); err != nil {
			return err
		}
		cfg.Outbox.RetryMultiplier = *raw.RetryMultiplier
	}
	if err := overlayInt("outbox.worker_batch_size", raw.WorkerBatchSize, &cfg.Outbox.WorkerBatchSize, 1, 1000); err != nil {
		return err
	}
	durFields := []struct {
		name     string
		src      *string
		dst      *time.Duration
		min, max time.Duration
	}{
		{"outbox.lease_ttl", raw.LeaseTTL, &cfg.Outbox.LeaseTTL, 5 * time.Second, 10 * time.Minute},
		{"outbox.retry_initial_delay", raw.RetryInitialDelay, &cfg.Outbox.RetryInitialDelay, 100 * time.Millisecond, time.Hour},
		{"outbox.max_advance_delay", raw.MaxAdvanceDelay, &cfg.Outbox.MaxAdvanceDelay, 100 * time.Millisecond, 30 * time.Second},
	}
	for _, f := range durFields {
		if err := overlayDuration(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	if err := overlayDuration("outbox.retry_max_delay", raw.RetryMaxDelay, &cfg.Outbox.RetryMaxDelay, 0, 0); err != nil {
		return err
	}
	if cfg.Outbox.RetryMaxDelay < cfg.Outbox.RetryInitialDelay {
		return configError("outbox.retry_max_delay", "%s < retry_initial_delay %s", cfg.Outbox.RetryMaxDelay, cfg.Outbox.RetryInitialDelay)
	}
	return nil
}

func normalizeForge(raw *RawForge, cfg *Config) error {
	if raw.HourlyAPILimit != nil {
		if err := checkIntRange("forge.hourly_api_limit", *raw.HourlyAPILimit, 1, 100000); err != nil {
			return err
		}
		cfg.Forge.HourlyAPILimit = *raw.HourlyAPILimit
	}
	if raw.WarningRatio != nil {
		v := *raw.WarningRatio
		if v <= 0 || v >= 1 {
			return configError("forge.warning_ratio", "must be in open interval (0,1), got %v", v)
		}
		cfg.Forge.WarningRatio = v
	}
	if err := overlayDuration("forge.command_timeout", raw.CommandTimeout, &cfg.Forge.CommandTimeout, time.Second, 10*time.Minute); err != nil {
		return err
	}
	// slow_poll_interval has no standalone range; its only bound is
	// "not less than the active interval", enforced by checkSlowPoll after all
	// sections resolve.
	if err := overlayDuration("forge.slow_poll_interval", raw.SlowPollInterval, &cfg.Forge.SlowPollInterval, 0, 0); err != nil {
		return err
	}
	return nil
}

func normalizeAttention(raw *RawAttention, cfg *Config) error {
	if raw.DayTimezone != nil {
		tz := *raw.DayTimezone
		if tz != "local" {
			if _, err := time.LoadLocation(tz); err != nil {
				return configError("attention.day_timezone", "invalid IANA timezone %q: %v", tz, err)
			}
		}
		cfg.Attention.DayTimezone = tz
	}
	if raw.DailyQuota != nil {
		if err := overlayInt("attention.daily_quota.low", raw.DailyQuota.Low, &cfg.Attention.DailyQuota.Low, 0, 1000); err != nil {
			return err
		}
		if err := overlayInt("attention.daily_quota.normal", raw.DailyQuota.Normal, &cfg.Attention.DailyQuota.Normal, 0, 1000); err != nil {
			return err
		}
		if err := overlayInt("attention.daily_quota.high", raw.DailyQuota.High, &cfg.Attention.DailyQuota.High, 0, 1000); err != nil {
			return err
		}
	}
	if err := overlayInt("attention.max_escalations", raw.MaxEscalations, &cfg.Attention.MaxEscalations, 0, 10); err != nil {
		return err
	}
	if raw.CriticalFuse != nil {
		if err := overlayDuration("attention.critical_fuse.window", raw.CriticalFuse.Window, &cfg.Attention.CriticalFuse.Window, time.Minute, 24*time.Hour); err != nil {
			return err
		}
		if err := overlayInt("attention.critical_fuse.total_limit", raw.CriticalFuse.TotalLimit, &cfg.Attention.CriticalFuse.TotalLimit, 1, 1000); err != nil {
			return err
		}
		if err := overlayInt("attention.critical_fuse.per_run_limit", raw.CriticalFuse.PerRunLimit, &cfg.Attention.CriticalFuse.PerRunLimit, 1, cfg.Attention.CriticalFuse.TotalLimit); err != nil {
			return err
		}
	}
	if raw.DailySummaryAt != nil {
		if !isValidHHMM(*raw.DailySummaryAt) {
			return configError("attention.daily_summary_at", "must be HH:MM, got %q", *raw.DailySummaryAt)
		}
		cfg.Attention.DailySummaryAt = *raw.DailySummaryAt
	}
	if err := overlayDuration("attention.hold_max_duration", raw.HoldMaxDuration, &cfg.Attention.HoldMaxDuration, time.Minute, 8760*time.Hour); err != nil {
		return err
	}
	if err := overlayInt("attention.channel_failure_alert_after", raw.ChannelFailureAlertAfter, &cfg.Attention.ChannelFailureAlertAfter, 1, 100); err != nil {
		return err
	}
	return nil
}

func normalizeReport(raw *RawReport, cfg *Config) error {
	intFields := []struct {
		name     string
		src      *int
		dst      *int
		min, max int
	}{
		{"report.events_per_minute", raw.EventsPerMinute, &cfg.Report.EventsPerMinute, 1, 10000},
		{"report.burst", raw.Burst, &cfg.Report.Burst, 1, 1000},
		{"report.max_payload_bytes", raw.MaxPayloadBytes, &cfg.Report.MaxPayloadBytes, 1024, 1048576},
		{"report.interrupts_per_run_daily_quota", raw.InterruptsPerRunDailyQuota, &cfg.Report.InterruptsPerRunDailyQuota, 1, 100},
	}
	for _, f := range intFields {
		if err := overlayInt(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	if raw.OnInterruptQuotaExceeded != nil {
		cfg.Report.OnInterruptQuotaExceeded = *raw.OnInterruptQuotaExceeded
	}
	durFields := []struct {
		name     string
		src      *string
		dst      *time.Duration
		min, max time.Duration
	}{
		{"report.dedupe_window", raw.DedupeWindow, &cfg.Report.DedupeWindow, 0, time.Hour},
		{"report.not_ready_initial_delay", raw.NotReadyInitialDelay, &cfg.Report.NotReadyInitialDelay, 10 * time.Millisecond, 5 * time.Second},
	}
	for _, f := range durFields {
		if err := overlayDuration(f.name, f.src, f.dst, f.min, f.max); err != nil {
			return err
		}
	}
	if err := overlayDuration("report.not_ready_max_delay", raw.NotReadyMaxDelay, &cfg.Report.NotReadyMaxDelay, 0, 0); err != nil {
		return err
	}
	if cfg.Report.NotReadyMaxDelay < cfg.Report.NotReadyInitialDelay {
		return configError("report.not_ready_max_delay", "%s < not_ready_initial_delay %s", cfg.Report.NotReadyMaxDelay, cfg.Report.NotReadyInitialDelay)
	}
	if err := overlayDuration("report.not_ready_total_timeout", raw.NotReadyTotalTimeout, &cfg.Report.NotReadyTotalTimeout, 0, time.Minute); err != nil {
		return err
	}
	if cfg.Report.NotReadyTotalTimeout < cfg.Report.NotReadyMaxDelay {
		return configError("report.not_ready_total_timeout", "%s < not_ready_max_delay %s", cfg.Report.NotReadyTotalTimeout, cfg.Report.NotReadyMaxDelay)
	}
	return nil
}

func normalizeGateDefaults(raw *RawGateDefaults, cfg *Config) error {
	if raw.ReviewPolicy != nil {
		cfg.GateDefaults.ReviewPolicy = *raw.ReviewPolicy
	}
	if err := overlayInt("gate_defaults.risky_review_threshold", raw.RiskyReviewThreshold, &cfg.GateDefaults.RiskyReviewThreshold, 0, 100); err != nil {
		return err
	}
	if raw.AutoMerge != nil {
		cfg.GateDefaults.AutoMerge = *raw.AutoMerge
	}
	if err := overlayInt("gate_defaults.flaky_retry_limit", raw.FlakyRetryLimit, &cfg.GateDefaults.FlakyRetryLimit, 0, 10); err != nil {
		return err
	}
	if err := overlayDuration("gate_defaults.checks_pending_timeout", raw.ChecksPendingTimeout, &cfg.GateDefaults.ChecksPendingTimeout, time.Minute, 24*time.Hour); err != nil {
		return err
	}
	return nil
}

func normalizeCertification(raw *RawCertification, cfg *Config) error {
	if err := overlayInt("certification.total_samples_min", raw.TotalSamplesMin, &cfg.Certification.TotalSamplesMin, 1, 100000); err != nil {
		return err
	}
	if raw.NegativeSamplesMin != nil {
		v := *raw.NegativeSamplesMin
		if err := checkIntRange("certification.negative_samples_min", v, 1, cfg.Certification.TotalSamplesMin); err != nil {
			return err
		}
		cfg.Certification.NegativeSamplesMin = v
	}
	if raw.LeakRateMax != nil {
		if err := checkFloatRange("certification.leak_rate_max", *raw.LeakRateMax, 0, 1); err != nil {
			return err
		}
		cfg.Certification.LeakRateMax = *raw.LeakRateMax
	}
	if raw.FalseBlockRateMax != nil {
		if err := checkFloatRange("certification.false_block_rate_max", *raw.FalseBlockRateMax, 0, 1); err != nil {
			return err
		}
		cfg.Certification.FalseBlockRateMax = *raw.FalseBlockRateMax
	}
	if err := overlayDuration("certification.window", raw.Window, &cfg.Certification.Window, 24*time.Hour, 8760*time.Hour); err != nil {
		return err
	}
	return nil
}

func normalizeMetrics(raw *RawMetrics, cfg *Config) error {
	type mf struct {
		name string
		src  *float64
		dst  *float64
	}
	fields := []mf{
		{"metrics.design_approval", raw.DesignApproval, &cfg.Metrics.DesignApproval},
		{"metrics.guardrail_violation", raw.GuardrailViolation, &cfg.Metrics.GuardrailViolation},
		{"metrics.code_review", raw.CodeReview, &cfg.Metrics.CodeReview},
		{"metrics.agent_blocked", raw.AgentBlocked, &cfg.Metrics.AgentBlocked},
		{"metrics.merge_conflict", raw.MergeConflict, &cfg.Metrics.MergeConflict},
		{"metrics.failure_review", raw.FailureReview, &cfg.Metrics.FailureReview},
		{"metrics.startup_stall", raw.StartupStall, &cfg.Metrics.StartupStall},
	}
	for _, f := range fields {
		if f.src == nil {
			continue
		}
		v := *f.src
		if v < 0 {
			return configError(f.name, "weight must be non-negative, got %v", v)
		}
		*f.dst = v
	}
	return nil
}

func normalizeLabels(raw *RawLabels, cfg *Config) {
	// Labels are free-form strings; the closed contract already rejected
	// unknown keys. An empty value falls back to the default projection.
	overlayStr(raw.Trigger, &cfg.Labels.Trigger)
	overlayStr(raw.Approved, &cfg.Labels.Approved)
	overlayStr(raw.Queued, &cfg.Labels.Queued)
	overlayStr(raw.Running, &cfg.Labels.Running)
	overlayStr(raw.WaitingHuman, &cfg.Labels.WaitingHuman)
	overlayStr(raw.Done, &cfg.Labels.Done)
	overlayStr(raw.Failed, &cfg.Labels.Failed)
}

func normalizeLogging(raw *RawLogging, cfg *Config) error {
	if err := overlayInt("logging.system_max_bytes", raw.SystemMaxBytes, &cfg.Logging.SystemMaxBytes, 1048576, 1073741824); err != nil {
		return err
	}
	if err := overlayInt("logging.agent_max_bytes", raw.AgentMaxBytes, &cfg.Logging.AgentMaxBytes, 1048576, 10737418240); err != nil {
		return err
	}
	if err := overlayInt("logging.retained_files", raw.RetainedFiles, &cfg.Logging.RetainedFiles, 1, 100); err != nil {
		return err
	}
	return nil
}

// Cross-field checks that depend on resolved values from multiple sections.

func checkHeartbeat(cfg *Config) error {
	if cfg.Runtime.HeartbeatStaleAfter < cfg.Runtime.HeartbeatInterval {
		return configError("runtime.heartbeat_stale_after", "%s < heartbeat_interval %s", cfg.Runtime.HeartbeatStaleAfter, cfg.Runtime.HeartbeatInterval)
	}
	return nil
}

func checkSlowPoll(cfg *Config) error {
	if cfg.Forge.SlowPollInterval < cfg.Scheduler.IntakeActiveInterval {
		return configError("forge.slow_poll_interval", "%s < scheduler.intake_active_interval %s", cfg.Forge.SlowPollInterval, cfg.Scheduler.IntakeActiveInterval)
	}
	return nil
}

// --- small overlay/range helpers ---

func overlayDuration(field string, src *string, dst *time.Duration, min, max time.Duration) error {
	if src == nil {
		return nil
	}
	d, err := time.ParseDuration(*src)
	if err != nil {
		return configError(field, "invalid duration %q: %v", *src, err)
	}
	if min != 0 || max != 0 {
		if err := checkRange(field, d, min, max); err != nil {
			return err
		}
	}
	*dst = d
	return nil
}

func overlayInt(field string, src *int, dst *int, min, max int) error {
	if src == nil {
		return nil
	}
	if err := checkIntRange(field, *src, min, max); err != nil {
		return err
	}
	*dst = *src
	return nil
}

func overlayStr(src *string, dst *string) {
	if src != nil && *src != "" {
		*dst = *src
	}
}

func checkRange(field string, d, min, max time.Duration) error {
	if d < min || d > max {
		return configError(field, "duration %s out of range [%s, %s]", d, min, max)
	}
	return nil
}

func checkIntRange(field string, v, min, max int) error {
	if v < min || v > max {
		return configError(field, "value %d out of range [%d, %d]", v, min, max)
	}
	return nil
}

func checkFloatRange(field string, v, min, max float64) error {
	if v < min || v > max {
		return configError(field, "value %v out of range [%v, %v]", v, min, max)
	}
	return nil
}

func parseDuration(field string, src *string, def time.Duration) (time.Duration, error) {
	if src == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*src)
	if err != nil {
		return 0, configError(field, "invalid duration %q: %v", *src, err)
	}
	return d, nil
}

func isValidHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	hh, mm := s[0:2], s[3:5]
	for _, c := range hh + mm {
		if c < '0' || c > '9' {
			return false
		}
	}
	if hh > "23" || mm > "59" {
		return false
	}
	return true
}
