package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

func TestEmptyDBDaemonTickPersistsForgeIntakeAndT1(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Version:   1,
		Projects:  []config.Project{{ID: "project-1", Enabled: true, Forge: config.ForgeRef{Kind: config.ForgeKindGitHub, Host: "github.com", Project: "acme/widgets", CLI: "gh"}}},
		Brain:     config.Brain{CallTimeout: time.Second},
		Forge:     config.Forge{HourlyAPILimit: 10, WarningRatio: .8, SlowPollInterval: time.Minute},
		Outbox:    config.Outbox{LeaseTTL: time.Minute},
		Scheduler: config.Scheduler{IntakeIdleInterval: time.Minute, IntakeActiveInterval: time.Second},
		Labels:    config.Labels{Trigger: "sift"},
		Operators: config.Operators{GitHub: []string{"operator"}},
	}
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := &config.Snapshot{Config: cfg, Hash: "empty-db-tick", CanonicalJSON: []byte(`{"version":1}`)}
	if err := db.ActivateConfig(ctx, snapshot, "test", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	runner := func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) < 2 {
			return nil, nil, fmt.Errorf("fixture received incomplete args: %v", args)
		}
		path := args[1]
		switch {
		case strings.HasPrefix(path, "/repos/acme/widgets/issues?labels="):
			return []byte(`[{"number":42,"title":"Fix it","body":"details","html_url":"https://github.com/acme/widgets/issues/42","state":"open","user":{"login":"contributor"},"labels":[{"name":"sift"}],"updated_at":"2026-07-29T11:59:00Z"}]`), nil, nil
		case path == "/repos/acme/widgets/issues/42":
			return []byte(`{"number":42,"title":"Fix it","body":"details","html_url":"https://github.com/acme/widgets/issues/42","state":"open","user":{"login":"contributor"},"updated_at":"2026-07-29T11:59:00Z"}`), nil, nil
		case strings.HasPrefix(path, "/repos/acme/widgets/issues/42/timeline"):
			return []byte(`[{"id":7,"label":{"name":"sift"},"actor":{"login":"operator"},"event":"labeled","created_at":"2026-07-29T11:58:00Z"}]`), nil, nil
		default:
			return nil, nil, fmt.Errorf("unexpected fixture path %q", path)
		}
	}
	d, err := AssembleWithRunner(db, cfg, func() time.Time { return now }, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	status, err := db.ForgeAPIBudgetStatus(ctx, "project-1", now.UnixMilli(), 10, .8)
	if err != nil || status.Consumed == 0 {
		t.Fatalf("forge budget = %+v, err=%v; want persisted charge", status, err)
	}
	cur, err := db.IntakeCursor(ctx, "project-1", "issues")
	if err != nil || cur.Cursor == "" {
		t.Fatalf("intake cursor = %+v, err=%v; want persisted receipt cursor", cur, err)
	}

	checkDB, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()
	var receipts, items, calls, runs int
	for _, q := range []struct {
		name, query string
		dest        *int
	}{
		{"receipt", `SELECT COUNT(*) FROM forge_event_receipts WHERE project_id='project-1'`, &receipts},
		{"intake", `SELECT COUNT(*) FROM intake_items WHERE project_id='project-1'`, &items},
		{"t1", `SELECT COUNT(*) FROM brain_calls WHERE project_id='project-1' AND touchpoint='T1'`, &calls},
		{"run", `SELECT COUNT(*) FROM runs WHERE project_id='project-1' AND status='queued'`, &runs},
	} {
		if err := checkDB.QueryRow(q.query).Scan(q.dest); err != nil {
			t.Fatalf("%s query: %v", q.name, err)
		}
	}
	if receipts != 1 || items != 1 || calls != 1 || runs != 1 {
		t.Fatalf("persisted counts: receipts=%d intake=%d t1=%d queued_runs=%d", receipts, items, calls, runs)
	}
}

