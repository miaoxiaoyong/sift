package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/daemon"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/forgeworker"
	"github.com/miaoxiaoyong/sift/internal/intake"
	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

const productionWakeNow = int64(1_800_000_000_000)

func TestProductionSchedulerWakesOutboxAfterEnqueueAndEmitInterrupt(t *testing.T) {
	testProductionWake(t, true, func(ctx context.Context, db *storage.DB, now int64) error {
		payload, err := json.Marshal(map[string]any{
			"project_id": "project", "forge_kind": "github", "forge_host": "github.com",
			"forge_project_key": "org/repo", "target_kind": "issue", "target_id": "42",
			"purpose": "summary", "markdown": "queued",
		})
		if err != nil {
			return err
		}
		_, err = db.EnqueueOperation(ctx, storage.Operation{
			Key: storage.CommentOperationKey("summary", "42", 1), Kind: storage.OperationForgeComment, Payload: payload,
		}, now)
		return err
	})

	testProductionWake(t, true, func(ctx context.Context, db *storage.DB, now int64) error {
		const head = "0123456789abcdef0123456789abcdef01234567"
		if err := db.SeedGateCandidateForTest(ctx, "run", "project", "cfg", "change-1", now); err != nil {
			return err
		}
		if err := db.SetRunChangeHeadForTest(ctx, "run", "change-1", head); err != nil {
			return err
		}
		version, err := db.FreezeGateChangeHead(ctx, "run", "change-1", head, 1, now)
		if err != nil {
			return err
		}
		attempt := 1
		_, err = db.EmitInterrupt(ctx, storage.EmitInterruptCmd{
			RunID: "run", ExpectedRunVersion: version, AttemptNo: &attempt, Reason: storage.InterruptStartupStall,
			Facts:      map[string]string{"attempt_no": "1", "generation": "1", "diagnostic_cause": "termination_unconfirmed", "isolation_consequence": "worktree held", "recommended_action": "retry", "attempt_diagnostic_ref": "/attempt", "worktree_ref": "/worktree"},
			Generation: storage.InterruptGeneration{AttemptNo: 1, Generation: 1},
			GatePhase:  storage.GateNone, GuardrailLevel: storage.GuardrailNone,
			AttentionDailyQuota: map[storage.InterruptSeverity]int{storage.SeverityLow: 10, storage.SeverityNormal: 10, storage.SeverityHigh: 10},
			DayTimezone:         "UTC", Source: storage.SourceSystem, NowMS: now,
		})
		return err
	})
}

// TestProductionSchedulerCommitWakeCannotPassFromStartupRecovery proves the
// startup sweep is drained before a write is made: disconnecting the commit
// hook after that barrier leaves the operation pending.
func TestProductionSchedulerCommitWakeCannotPassFromStartupRecovery(t *testing.T) {
	testProductionWake(t, false, func(ctx context.Context, db *storage.DB, now int64) error {
		payload, err := json.Marshal(map[string]any{
			"project_id": "project", "forge_kind": "github", "forge_host": "github.com",
			"forge_project_key": "org/repo", "target_kind": "issue", "target_id": "42",
			"purpose": "summary", "markdown": "queued",
		})
		if err != nil {
			return err
		}
		_, err = db.EnqueueOperation(ctx, storage.Operation{Key: storage.CommentOperationKey("summary", "42", 1), Kind: storage.OperationForgeComment, Payload: payload}, now)
		return err
	})
}

