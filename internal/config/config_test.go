package config

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/decode"
)

// tempHome returns a fresh SIFT_HOME under a temp dir, with the home directory
// created at 0700 and no config.yaml. Tests that need a file write it via
// writeConfig.
func tempHome(t *testing.T) Home {
	t.Helper()
	root := t.TempDir()
	home, err := ResolveHomeWith(func() (string, error) { return root, nil })
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	if err := os.MkdirAll(home.Path, HomeDirMode); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := os.Chmod(home.Path, HomeDirMode); err != nil {
		t.Fatalf("chmod home: %v", err)
	}
	return home
}

func writeConfig(t *testing.T, home Home, yaml string) {
	t.Helper()
	if err := os.WriteFile(ConfigPath(home), []byte(yaml), ConfigFileMode); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Chmod(ConfigPath(home), ConfigFileMode); err != nil {
		t.Fatalf("chmod config: %v", err)
	}
}

// TestV12Scenario1MissingFile: an absent config.yaml boots as a healthy idle
// daemon with empty operators/agents/projects and every documented default
// (config.md §6 scenario 1). doctor would report clean; no gh/glab/tmux/brain
// probing is implied by an empty config.
func TestV12Scenario1MissingFile(t *testing.T) {
	home := tempHome(t)
	snap, err := Load(home, time.Now())
	if err != nil {
		t.Fatalf("zero-config load: %v", err)
	}
	if snap.Source.Present {
		t.Fatal("absent file must report Source.Present=false")
	}
	cfg := snap.Config
	if len(cfg.Agents) != 0 || len(cfg.Projects) != 0 {
		t.Fatalf("zero-config must have no agents/projects, got %d/%d", len(cfg.Agents), len(cfg.Projects))
	}
	if len(cfg.Operators.GitHub) != 0 || len(cfg.Operators.GitLab) != 0 {
		t.Fatal("zero-config operators must be empty")
	}
	assertFullDefaults(t, cfg)
}

// TestV12Scenario2MinimalSchedulable: the smallest schedulable config — one
// agent's id/executable and one project's id/repo/forge — fills every optional
// value from the defaults table (config.md §6 scenario 2). Any missing default
// fails the slice.
func TestV12Scenario2MinimalSchedulable(t *testing.T) {
	home := tempHome(t)
	repo := t.TempDir() // project repo must exist for the project probe
	writeConfig(t, home, "version: 1\n"+
		"agents:\n"+
		"  - id: claude-code\n"+
		"    executable: echo\n"+
		"projects:\n"+
		"  - id: sift\n"+
		"    repo: "+repo+"\n"+
		"    forge:\n"+
		"      kind: github\n"+
		"      project: miaoxiaoyong/sift\n")

	snap, err := Load(home, time.Now())
	if err != nil {
		t.Fatalf("minimal load: %v", err)
	}
	cfg := snap.Config
	if len(cfg.Agents) != 1 || cfg.Agents[0].ID != "claude-code" {
		t.Fatalf("agent = %+v", cfg.Agents)
	}
	if cfg.Agents[0].MaxConcurrent != cfg.Runtime.DefaultAgentMaxConcurrent {
		t.Fatalf("agent max_concurrent must inherit default %d, got %d", cfg.Runtime.DefaultAgentMaxConcurrent, cfg.Agents[0].MaxConcurrent)
	}
	if cfg.Agents[0].TaskTransport != TaskTransportStdin {
		t.Fatalf("task_transport default = %v", cfg.Agents[0].TaskTransport)
	}
	if len(cfg.Agents[0].VersionArgs) != 1 || cfg.Agents[0].VersionArgs[0] != "--version" {
		t.Fatalf("version_args default = %v", cfg.Agents[0].VersionArgs)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].ID != "sift" {
		t.Fatalf("project = %+v", cfg.Projects)
	}
	if cfg.Projects[0].Forge.Host != "github.com" {
		t.Fatalf("forge.host default = %q", cfg.Projects[0].Forge.Host)
	}
	if cfg.Projects[0].Forge.CLI != "gh" {
		t.Fatalf("forge.cli default = %q", cfg.Projects[0].Forge.CLI)
	}
	if !cfg.Projects[0].Enabled {
		t.Fatal("project enabled default must be true")
	}
	// Empty project agents list means all defined agents.
	if len(cfg.Projects[0].Agents) != 1 || cfg.Projects[0].Agents[0] != "claude-code" {
		t.Fatalf("project agents default = %v", cfg.Projects[0].Agents)
	}
	// Every other section still carries its full default set.
	assertFullDefaultsExceptAgentProject(t, cfg)
}

