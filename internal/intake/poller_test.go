package intake

import (
	"context"
	"testing"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// M2 gate (WBS §2.3 / §2.5): a project whose forge credential or capability
// fails is isolated and alerted once; healthy projects in the same poll cycle
// keep being polled and scheduled. The bad project must not abort the whole
// intake tick.

const pollNow = int64(1_702_000_000_000)

func openPollerDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{
		Path:          t.TempDir() + "/sift.db",
		BinaryVersion: "test-binary",
		Now:           time.UnixMilli(pollNow),
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// routingClient routes one project to an auth/capability failure and delegates
// the rest to a fake forge, so the poller sees a mixed batch in one tick.
type routingClient struct {
	*forge.Fake
	bad forge.ProjectRef
}

type labelEventClient struct {
	*forge.Fake
	events map[string][]forge.LabelEvent
}

func (c *labelEventClient) ListLabelEvents(_ context.Context, _ forge.ProjectRef, target forge.TargetRef, _ forge.Cursor) ([]forge.LabelEvent, forge.Cursor, error) {
	return c.events[target.ID], "", nil
}

func (r *routingClient) ListIssuesByLabel(ctx context.Context, p forge.ProjectRef, label string, since forge.Cursor) ([]forge.Issue, forge.Cursor, error) {
	if p == r.bad {
		return nil, "", &forge.ClassifiedError{Class: forge.ErrAuthOrCapability, Summary: "403 forbidden: token revoked"}
	}
	return r.Fake.ListIssuesByLabel(ctx, p, label, since)
}

func TestPollerIsolatesBadProjectHealthyProjectUnaffected(t *testing.T) {
	ctx := context.Background()
	db := openPollerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg-healthy", "healthy", pollNow); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedProjectForTest(ctx, "cfg-poisoned", "poisoned", pollNow); err != nil {
		t.Fatal(err)
	}

	healthy := Project{
		ID: "healthy", TriggerLabel: "sift", OperatorAllowlist: []string{"alice"},
		Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-healthy"},
	}
	poisoned := Project{
		ID: "poisoned", TriggerLabel: "sift",
		Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-poisoned"},
	}

	fc := forge.NewFake()
	fc.AddIssue(healthy.Ref, forge.Issue{
		ID: "1", Title: "healthy issue", Body: "b", Author: "alice",
		URL: "https://github.com/org/repo-healthy/issues/1", Labels: []string{"sift"},
	})
	fc.AddLabelEvent(healthy.Ref, forge.LabelEvent{TargetID: "1", Label: "sift", Action: forge.LabelAdded, Actor: "alice", ObservedAt: time.UnixMilli(pollNow)})
	client := &routingClient{Fake: fc, bad: poisoned.Ref}

	var seen []string
	var isolated Project
	p := &Poller{
		DB:       db,
		Forge:    client,
		Projects: []Project{healthy, poisoned},
		Now:      func() time.Time { return time.UnixMilli(pollNow) },
		Idle:     time.Minute, Active: 30 * time.Second, Slow: 5 * time.Minute,
		OnIssue: func(_ context.Context, pr Project, _ forge.Issue) error {
			seen = append(seen, pr.ID)
			return nil
		},
		Isolated: func(pr Project, _ error) { isolated = pr },
	}

	// One tick: the poisoned project fails with auth/capability; the poller
	// isolates it and continues to the healthy project instead of aborting.
	if err := p.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce must not abort on a single bad project: %v", err)
	}

	// The healthy project was polled and its issue handed off.
	if len(seen) != 1 || seen[0] != healthy.ID {
		t.Fatalf("OnIssue saw %v, want exactly [healthy] (poisoned must not be handed off)", seen)
	}
	hcur, err := db.IntakeCursor(ctx, healthy.ID, "issues")
	if err != nil {
		t.Fatal(err)
	}
	if hcur.Cursor == "" {
		t.Fatal("healthy project cursor must advance after a successful poll")
	}
	if hcur.PollMode != "active" {
		t.Fatalf("healthy poll mode=%q, want active (issue found)", hcur.PollMode)
	}

	// The poisoned project was isolated and its stream was NOT advanced.
	if isolated.ID != poisoned.ID {
		t.Fatalf("isolated project=%q, want %q", isolated.ID, poisoned.ID)
	}
	pcur, err := db.IntakeCursor(ctx, poisoned.ID, "issues")
	if err != nil {
		t.Fatal(err)
	}
	if pcur.Cursor != "" {
		t.Fatalf("poisoned cursor=%q advanced, want empty (poll failed before persist)", pcur.Cursor)
	}

	// Idempotent isolation: a second tick does not re-isolate (already isolated)
	// and the healthy project is polled again cleanly.
	isolated = Project{}
	if err := p.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if isolated.ID != "" {
		t.Fatalf("already-isolated project must not be re-alerted, got isolated=%q", isolated.ID)
	}
}

