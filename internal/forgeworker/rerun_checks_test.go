package forgeworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

type rerunClientFunc func(context.Context, forge.ProjectRef, string, string) error

func (f rerunClientFunc) RerunCheck(ctx context.Context, p forge.ProjectRef, id, head string) error {
	return f(ctx, p, id, head)
}

func seedRerunWorkerOperation(t *testing.T, db *storage.DB, extra map[string]any) {
	t.Helper()
	ctx := context.Background()
	if err := db.SeedProjectForTest(ctx, "cfg-rerun", "p-rerun", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-rerun", "p-rerun", "cfg-rerun", "42", cwNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRunChangeHeadForTest(ctx, "run-rerun", "change-1", "head-frozen"); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"run_id": "run-rerun", "change_id": "change-1", "head_sha": "head-frozen",
		"check_run_id": "77", "retry_no": 1,
		"triage_source_digest":  strings.Repeat("a", 64),
		"created_from_event_id": "event:gate-source",
	}
	for k, v := range extra {
		payload[k] = v
	}
	body, err := storage.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	op := storage.Operation{Key: storage.RerunChecksOperationKey("run-rerun", "head-frozen", "77", 1), Kind: storage.OperationRerunChecks, RunID: "run-rerun", Payload: body}
	if _, err := db.EnqueueOperation(ctx, op, cwNow); err != nil {
		t.Fatal(err)
	}
}

func TestRerunChecksWorkerMarksBeforeForgeAndCompletes(t *testing.T) {
	db := openWorkerDB(t)
	seedRerunWorkerOperation(t, db, nil)
	var order []string
	client := rerunClientFunc(func(_ context.Context, p forge.ProjectRef, id, head string) error {
		order = append(order, "forge")
		if p.Kind != forge.KindGitHub || p.Host != "github.com" || p.ProjectKey != "org/repo-p-rerun" || id != "77" || head != "head-frozen" {
			t.Fatalf("rerun args=%+v %s %s", p, id, head)
		}
		return nil
	})
	var got storage.CompleteOutcome
	w := &RerunChecksWorker{
		DB: db, Clients: map[string]RerunCheckClient{"github|github.com|org/repo-p-rerun": client},
		Now: func() time.Time { return time.UnixMilli(cwNow + 1) }, Lease: time.Minute, WorkerID: "rerun",
		Mark: func(context.Context, storage.ClaimedOperation, int64) error {
			order = append(order, "mark")
			return nil
		},
		Complete: func(_ context.Context, _ storage.ClaimedOperation, o storage.CompleteOutcome) error {
			order = append(order, "complete")
			got = o
			return nil
		},
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "mark,forge,complete" || got.State != storage.OperationSucceeded {
		t.Fatalf("order=%v outcome=%+v", order, got)
	}
}

func TestRerunChecksWorkerPostStartFailureIsConflict(t *testing.T) {
	db := openWorkerDB(t)
	seedRerunWorkerOperation(t, db, nil)
	calls := 0
	client := rerunClientFunc(func(context.Context, forge.ProjectRef, string, string) error {
		calls++
		return &forge.ClassifiedError{Class: forge.ErrRateLimited, Summary: "response lost after request"}
	})
	var got storage.CompleteOutcome
	w := &RerunChecksWorker{
		DB: db, Clients: map[string]RerunCheckClient{"github|github.com|org/repo-p-rerun": client},
		Now: func() time.Time { return time.UnixMilli(cwNow + 1) }, Lease: time.Minute, WorkerID: "rerun",
		Mark: func(context.Context, storage.ClaimedOperation, int64) error { return nil },
		Complete: func(_ context.Context, _ storage.ClaimedOperation, o storage.CompleteOutcome) error {
			got = o
			return nil
		},
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.State != storage.OperationConflict || got.ErrorClass != storage.ErrorRateLimited {
		t.Fatalf("calls=%d outcome=%+v", calls, got)
	}
}

func TestRerunChecksWorkerCompleteLossDoesNotRetryInline(t *testing.T) {
	db := openWorkerDB(t)
	seedRerunWorkerOperation(t, db, nil)
	calls := 0
	lost := errors.New("complete lost")
	w := &RerunChecksWorker{
		DB: db, Clients: map[string]RerunCheckClient{"github|github.com|org/repo-p-rerun": rerunClientFunc(func(context.Context, forge.ProjectRef, string, string) error { calls++; return nil })},
		Now: func() time.Time { return time.UnixMilli(cwNow + 1) }, Lease: time.Minute, WorkerID: "rerun",
		Mark:     func(context.Context, storage.ClaimedOperation, int64) error { return nil },
		Complete: func(context.Context, storage.ClaimedOperation, storage.CompleteOutcome) error { return lost },
	}
	if err := w.RunOnce(context.Background()); !errors.Is(err, lost) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestRerunChecksWorkerRejectsUnknownPayloadBeforeMark(t *testing.T) {
	db := openWorkerDB(t)
	seedRerunWorkerOperation(t, db, map[string]any{"unexpected": true})
	marked := false
	var got storage.CompleteOutcome
	w := &RerunChecksWorker{
		DB: db, Now: func() time.Time { return time.UnixMilli(cwNow + 1) }, Lease: time.Minute, WorkerID: "rerun",
		Mark: func(context.Context, storage.ClaimedOperation, int64) error { marked = true; return nil },
		Complete: func(_ context.Context, _ storage.ClaimedOperation, o storage.CompleteOutcome) error {
			got = o
			return nil
		},
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if marked || got.State != storage.OperationFailed || got.ErrorClass != storage.ErrorContract {
		t.Fatalf("marked=%v outcome=%+v", marked, got)
	}
}