// assertFullDefaults pins the complete §3 default table for a zero-config
// snapshot. This is the heart of V12: a missing default is a slice failure.
func assertFullDefaults(t *testing.T, cfg *Config) {
	t.Helper()
	assertFullDefaultsExceptAgentProject(t, cfg)
	if cfg.Version != Version {
		t.Fatalf("version = %d", cfg.Version)
	}
}

func assertFullDefaultsExceptAgentProject(t *testing.T, cfg *Config) {
	t.Helper()
	want := DefaultConfig()
	type chk struct {
		name string
		got  any
		want any
	}
	// Compare the sections that are independent of agents/projects. Durations
	// and floats compare by value; slices by DeepEqual.
	checks := []chk{
		{"brain.protocol", cfg.Brain.Protocol, want.Brain.Protocol},
		{"brain.daily_token_limit", cfg.Brain.DailyTokenLimit, want.Brain.DailyTokenLimit},
		{"brain.call_timeout", cfg.Brain.CallTimeout, want.Brain.CallTimeout},
		{"brain.schema_retries", cfg.Brain.SchemaRetries, want.Brain.SchemaRetries},
		{"brain.max_input_bytes", cfg.Brain.MaxInputBytes, want.Brain.MaxInputBytes},
		{"brain.max_raw_output_bytes", cfg.Brain.MaxRawOutputBytes, want.Brain.MaxRawOutputBytes},
		{"brain.version_args", cfg.Brain.VersionArgs, want.Brain.VersionArgs},
		{"scheduler.intake_idle_interval", cfg.Scheduler.IntakeIdleInterval, want.Scheduler.IntakeIdleInterval},
		{"scheduler.intake_active_interval", cfg.Scheduler.IntakeActiveInterval, want.Scheduler.IntakeActiveInterval},
		{"scheduler.intake_interrupt_interval", cfg.Scheduler.IntakeInterruptInterval, want.Scheduler.IntakeInterruptInterval},
		{"scheduler.intake_interrupt_burst", cfg.Scheduler.IntakeInterruptBurst, want.Scheduler.IntakeInterruptBurst},
		{"scheduler.supervisor_interval", cfg.Scheduler.SupervisorInterval, want.Scheduler.SupervisorInterval},
		{"scheduler.config_drift_check_interval", cfg.Scheduler.ConfigDriftCheckInterval, want.Scheduler.ConfigDriftCheckInterval},
		{"scheduler.per_class_tick_limit", cfg.Scheduler.PerClassTickLimit, want.Scheduler.PerClassTickLimit},
		{"runtime.max_concurrent_total", cfg.Runtime.MaxConcurrentTotal, want.Runtime.MaxConcurrentTotal},
		{"runtime.default_agent_max_concurrent", cfg.Runtime.DefaultAgentMaxConcurrent, want.Runtime.DefaultAgentMaxConcurrent},
		{"runtime.max_attempts", cfg.Runtime.MaxAttempts, want.Runtime.MaxAttempts},
		{"runtime.attempt_timeout", cfg.Runtime.AttemptTimeout, want.Runtime.AttemptTimeout},
		{"runtime.agent_silence_timeout", cfg.Runtime.AgentSilenceTimeout, want.Runtime.AgentSilenceTimeout},
		{"runtime.retry_initial_delay", cfg.Runtime.RetryInitialDelay, want.Runtime.RetryInitialDelay},
		{"runtime.retry_max_delay", cfg.Runtime.RetryMaxDelay, want.Runtime.RetryMaxDelay},
		{"runtime.retry_multiplier", cfg.Runtime.RetryMultiplier, want.Runtime.RetryMultiplier},
		{"runtime.spawn_operation_lease_ttl", cfg.Runtime.SpawnOperationLeaseTTL, want.Runtime.SpawnOperationLeaseTTL},
		{"runtime.starting_permit_timeout", cfg.Runtime.StartingPermitTimeout, want.Runtime.StartingPermitTimeout},
		{"runtime.spawning_started_timeout", cfg.Runtime.SpawningStartedTimeout, want.Runtime.SpawningStartedTimeout},
		{"runtime.heartbeat_interval", cfg.Runtime.HeartbeatInterval, want.Runtime.HeartbeatInterval},
		{"runtime.heartbeat_stale_after", cfg.Runtime.HeartbeatStaleAfter, want.Runtime.HeartbeatStaleAfter},
		{"runtime.termination_term_grace", cfg.Runtime.TerminationTermGrace, want.Runtime.TerminationTermGrace},
		{"runtime.termination_kill_grace", cfg.Runtime.TerminationKillGrace, want.Runtime.TerminationKillGrace},
		{"runtime.absence_recheck_count", cfg.Runtime.AbsenceRecheckCount, want.Runtime.AbsenceRecheckCount},
		{"runtime.absence_recheck_interval", cfg.Runtime.AbsenceRecheckInterval, want.Runtime.AbsenceRecheckInterval},
		{"outbox.lease_ttl", cfg.Outbox.LeaseTTL, want.Outbox.LeaseTTL},
		{"outbox.retry_initial_delay", cfg.Outbox.RetryInitialDelay, want.Outbox.RetryInitialDelay},
		{"outbox.retry_max_delay", cfg.Outbox.RetryMaxDelay, want.Outbox.RetryMaxDelay},
		{"outbox.retry_multiplier", cfg.Outbox.RetryMultiplier, want.Outbox.RetryMultiplier},
		{"outbox.max_attempts", cfg.Outbox.MaxAttempts, want.Outbox.MaxAttempts},
		{"outbox.max_advance_delay", cfg.Outbox.MaxAdvanceDelay, want.Outbox.MaxAdvanceDelay},
		{"outbox.worker_batch_size", cfg.Outbox.WorkerBatchSize, want.Outbox.WorkerBatchSize},
		{"forge.hourly_api_limit", cfg.Forge.HourlyAPILimit, want.Forge.HourlyAPILimit},
		{"forge.warning_ratio", cfg.Forge.WarningRatio, want.Forge.WarningRatio},
		{"forge.slow_poll_interval", cfg.Forge.SlowPollInterval, want.Forge.SlowPollInterval},
		{"forge.command_timeout", cfg.Forge.CommandTimeout, want.Forge.CommandTimeout},
		{"attention.day_timezone", cfg.Attention.DayTimezone, want.Attention.DayTimezone},
		{"attention.daily_quota", cfg.Attention.DailyQuota, want.Attention.DailyQuota},
		{"attention.max_escalations", cfg.Attention.MaxEscalations, want.Attention.MaxEscalations},
		{"attention.critical_fuse", cfg.Attention.CriticalFuse, want.Attention.CriticalFuse},
		{"attention.daily_summary_at", cfg.Attention.DailySummaryAt, want.Attention.DailySummaryAt},
		{"attention.hold_max_duration", cfg.Attention.HoldMaxDuration, want.Attention.HoldMaxDuration},
		{"attention.channel_failure_alert_after", cfg.Attention.ChannelFailureAlertAfter, want.Attention.ChannelFailureAlertAfter},
		{"report.events_per_minute", cfg.Report.EventsPerMinute, want.Report.EventsPerMinute},
		{"report.burst", cfg.Report.Burst, want.Report.Burst},
		{"report.dedupe_window", cfg.Report.DedupeWindow, want.Report.DedupeWindow},
		{"report.max_payload_bytes", cfg.Report.MaxPayloadBytes, want.Report.MaxPayloadBytes},
		{"report.not_ready_initial_delay", cfg.Report.NotReadyInitialDelay, want.Report.NotReadyInitialDelay},
		{"report.not_ready_max_delay", cfg.Report.NotReadyMaxDelay, want.Report.NotReadyMaxDelay},
		{"report.not_ready_total_timeout", cfg.Report.NotReadyTotalTimeout, want.Report.NotReadyTotalTimeout},
		{"report.interrupts_per_run_daily_quota", cfg.Report.InterruptsPerRunDailyQuota, want.Report.InterruptsPerRunDailyQuota},
		{"report.on_interrupt_quota_exceeded", cfg.Report.OnInterruptQuotaExceeded, want.Report.OnInterruptQuotaExceeded},
		{"gate_defaults.review_policy", cfg.GateDefaults.ReviewPolicy, want.GateDefaults.ReviewPolicy},
		{"gate_defaults.risky_review_threshold", cfg.GateDefaults.RiskyReviewThreshold, want.GateDefaults.RiskyReviewThreshold},
		{"gate_defaults.auto_merge", cfg.GateDefaults.AutoMerge, want.GateDefaults.AutoMerge},
		{"gate_defaults.checks_pending_timeout", cfg.GateDefaults.ChecksPendingTimeout, want.GateDefaults.ChecksPendingTimeout},
		{"gate_defaults.flaky_retry_limit", cfg.GateDefaults.FlakyRetryLimit, want.GateDefaults.FlakyRetryLimit},
		{"certification", cfg.Certification, want.Certification},
		{"metrics", cfg.Metrics, want.Metrics},
		{"labels", cfg.Labels, want.Labels},
		{"logging", cfg.Logging, want.Logging},
	}
	for _, c := range checks {
		if !deepEqual(c.got, c.want) {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func deepEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// TestClosedContractRejectsUnknownField: config.md §1.1 — the closed contract
// rejects unknown fields. V14-style for the config type.
func TestClosedContractRejectsUnknownField(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "version: 1\nbogus_field: 7\n")
	if _, err := Load(home, time.Now()); err == nil {
		t.Fatal("unknown top-level field must be rejected")
	}
}

func TestClosedContractRejectsUnknownNestedField(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "version: 1\nruntime:\n  nope: 1\n")
	_, err := Load(home, time.Now())
	if err == nil {
		t.Fatal("unknown nested field must be rejected")
	}
	var de *decode.DecodeError
	if !errors.As(err, &de) || de.Kind != decode.KindUnknownField {
		t.Fatalf("expected unknown_field, got %v", err)
	}
}

func TestClosedContractVersionRequiredWhenFilePresent(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "agents: []\n") // no version
	_, err := Load(home, time.Now())
	if err == nil {
		t.Fatal("file present without version must be rejected")
	}
}