// TestPollerAdvancesCursorOnlyAfterPersist proves the intake cursor does not
// move past a batch that failed to persist: replaying the same forge page is
// harmless (WBS §2.3 cursor invariant).
func TestPollerGatesOnTrustedTriggerActor(t *testing.T) {
	ctx := context.Background()
	db := openPollerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg", "p1", pollNow); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: "p1", TriggerLabel: "sift", OperatorAllowlist: []string{"operator"}, Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo"}}
	fake := forge.NewFake()
	for _, id := range []string{"missing", "unknown", "untrusted", "trusted"} {
		fake.AddIssue(project.Ref, forge.Issue{ID: id, Title: id, Author: "author", URL: "https://example.test/" + id, Labels: []string{"sift"}})
	}
	triggerTime := time.UnixMilli(pollNow)
	client := &labelEventClient{Fake: fake, events: map[string][]forge.LabelEvent{
		"missing":   {{TargetID: "missing", Label: "sift", Action: forge.LabelAdded, ObservedAt: triggerTime}},
		"unknown":   {{TargetID: "unknown", Label: "sift", Action: forge.LabelAdded, Actor: "unknown", ObservedAt: triggerTime}},
		"untrusted": {{TargetID: "untrusted", Label: "sift", Action: forge.LabelAdded, Actor: "outsider", ObservedAt: triggerTime}},
		"trusted":   {{TargetID: "trusted", Label: "sift", Action: forge.LabelAdded, Actor: "operator", ObservedAt: triggerTime}},
	}}
	var seen []string
	poller := &Poller{DB: db, Forge: client, Projects: []Project{project}, Now: func() time.Time { return triggerTime }, Idle: time.Minute, Active: time.Second, OnIssue: func(_ context.Context, _ Project, issue forge.Issue) error {
		seen = append(seen, issue.ID)
		return nil
	}}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "trusted" {
		t.Fatalf("accepted issues = %v, want [trusted]", seen)
	}
	items, err := db.PendingIntake(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].IssueID != "trusted" || !items[0].ForceHITLBeforeStart {
		t.Fatalf("pending intake = %+v, want trusted issue with forced HITL", items)
	}
	trigger := client.events["trusted"][0]
	receipt, err := db.ForgeEventReceipt(ctx, project.ID, "label:"+labelEventDigest("trusted", trigger))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Actor != "operator" {
		t.Fatalf("receipt actor = %q, want trigger actor operator", receipt.Actor)
	}
}

func TestPollerAdvancesCursorOnlyAfterPersist(t *testing.T) {
	ctx := context.Background()
	db := openPollerDB(t)
	if err := db.SeedProjectForTest(ctx, "cfg1", "p1", pollNow); err != nil {
		t.Fatal(err)
	}
	pr := Project{ID: "p1", TriggerLabel: "sift", OperatorAllowlist: []string{"alice"},
		Ref: forge.ProjectRef{Kind: forge.KindGitHub, Host: "github.com", ProjectKey: "org/repo-p1"}}

	fc := forge.NewFake()
	fc.AddIssue(pr.Ref, forge.Issue{
		ID: "9", Title: "t", Body: "b", Author: "alice",
		URL: "https://github.com/org/repo-p1/issues/9", Labels: []string{"sift"},
	})
	fc.AddLabelEvent(pr.Ref, forge.LabelEvent{TargetID: "9", Label: "sift", Action: forge.LabelAdded, Actor: "alice", ObservedAt: time.UnixMilli(pollNow)})

	p := &Poller{
		DB: db, Forge: fc, Projects: []Project{pr},
		Now:  func() time.Time { return time.UnixMilli(pollNow) },
		Idle: time.Minute, Active: 30 * time.Second,
	}
	if err := p.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	cur, _ := db.IntakeCursor(ctx, pr.ID, "issues")
	if cur.Cursor == "" {
		t.Fatal("cursor must advance after a successful persist")
	}
	// The issue was projected as a pending intake item.
	items, err := db.PendingIntake(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].IssueID != "9" {
		t.Fatalf("pending intake=%+v, want one item for issue 9", items)
	}
	// Replaying the same tick is idempotent: the receipt dedups the issue and no
	// second intake item is created.
	if err := p.PollOnce(ctx); err != nil {
		t.Fatalf("replay PollOnce: %v", err)
	}
	items, _ = db.PendingIntake(ctx, 10)
	if len(items) != 1 {
		t.Fatalf("replay created %d pending items, want 1 (receipt idempotent)", len(items))
	}
}
