package gate_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/forgeworker"
	"github.com/miaoxiaoyong/sift/internal/gate"
	"github.com/miaoxiaoyong/sift/internal/intake"
	"github.com/miaoxiaoyong/sift/internal/replay"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/worktree"
)

// TestReconcilerAssemblyEvidence exercises Gate reconciliation inputs and
// replays their exported traces with the production runners.
func TestReconcilerAssemblyEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, path, checks string
		responses          []brain.FakeResponse
		disabled, drift    bool
		wantT5, wantMerge  bool
	}{
		{"github_ci_hard_path", ".github/workflows/ci.yml", "success", []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}, false, false, false, false},
		{"gitlab_ci_hard_path", ".gitlab-ci.yml", "success", []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}, false, false, false, false},
		{"merge_with_shadow_and_replay", "cmd/a.go", "success", []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}, false, false, false, true},
		{"t3_and_t5_valid", "cmd/a.go", "failure", []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}, {ResultText: `{"classification":"flaky","retry_check_id":"job-1","rationale":"transient"}`}}, false, false, true, false},
		{"t5_fallback", "cmd/a.go", "failure", []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}, false, false, true, false},
		{"t3_fallback", "cmd/a.go", "success", nil, true, false, false, true},
		{"head_drift_stops_before_gate_or_operation", "cmd/a.go", "success", nil, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.UnixMilli(1_700_000_000_000)
			db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.SeedProjectForTest(ctx, "cfg", "p", now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if err := db.SeedGateCandidateForTest(ctx, "r", "p", "cfg", "42", now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if err := db.SeedCertificationForTest(ctx, "feature", strings.Repeat("c", 64), now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateProjectAutoMergeCapability(ctx, "p", true, "test", now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			client := phaseForge{path: tc.path, checks: tc.checks, drift: tc.drift}
			repo := initPolicyRepo(t)
			brainCfg := config.Brain{Executable: "fake", DailyTokenLimit: 100, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}
			if tc.disabled {
				brainCfg.Executable = ""
			}
			r := &gate.Reconciler{DB: db, Forge: &client, Brain: brain.NewShell(db, brainCfg, &brain.FakeProvider{Responses: tc.responses}, func() time.Time { return now }), ProjectID: "p", Project: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p"}, Repo: repo, Defaults: config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 100, AutoMerge: true, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}, Attention: config.Attention{DayTimezone: "UTC", DailyQuota: config.DailyQuota{Low: 3, Normal: 3, High: 3}, MaxEscalations: 1}, Now: func() time.Time { return now }}
			if err := r.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}

			var exported bytes.Buffer
			if err := db.ExportReplayJSONL(ctx, &exported); err != nil {
				t.Fatal(err)
			}
			if tc.drift {
				if exported.Len() != 0 {
					t.Fatalf("head drift exported Gate evidence: %s", exported.String())
				}
				op, err := db.ClaimOutboxOperationKindProject(ctx, "test", storage.OperationMergeChange, "p", now.UnixMilli(), int64(time.Minute/time.Millisecond))
				if err != nil || op != nil {
					t.Fatalf("head drift merge operation = %#v, %v", op, err)
				}
				return
			}
			if !strings.Contains(exported.String(), `"record_type":"gate"`) {
				t.Fatalf("missing frozen Gate snapshot: %s", exported.String())
			}
			if tc.wantT5 && !strings.Contains(exported.String(), `"touchpoint":"T5"`) {
				t.Fatalf("missing T5 trace: %s", exported.String())
			}
			operation, err := db.ClaimOutboxOperationKindProject(ctx, "test", storage.OperationMergeChange, "p", now.UnixMilli(), int64(time.Minute/time.Millisecond))
			if err != nil || (operation != nil) != tc.wantMerge {
				t.Fatalf("merge operation = %#v, %v; want present=%v", operation, err, tc.wantMerge)
			}
			if !strings.Contains(exported.String(), `"gate_input_snapshot_ids":[`) {
				t.Fatalf("Brain trace was not linked to Gate snapshot: %s", exported.String())
			}
			if _, err := replay.ReplayGateJSONL(bytes.NewReader(exported.Bytes())); err != nil {
				t.Fatalf("Gate replay: %v", err)
			}
			contracts := map[string]brain.TouchpointContract{"T3": brain.T3Contract(), "T5": brain.T5Contract([]brain.T5Job{{ID: "job-1", Name: "test", WebURL: "https://ci.example/job-1"}})}
			report, err := replay.ReplayBrainJSONL(bytes.NewReader(exported.Bytes()), contracts)
			if err != nil {
				t.Fatalf("Brain replay: %v", err)
			}
			if report.Records == 0 {
				t.Fatal("Brain replay did not receive production trace")
			}
		})
	}
}

// TestM4VerticalVerifiedSuccessToExternalMerge exercises the M4 phase path
// without seeding a Gate candidate or certification: execution evidence is
// verified, the create worker records the Change, then Gate, HITL, Ledger,
// certification and both replay formats consume their production records.
func TestM4VerticalVerifiedSuccessToExternalMerge(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedProjectForTest(ctx, "cfg", "p", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	repo := initPolicyRepo(t)
	manager, err := worktree.NewManager(repo, filepath.Join(t.TempDir(), "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := manager.Create(ctx, "r", 1, "main", "sift/r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "change.txt"}, {"commit", "-m", "verified change"}} {
		if out, err := exec.Command("git", append([]string{"-C", wt.Path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	headBytes, err := exec.Command("git", "-C", wt.Path, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headBytes))
	if err := db.SeedLaunchRunForTest(ctx, "r", "p", "cfg", now.UnixMilli(), wt.Path); err != nil {
		t.Fatal(err)
	}
	q, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, err := q.Exec(`UPDATE runs SET kind='feature' WHERE id='r'; UPDATE attempts SET phase='spawning', worktree_path=?, branch_name=?, base_ref='main' WHERE run_id='r'`, wt.Path, wt.Branch); err != nil {
		t.Fatal(err)
	}
	agent := storage.AgentIdentity{PID: 123, StartedAtMS: now.UnixMilli(), Executable: "/test/agent"}
	zero := 0
	if _, err := db.ResolveAttemptRace(ctx, storage.AttemptRaceCommand{RunID: "r", AttemptNo: 1, ExpectedGeneration: 1, FactKey: "verified-result", NowMS: now.UnixMilli(), Agent: &agent, Result: &storage.AttemptResult{Agent: agent, ExitCode: &zero, FinalHeadSHA: head, Digest: "result-digest", FinishedAtMS: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	if err := (&gate.SuccessReconciler{DB: db, ProjectID: "p", Worktrees: manager, Now: func() time.Time { return now }}).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p"}
	client := &verticalForge{Fake: forge.NewFake(), head: head}
	client.AddIssue(ref, forge.Issue{ID: "issue-1", Title: "issue", Body: "body", Author: "author", URL: "https://forge.example/issues/1", State: forge.IssueOpen})
	if err := (&forgeworker.ChangeWorker{DB: db, Client: client, ProjectID: "p", WorkerID: "create", Lease: time.Minute, Now: func() time.Time { return now }}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := db.Run(ctx, "r")
	if err != nil || run.ChangeID == "" {
		t.Fatalf("create worker run=%+v err=%v", run, err)
	}

	provider := &brain.FakeProvider{Responses: []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}, {ResultText: `{"classification":"real_failure","rationale":"real failure"}`}}}
	brainCfg := config.Brain{Executable: "fake", DailyTokenLimit: 100, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}
	gateReconciler := &gate.Reconciler{DB: db, Forge: client, Brain: brain.NewShell(db, brainCfg, provider, func() time.Time { return now }), ProjectID: "p", Project: ref, Repo: repo, Defaults: config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 100, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 0}, Certification: config.DefaultConfig().Certification, Attention: config.Attention{DayTimezone: "UTC", DailyQuota: config.DailyQuota{Low: 3, Normal: 3, High: 3}, MaxEscalations: 1}, Now: func() time.Time { return now }}
	if err := gateReconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var gates, interrupts int
	if err := q.QueryRow(`SELECT COUNT(*) FROM gate_evaluations WHERE run_id='r'`).Scan(&gates); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM interrupts WHERE run_id='r' AND status='open'`).Scan(&interrupts); err != nil {
		t.Fatal(err)
	}
	if gates != 1 || interrupts != 1 {
		t.Fatalf("Gate/Shadow/HITL records=%d/%d, want 1/1", gates, interrupts)
	}
	rows, err := q.Query(`SELECT c.touchpoint, COUNT(l.gate_input_snapshot_id) FROM brain_calls c LEFT JOIN brain_gate_input_links l ON l.logical_call_id=c.id WHERE c.run_id='r' AND c.touchpoint IN ('T3','T5') GROUP BY c.id,c.touchpoint`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	links := map[string]int{}
	for rows.Next() {
		var touchpoint string
		var count int
		if err := rows.Scan(&touchpoint, &count); err != nil {
			t.Fatal(err)
		}
		links[touchpoint] = count
	}
	if err := rows.Err(); err != nil || links["T3"] == 0 || links["T5"] == 0 {
		t.Fatalf("actual T3/T5 Gate snapshot links=%v err=%v", links, err)
	}

	if _, err := client.InjectMerged(ref, run.ChangeID, now); err != nil {
		t.Fatal(err)
	}
	project := intake.Project{ID: "p", TriggerLabel: "sift", Ref: ref}
	if err := (&intake.Reconciler{DB: db, Forge: client, Projects: []intake.Project{project}, Certification: config.DefaultConfig().Certification, Now: func() time.Time { return now }}).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	run, err = db.Run(ctx, "r")
	if err != nil || run.Status != storage.RunDone || !run.GateBypassed {
		t.Fatalf("external merge run=%+v err=%v", run, err)
	}
	var decisions, settled, certs int
	if err := q.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE run_id='r' AND entry_kind='human_decision'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM human_decision_receipts`).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM certification_current WHERE task_kind='feature'`).Scan(&certs); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || settled != 1 || certs != 1 {
		t.Fatalf("Ledger/certification settlement=%d/%d/%d, want 1/1/1", decisions, settled, certs)
	}
	var exported bytes.Buffer
	if err := db.ExportReplayJSONL(ctx, &exported); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.ReplayGateJSONL(bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatalf("Gate replay: %v", err)
	}
	if _, err := replay.ReplayBrainJSONL(bytes.NewReader(exported.Bytes()), map[string]brain.TouchpointContract{"T3": brain.T3Contract(), "T5": brain.T5Contract([]brain.T5Job{{ID: "job-1", Name: "test", WebURL: "https://ci.example/job-1"}})}); err != nil {
		t.Fatalf("Brain replay: %v", err)
	}
}

type verticalForge struct {
	*forge.Fake
	head string
}

func (f *verticalForge) CreateChange(ctx context.Context, p forge.ProjectRef, branch, base, title, body string) (forge.Change, error) {
	if _, err := f.Fake.CreateChange(ctx, p, branch, base, title, body); err != nil {
		return forge.Change{}, err
	}
	return f.GetChange(ctx, p, "1")
}
func (f *verticalForge) GetChange(ctx context.Context, p forge.ProjectRef, id string) (forge.Change, error) {
	c, err := f.Fake.GetChange(ctx, p, id)
	if err != nil {
		return c, err
	}
	c.URL, c.HeadSHA, c.Mergeability, c.ReviewState = "https://forge.example/"+id, f.head, forge.Mergeable, forge.Approved
	return c, nil
}
func (f *verticalForge) GetChangeDiff(context.Context, forge.ProjectRef, string) (string, error) {
	return "diff --git a/cmd/a.go b/cmd/a.go\n+++ b/cmd/a.go", nil
}
func (f *verticalForge) GetChecks(context.Context, forge.ProjectRef, string) (forge.CheckSuite, error) {
	return forge.CheckSuite{Conclusion: "failure", ExternalURL: "https://ci.example/run", FailedJobs: []forge.CheckJob{{ID: "job-1", Name: "test", WebURL: "https://ci.example/job-1"}}}, nil
}

func TestReconcilerPolicyReadErrorMatrixIsolatesOnlyBadProject(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)
	for _, tc := range []struct {
		name         string
		repo         func(t *testing.T) (string, string)
		wantIsolated bool
	}{
		{"policy_missing_is_valid_defaults", func(t *testing.T) (string, string) { return initPolicyRepo(t), "main" }, false},
		{"invalid_policy", func(t *testing.T) (string, string) { return policyRepo(t, "version: 2\n"), "main" }, true},
		{"unknown_base", func(t *testing.T) (string, string) { return initPolicyRepo(t), "does-not-exist" }, true},
		{"unreadable_repository", func(t *testing.T) (string, string) { return filepath.Join(t.TempDir(), "missing-repo"), "main" }, true},
		{"existing_policy_git_show_failure", func(t *testing.T) (string, string) {
			repo := policyRepo(t, "version: 1\n")
			out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD:.sift/policy.yaml").Output()
			if err != nil {
				t.Fatal(err)
			}
			hash := strings.TrimSpace(string(out))
			if err := os.Remove(filepath.Join(repo, ".git", "objects", hash[:2], hash[2:])); err != nil {
				t.Fatal(err)
			}
			return repo, "main"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, project := range []string{"bad", "good"} {
				if err := db.SeedProjectForTest(ctx, "cfg-"+project, project, now.UnixMilli()); err != nil {
					t.Fatal(err)
				}
				if err := db.SeedGateCandidateForTest(ctx, "run-"+project, project, "cfg-"+project, "42", now.UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			badRepo, badBase := tc.repo(t)
			checkBase, err := sql.Open("sqlite", db.Path())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := checkBase.Exec(`UPDATE attempts SET base_ref=? WHERE run_id='run-bad'`, badBase); err != nil {
				checkBase.Close()
				t.Fatal(err)
			}
			checkBase.Close()
			newReconciler := func(project, repo, base string) *gate.Reconciler {
				return &gate.Reconciler{DB: db, Forge: &phaseForge{path: "cmd/a.go", checks: "success"}, Brain: brain.NewShell(db, config.Brain{Executable: "fake", DailyTokenLimit: 100, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}, &brain.FakeProvider{Responses: []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}}, func() time.Time { return now }), ProjectID: project, Project: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + project}, Repo: repo, Defaults: config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 100, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}, Now: func() time.Time { return now }}
			}
			if err := newReconciler("bad", badRepo, badBase).ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err := newReconciler("good", initPolicyRepo(t), "main").ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			isolated, err := db.ProjectIsolated(ctx, "bad")
			if err != nil || isolated != tc.wantIsolated {
				t.Fatalf("bad isolation=%v, want %v: %v", isolated, tc.wantIsolated, err)
			}
			check, err := sql.Open("sqlite", db.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			var badGates, badMerges, goodGates int
			if err := check.QueryRow(`SELECT COUNT(*) FROM gate_evaluations WHERE run_id='run-bad'`).Scan(&badGates); err != nil {
				t.Fatal(err)
			}
			if err := check.QueryRow(`SELECT COUNT(*) FROM outbox_operations WHERE run_id='run-bad' AND kind='merge_change'`).Scan(&badMerges); err != nil {
				t.Fatal(err)
			}
			if err := check.QueryRow(`SELECT COUNT(*) FROM gate_evaluations WHERE run_id='run-good'`).Scan(&goodGates); err != nil {
				t.Fatal(err)
			}
			if tc.wantIsolated && (badGates != 0 || badMerges != 0) {
				t.Fatalf("isolated project wrote gates=%d merges=%d", badGates, badMerges)
			}
			if !tc.wantIsolated && badGates != 1 {
				t.Fatalf("missing policy must use defaults, gates=%d", badGates)
			}
			if goodGates != 1 {
				t.Fatalf("healthy project was not reconciled, gates=%d", goodGates)
			}
		})
	}
}

func policyRepo(t *testing.T, content string) string {
	t.Helper()
	repo := initPolicyRepo(t)
	if err := os.Mkdir(filepath.Join(repo, ".sift"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".sift", "policy.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "add", ".sift/policy.yaml").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "commit", "-m", "policy").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	return repo
}

func TestReconcilerIsolatesBadPolicyProjectWithoutStoppingHealthyProject(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(t.TempDir(), "sift.db"), BinaryVersion: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, project := range []string{"bad", "good"} {
		if err := db.SeedProjectForTest(ctx, "cfg-"+project, project, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if err := db.SeedGateCandidateForTest(ctx, "run-"+project, project, "cfg-"+project, "42", now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	goodRepo := initPolicyRepo(t)
	newReconciler := func(project, repo string) *gate.Reconciler {
		return &gate.Reconciler{DB: db, Forge: &phaseForge{path: "cmd/a.go", checks: "success"}, Brain: brain.NewShell(db, config.Brain{Executable: "fake", DailyTokenLimit: 100, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}, &brain.FakeProvider{Responses: []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}}, func() time.Time { return now }), ProjectID: project, Project: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-" + project}, Repo: repo, Defaults: config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 100, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}, Now: func() time.Time { return now }}
	}
	if err := newReconciler("bad", filepath.Join(t.TempDir(), "missing-repo")).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	isolated, err := db.ProjectIsolated(ctx, "bad")
	if err != nil || !isolated {
		t.Fatalf("bad project isolation = %v, %v", isolated, err)
	}
	if err := newReconciler("good", goodRepo).ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var gates, merges int
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow(`SELECT COUNT(*) FROM gate_evaluations WHERE run_id='run-bad'`).Scan(&gates); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM outbox_operations WHERE run_id='run-bad' AND kind='merge_change'`).Scan(&merges); err != nil {
		t.Fatal(err)
	}
	if gates != 0 || merges != 0 {
		t.Fatalf("bad project wrote gates=%d merges=%d", gates, merges)
	}
	if err := check.QueryRow(`SELECT COUNT(*) FROM gate_evaluations WHERE run_id='run-good'`).Scan(&gates); err != nil || gates != 1 {
		t.Fatalf("healthy project gate evaluations=%d, %v", gates, err)
	}
}

func initPolicyRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Sift test"}, {"commit", "--allow-empty", "-m", "initial"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

type phaseForge struct {
	path, checks string
	drift        bool
	calls        int
}

func (f *phaseForge) ListIssuesByLabel(context.Context, forge.ProjectRef, string, forge.Cursor) ([]forge.Issue, forge.Cursor, error) {
	return nil, "", nil
}
func (f *phaseForge) GetIssue(context.Context, forge.ProjectRef, string) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *phaseForge) ListIssueComments(context.Context, forge.ProjectRef, string, forge.Cursor) ([]forge.Comment, forge.Cursor, error) {
	return nil, "", nil
}
func (f *phaseForge) ListLabelEvents(context.Context, forge.ProjectRef, forge.TargetRef, forge.Cursor) ([]forge.LabelEvent, forge.Cursor, error) {
	return nil, "", nil
}
func (f *phaseForge) CommentTarget(context.Context, forge.ProjectRef, forge.TargetRef, string) (string, error) {
	return "", nil
}
func (f *phaseForge) SetLabels(context.Context, forge.ProjectRef, forge.TargetRef, []string, []string) error {
	return nil
}
func (f *phaseForge) CreateChange(context.Context, forge.ProjectRef, string, string, string, string) (forge.Change, error) {
	return forge.Change{}, nil
}
func (f *phaseForge) FindChangeForCreateOperation(context.Context, forge.ProjectRef, string, string, string) (*forge.Change, forge.FindResult, error) {
	return nil, forge.NoMatch, nil
}
func (f *phaseForge) GetChange(context.Context, forge.ProjectRef, string) (forge.Change, error) {
	f.calls++
	sha := strings.Repeat("a", 40)
	if f.drift && f.calls == 2 {
		sha = strings.Repeat("b", 40)
	}
	return forge.Change{ID: "42", URL: "https://forge.example/42", HeadSHA: sha, State: forge.ChangeOpen, Mergeability: forge.Mergeable, ReviewState: forge.Approved}, nil
}
func (f *phaseForge) GetChangeDiff(context.Context, forge.ProjectRef, string) (string, error) {
	return "diff --git a/" + f.path + " b/" + f.path + "\n+++ b/" + f.path, nil
}
func (f *phaseForge) ListChangeComments(context.Context, forge.ProjectRef, string, forge.Cursor) ([]forge.Comment, forge.Cursor, error) {
	return nil, "", nil
}
func (f *phaseForge) GetChecks(context.Context, forge.ProjectRef, string) (forge.CheckSuite, error) {
	s := forge.CheckSuite{Conclusion: f.checks, ExternalURL: "https://ci.example/run"}
	if f.checks == "failure" {
		s.FailedJobs = []forge.CheckJob{{ID: "job-1", Name: "test", WebURL: "https://ci.example/job-1"}}
	}
	return s, nil
}
func (f *phaseForge) MergeChange(context.Context, forge.ProjectRef, string, string, string) (forge.Change, error) {
	return forge.Change{}, nil
}

var _ forge.Client = (*phaseForge)(nil)
