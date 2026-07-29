package forge

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// V3 dual-platform contract suite (WBS M2 §2.5 / gate, forge.md §10).
//
// The same suite of contract assertions runs against both the GitHub and the
// GitLab adapter. Each platform serves its own recorded-style JSON fixtures
// (testdata/fixtures/{github,gitlab}); the assertions check the domain-neutral
// result, proving the platform difference is normalized at the boundary. The
// suite covers the V3 surface the M2 gate requires: pagination, actor
// fail-closed, rate-limit mapping, platform normalization (number/iid,
// head.sha/diff_refs, draft bool/Draft: prefix, mergeable/detailed status),
// change-marker cross-state search, merge expected-head CAS and the structural
// auto_merge capability disable.

//go:embed testdata/fixtures/github/*.json testdata/fixtures/gitlab/*.json
var v3Fixtures embed.FS

const fixtureMarkerDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func v3Fixture(t *testing.T, platform, name string) []byte {
	t.Helper()
	b, err := v3Fixtures.ReadFile("testdata/fixtures/" + platform + "/" + name)
	if err != nil {
		t.Fatalf("read fixture %s/%s: %v", platform, name, err)
	}
	return b
}

// v3Platform carries the per-platform wiring + fixtures + expected neutral
// values the shared suite runs against.
type v3Platform struct {
	name             string
	kind             Kind
	project          ProjectRef
	newAdapter       func(string, Runner) *Adapter
	changeNormal     []byte // GetChange response exercising platform normalization
	changeMerged     []byte // GetChange response after a merge
	findMarker       []byte // list: one closed change carrying the op marker
	findConflict     []byte // list: one change without the op marker
	eventsActor      []byte // label events: one with actor, one ghost
	commentsActor    []byte // comments: one with author, one ghost
	wantID           string
	wantDraft        bool
	wantMerge        Mergeability
	wantMergeSHA     string
	eventsTargetID   string
	commentsTargetID string
}

func TestV3ContractSuiteGitHub(t *testing.T) {
	runV3Suite(t, v3Platform{
		name:             "github",
		kind:             KindGitHub,
		project:          ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"},
		newAdapter:       NewGitHub,
		changeNormal:     v3Fixture(t, "github", "change_open.json"),
		changeMerged:     v3Fixture(t, "github", "change_merged.json"),
		findMarker:       v3Fixture(t, "github", "find_marker_closed.json"),
		findConflict:     v3Fixture(t, "github", "find_conflict.json"),
		eventsActor:      v3Fixture(t, "github", "timeline_missing_actor.json"),
		commentsActor:    v3Fixture(t, "github", "comments_missing_actor.json"),
		wantID:           "7",
		wantDraft:        false,
		wantMerge:        Mergeable,
		wantMergeSHA:     "1111111111111111111111111111111111111111",
		eventsTargetID:   "42",
		commentsTargetID: "42",
	})
}

func TestV3ContractSuiteGitLab(t *testing.T) {
	runV3Suite(t, v3Platform{
		name:             "gitlab",
		kind:             KindGitLab,
		project:          ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "owner/repo"},
		newAdapter:       NewGitLab,
		changeNormal:     v3Fixture(t, "gitlab", "change_draft.json"),
		changeMerged:     v3Fixture(t, "gitlab", "change_merged.json"),
		findMarker:       v3Fixture(t, "gitlab", "find_marker_closed.json"),
		findConflict:     v3Fixture(t, "gitlab", "find_conflict.json"),
		eventsActor:      v3Fixture(t, "gitlab", "resource_label_events_missing_actor.json"),
		commentsActor:    v3Fixture(t, "gitlab", "notes_missing_actor.json"),
		wantID:           "7",
		wantDraft:        true, // Draft: prefix normalization
		wantMerge:        Mergeable,
		wantMergeSHA:     "2222222222222222222222222222222222222222",
		eventsTargetID:   "42",
		commentsTargetID: "42",
	})
}