func testProductionWake(t *testing.T, commitWakeup bool, enqueue func(context.Context, *storage.DB, int64) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.UnixMilli(productionWakeNow)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", productionWakeNow); err != nil {
		t.Fatal(err)
	}

	client := forge.NewFake()
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	client.AddIssue(ref, forge.Issue{ID: "42", Title: "issue", Body: "body", Author: "alice", URL: "https://example.test/42"})
	completed := make(chan storage.CompleteOutcome, 1)
	workers := &daemon.Daemon{DB: db, Now: func() time.Time { return now }, Comments: []*forgeworker.CommentWorker{{
		DB: db, Client: client, ProjectID: "project", WorkerID: "test:comment", Lease: time.Minute, Now: func() time.Time { return now },
		Complete: func(ctx context.Context, claim storage.ClaimedOperation, outcome storage.CompleteOutcome) error {
			completed <- outcome
			return db.CompleteOutboxAttempt(ctx, claim, outcome)
		},
	}}}
	termination := &daemon.TerminationCoordinator{DB: db, Terminator: runtime.Terminator{}, Runtime: config.Runtime{}, Now: func() time.Time { return now }}
	if err := startSchedulers(ctx, db, workers, termination, nil, schedulerWithLongIntervals()); err != nil {
		t.Fatal(err)
	}
	// startSchedulers returns only after the outbox startup sweep observed an
	// empty queue. The long interval is much larger than this assertion window.
	if !commitWakeup {
		db.SetOutboxWakeup(nil)
	}
	if err := enqueue(ctx, db, productionWakeNow); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-completed:
		if !commitWakeup {
			t.Fatal("outbox advanced without the commit wakeup after startup drain")
		}
		if outcome.State != storage.OperationSucceeded {
			t.Fatalf("production outbox worker outcome = %s, error=%s", outcome.State, outcome.ErrorSummary)
		}
	case <-time.After(750 * time.Millisecond):
		if commitWakeup {
			t.Fatal("production outbox worker did not run after commit wakeup")
		}
	}
}

