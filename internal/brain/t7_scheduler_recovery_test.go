package brain

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xsift/sift/internal/storage"
)

func openT7DBAt(t *testing.T, path string, now int64) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: path, BinaryVersion: "test-binary", Now: time.UnixMilli(now)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestT7SchedulerRestartFinishesTerminalCallWithoutProviderReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sift.db")
	db := openT7DBAt(t, path, 300)
	evidenceID := seedT7SchedulerDB(t, db)
	appendT7Fixture(t, db, "global", "", "bug")
	pending, err := db.PendingT7Aggregates(ctx, 300, 1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	a := pending[0]
	input := t7InputFromStorage(a)
	canonical, err := BuildT7Input(input)
	if err != nil {
		t.Fatal(err)
	}
	firstProvider := &FakeProvider{Responses: []FakeResponse{{ResultText: t7ProviderOutput("global", "policy", evidenceID), InputTokens: 1, OutputTokens: 1}}}
	firstShell := NewShell(db, shellCfg(100), firstProvider, func() time.Time { return time.UnixMilli(300) })
	result, err := firstShell.Call(ctx, T7Contract(a.AggregateKey, "", nil, t7EvidenceIDs(input)), CallParams{
		Scope: storage.BrainScopeAggregate, SubjectKey: a.AggregateKey, Input: canonical, T7AggregateOnce: true,
	})
	if err != nil || result.Status != storage.BrainCallValid {
		t.Fatalf("first call status=%s err=%v", result.Status, err)
	}
	// Simulate a crash after terminal trace commit but before draft/cursor.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openT7DBAt(t, path, 301)
	defer db.Close()
	restartProvider := &FakeProvider{}
	restartShell := NewShell(db, shellCfg(100), restartProvider, func() time.Time { return time.UnixMilli(301) })
	scheduler := &T7Scheduler{DB: db, Shell: restartShell, Now: func() time.Time { return time.UnixMilli(301) }}
	if err := scheduler.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(restartProvider.Requests) != 0 {
		t.Fatalf("restart repeated provider call: %d", len(restartProvider.Requests))
	}
	draft, err := db.ProposalDraft(ctx, result.CallID)
	if err != nil || draft.Status != "pending_human_approval" {
		t.Fatalf("draft status=%s err=%v", draft.Status, err)
	}
}

func TestT7SchedulerRecoveryFallbackDoesNotRetryAmbiguousProviderWindow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sift.db")
	db := openT7DBAt(t, path, 300)
	seedT7SchedulerDB(t, db)
	appendT7Fixture(t, db, "global", "", "bug")
	pending, err := db.PendingT7Aggregates(ctx, 300, 1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	a := pending[0]
	input := t7InputFromStorage(a)
	canonical, err := BuildT7Input(input)
	if err != nil {
		t.Fatal(err)
	}
	message := BuildMessage(T7Asset(), canonical)
	reserved, err := db.ReserveBrainCall(ctx, storage.ReserveBrainCallCmd{
		Scope: storage.BrainScopeAggregate, SubjectKey: a.AggregateKey, Touchpoint: "T7",
		PromptVersion: T7Asset().PromptVersion, OutputSchemaVersion: T7Asset().OutputSchemaVersion,
		InputJSON: canonical, InputDigest: DigestBytes(message), StartedAtMS: 300, T7AggregateOnce: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openT7DBAt(t, path, 301)
	defer db.Close()
	provider := &FakeProvider{}
	shell := NewShell(db, shellCfg(100), provider, func() time.Time { return time.UnixMilli(301) })
	scheduler := &T7Scheduler{DB: db, Shell: shell, Now: func() time.Time { return time.UnixMilli(301) }}
	if err := scheduler.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("recovery called provider: %d", len(provider.Requests))
	}
	call, _, err := db.BrainCallTrace(ctx, reserved.ID)
	if err != nil || call.Status != storage.BrainCallFallback || !strings.Contains(call.FallbackReason, "recovery") {
		t.Fatalf("call status=%s fallback=%q err=%v", call.Status, call.FallbackReason, err)
	}
	if _, err := db.ProposalDraft(ctx, reserved.ID); err == nil {
		t.Fatal("recovery fallback created draft")
	}
}

func TestT7SchedulerTokenThresholdFallbackDoesNotCreateDraft(t *testing.T) {
	db := openShellDB(t)
	seedT7SchedulerDB(t, db)
	appendT7Fixture(t, db, "global", "", "bug")
	if _, err := db.ExecForTest(context.Background(), `INSERT INTO budget_counters(kind,scope,scope_id,bucket_start_ms,bucket_end_ms,limit_value,consumed_value,version,updated_at_ms) VALUES('token','global','global',0,86400000,1,1,1,300)`); err != nil {
		t.Fatal(err)
	}
	provider := &FakeProvider{}
	shell := NewShell(db, shellCfg(1), provider, func() time.Time { return time.UnixMilli(300) })
	scheduler := &T7Scheduler{DB: db, Shell: shell, Now: func() time.Time { return time.UnixMilli(300) }}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("token threshold called provider: %d", len(provider.Requests))
	}
	assertT7FallbackTrace(t, db, "token_threshold")
}
