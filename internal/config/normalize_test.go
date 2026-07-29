package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Agent definition schema and multi-agent validation (config.md §3.2, WBS §1.4
// "至少两个 Agent 配置的校验能力").

func TestNormalizeTwoAgentsUniqueness(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "duplicate_id_rejected",
			yaml:    twoAgentYAML("claude-code", "claude-code"),
			wantErr: "duplicate id",
		},
		{
			name:    "two_distinct_agents_ok",
			yaml:    twoAgentYAML("claude-code", "codex"),
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := mustLoadYAMLOrErr(t, "version: 1\n"+tc.yaml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected accept, got %v", err)
				}
				if len(snap.Config.Agents) != 2 {
					t.Fatalf("expected 2 agents, got %d", len(snap.Config.Agents))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func twoAgentYAML(id1, id2 string) string {
	return "agents:\n" +
		"  - id: " + id1 + "\n    executable: echo\n" +
		"  - id: " + id2 + "\n    executable: cat\n"
}

func TestAgentIDGrammar(t *testing.T) {
	bad := []string{"Claude", "1agent", "agent_underscore", "-leading", strings.Repeat("a", 64)}
	for _, id := range bad {
		yaml := "version: 1\nagents:\n  - id: " + id + "\n    executable: e\n"
		if _, err := mustLoadYAMLOrErr(t, yaml); err == nil {
			t.Errorf("agent id %q must be rejected", id)
		}
	}
	good := []string{"a", "claude-code", "codex1", strings.Repeat("a", 63)}
	for _, id := range good {
		yaml := "version: 1\nagents:\n  - id: " + id + "\n    executable: e\n"
		if _, err := mustLoadYAMLOrErr(t, yaml); err != nil {
			t.Errorf("agent id %q must be accepted, got %v", id, err)
		}
	}
}

func TestAgentMaxConcurrentInheritsDefault(t *testing.T) {
	yaml := "version: 1\n" +
		"runtime:\n  default_agent_max_concurrent: 4\n" +
		"agents:\n  - id: a\n    executable: e\n" + // omits max_concurrent
		"  - id: b\n    executable: e\n    max_concurrent: 2\n"
	snap, err := mustLoadYAMLOrErr(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Config.Agents[0].MaxConcurrent != 4 {
		t.Fatalf("agent a must inherit 4, got %d", snap.Config.Agents[0].MaxConcurrent)
	}
	if snap.Config.Agents[1].MaxConcurrent != 2 {
		t.Fatalf("agent b must keep 2, got %d", snap.Config.Agents[1].MaxConcurrent)
	}
}

func TestAgentMaxConcurrentRange(t *testing.T) {
	for _, v := range []int{0, 33} {
		yaml := "version: 1\nagents:\n  - id: a\n    executable: e\n    max_concurrent: " + itoa(v) + "\n"
		if _, err := mustLoadYAMLOrErr(t, yaml); err == nil {
			t.Errorf("max_concurrent %d must be rejected", v)
		}
	}
}

func TestTaskFilePlaceholderRules(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"stdin_forbids_placeholder", "version: 1\nagents:\n  - id: a\n    executable: e\n    task_transport: stdin\n    args: [\"{task_file}\"]\n", true},
		{"file_requires_placeholder", "version: 1\nagents:\n  - id: a\n    executable: e\n    task_transport: file\n", true},
		{"file_one_placeholder_ok", "version: 1\nagents:\n  - id: a\n    executable: e\n    task_transport: file\n    args: [\"{task_file}\"]\n", false},
		{"substring_placeholder_rejected", "version: 1\nagents:\n  - id: a\n    executable: e\n    args: [\"x{task_file}y\"]\n", true},
		{"nul_byte_rejected", "version: 1\nagents:\n  - id: a\n    executable: e\n    args: [\"a\\u0000b\"]\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mustLoadYAMLOrErr(t, tc.yaml)
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
		})
	}
}

func TestProjectValidation(t *testing.T) {
	repo := t.TempDir()
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"relative_repo_rejected", "version: 1\nprojects:\n  - id: p\n    repo: relative/path\n    forge: {kind: github, project: o/r}\n", "absolute"},
		{"unknown_agent_ref_rejected", "version: 1\nagents:\n  - id: a\n    executable: e\nprojects:\n  - id: p\n    repo: " + repo + "\n    forge: {kind: github, project: o/r}\n    agents: [ghost]\n", "unknown agent"},
		{"duplicate_enabled_repo_rejected", "version: 1\nprojects:\n  - id: p1\n    repo: " + repo + "\n    forge: {kind: github, project: o/r}\n  - id: p2\n    repo: " + repo + "\n    forge: {kind: github, project: o/r2}\n", "duplicate"},
		{"disabled_duplicate_repo_ok", "version: 1\nprojects:\n  - id: p1\n    repo: " + repo + "\n    forge: {kind: github, project: o/r}\n    enabled: false\n  - id: p2\n    repo: " + repo + "\n    forge: {kind: github, project: o/r2}\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mustLoadYAMLOrErr(t, tc.yaml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected accept, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRangeValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"brain_call_timeout_too_long", "version: 1\nbrain:\n  call_timeout: 31m\n"},
		{"brain_daily_token_limit_bad", "version: 1\nbrain:\n  daily_token_limit: 500\n"},
		{"brain_schema_retries_not_one", "version: 1\nbrain:\n  schema_retries: 2\n"},
		{"runtime_retry_multiplier_too_big", "version: 1\nruntime:\n  retry_multiplier: 11\n"},
		{"runtime_heartbeat_stale_lt_interval", "version: 1\nruntime:\n  heartbeat_interval: 10s\n  heartbeat_stale_after: 5s\n"},
		{"outbox_max_attempts_too_big", "version: 1\noutbox:\n  max_attempts: 1001\n"},
		{"forge_warning_ratio_one", "version: 1\nforge:\n  warning_ratio: 1.0\n"},
		{"certification_negative_gt_total", "version: 1\ncertification:\n  total_samples_min: 10\n  negative_samples_min: 20\n"},
		{"forge_slow_poll_below_active", "version: 1\nforge:\n  slow_poll_interval: 5s\n"}, // active default is 15s
		{"report_quota_zero", "version: 1\nreport:\n  interrupts_per_run_daily_quota: 0\n"},
		{"logging_retained_zero", "version: 1\nlogging:\n  retained_files: 0\n"},
		{"metrics_negative", "version: 1\nmetrics:\n  code_review: -1\n"},
		{"attention_bad_tz", "version: 1\nattention:\n  day_timezone: Not/A/Zone\n"},
		{"attention_daily_summary_bad", "version: 1\nattention:\n  daily_summary_at: 25:00\n"},
		{"gate_review_policy_bad", "version: 1\ngate_defaults:\n  review_policy: sometimes\n"},
		{"gate_risky_review_threshold_bad", "version: 1\ngate_defaults:\n  risky_review_threshold: 101\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mustLoadYAMLOrErr(t, tc.yaml); err == nil {
				t.Fatalf("expected range error, got nil")
			}
		})
	}
}

