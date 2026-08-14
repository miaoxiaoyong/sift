package forgeworker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/xsift/sift/internal/brain"
	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/gate"
	"github.com/xsift/sift/internal/storage"
)

func enqueueMerge(t *testing.T, db *storage.DB, head string) {
	t.Helper()
	p, err := json.Marshal(mergeChangePayload{ProjectID: "p1", RunID: "r1", ChangeID: "c1", GateEvaluationID: "gate-" + head, ExpectedHeadSHA: head, Method: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOperation(context.Background(), storage.Operation{Key: storage.MergeChangeOperationKey("r1", head), Kind: storage.OperationMergeChange, RunID: "r1", Payload: p}, cwNow); err != nil {
		t.Fatal(err)
	}
}

func mergeState(t *testing.T, db *storage.DB, head string) storage.OperationState {
	t.Helper()
	q, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var state storage.OperationState
	if err := q.QueryRow(`SELECT state FROM outbox_operations WHERE operation_key=?`, storage.MergeChangeOperationKey("r1", head)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestMergeWorkerRequiresProductionGateForReplacementHead proves that a stale
// Gate(A) operation cannot merge B, and that B reaches the worker only after a
// second production Gate reconciliation freezes B's facts and evaluation.
func TestMergeWorkerRequiresProductionGateForReplacementHead(t *testing.T) {
	testMergeWorkerProductionReplacementHead(t, false)
}

func TestMergeWorkerRecognizesAlreadyMergedReplacementHeadFromProductionGate(t *testing.T) {
	testMergeWorkerProductionReplacementHead(t, true)
}

func testMergeWorkerProductionReplacementHead(t *testing.T, mergedB bool) {
	ctx := context.Background()
	db := openWorkerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", "p1", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedGateCandidateForTest(ctx, "r1", "p1", "cfg1", "c1", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedCertificationForTest(ctx, "feature", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateProjectAutoMergeCapability(ctx, "p1", true, "test", cwNow); err != nil {
		t.Fatal(err)
	}
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p1"}
	f := &productionMergeForge{Fake: forge.NewFake()}
	headA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f.AddChange(ref, "c1", headA)
	repo := mergePolicyRepo(t)
	provider := &brain.FakeProvider{Responses: []brain.FakeResponse{{ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}, {ResultText: `{"risk_score":1,"risk_points":["small"],"rationale":"bounded"}`}}}
	r := &gate.Reconciler{DB: db, Forge: f, Brain: brain.NewShell(db, config.Brain{Executable: "fake", DailyTokenLimit: 100, MaxInputBytes: 1 << 20, MaxRawOutputBytes: 1 << 20}, provider, func() time.Time { return time.UnixMilli(cwNow) }), ProjectID: "p1", Project: ref, Repo: repo, Defaults: config.GateDefaults{ReviewPolicy: config.ReviewPolicyNever, RiskyReviewThreshold: 100, AutoMerge: true, ChecksPendingTimeout: time.Hour, FlakyRetryLimit: 1}, Certification: config.DefaultConfig().Certification, Now: func() time.Time { return time.UnixMilli(cwNow) }}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f.AddChange(ref, "c1", headB)
	w := &MergeWorker{DB: db, Client: f, ProjectID: "p1", WorkerID: "w", Lease: cwLease, Now: func() time.Time { return time.UnixMilli(cwNow) }}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mergeState(t, db, headA); got != storage.OperationStale {
		t.Fatalf("Gate(A) operation=%s, want stale against B", got)
	}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mergedB {
		if _, err := f.InjectMerged(ref, "c1", time.UnixMilli(cwNow)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mergeState(t, db, headB); got != storage.OperationSucceeded {
		t.Fatalf("Gate(B) operation=%s, want succeeded", got)
	}
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var evidence string
	if err := check.QueryRow(`SELECT remote_evidence_json FROM outbox_operations WHERE operation_key=?`, storage.MergeChangeOperationKey("r1", headB)).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(evidence), &got); err != nil || got["state"] != "merged" || got["head_sha"] != headB || got["merge_sha"] != "merge-"+headB {
		t.Fatalf("Gate(B) merge evidence=%#v err=%v, want merge SHA %q", got, err, "merge-"+headB)
	}
}

type productionMergeForge struct{ *forge.Fake }

func (f *productionMergeForge) GetChange(ctx context.Context, p forge.ProjectRef, id string) (forge.Change, error) {
	c, err := f.Fake.GetChange(ctx, p, id)
	c.URL, c.Mergeability, c.ReviewState = "https://forge.example/"+id, forge.Mergeable, forge.Approved
	return c, err
}
func (f *productionMergeForge) GetChangeDiff(context.Context, forge.ProjectRef, string) (string, error) {
	return "diff --git a/cmd/a.go b/cmd/a.go\n+++ b/cmd/a.go", nil
}
func (f *productionMergeForge) GetChecks(context.Context, forge.ProjectRef, string) (forge.CheckSuite, error) {
	return forge.CheckSuite{Conclusion: "success", ExternalURL: "https://ci.example/1"}, nil
}

func mergePolicyRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Sift test"}, {"commit", "--allow-empty", "-m", "initial"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

func TestMergeWorkerStalesOldOperationWhenNewHeadIsAlreadyMerged(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", "p1", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "r1", "p1", "cfg1", "i1", cwNow); err != nil {
		t.Fatal(err)
	}
	f := forge.NewFake()
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p1"}
	f.AddChange(ref, "c1", "head-b")
	if _, err := f.InjectMerged(ref, "c1", time.UnixMilli(cwNow)); err != nil {
		t.Fatal(err)
	}
	enqueueMerge(t, db, "head-a")
	w := &MergeWorker{DB: db, Client: f, ProjectID: "p1", WorkerID: "w", Lease: cwLease, Now: func() time.Time { return time.UnixMilli(cwNow) }}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mergeState(t, db, "head-a"); got != storage.OperationStale {
		t.Fatalf("old operation state=%s, want stale", got)
	}
}

func TestMergeWorkerUsesGateHeadCASAndStalesOldOperation(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", "p1", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "r1", "p1", "cfg1", "i1", cwNow); err != nil {
		t.Fatal(err)
	}
	f := forge.NewFake()
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p1"}
	f.AddChange(ref, "c1", "head-b")
	enqueueMerge(t, db, "head-a")
	w := &MergeWorker{DB: db, Client: f, ProjectID: "p1", WorkerID: "w", Lease: cwLease, Now: func() time.Time { return time.UnixMilli(cwNow) }}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mergeState(t, db, "head-a"); got != storage.OperationStale {
		t.Fatalf("old operation state=%s, want stale", got)
	}
	c, err := f.GetChange(ctx, ref, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != forge.ChangeOpen {
		t.Fatalf("old Gate operation merged new head: %+v", c)
	}

	enqueueMerge(t, db, "head-b")
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mergeState(t, db, "head-b"); got != storage.OperationSucceeded {
		t.Fatalf("new operation state=%s, want succeeded", got)
	}
	c, err = f.GetChange(ctx, ref, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != forge.ChangeMerged {
		t.Fatalf("new Gate operation did not merge: %+v", c)
	}
	check, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var evidence string
	if err := check.QueryRow(`SELECT remote_evidence_json FROM outbox_operations WHERE operation_key=?`, storage.MergeChangeOperationKey("r1", "head-b")).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(evidence), &got); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "merged" || got["head_sha"] != "head-b" || got["merge_sha"] != "merge-head-b" {
		t.Fatalf("merge evidence = %#v, want merged expected head and merge SHA %q", got, "merge-head-b")
	}
}
