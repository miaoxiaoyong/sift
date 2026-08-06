package config

import (
	"time"
)

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