func TestStartSchedulersKeepsProductionEdgesIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.UnixMilli(productionWakeNow)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistIntakeBatch(ctx, storage.PersistIntakeBatchCmd{ProjectID: "project", Stream: "issues", Cursor: "not-due", NextPollAtMS: now.Add(time.Hour).UnixMilli(), NowMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	client := &countingForge{Fake: forge.NewFake()}
	workers := &daemon.Daemon{DB: db, Now: func() time.Time { return now }, Pollers: []*intake.Poller{{
		DB: db, Forge: client, Projects: []intake.Project{{ID: "project", Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo"}}}, Now: func() time.Time { return now }, Idle: time.Minute,
	}}}
	certificationVersion := strings.Repeat("a", 64)
	if _, err := db.ExecForTest(ctx, `INSERT INTO certifications(task_kind,certification_version,total_samples,negative_samples,leak_count,false_block_count,certified,evidence_digest,updated_at_ms,certification_rules_version,window_start_ms,window_end_ms) VALUES('bug',?,1,1,0,0,1,?,?,?,?,?)`, certificationVersion, strings.Repeat("b", 64), now.UnixMilli(), strings.Repeat("c", 64), now.Add(-time.Hour).UnixMilli(), now.Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `INSERT INTO certification_current(task_kind,certification_version,version,updated_at_ms) VALUES('bug',?,1,?)`, certificationVersion, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendT7ReplayEvidence(ctx, storage.AppendT7ReplayEvidenceCmd{Scope: "global", TaskKind: "bug", WindowStartMS: now.Add(-time.Hour).UnixMilli(), WindowEndMS: now.Add(-time.Second).UnixMilli(), DatasetVersion: "dataset/v1", GateVersion: "gate/v1", TotalSamples: 1, NegativeSamples: 1, CreatedAtMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	evidenceID := "t7:certification:bug:" + certificationVersion
	t7Provider := &brain.FakeProvider{Responses: []brain.FakeResponse{{ResultText: `{"proposal_kind":"policy","target_scope":"global","title":"Review","body":"Human review.","evidence_entry_ids":["` + evidenceID + `"],"requires_human_approval":true}`, InputTokens: 1, OutputTokens: 1}}}
	brainCfg := config.Brain{Executable: "fake", DailyTokenLimit: 100, CallTimeout: time.Minute, SchemaRetries: 1, MaxInputBytes: 262144, MaxRawOutputBytes: 1048576}
	workers.SetT7Scheduler(&brain.T7Scheduler{DB: db, Shell: brain.NewShell(db, brainCfg, t7Provider, func() time.Time { return now }), Now: func() time.Time { return now }})
	termination := &daemon.TerminationCoordinator{DB: db, Runtime: config.Runtime{}, Now: func() time.Time { return now }}
	factory := &manualSchedulerFactory{}
	var intakeEdges, supervisorEdges, outboxEdges int
	if err := startSchedulersWithFactory(ctx, db, workers, termination, nil, schedulerWithLongIntervals(), factory, schedulerHooks{
		Intake: func() { intakeEdges++ }, Supervisor: func() { supervisorEdges++ }, Outbox: func() { outboxEdges++ },
	}); err != nil {
		t.Fatal(err)
	}
	// The outbox startup sweep is complete before this function returned.
	if outboxEdges != 1 {
		t.Fatalf("outbox startup sweeps = %d, want 1", outboxEdges)
	}
	intakeEdges, supervisorEdges, outboxEdges = 0, 0, 0

	factory.intake.Edge(t, ctx)
	if intakeEdges != 1 || supervisorEdges != 0 || outboxEdges != 0 {
		t.Fatalf("intake edge hooks = intake:%d supervisor:%d outbox:%d", intakeEdges, supervisorEdges, outboxEdges)
	}
	cursor, err := db.IntakeCursor(ctx, "project", "issues")
	if err != nil {
		t.Fatal(err)
	}
	if cursor.NextPollAtMS != now.Add(time.Hour).UnixMilli() || client.issuePolls != 0 {
		t.Fatalf("intake edge polled not-due cursor: cursor=%d polls=%d", cursor.NextPollAtMS, client.issuePolls)
	}

	factory.supervisor.Edge(t, ctx)
	if intakeEdges != 1 || supervisorEdges != 1 || outboxEdges != 0 {
		t.Fatalf("supervisor edge hooks = intake:%d supervisor:%d outbox:%d", intakeEdges, supervisorEdges, outboxEdges)
	}
	if len(t7Provider.Requests) != 1 {
		t.Fatalf("supervisor T7 provider calls = %d, want 1", len(t7Provider.Requests))
	}
	// A committed operation wakes only the outbox edge. Manually advancing that
	// clock proves it does not invoke intake or supervisor callbacks.
	if _, err := db.EnqueueOperation(ctx, storage.Operation{Key: "alert:edge:1", Kind: storage.OperationForgeAlert, Payload: []byte(`{}`)}, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	factory.outbox.Edge(t, ctx)
	if intakeEdges != 1 || supervisorEdges != 1 || outboxEdges != 1 {
		t.Fatalf("outbox edge hooks = intake:%d supervisor:%d outbox:%d", intakeEdges, supervisorEdges, outboxEdges)
	}
}

type countingForge struct {
	*forge.Fake
	issuePolls int
}

func (f *countingForge) ListIssuesByLabel(ctx context.Context, ref forge.ProjectRef, label string, cursor forge.Cursor) ([]forge.Issue, forge.Cursor, error) {
	f.issuePolls++
	return f.Fake.ListIssuesByLabel(ctx, ref, label, cursor)
}

type manualSchedulerFactory struct {
	intake, supervisor, outbox *manualScheduler
}

func (f *manualSchedulerFactory) Intake(run func(context.Context) error) wakeScheduler {
	f.intake = &manualScheduler{run: run}
	return f.intake
}
func (f *manualSchedulerFactory) Supervisor(run func(context.Context) error) wakeScheduler {
	f.supervisor = &manualScheduler{run: run}
	return f.supervisor
}
func (f *manualSchedulerFactory) Outbox(run func(context.Context) error) wakeScheduler {
	f.outbox = &manualScheduler{run: run}
	return f.outbox
}

type manualScheduler struct{ run func(context.Context) error }

func (*manualScheduler) Wake()                                   {}
func (s *manualScheduler) WakeAndWait(ctx context.Context) error { return s.run(ctx) }
func (s *manualScheduler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *manualScheduler) Edge(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
}

func schedulerWithLongIntervals() config.Scheduler {
	return config.Scheduler{IntakeIdleInterval: 10 * time.Second, IntakeActiveInterval: 10 * time.Second, IntakeInterruptInterval: 10 * time.Second, SupervisorInterval: 10 * time.Second}
}
