package brain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/storage"
)

const (
	t7WindowStart = int64(100)
	t7WindowEnd   = int64(200)
)

func seedT7SchedulerDB(t *testing.T, db *storage.DB) string {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-t7", "project/alpha", t7WindowStart); err != nil {
		t.Fatal(err)
	}
	version := strings.Repeat("a", 64)
	for i, kind := range []string{"bug", "docs"} {
		v := version
		if i == 1 {
			v = strings.Repeat("d", 64)
		}
		if _, err := db.ExecForTest(ctx, `INSERT INTO certifications(task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms) VALUES(?,?,10,2,1,1,1,?,?,?,?,?)`, kind, v, strings.Repeat("b", 64), t7WindowEnd, strings.Repeat("c", 64), t7WindowStart, t7WindowEnd); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecForTest(ctx, `INSERT INTO certification_current(task_kind,certification_version,version,updated_at_ms) VALUES(?,?,1,?)`, kind, v, t7WindowEnd); err != nil {
			t.Fatal(err)
		}
	}
	return "t7:certification:bug:" + version
}

func appendT7Fixture(t *testing.T, db *storage.DB, scope, projectID, kind string) {
	t.Helper()
	_, err := db.AppendT7ReplayEvidence(context.Background(), storage.AppendT7ReplayEvidenceCmd{
		Scope: scope, ProjectID: projectID, TaskKind: kind,
		WindowStartMS: t7WindowStart, WindowEndMS: t7WindowEnd,
		DatasetVersion: "dataset/v1", GateVersion: "gate/v1",
		TotalSamples: 10, NegativeSamples: 2, LeakCount: 1, FalseBlockCount: 1,
		CreatedAtMS: t7WindowEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func t7ProviderOutput(scope, kind, evidenceID string) string {
	return `{"proposal_kind":"` + kind + `","target_scope":"` + scope + `","title":"Review aggregate evidence","body":"Inert draft for human review.","evidence_entry_ids":["` + evidenceID + `"],"requires_human_approval":true}`
}

func TestT7SchedulerGlobalProjectConcreteAllAndOnce(t *testing.T) {
	db := openShellDB(t)
	evidenceID := seedT7SchedulerDB(t, db)
	for _, fixture := range []struct{ scope, project, kind string }{
		{"global", "", "bug"}, {"global", "", "all"},
		{"project", "project/alpha", "bug"}, {"project", "project/alpha", "all"},
	} {
		appendT7Fixture(t, db, fixture.scope, fixture.project, fixture.kind)
	}

	fake := &FakeProvider{Responses: []FakeResponse{
		{ResultText: t7ProviderOutput("global", "policy", evidenceID), InputTokens: 1, OutputTokens: 1},
		{ResultText: t7ProviderOutput("global", "context", evidenceID), InputTokens: 1, OutputTokens: 1},
		{ResultText: t7ProviderOutput("project", "policy", evidenceID), InputTokens: 1, OutputTokens: 1},
		{ResultText: t7ProviderOutput("project", "context", evidenceID), InputTokens: 1, OutputTokens: 1},
	}}
	shell := NewShell(db, shellCfg(1000), fake, func() time.Time { return time.UnixMilli(300) })
	scheduler := &T7Scheduler{DB: db, Shell: shell, Now: func() time.Time { return time.UnixMilli(300) }, Limit: 10}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests) != 4 {
		t.Fatalf("provider calls=%d, want 4", len(fake.Requests))
	}
	assertT7TraceIdentitiesAndDrafts(t, db, 4)

	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests) != 4 {
		t.Fatalf("completed windows called provider again: %d", len(fake.Requests))
	}
	pending, err := db.PendingT7Aggregates(context.Background(), 300, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
}

func assertT7TraceIdentitiesAndDrafts(t *testing.T, db *storage.DB, want int) {
	t.Helper()
	var trace strings.Builder
	if err := db.ExportBrainCallsJSONL(context.Background(), &trace); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(trace.String()), "\n")
	if len(lines) != want {
		t.Fatalf("trace records=%d, want %d", len(lines), want)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		key, _ := record["subject_key"].(string)
		if record["touchpoint"] != "T7" || record["scope"] != storage.BrainScopeAggregate || !strings.HasPrefix(key, "aggregate:v1:") || record["run_id"] != nil || record["attempt_no"] != nil {
			t.Fatalf("invalid T7 trace identity: %v", record)
		}
		if seen[key] {
			t.Fatalf("duplicate aggregate call: %s", key)
		}
		seen[key] = true
		callID := record["record_id"].(string)
		draft, err := db.ProposalDraft(context.Background(), callID)
		if err != nil || draft.Status != "pending_human_approval" {
			t.Fatalf("draft call=%s status=%s err=%v", callID, draft.Status, err)
		}
	}
	if want == 4 {
		for _, key := range []string{
			"aggregate:v1:global:all:100:200", "aggregate:v1:global:bug:100:200",
			"aggregate:v1:project:cHJvamVjdC9hbHBoYQ:all:100:200", "aggregate:v1:project:cHJvamVjdC9hbHBoYQ:bug:100:200",
		} {
			if !seen[key] {
				t.Errorf("missing aggregate identity %s", key)
			}
		}
	}
}

func TestT7SchedulerFallbacksTraceWithoutDraft(t *testing.T) {
	cases := []struct {
		name  string
		setup func() configCase
	}{
		{"provider_disabled", func() configCase {
			cfg := shellCfg(100)
			cfg.Executable = ""
			return configCase{cfg: cfg, provider: &FakeProvider{}}
		}},
		{"input_too_large", func() configCase {
			cfg := shellCfg(100)
			cfg.MaxInputBytes = 1
			return configCase{cfg: cfg, provider: &FakeProvider{}}
		}},
		{"invalid_output", func() configCase {
			return configCase{cfg: shellCfg(100), provider: &FakeProvider{Responses: []FakeResponse{{ResultText: `{}`}, {ResultText: `{}`}}}}
		}},
		{"provider_error", func() configCase {
			return configCase{cfg: shellCfg(100), provider: &FakeProvider{Responses: []FakeResponse{{SpawnErr: true}, {SpawnErr: true}}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openShellDB(t)
			seedT7SchedulerDB(t, db)
			appendT7Fixture(t, db, "global", "", "bug")
			setup := tc.setup()
			shell := NewShell(db, setup.cfg, setup.provider, func() time.Time { return time.UnixMilli(300) })
			scheduler := &T7Scheduler{DB: db, Shell: shell, Now: func() time.Time { return time.UnixMilli(300) }}
			if err := scheduler.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertT7FallbackTrace(t, db, tc.name)
		})
	}
}

type configCase struct {
	cfg      config.Brain
	provider Provider
}

func assertT7FallbackTrace(t *testing.T, db *storage.DB, reason string) {
	t.Helper()
	var trace strings.Builder
	if err := db.ExportBrainCallsJSONL(context.Background(), &trace); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(trace.String())), &record); err != nil {
		t.Fatal(err)
	}
	if record["status"] != storage.BrainCallFallback || !strings.Contains(record["fallback_reason"].(string), reason) {
		t.Fatalf("trace status=%v fallback=%v", record["status"], record["fallback_reason"])
	}
	if _, err := db.ProposalDraft(context.Background(), record["record_id"].(string)); err == nil {
		t.Fatal("fallback created proposal draft")
	}
}
