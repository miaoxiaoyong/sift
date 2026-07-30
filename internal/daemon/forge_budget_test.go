package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/forge"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// End-to-end: the Forge adapter charges through the real storage budget port.
// After the hourly limit is reached the adapter refuses with forge.ErrRateLimited
// without launching the CLI, and the consumption is persisted so a restart keeps
// the same bucket (forge.md §9, storage.md §9.1).

const e2eNow = int64(1_699_999_200_000) // 2023-11-14 22:00:00 UTC

func openE2EDB(t *testing.T) *storage.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sift.db")
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path:          path,
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(e2eNow),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAdapterEnforcesPersistedBudget(t *testing.T) {
	ctx := context.Background()
	db := openE2EDB(t)
	// SeedProjectForTest inserts forge github.com / org/repo-proj-1.
	if err := db.SeedProjectForTest(ctx, "cfg-1", "proj-1", e2eNow); err != nil {
		t.Fatalf("seed: %v", err)
	}

	launched := 0
	run := func(_ context.Context, _ string, _ []string, _ []byte) ([]byte, []byte, error) {
		launched++
		return []byte(`{"number":1,"title":"t","body":"b","html_url":"https://x/1","state":"open","user":{"login":"a"},"labels":[{"name":"sift"}]}`), nil, nil
	}
	now := time.UnixMilli(e2eNow)
	ch := &forgeBudgetCharger{DB: db, Limit: 2, WarningRatio: 0.8, Now: func() time.Time { return now }}
	a := forge.NewGitHub("gh", run).WithCharger(ch)

	project := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-proj-1"}
	callCtx := forge.WithChargeKey(ctx, "forge-call:att-1")

	// Two distinct requests consume the limit of 2.
	if _, err := a.GetIssue(callCtx, project, "1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := a.GetIssue(callCtx, project, "1"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if launched != 2 {
		t.Fatalf("launched = %d, want 2", launched)
	}

	// Third request is refused before the CLI runs.
	pre := launched
	_, err := a.GetIssue(callCtx, project, "1")
	if !errors.Is(err, forge.ErrRateLimited) {
		t.Fatalf("third call err = %v, want forge.ErrRateLimited", err)
	}
	if launched != pre {
		t.Fatalf("CLI launched on budget refusal: %d -> %d", pre, launched)
	}
}

func TestAdapterBudgetSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sift.db")
	open := func() *storage.DB {
		db, err := storage.Open(ctx, storage.OpenConfig{
			Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(e2eNow),
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return db
	}

	db := open()
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SeedProjectForTest(ctx, "cfg-1", "proj-1", e2eNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	project := forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-proj-1"}
	now := time.UnixMilli(e2eNow)

	// Process 1: consume the whole limit of 1.
	ch1 := &forgeBudgetCharger{DB: db, Limit: 1, WarningRatio: 0.8, Now: func() time.Time { return now }}
	a1 := forge.NewGitHub("gh", func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return []byte(`{"number":1,"title":"t","body":"b","html_url":"https://x/1","state":"open","user":{"login":"a"}}`), nil, nil
	}).WithCharger(ch1)
	if _, err := a1.GetIssue(forge.WithChargeKey(ctx, "forge-call:att-1"), project, "1"); err != nil {
		t.Fatalf("first process call: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Process 2: same bucket file — the limit is still exhausted, so a new
	// request is refused without recounting.
	db2 := open()
	t.Cleanup(func() { _ = db2.Close() })
	ch2 := &forgeBudgetCharger{DB: db2, Limit: 1, WarningRatio: 0.8, Now: func() time.Time { return now }}
	a2 := forge.NewGitHub("gh", func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		t.Fatal("CLI must not launch when the persisted bucket is exhausted")
		return nil, nil, nil
	}).WithCharger(ch2)
	_, err := a2.GetIssue(forge.WithChargeKey(ctx, "forge-call:att-2"), project, "1")
	if !errors.Is(err, forge.ErrRateLimited) {
		t.Fatalf("post-restart call err = %v, want forge.ErrRateLimited", err)
	}
}
