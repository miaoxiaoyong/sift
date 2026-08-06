package config

import (
	"fmt"
	"regexp"
)

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

	// Runtime defaults must be resolved before agents: an omitted agent backend
	// inherits runtime.backend just like the other effective agent settings.
	if raw.Runtime != nil {
		if err := normalizeRuntime(raw.Runtime, cfg); err != nil {
			return nil, err
		}
	}
	defined, err := normalizeAgents(raw.Agents, cfg.Runtime.DefaultAgentMaxConcurrent, cfg.Runtime.Backend)
	if err != nil {
		return nil, err
	}
	cfg.Agents = defined

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
