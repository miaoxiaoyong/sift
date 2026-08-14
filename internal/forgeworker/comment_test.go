package forgeworker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// V7 comment-worker crash-marker convergence (WBS M2 §2.3 / §2.5, outbox.md §5,
// forge.md §4.5): a clarification comment that succeeded remotely but whose
// local completion did not commit (crash before commit) must NOT be re-sent on
// recovery. The worker re-lists comments and converges on the embedded
// operation marker, completing the operation as succeeded with the original
// comment id and never calling CommentTarget a second time.

const (
	cwNow   = int64(1_700_000_000_000)
	cwLease = 5 * time.Second
)

func openWorkerDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path:          t.TempDir() + "/sift.db",
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(cwNow),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// enqueueClarificationComment creates a forge_comment operation with the same
// payload shape PersistIntakeDecision produces, without needing a brain call.
func enqueueClarificationComment(t *testing.T, db *storage.DB) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"project_id": "p1", "forge_kind": "github", "forge_host": "github.com",
		"forge_project_key": "org/repo", "target_kind": "issue", "target_id": "42",
		"purpose": "intake-clarification", "intake_id": "ii-1", "generation": 1,
		"markdown": "Sift needs clarification: is this a bug or feature?",
	})
	if err != nil {
		t.Fatal(err)
	}
	op := storage.Operation{
		Key:     storage.CommentOperationKey("intake-clarification", "ii-1", 1),
		Kind:    storage.OperationForgeComment,
		Payload: body,
	}
	if _, err := db.EnqueueOperation(context.Background(), op, cwNow); err != nil {
		t.Fatalf("EnqueueOperation: %v", err)
	}
}

// countingFake wraps forge.Fake and counts CommentTarget sends so the test can
// prove the recovery path does not re-post.
type countingFake struct {
	*forge.Fake
	mu       sync.Mutex
	sends    int
	lastBody string
}

func (c *countingFake) CommentTarget(ctx context.Context, p forge.ProjectRef, t forge.TargetRef, body string) (string, error) {
	c.mu.Lock()
	c.sends++
	c.lastBody = body
	c.mu.Unlock()
	return c.Fake.CommentTarget(ctx, p, t, body)
}

func (c *countingFake) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sends
}

func TestV7CommentWorkerCrashReplayNoResend(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", "p1", cwNow); err != nil {
		t.Fatal(err)
	}
	enqueueClarificationComment(t, db)

	fc := &countingFake{Fake: forge.NewFake()}
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	fc.AddIssue(ref, forge.Issue{ID: "42", Title: "t", Body: "b", Author: "alice", URL: "https://x/42"})

	// Complete simulates the crash: the first finish (after the remote send
	// succeeded) returns an error instead of committing, so the operation stays
	// "executing" with an outstanding lease. The second finish commits normally
	// and the test captures its outcome.
	var recovered storage.CompleteOutcome
	crashed := false
	w := &CommentWorker{
		DB:       db,
		Client:   fc,
		WorkerID: "worker-1",
		Lease:    cwLease,
		Complete: func(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
			if !crashed {
				crashed = true
				return errors.New("crash before local commit")
			}
			recovered = o
			return db.CompleteOutboxAttempt(ctx, c, o)
		},
	}

	// Run 1: no prior comment → worker sends the comment (remote success), then
	// "crashes" before committing completion. Exactly one send so far.
	t0 := time.UnixMilli(cwNow)
	w.Now = func() time.Time { return t0 }
	if err := w.RunOnce(ctx); err == nil {
		t.Fatal("run 1 must surface the simulated crash error")
	}
	if n := fc.sentCount(); n != 1 {
		t.Fatalf("after run 1: CommentTarget sends=%d, want 1", n)
	}
	// The posted comment carries an embedded operation marker (forge.md §4.5).
	if !strings.Contains(fc.lastBody, "sift-op:v1") {
		t.Fatalf("posted body must embed an operation marker: %q", fc.lastBody)
	}

	// Run 2: the lease has expired; the worker reclaims, re-lists comments,
	// finds the marker from run 1 and converges WITHOUT re-sending.
	t1 := t0.Add(cwLease + time.Second)
	w.Now = func() time.Time { return t1 }
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run 2 (recovery): %v", err)
	}
	if n := fc.sentCount(); n != 1 {
		t.Fatalf("after recovery: CommentTarget sends=%d, want 1 (no resend)", n)
	}
	if recovered.State != storage.OperationSucceeded {
		t.Fatalf("recovery outcome state=%q, want succeeded", recovered.State)
	}
	if !strings.Contains(string(recovered.Evidence), "comment_id") {
		t.Fatalf("recovery evidence must carry the comment id: %s", recovered.Evidence)
	}

	// Run 3: nothing is claimable — the operation is terminal (succeeded), so a
	// recovery scan finds no outstanding work. This proves the operation did not
	// stay mid-flight and will not be reprocessed.
	if c, err := db.ClaimOutboxOperation(ctx, "worker-probe", t1.Add(time.Second).UnixMilli(), cwLease.Milliseconds()); err != nil || c != nil {
		t.Fatalf("post-recovery claim must be empty (operation terminal): c=%v err=%v", c, err)
	}

	// Only one comment exists remotely: the recovery path found it via marker,
	// it did not add a duplicate.
	comments, _, err := fc.ListIssueComments(ctx, ref, "42", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("remote comments=%d, want 1 (marker convergence, no duplicate)", len(comments))
	}
}

// TestV7CommentWorkerFreshSendEmbedsMarkerAndCompletes is the non-crash control:
// with no prior comment the worker posts exactly once, embeds the marker, and
// completes succeeded. It pins the happy path the crash test recovers from.
func TestV7CommentWorkerFreshSendEmbedsMarkerAndCompletes(t *testing.T) {
	ctx := context.Background()
	db := openWorkerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", "p1", cwNow); err != nil {
		t.Fatal(err)
	}
	enqueueClarificationComment(t, db)

	fc := &countingFake{Fake: forge.NewFake()}
	ref := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	fc.AddIssue(ref, forge.Issue{ID: "42", Title: "t", Body: "b", Author: "alice", URL: "https://x/42"})

	var got storage.CompleteOutcome
	w := &CommentWorker{
		DB:       db,
		Client:   fc,
		WorkerID: "worker-1",
		Lease:    cwLease,
		Now:      func() time.Time { return time.UnixMilli(cwNow) },
		Complete: func(ctx context.Context, c storage.ClaimedOperation, o storage.CompleteOutcome) error {
			got = o
			return db.CompleteOutboxAttempt(ctx, c, o)
		},
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n := fc.sentCount(); n != 1 {
		t.Fatalf("sends=%d, want 1", n)
	}
	if !strings.Contains(fc.lastBody, "sift-op:v1") {
		t.Fatalf("body must embed marker: %q", fc.lastBody)
	}
	if got.State != storage.OperationSucceeded {
		t.Fatalf("state=%q, want succeeded", got.State)
	}
}
