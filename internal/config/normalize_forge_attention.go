package config

import (
	"sort"
	"strings"
	"time"
)

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
	if raw.Channels != nil {
		seen := map[string]bool{}
		defaults := 0
		channels := make([]AttentionChannel, 0, len(*raw.Channels))
		for _, c := range *raw.Channels {
			if !agentIDRe.MatchString(c.ID) || seen[c.ID] || c.Type != "webhook" || c.Target.SecretRef == "" || strings.ContainsAny(c.Target.SecretRef, "\r\n\x00") || len(c.Capabilities) == 0 || (c.Renderer != "" && c.Renderer != "plain-v1") {
				return configError("attention.channels", "channels must have a valid unique id, webhook type, secret_ref, plain-v1 renderer and capabilities")
			}
			seen[c.ID] = true
			if c.Default {
				defaults++
			}
			caps := append([]string(nil), c.Capabilities...)
			sort.Strings(caps)
			for i, capability := range caps {
				if capability != "voice" && capability != "text" && capability != "visual" {
					return configError("attention.channels", "channel capabilities must use the closed enum")
				}
				if i > 0 && capability == caps[i-1] {
					return configError("attention.channels", "channel capabilities must be unique")
				}
			}
			enabled := true
			if c.Enabled != nil {
				enabled = *c.Enabled
			}
			renderer := c.Renderer
			if renderer == "" {
				renderer = "plain-v1"
			}
			channels = append(channels, AttentionChannel{ID: c.ID, Enabled: enabled, Renderer: renderer, Type: c.Type, TargetRef: "secret_ref:" + c.Target.SecretRef, Capabilities: caps, Default: c.Default})
		}
		if defaults > 1 {
			return configError("attention.channels", "at most one default channel")
		}
		cfg.Attention.Channels = channels
	}
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
	if raw.ReasonDefaults != nil {
		allowed := map[string]bool{"design_approval": true, "guardrail_violation": true, "code_review": true, "agent_blocked": true, "merge_conflict": true, "failure_review": true, "startup_stall": true}
		for reason, value := range raw.ReasonDefaults {
			if !allowed[reason] {
				return configError("attention.reason_defaults", "unknown reason %q", reason)
			}
			d := cfg.Attention.ReasonDefaults[reason]
			if value.ExpiresAfter != nil {
				var err error
				var duration time.Duration
				duration, err = parseDuration("attention.reason_defaults."+reason+".expires_after", value.ExpiresAfter, time.Minute)
				if err != nil {
					return err
				}
				if duration < time.Minute || duration > 8760*time.Hour {
					return configError("attention.reason_defaults."+reason+".expires_after", "duration must be 1m..8760h")
				}
				d.ExpiresAfterMS = duration.Milliseconds()
			}
			if value.OnExpire != nil {
				d.OnExpire = *value.OnExpire
			}
			if value.OnMaxEscalations != nil {
				d.OnMaxEscalations = *value.OnMaxEscalations
			}
			if d.OnExpire != "hold" && d.OnExpire != "escalate" && d.OnExpire != "auto_reject" {
				return configError("attention.reason_defaults."+reason+".on_expire", "invalid action")
			}
			if d.OnMaxEscalations != "hold" && d.OnMaxEscalations != "auto_reject" {
				return configError("attention.reason_defaults."+reason+".on_max_escalations", "invalid action")
			}
			if reason == "startup_stall" && (d.OnExpire == "auto_reject" || d.OnMaxEscalations == "auto_reject") {
				return configError("attention.reason_defaults.startup_stall", "auto_reject is forbidden")
			}
			cfg.Attention.ReasonDefaults[reason] = d
		}
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
