package config

import (
	"time"
)

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