func runV3Suite(t *testing.T, p v3Platform) {
	ctx := context.Background()

	// Pagination: page 1 has a full 100-item page, page 2 a single item. The
	// adapter must walk both pages and terminate, charging one subprocess per
	// page (forge.md §8.4: no --paginate hiding request counts).
	t.Run("pagination_walks_full_pages", func(t *testing.T) {
		calls := 0
		a := p.newAdapter("", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
			calls++
			page := 1
			if i := strings.Index(args[1], "page="); i >= 0 {
				fmt.Sscanf(args[1][i+len("page="):], "%d", &page)
			}
			if page == 1 {
				return v3Page(p.kind, 1, 100), nil, nil
			}
			return v3Page(p.kind, 101, 1), nil, nil
		})
		issues, cur, err := a.ListIssuesByLabel(ctx, p.project, "sift", "")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(issues) != 101 {
			t.Fatalf("issues=%d, want 101 (both pages walked)", len(issues))
		}
		if calls != 2 {
			t.Fatalf("subprocess calls=%d, want 2 (one per page)", calls)
		}
		if cur == "" || string(cur) == "101" {
			t.Fatalf("cursor=%q, want opaque timestamp/id cursor", cur)
		}
		if issues[0].Author == "" || issues[0].URL == "" {
			t.Fatalf("first issue not normalized: %+v", issues[0])
		}
	})

	// Actor fail-closed (forge.md §7): a label event with no actor is dropped
	// in the adapter; the caller never sees a ghost driving event.
	t.Run("actor_missing_label_event_dropped", func(t *testing.T) {
		a := p.newAdapter("", constRunner(p.eventsActor))
		events, _, err := a.ListLabelEvents(ctx, p.project, TargetRef{Kind: TargetIssue, ID: p.eventsTargetID}, "")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(events) != 1 {
			t.Fatalf("events=%d, want 1 (ghost dropped): %+v", len(events), events)
		}
		if events[0].Actor != "trusted-operator" || events[0].TargetID != p.eventsTargetID {
			t.Fatalf("surviving event=%+v", events[0])
		}
		if events[0].Action != LabelAdded {
			t.Fatalf("action=%q, want added", events[0].Action)
		}
	})

	// Actor fail-closed on comments (forge.md §7): a comment with no author is
	// dropped without affecting the rest of the batch.
	t.Run("actor_missing_comment_dropped", func(t *testing.T) {
		a := p.newAdapter("", constRunner(p.commentsActor))
		comments, _, err := a.ListIssueComments(ctx, p.project, p.commentsTargetID, "")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(comments) != 1 {
			t.Fatalf("comments=%d, want 1 (ghost dropped): %+v", len(comments), comments)
		}
		if comments[0].Author != "alice" {
			t.Fatalf("surviving comment author=%q", comments[0].Author)
		}
	})

	// Rate-limit mapping (forge.md §3): a 429/rate-limit stderr classifies to
	// ErrRateLimited on both platforms.
	t.Run("rate_limit_classified", func(t *testing.T) {
		a := p.newAdapter("", func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
			return nil, []byte("HTTP 429: rate limit exceeded"), errors.New("exit status 1")
		})
		if _, err := a.GetIssue(ctx, p.project, "1"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("err=%v, want ErrRateLimited", err)
		}
	})

	// Platform normalization (forge.md §6): number/iid, head.sha/diff_refs,
	// draft bool vs Draft: prefix, and mergeability all collapse to the same
	// neutral Change projection.
	t.Run("platform_difference_normalized", func(t *testing.T) {
		a := p.newAdapter("", func(ctx context.Context, cli string, args []string, in []byte) ([]byte, []byte, error) {
			if strings.Contains(args[1], "/reviews") {
				return []byte(`[]`), nil, nil
			}
			if strings.Contains(args[1], "/approvals") {
				return []byte(`{}`), nil, nil
			}
			return constRunner(p.changeNormal)(ctx, cli, args, in)
		})
		c, err := a.GetChange(ctx, p.project, p.wantID)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if c.ID != p.wantID {
			t.Fatalf("id=%q, want %q (number/iid normalized)", c.ID, p.wantID)
		}
		if c.IsDraft != p.wantDraft {
			t.Fatalf("draft=%v, want %v", c.IsDraft, p.wantDraft)
		}
		if c.Mergeability != p.wantMerge {
			t.Fatalf("mergeability=%q, want %q", c.Mergeability, p.wantMerge)
		}
		if c.HeadSHA == "" || c.URL == "" {
			t.Fatalf("change missing head/url: %+v", c)
		}
	})

	// Change marker search (forge.md §4.8): a marker hit across a closed Change
	// is still returned (the operation is converged on the existing object).
	t.Run("marker_hit_across_closed", func(t *testing.T) {
		a := p.newAdapter("", matchRunner("state=all", p.findMarker))
		ch, got, err := a.FindChangeForCreateOperation(ctx, p.project, "run:demo:create-change:head-9", fixtureMarkerDigest, "branch", "main")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if got != MarkerHit {
			t.Fatalf("result=%q, want marker_hit", got)
		}
		if ch == nil || ch.State != ChangeClosed {
			t.Fatalf("closed marker change=%+v", ch)
		}
	})

	// Same base/head Change with no marker is a semantic conflict: the adapter
	// never takes over someone else's object (forge.md §4.8 / DESIGN §6.4).
	t.Run("marker_missing_is_semantic_conflict", func(t *testing.T) {
		a := p.newAdapter("", matchRunner("state=all", p.findConflict))
		_, got, err := a.FindChangeForCreateOperation(ctx, p.project, "run:demo:create-change:head-9", fixtureMarkerDigest, "branch", "main")
		if err != nil || got != SemanticConflict {
			t.Fatalf("result=%q err=%v, want semantic_conflict", got, err)
		}
	})

	t.Run("marker_no_match", func(t *testing.T) {
		a := p.newAdapter("", matchRunner("state=all", []byte("[]")))
		_, got, err := a.FindChangeForCreateOperation(ctx, p.project, "run:demo:create-change:head-9", fixtureMarkerDigest, "branch", "main")
		if err != nil || got != NoMatch {
			t.Fatalf("result=%q err=%v, want no_match", got, err)
		}
	})

	// Merge expected-head CAS success (forge.md §4.13 / ADR-011): the merge
	// succeeds and the adapter re-reads the merged Change rather than trusting
	// the transport ack.
	t.Run("merge_expected_head_cas_success", func(t *testing.T) {
		a := p.newAdapter("", mergeRunner(p.changeMerged))
		proveAutoMerge(t, a, p.project)
		c, err := a.MergeChange(ctx, p.project, p.wantID, "head-7", "merge")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if c.State != ChangeMerged {
			t.Fatalf("state=%q, want merged", c.State)
		}
		if c.MergedAt.IsZero() {
			t.Fatal("merged change must carry MergedAt")
		}
		if c.MergeSHA != p.wantMergeSHA {
			t.Fatalf("merge SHA=%q, want checked-in fixture value %q", c.MergeSHA, p.wantMergeSHA)
		}
	})

	// Stale head → SemanticConflict (forge.md §4.13).
	t.Run("merge_stale_head_semantic_conflict", func(t *testing.T) {
		a := p.newAdapter("", mergeErrRunner("409 head SHA does not match"))
		proveAutoMerge(t, a, p.project)
		_, err := a.MergeChange(ctx, p.project, p.wantID, "head-7", "merge")
		if !errors.Is(err, ErrSemanticConflict) {
			t.Fatalf("err=%v, want ErrSemanticConflict", err)
		}
	})

	// Capability disable is structural (forge.md §4.13 / gate): a platform that
	// cannot supply expected-head CAS permanently loses auto_merge for that
	// project; it must not downgrade to an unconditional merge.
	t.Run("merge_capability_structurally_disabled", func(t *testing.T) {
		a := p.newAdapter("", mergeErrRunner("unknown parameter: sha; capability_unsupported"))
		if a.AutoMergeSupported(p.project) {
			t.Fatal("auto_merge must be disabled before startup proof")
		}
		proveAutoMerge(t, a, p.project)
		if _, err := a.MergeChange(ctx, p.project, p.wantID, "head-7", "merge"); !errors.Is(err, ErrAuthOrCapability) {
			t.Fatalf("first err=%v, want ErrAuthOrCapability", err)
		}
		if a.AutoMergeSupported(p.project) {
			t.Fatal("auto_merge must be structurally disabled after a capability failure")
		}
		// A second attempt is rejected without re-probing the platform.
		if _, err := a.MergeChange(ctx, p.project, p.wantID, "head-7", "merge"); !errors.Is(err, ErrAuthOrCapability) {
			t.Fatalf("second err=%v, want ErrAuthOrCapability (no downgrade)", err)
		}
	})

	// Only the V0 "merge" method is accepted (forge.md §4.13).
	t.Run("merge_rejects_non_merge_method", func(t *testing.T) {
		a := p.newAdapter("", mergeRunner(p.changeMerged))
		proveAutoMerge(t, a, p.project)
		if _, err := a.MergeChange(ctx, p.project, p.wantID, "head-7", "squash"); !errors.Is(err, ErrContractViolation) {
			t.Fatalf("err=%v, want ErrContractViolation", err)
		}
	})
}