func TestRangeAcceptanceBoundaries(t *testing.T) {
	// Boundary values that must be accepted (min/max inclusive).
	cases := []string{
		"version: 1\noutbox:\n  max_attempts: 0\n",          // 0 = retry forever
		"version: 1\nruntime:\n  retry_initial_delay: 0s\n", // 0s allowed
		"version: 1\nforge:\n  warning_ratio: 0.0001\n",
		"version: 1\ncertification:\n  leak_rate_max: 0.0\n",
		"version: 1\nreport:\n  dedupe_window: 0s\n",
		"version: 1\nmetrics:\n  code_review: 0\n", // non-negative incl 0
		"version: 1\ngate_defaults:\n  risky_review_threshold: 100\n",
		"version: 1\nforge:\n  slow_poll_interval: 10m\n", // >= active interval ok
	}
	for _, yaml := range cases {
		if _, err := mustLoadYAMLOrErr(t, yaml); err != nil {
			t.Fatalf("expected accept for boundary, got %v: %s", err, yaml)
		}
	}
}

func TestOperatorsDedupSorted(t *testing.T) {
	yaml := "version: 1\noperators:\n  github: [bob, alice, bob]\n  gitlab: [zed, amy]\n"
	snap, err := mustLoadYAMLOrErr(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "bob"}
	if !deepEqual(snap.Config.Operators.GitHub, want) {
		t.Fatalf("github = %v, want %v", snap.Config.Operators.GitHub, want)
	}
	if !deepEqual(snap.Config.Operators.GitLab, []string{"amy", "zed"}) {
		t.Fatalf("gitlab = %v", snap.Config.Operators.GitLab)
	}
}

func TestOperatorsEmptyStringRejected(t *testing.T) {
	yaml := "version: 1\noperators:\n  github: [\"\", bob]\n"
	if _, err := mustLoadYAMLOrErr(t, yaml); err == nil {
		t.Fatal("empty operator login must be rejected")
	}
}

func mustLoadYAMLOrErr(t *testing.T, yaml string) (*Snapshot, error) {
	t.Helper()
	home := tempHome(t)
	writeConfig(t, home, yaml)
	return Load(home, time.Now())
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
