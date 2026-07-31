package gate_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/command"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/forgeworker"
	"github.com/miaoxiaoyong/sift/internal/gate"
	"github.com/miaoxiaoyong/sift/internal/intake"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/worktree"
)

const v9RunID = "0123456789abcdef0123456789abcdef"

// TestV9FullFakeGateInterruptCommandMergeChain starts from verified fake Agent
// result evidence, then requires every production successor: create Change,
// Gate HITL, Command effect, Gate re-evaluation, merge operation and Forge fact
// convergence. It deliberately never seeds a Change or Gate candidate.
func TestV9FullFakeGateInterruptCommandMergeChain(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg-v9", "p-v9", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	repo := initPolicyRepo(t)
	manager, err := worktree.NewManager(repo, filepath.Join(t.TempDir(), "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := manager.Create(ctx, v9RunID, 1, "main", "sift/"+v9RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("full fake V9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "change.txt"}, {"commit", "-m", "full fake V9"}} {
		if output, err := exec.Command("git", append([]string{"-C", wt.Path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	headBytes, err := exec.Command("git", "-C", wt.Path, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headBytes))

	if err := db.SeedLaunchRunForTest(ctx, v9RunID, "p-v9", "cfg-v9", now.UnixMilli(), wt.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE runs SET kind='feature' WHERE id=?`, v9RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(ctx, `UPDATE attempts SET phase='spawning',worktree_path=?,branch_name=?,base_ref='main' WHERE run_id=?`, wt.Path, wt.Branch, v9RunID); err != nil {
		t.Fatal(err)
	}
	agent := storage.AgentIdentity{PID: 123, StartedAtMS: now.UnixMilli(), Executable: "/test/fake-agent"}
	exitCode := 0
	if _, err := db.ResolveAttemptRace(ctx, storage.AttemptRaceCommand{
		RunID: v9RunID, AttemptNo: 1, ExpectedGeneration: 1, FactKey: "v9-result", NowMS: now.UnixMilli(),
		Agent: &agent, Result: &storage.AttemptResult{Agent: agent, ExitCode: &exitCode, FinalHeadSHA: head, Digest: "v9-result-digest", FinishedAtMS: now.UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}

	if err := (&gate.SuccessReconciler{DB: db, ProjectID: "p-v9", Worktrees: manager, Now: func() time.Time { return now }}).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertV9Count(t, db, `SELECT COUNT(*) FROM outbox_operations WHERE run_id=? AND kind='create_change'`, 1)
	run, err := db.Run(ctx, v9RunID)
	if err != nil || run.ChangeID != "" {
		t.Fatalf("before create worker run=%+v err=%v", run, err)
	}

	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p-v9"}
	baseForge := &verticalForge{Fake: forge.NewFake(), head: head}
	client := &v9FullFakeForge{verticalForge: baseForge}
	client.AddIssue(ref, forge.Issue{ID: "issue-1", Title: "V9", Body: "full fake", Author: "alice", URL: "https://forge.example/issues/1", State: forge.IssueOpen})
	if err := (&forgeworker.ChangeWorker{DB: db, Client: client, ProjectID: "p-v9", WorkerID: "v9-create", Lease: time.Minute, Now: func() time.Time { return now.Add(time.Second) }}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	run, err = db.Run(ctx, v9RunID)
	if err != nil || run.ChangeID == "" {
		t.Fatalf("create worker run=%+v err=%v", run, err)
	}
	changeID := run.ChangeID

	certification := config.DefaultConfig().Certification
	certificationVersion, err := config.CertificationRulesVersion(certification)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SeedCertificationForTest(ctx, "feature", certificationVersion, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateProjectAutoMergeCapability(ctx, "p-v9", true, "fake Forge supports expected-head CAS", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	provider := &brain.FakeProvider{Responses: []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`, InputTokens: 1, OutputTokens: 1}}}
	brainCfg := config.Brain{Executable: "fake", DailyTokenLimit: 1000, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}
	gateReconciler := &gate.Reconciler{
		DB: db, Forge: client, Brain: brain.NewShell(db, brainCfg, provider, func() time.Time { return now.Add(2 * time.Second) }),
		ProjectID: "p-v9", Project: ref, Repo: repo,
		Defaults:      config.GateDefaults{ReviewPolicy: config.ReviewPolicyAlways, RiskyReviewThreshold: 100, AutoMerge: true, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1},
		Certification: certification,
		Attention:     config.Attention{DayTimezone: "UTC", DailyQuota: config.DailyQuota{Low: 10, Normal: 10, High: 10}, MaxEscalations: 1},
		Channels:      []storage.InterruptChannel{{ID: "review", Type: "webhook", TargetRef: "secret_ref:REVIEW", Renderer: "plain-v1", Capabilities: []string{"visual"}, Default: true}},
		Now:           func() time.Time { return now.Add(2 * time.Second) },
	}
	if err := gateReconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertV9Count(t, db, `SELECT COUNT(*) FROM gate_evaluations WHERE run_id=?`, 1)
	assertV9Count(t, db, `SELECT COUNT(*) FROM interrupts WHERE run_id=? AND reason='code_review' AND status='open'`, 1)

	var interruptID, nonce, targetKind, targetID string
	if err := db.QueryRowForTest(ctx, `SELECT i.id,i.nonce,t.target_kind,t.target_id FROM interrupts i JOIN interrupt_command_targets t ON t.interrupt_id=i.id WHERE i.run_id=? AND i.status='open'`, v9RunID).Scan(&interruptID, &nonce, &targetKind, &targetID); err != nil {
		t.Fatal(err)
	}
	body := "/sift approve " + v9RunID + " " + nonce
	eventKey, err := command.RecomputeEventKey("p-v9", command.SourceForgeComment, "v9-command")
	if err != nil {
		t.Fatal(err)
	}
	actor := "alice"
	envelope := command.CommandEventEnvelopeV1{
		SchemaVersion: 1, EventKey: eventKey, ProjectID: "p-v9", Source: command.SourceForgeComment,
		RemoteEventID: "v9-command", Target: command.CommandTarget{Kind: command.CommandTargetKind(targetKind), ID: targetID},
		Actor: &actor, RawDigest: strings.Repeat("a", 64), OccurredAtMS: now.Add(3 * time.Second).UnixMilli(),
		Comment: &command.CommandComment{ID: "v9-command", Body: body},
	}
	commandResult, err := db.ApplyCommandEvent(ctx, storage.ApplyCommandEventCmd{Envelope: envelope, Allowlist: []string{"alice"}, NowMS: now.Add(3 * time.Second).UnixMilli()})
	if err != nil || commandResult.Outcome != command.OutcomeApplied {
		t.Fatalf("command outcome=%s err=%v", commandResult.Outcome, err)
	}
	assertV9Count(t, db, `SELECT COUNT(*) FROM command_effects WHERE run_id=? AND effect_kind='human_review_approval'`, 1)
	assertV9Count(t, db, `SELECT COUNT(*) FROM outbox_operations WHERE run_id=? AND kind='gate_re_evaluation'`, 1)

	reevaluation := &forgeworker.GateReEvaluationWorker{
		DB: db, WorkerID: "v9-reeval", Lease: time.Minute, Now: func() time.Time { return now.Add(4 * time.Second) },
		Produce: func(ctx context.Context, payload storage.GateReEvaluationPayload) ([]byte, error) {
			source, err := db.GateReevaluationSource(ctx, payload.RunID)
			if err != nil {
				return nil, err
			}
			return gateReconciler.ProduceReevaluation(ctx, payload, source)
		},
	}
	if err := reevaluation.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertV9Count(t, db, `SELECT COUNT(*) FROM outbox_operations WHERE run_id=? AND kind='gate_re_evaluation' AND state='succeeded'`, 1)
	assertV9Count(t, db, `SELECT COUNT(*) FROM outbox_operations WHERE run_id=? AND kind='merge_change'`, 1)

	if err := (&forgeworker.MergeWorker{DB: db, Client: client, ProjectID: "p-v9", WorkerID: "v9-merge", Lease: time.Minute, Now: func() time.Time { return now.Add(5 * time.Second) }}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if client.mergeCalls != 1 {
		t.Fatalf("Forge merge calls=%d, want 1", client.mergeCalls)
	}
	if err := (&intake.Reconciler{DB: db, Forge: client, Projects: []intake.Project{{ID: "p-v9", TriggerLabel: "sift", Ref: ref, OperatorAllowlist: []string{"alice"}}}, Certification: certification, Now: func() time.Time { return now.Add(6 * time.Second) }}).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	run, err = db.Run(ctx, v9RunID)
	if err != nil || run.Status != storage.RunDone || run.GateBypassed || run.ChangeID != changeID {
		t.Fatalf("final run=%+v err=%v", run, err)
	}
	assertV9Count(t, db, `SELECT COUNT(*) FROM gate_evaluations WHERE run_id=?`, 2)
	assertV9Count(t, db, `SELECT COUNT(*) FROM interrupts WHERE run_id=?`, 1)
	assertV9Count(t, db, `SELECT COUNT(*) FROM outbox_operations WHERE run_id=? AND kind='merge_change' AND state='succeeded'`, 1)
}

func assertV9Count(t *testing.T, db *storage.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowForTest(context.Background(), query, v9RunID).Scan(&got); err != nil || got != want {
		t.Fatalf("count query %q=%d, want %d (err=%v)", query, got, want, err)
	}
}

type v9FullFakeForge struct {
	*verticalForge
	mergeCalls int
}

func (f *v9FullFakeForge) GetChange(ctx context.Context, project forge.ProjectRef, id string) (forge.Change, error) {
	change, err := f.verticalForge.GetChange(ctx, project, id)
	if err == nil && change.State == forge.ChangeOpen {
		change.ReviewState = forge.NotApproved
	}
	return change, err
}

func (f *v9FullFakeForge) GetChecks(context.Context, forge.ProjectRef, string) (forge.CheckSuite, error) {
	return forge.CheckSuite{Conclusion: "success", ExternalURL: "https://ci.example/v9"}, nil
}

func (f *v9FullFakeForge) MergeChange(ctx context.Context, project forge.ProjectRef, id, expectedHead, method string) (forge.Change, error) {
	f.mergeCalls++
	if expectedHead != f.head || method != "merge" {
		return forge.Change{}, &forge.ClassifiedError{Class: forge.ErrSemanticConflict, Summary: "merge precondition changed"}
	}
	return f.Fake.InjectMerged(project, id, time.UnixMilli(1_700_000_005_000))
}