func proveAutoMerge(t *testing.T, a *Adapter, p ProjectRef) {
	t.Helper()
	if proven, evidence := a.ProbeAutoMergeCapability(context.Background(), p); !proven {
		t.Fatalf("startup capability probe = false: %s", evidence)
	}
}

// v3Page builds a full page of platform-shaped issue JSON for the pagination
// boundary (the 100-row page is the adapter's terminate condition, not a
// platform semantic; the row shape still exercises number/iid + actor fields).
func v3Page(k Kind, first, count int) []byte {
	rows := make([]string, count)
	for i := 0; i < count; i++ {
		n := first + i
		if k == KindGitLab {
			rows[i] = fmt.Sprintf(`{"iid":%d,"title":"t","body":"b","web_url":"https://x/%d","state":"opened","updated_at":"2026-01-01T00:00:%02dZ","author":{"username":"a"},"labels":[{"name":"sift"}]}`, n, n, n%60)
			continue
		}
		rows[i] = fmt.Sprintf(`{"number":%d,"title":"t","body":"b","html_url":"https://x/%d","state":"open","updated_at":"2026-01-01T00:00:%02dZ","user":{"login":"a"},"labels":[{"name":"sift"}]}`, n, n, n%60)
	}
	return []byte("[" + strings.Join(rows, ",") + "]")
}

