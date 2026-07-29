package forge

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The Fake exercises the Client contract the real adapter (M2) will honor: the
// neutral types, the required actor invariant and the error classes.
func TestFakeClientContract(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	p := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "org/repo"}

	// AddIssue normalizes labels and rejects an empty author/url/id at script
	// time — the boundary never emits a driving payload missing required fields.
	iss := f.AddIssue(p, Issue{
		ID: "1", Title: "t", Body: "b", Author: "alice",
		URL: "https://x/1", Labels: []string{"b", "a", "a", "sift"},
	})
	if got := iss.Labels[0]; got != "a" {
		t.Fatalf("labels not sorted/deduped: %v", iss.Labels)
	}

	got, next, err := f.ListIssuesByLabel(ctx, p, "sift", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("ListIssuesByLabel = %+v", got)
	}
	// Boundary records may be replayed; callers deduplicate by remote ID.
	got2, _, err := f.ListIssuesByLabel(ctx, p, "sift", next)
	if err != nil || len(got2) != 1 || got2[0].ID != "1" {
		t.Fatalf("second ListIssuesByLabel = %+v %v", got2, err)
	}
	// Filtering by a label no issue carries returns nothing.
	got3, _, err := f.ListIssuesByLabel(ctx, p, "other", "")
	if err != nil || len(got3) != 0 {
		t.Fatalf("ListIssuesByLabel other = %+v %v", got3, err)
	}

	// Label events always carry an actor; scripting one without is rejected.
	f.AddLabelEvent(p, LabelEvent{
		TargetID: "1", Label: "sift", Action: LabelAdded,
		Actor: "alice", ObservedAt: time.UnixMilli(1),
	})
	evs, _, err := f.ListLabelEvents(ctx, p, TargetRef{Kind: TargetIssue, ID: "1"}, "")
	if err != nil || len(evs) != 1 || evs[0].Actor != "alice" {
		t.Fatalf("ListLabelEvents = %+v %v", evs, err)
	}

	// A Change starts open; MergeChange injects the merge fact idempotently on
	// the project/change identity.
	f.AddChange(p, "c1", "sha1")
	ch, err := f.GetChange(ctx, p, "c1")
	if err != nil || ch.State != ChangeOpen {
		t.Fatalf("open change = %+v %v", ch, err)
	}
	merged, err := f.InjectMerged(p, "c1", time.UnixMilli(5))
	if err != nil || merged.State != ChangeMerged || merged.HeadSHA != "sha1" {
		t.Fatalf("merge = %+v %v", merged, err)
	}
	ch2, err := f.GetChange(ctx, p, "c1")
	if err != nil || ch2.State != ChangeMerged {
		t.Fatalf("merged change = %+v %v", ch2, err)
	}
}

// Unknown change/project surfaces the SemanticConflict class — the only error
// semantics the upper layer sees (DESIGN §8.1).
func TestFakeUnknownChangeIsSemanticConflict(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	p := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	_, err := f.GetChange(ctx, p, "nope")
	if !errors.Is(err, ErrSemanticConflict) {
		t.Fatalf("err = %v, want ErrSemanticConflict", err)
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Class != ErrSemanticConflict {
		t.Fatalf("err not a ClassifiedError: %v", err)
	}
	if _, err := f.InjectMerged(p, "nope", time.Now()); !errors.Is(err, ErrSemanticConflict) {
		t.Fatalf("merge unknown err = %v", err)
	}
}

func TestFakeChangeMarkerConflictAndExpectedHeadCAS(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	p := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f.AddChangeWithBody(p, "7", "head-a", OperationMarker("run:1", digest))
	change, result, err := f.FindChangeForCreateOperation(ctx, p, "run:1", digest, "branch", "main")
	if err != nil || result != MarkerHit || change == nil || change.ID != "7" {
		t.Fatalf("marker lookup: change=%+v result=%q err=%v", change, result, err)
	}
	if _, err := f.MergeChange(ctx, p, "7", "head-b", "merge"); !errors.Is(err, ErrSemanticConflict) {
		t.Fatalf("stale merge error=%v", err)
	}

	f = NewFake()
	f.AddChangeWithBody(p, "8", "head-a", "human change")
	_, result, err = f.FindChangeForCreateOperation(ctx, p, "run:1", digest, "branch", "main")
	if err != nil || result != SemanticConflict {
		t.Fatalf("unmarked change result=%q err=%v", result, err)
	}
}

func TestFakeRejectsIncompleteScripting(t *testing.T) {
	f := NewFake()
	p := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "org/repo"}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("AddIssue with empty author must panic")
		}
	}()
	f.AddIssue(p, Issue{ID: "1", Title: "t"})
}
