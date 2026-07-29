package gate_test

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/gate"
	"github.com/miaoxiaoyong/sift/internal/replay"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// TestReconcilerPhaseEvidence exercises the production reconciliation boundary,
// rather than assembling Gate inputs in a component test. The exported traces
// are then replayed with both production replay runners.
func TestReconcilerPhaseEvidence(t *testing.T) {
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
