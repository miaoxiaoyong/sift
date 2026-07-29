package forgeworker

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
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
}