// constRunner returns the fixture for ordinary requests and an empty review set.
func constRunner(b []byte) Runner {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		// GetChange also reads the platform review endpoint. Keep this helper
		// focused on the requested fixture while supplying an empty review set.
		if strings.Contains(args[1], "/reviews") || strings.Contains(args[1], "/approvals") {
			return []byte(`[]`), nil, nil
		}
		return b, nil, nil
	}
}

// matchRunner serves b when the request path contains key, else errors.
func matchRunner(key string, b []byte) Runner {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if strings.Contains(args[1], key) {
			return b, nil, nil
		}
		return nil, []byte("unexpected path: " + args[1]), errors.New("exit status 1")
	}
}

// mergeRunner serves the merge PUT ack, then the merged-Change re-read.
func mergeRunner(merged []byte) Runner {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("--input file"), nil, nil
		}
		if strings.HasSuffix(args[1], "/merge") {
			return []byte(`{"merged":true}`), nil, nil
		}
		return merged, nil, nil
	}
}

// mergeErrRunner makes the merge PUT fail with the given stderr substring; the
// classified error is derived from it (classify keys off stderr).
func mergeErrRunner(stderr string) Runner {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("--input file"), nil, nil
		}
		if strings.HasSuffix(args[1], "/merge") {
			return nil, []byte(stderr), errors.New("exit status 1")
		}
		return nil, []byte(stderr), errors.New("exit status 1")
	}
}