func TestClosedContractRejectsBadVersion(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "version: 2\n")
	if _, err := Load(home, time.Now()); err == nil {
		t.Fatal("version != 1 must be rejected")
	}
}

func TestClosedContractRejectsBadEnum(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "version: 1\nagents:\n  - id: a\n    executable: e\n    task_transport: carrier-pigeon\n")
	_, err := Load(home, time.Now())
	if err == nil {
		t.Fatal("bad task_transport enum must be rejected")
	}
}

// TestEmptyPresentFileErrors: an empty (but present) file is distinct from an
// absent file; it errors clearly rather than being silently treated as defaults.
func TestEmptyPresentFileErrors(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "")
	_, err := Load(home, time.Now())
	if !errors.Is(err, ErrEmptyConfigFile) {
		t.Fatalf("expected ErrEmptyConfigFile, got %v", err)
	}
}

// TestCanonicalJSONSortedKeys: the fingerprint's canonical JSON has object keys
// in lexicographic order at every level (config.md §4 step 6).
func TestCanonicalJSONSortedKeys(t *testing.T) {
	home := tempHome(t)
	writeConfig(t, home, "version: 1\nruntime:\n  max_attempts: 5\n")
	snap, err := Load(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := string(snap.CanonicalJSON)
	ai := strings.Index(body, `"agents"`)
	vi := strings.Index(body, `"version"`)
	if ai < 0 || vi < 0 {
		t.Fatalf("canonical JSON missing agents/version keys: %s", body)
	}
	if ai >= vi {
		t.Fatalf("canonical keys not sorted: agents@%d must precede version@%d in %s", ai, vi, body)
	}
	// Recompute must be byte-identical (deterministic).
	again, _, err := Fingerprint(snap.Config)
	if err != nil || again != snap.Hash {
		t.Fatalf("fingerprint not deterministic: %v vs %v (%v)", again, snap.Hash, err)
	}
}

// TestFingerprintIgnoresDurationFormatting: "30s" and "0.5m" encode the same
// duration and must produce the same fingerprint after normalization.
func TestFingerprintIgnoresDurationFormatting(t *testing.T) {
	a := mustLoadYAML(t, "version: 1\nscheduler:\n  supervisor_interval: 30s\n")
	b := mustLoadYAML(t, "version: 1\nscheduler:\n  supervisor_interval: 0.5m\n")
	if a.Hash != b.Hash {
		t.Fatalf("duration formatting changed fingerprint: %s vs %s", a.Hash, b.Hash)
	}
	if a.Config.Scheduler.SupervisorInterval != b.Config.Scheduler.SupervisorInterval {
		t.Fatalf("durations differ: %v vs %v", a.Config.Scheduler.SupervisorInterval, b.Config.Scheduler.SupervisorInterval)
	}
}

func mustLoadYAML(t *testing.T, yaml string) *Snapshot {
	t.Helper()
	home := tempHome(t)
	writeConfig(t, home, yaml)
	snap, err := Load(home, time.Now())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return snap
}