func TestAssembleWiresIntakeT1ReconcilerCommentsAndBudget(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1000)
	db, err := storage.Open(ctx, storage.OpenConfig{
		Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, projectID := range []string{"github-project", "gitlab-project"} {
		if err := db.SeedProjectForTest(ctx, "cfg-"+projectID, projectID, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		Projects: []config.Project{
			{ID: "github-project", Enabled: true, Forge: config.ForgeRef{Kind: config.ForgeKind("github"), Host: "github.example", Project: "acme/widgets", CLI: "unused"}},
			{ID: "gitlab-project", Enabled: true, Forge: config.ForgeRef{Kind: config.ForgeKind("gitlab"), Host: "gitlab.example", Project: "acme/widgets", CLI: "unused"}},
		},
		Brain:     config.Brain{CallTimeout: time.Second},
		Forge:     config.Forge{HourlyAPILimit: 10, WarningRatio: .8, SlowPollInterval: time.Minute},
		Outbox:    config.Outbox{LeaseTTL: time.Minute},
		Scheduler: config.Scheduler{IntakeIdleInterval: time.Minute, IntakeActiveInterval: time.Second},
		Labels:    config.Labels{Trigger: "sift"},
	}
	workers, err := Assemble(db, cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if len(workers.Pollers) != 2 || len(workers.Evaluators) != 2 || len(workers.Reconcilers) != 2 || len(workers.Comments) != 2 || len(workers.Merges) != 2 {
		t.Fatalf("assembly counts: pollers=%d evaluators=%d reconcilers=%d comments=%d merges=%d", len(workers.Pollers), len(workers.Evaluators), len(workers.Reconcilers), len(workers.Comments), len(workers.Merges))
	}
	for i, p := range workers.Pollers {
		if p.OnIssue == nil {
			t.Fatalf("poller %d has no T1 callback", i)
		}
		if len(p.Projects) != 1 {
			t.Fatalf("poller %d project scope=%d, want 1", i, len(p.Projects))
		}
		client, ok := p.Forge.(*forge.Adapter)
		if !ok {
			t.Fatalf("poller %d forge=%T, want production adapter", i, p.Forge)
		}
		if client.Kind != forge.Kind(p.Projects[0].Ref.Kind) {
			t.Fatalf("poller %d adapter kind=%s project kind=%s", i, client.Kind, p.Projects[0].Ref.Kind)
		}
		reconciler := workers.Reconcilers[i]
		if len(reconciler.Projects) != 1 || !reflect.DeepEqual(reconciler.Projects[0], p.Projects[0]) {
			t.Fatalf("reconciler %d project scope=%+v, want %+v", i, reconciler.Projects, p.Projects)
		}
		reconcilerClient, ok := reconciler.Forge.(*forge.Adapter)
		if !ok || reconcilerClient != client {
			t.Fatalf("reconciler %d is not scoped to its poller's adapter", i)
		}

		commentClient, ok := workers.Comments[i].Client.(*forge.Adapter)
		if !ok || commentClient != client {
			t.Fatalf("comment worker %d is not scoped to its poller's adapter", i)
		}
		mergeClient, ok := workers.Merges[i].Client.(*forge.Adapter)
		if !ok || mergeClient != client {
			t.Fatalf("merge worker %d is not scoped to its poller's adapter", i)
		}

		// Production adapters must reject a Forge call without the stable key;
		// this proves the assembled path is budget-enforcing rather than a test
		// adapter silently making an uncharged call.
		_, _, err = client.ListIssueComments(ctx, p.Projects[0].Ref, "1", "")
		var classified *forge.ClassifiedError
		if !errors.As(err, &classified) || !errors.Is(err, forge.ErrContractViolation) {
			t.Fatalf("adapter call without charge key: %v", err)
		}
	}

	// command_ack shares the per-project adapter map assembled for forge_alert.
	// It is a single worker whose routing is resolved per-operation from the
	// append-only receipt, so it must cover every enabled project's adapter.
	if workers.CommandAcks == nil || workers.Alerts == nil || workers.RerunChecks == nil {
		t.Fatalf("command_ack/alert/rerun worker not wired: ack=%v alert=%v rerun=%v", workers.CommandAcks, workers.Alerts, workers.RerunChecks)
	}
	if len(workers.CommandAcks.Clients) != 2 || len(workers.RerunChecks.Clients) != 2 {
		t.Fatalf("shared client map sizes: ack=%d rerun=%d, want 2", len(workers.CommandAcks.Clients), len(workers.RerunChecks.Clients))
	}
	for i, p := range workers.Pollers {
		pollerClient := p.Forge.(*forge.Adapter)
		ref := p.Projects[0].Ref
		clientKey := string(ref.Kind) + "|" + ref.Host + "|" + ref.ProjectKey
		ackClient := workers.CommandAcks.Clients[clientKey]
		rerunClient := workers.RerunChecks.Clients[clientKey]
		if ackClient == nil || ackClient.(*forge.Adapter) != pollerClient || rerunClient == nil || rerunClient.(*forge.Adapter) != pollerClient {
			t.Fatalf("command_ack/rerun worker %d not scoped to its poller's adapter", i)
		}
	}
}

func TestAssembleProbesAndRecordsAutoMergeCapabilityOnEveryStartup(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg-project", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	cfg := daemonTestConfig("project")
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-project"}
	probes := 0
	factory := daemonAdapterFactory(func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if !reflect.DeepEqual(args, []string{"api", "--help"}) {
			t.Fatalf("probe args = %q", args)
		}
		probes++
		return []byte("--input file"), nil, nil
	})
	if _, err := assemble(db, cfg, func() time.Time { return now }, nil, factory); err != nil {
		t.Fatal(err)
	}
	if enabled, err := db.AutoMergeEnabled(ctx, ref); err != nil || !enabled {
		t.Fatalf("first startup capability = %v, %v; want true, nil", enabled, err)
	}
	if _, err := assemble(db, cfg, func() time.Time { return now.Add(time.Second) }, nil, factory); err != nil {
		t.Fatal(err)
	}
	if probes != 2 {
		t.Fatalf("startup probes = %d, want 2", probes)
	}
}

func TestAssembleRecordsAmbiguousCapabilityAndFailsOnStorageError(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1000)
	ambiguousFactory := daemonAdapterFactory(func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return nil, []byte("CLI unavailable"), errors.New("exit status 1")
	})

	t.Run("ambiguous probe remains available and is persisted false", func(t *testing.T) {
		db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.SeedProjectForTest(ctx, "cfg-project", "project", now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		workers, err := assemble(db, daemonTestConfig("project"), func() time.Time { return now }, nil, ambiguousFactory)
		if err != nil || len(workers.Pollers) != 1 {
			t.Fatalf("ambiguous startup workers=%v err=%v", workers, err)
		}
		ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-project"}
		if enabled, err := db.AutoMergeEnabled(ctx, ref); err != nil || enabled {
			t.Fatalf("ambiguous capability = %v, %v; want false, nil", enabled, err)
		}
	})

	t.Run("storage failure stops startup", func(t *testing.T) {
		db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := assemble(db, daemonTestConfig("missing"), func() time.Time { return now }, nil, ambiguousFactory); err == nil {
			t.Fatal("Assemble succeeded despite capability storage failure")
		}
	})
}

func daemonTestConfig(projectID string) *config.Config {
	return &config.Config{
		Projects:  []config.Project{{ID: projectID, Enabled: true, Forge: config.ForgeRef{Kind: config.ForgeKind("github"), Host: "github.com", Project: "org/repo-" + projectID, CLI: "gh"}}},
		Brain:     config.Brain{CallTimeout: time.Second},
		Forge:     config.Forge{HourlyAPILimit: 10, WarningRatio: .8, SlowPollInterval: time.Minute},
		Outbox:    config.Outbox{LeaseTTL: time.Minute},
		Scheduler: config.Scheduler{IntakeIdleInterval: time.Minute, IntakeActiveInterval: time.Second},
		Labels:    config.Labels{Trigger: "sift"},
	}
}

func daemonAdapterFactory(r forge.Runner) func(forge.Kind, string, forge.Runner, forge.Charger) (*forge.Adapter, error) {
	return func(k forge.Kind, cli string, _ forge.Runner, charger forge.Charger) (*forge.Adapter, error) {
		return forge.NewProductionAdapter(k, cli, r, charger)
	}
}

// TestAssembleWiresGateReEvaluationWorker verifies the gate_re_evaluation
// worker (storage.md §8.1) is constructed by Assemble and runs inside
// OutboxTick. The terminal protocol itself is covered by storage tests; this
// only asserts the production wiring seam.
func TestAssembleWiresGateReEvaluationWorker(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "p", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	d, err := Assemble(db, daemonTestConfig("p"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if d.GateReEvaluations == nil || d.GateReEvaluations.WorkerID != "siftd:gate_re_evaluation" {
		t.Fatalf("gate_re_evaluation worker not wired: %+v", d.GateReEvaluations)
	}
	if d.GateReEvaluations.Produce == nil {
		t.Fatal("gate_re_evaluation producer not configured")
	}
	// OutboxTick must run the worker without error when nothing is claimable.
	if err := d.OutboxTick(ctx); err != nil {
		t.Fatalf("OutboxTick: %v", err)
	}
}
