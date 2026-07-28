package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestAdapterPaginationAndActorFailClosed(t *testing.T) {
	calls := 0
	run := func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		calls++
		if !strings.Contains(args[1], "page=2") {
			rows := make([]string, 100)
			for i := range rows {
				rows[i] = fmt.Sprintf(`{"number":%d,"title":"t","body":"b","html_url":"https://x/%d","state":"open","updated_at":"2026-01-01T00:00:%02dZ","user":{"login":"a"},"labels":[{"name":"sift"}]}`, i+1, i+1, i%60)
			}
			return []byte("[" + strings.Join(rows, ",") + "]"), nil, nil
		}
		return []byte(`[{"number":101,"title":"t","body":"b","html_url":"https://x/101","state":"open","updated_at":"2026-01-01T00:01:00Z","user":{"login":"a"},"labels":[{"name":"sift"}]}]`), nil, nil
	}
	a := NewGitHub("gh", run)
	issues, _, err := a.ListIssuesByLabel(context.Background(), ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "o/r"}, "sift", "")
	if err != nil || len(issues) != 101 || calls != 2 {
		t.Fatalf("pagination: %d calls=%d err=%v", len(issues), calls, err)
	}
	run = func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return []byte(`[{"id":1,"body":"x","created_at":"2026-01-01T00:00:00Z","user":{}}]`), nil, nil
	}
	a = NewGitHub("gh", run)
	comments, _, err := a.ListIssueComments(context.Background(), ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "o/r"}, "1", "")
	if err != nil || len(comments) != 0 {
		t.Fatalf("missing actor must be dropped: %#v %v", comments, err)
	}
}

func TestAdapterRateLimitMapping(t *testing.T) {
	a := NewGitHub("gh", func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return nil, []byte("HTTP 429: rate limit exceeded"), errors.New("exit status 1")
	})
	_, err := a.GetIssue(context.Background(), ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "o/r"}, "1")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error=%v", err)
	}
}

func TestGitLabCreateChangeRereadsMissingHeadSHA(t *testing.T) {
	project := ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "o/r"}
	calls := 0
	a := NewGitLab("glab", func(_ context.Context, _ string, args []string, input []byte) ([]byte, []byte, error) {
		calls++
		if calls == 1 {
			if !strings.HasSuffix(args[1], "/merge_requests") {
				t.Fatalf("create path=%q", args[1])
			}
			return []byte(`{"iid":7,"web_url":"https://gitlab/x/7","state":"opened","title":"change"}`), nil, nil
		}
		if !strings.HasSuffix(args[1], "/merge_requests/7") {
			t.Fatalf("reread path=%q", args[1])
		}
		return []byte(`{"iid":7,"web_url":"https://gitlab/x/7","state":"opened","diff_refs":{"head_sha":"head-7"},"title":"change"}`), nil, nil
	})
	change, err := a.CreateChange(context.Background(), project, "branch", "main", "change", "body")
	if err != nil || change.ID != "7" || change.HeadSHA != "head-7" || calls != 2 {
		t.Fatalf("change=%+v calls=%d err=%v", change, calls, err)
	}
}

func TestGitLabChangeCommentsUseMergeRequestNotes(t *testing.T) {
	a := NewGitLab("glab", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if !strings.Contains(args[1], "/merge_requests/7/notes") {
			t.Fatalf("change comments path=%q", args[1])
		}
		return []byte(`[]`), nil, nil
	})
	_, _, err := a.ListChangeComments(context.Background(), ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "o/r"}, "7", "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitLabNormalization(t *testing.T) {
	a := NewGitLab("glab", func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return []byte(`{"iid":7,"web_url":"https://gitlab/x/7","state":"opened","diff_refs":{"head_sha":"abc"},"title":"Draft: test","detailed_merge_status":"conflict"}`), nil, nil
	})
	c, err := a.GetChange(context.Background(), ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "o/r"}, "7")
	if err != nil || c.ID != "7" || !c.IsDraft || c.Mergeability != Conflicting {
		t.Fatalf("change=%+v err=%v", c, err)
	}
}

func TestFindChangeForCreateOperationMarkerAndConflict(t *testing.T) {
	project := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"}
	for _, test := range []struct {
		name string
		body string
		want FindResult
	}{
		{"marker hit across closed state", `<!-- sift-op:run:1 -->`, MarkerHit},
		{"unmarked same head conflicts", "human change", SemanticConflict},
		{"no change", "", NoMatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := NewGitHub("gh", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
				if !strings.Contains(args[1], "state=all") || !strings.Contains(args[1], "head=owner%3Abranch") {
					t.Fatalf("lookup path = %q", args[1])
				}
				if test.want == NoMatch {
					return []byte(`[]`), nil, nil
				}
				return []byte(`[{"number":7,"html_url":"https://x/7","state":"closed","head":{"sha":"head"},"body":` + strconv.Quote(test.body) + `}]`), nil, nil
			})
			change, got, err := a.FindChangeForCreateOperation(context.Background(), project, "run:1", "branch", "main")
			if err != nil || got != test.want {
				t.Fatalf("result=%q change=%+v err=%v", got, change, err)
			}
			if got == MarkerHit && (change == nil || change.State != ChangeClosed) {
				t.Fatalf("closed marker hit = %+v", change)
			}
		})
	}
}

type capabilityReader bool

func (r capabilityReader) AutoMergeEnabled(context.Context, ProjectRef) (bool, error) {
	return bool(r), nil
}

func TestAutoMergeFailsClosedUntilStartupProbeAndPersistedCapability(t *testing.T) {
	project := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"}
	calls := 0
	a := NewGitHub("gh", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		calls++
		if len(args) == 2 && args[1] == "--help" {
			return []byte("--input file"), nil, nil
		}
		return []byte(`{"merged":true}`), nil, nil
	}).WithAutoMergeCapabilityReader(capabilityReader(false))
	if _, err := a.MergeChange(context.Background(), project, "7", "head-a", "merge"); !errors.Is(err, ErrAuthOrCapability) || calls != 0 {
		t.Fatalf("unproven merge err=%v calls=%d; want fail closed without a CLI call", err, calls)
	}
	if proven, evidence := a.ProbeAutoMergeCapability(context.Background(), project); !proven {
		t.Fatalf("startup proof = false: %s", evidence)
	}
	if _, err := a.MergeChange(context.Background(), project, "7", "head-a", "merge"); !errors.Is(err, ErrAuthOrCapability) || calls != 1 {
		t.Fatalf("persisted disabled merge err=%v calls=%d; want fail closed", err, calls)
	}
}

func TestMergeChangeExpectedHeadCASAndCapabilityFailure(t *testing.T) {
	project := ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"}
	calls := 0
	a := NewGitHub("gh", func(_ context.Context, _ string, args []string, input []byte) ([]byte, []byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("--input file"), nil, nil
		}
		calls++
		if calls == 1 {
			if !strings.Contains(args[1], "/pulls/7/merge") {
				t.Fatalf("merge path = %q", args[1])
			}
			var payload map[string]string
			if err := json.Unmarshal(input, &payload); err != nil || payload["sha"] != "head-a" || payload["merge_method"] != "merge" {
				t.Fatalf("merge payload=%s err=%v", input, err)
			}
			return []byte(`{"merged":true}`), nil, nil
		}
		return []byte(`{"number":7,"html_url":"https://x/7","state":"closed","merged_at":"2026-01-01T00:00:00Z","head":{"sha":"head-a"}}`), nil, nil
	})
	if proven, evidence := a.ProbeAutoMergeCapability(context.Background(), project); !proven {
		t.Fatalf("startup probe failed: %s", evidence)
	}
	change, err := a.MergeChange(context.Background(), project, "7", "head-a", "merge")
	if err != nil || change.State != ChangeMerged || calls != 2 {
		t.Fatalf("merge=%+v calls=%d err=%v", change, calls, err)
	}

	a = NewGitHub("gh", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("--input file"), nil, nil
		}
		return nil, []byte("unknown parameter: sha; capability_unsupported"), errors.New("exit status 1")
	})
	a.ProbeAutoMergeCapability(context.Background(), project)
	_, err = a.MergeChange(context.Background(), project, "7", "head-a", "merge")
	if !errors.Is(err, ErrAuthOrCapability) || a.AutoMergeSupported(project) {
		t.Fatalf("missing expected-head CAS capability error=%v supported=%v", err, a.AutoMergeSupported(project))
	}
	_, err = a.MergeChange(context.Background(), project, "7", "head-a", "merge")
	if !errors.Is(err, ErrAuthOrCapability) {
		t.Fatalf("disabled auto-merge error=%v", err)
	}

	a = NewGitHub("gh", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("--input file"), nil, nil
		}
		return nil, []byte("409 head SHA does not match"), errors.New("exit status 1")
	})
	a.ProbeAutoMergeCapability(context.Background(), project)
	_, err = a.MergeChange(context.Background(), project, "7", "head-a", "merge")
	if !errors.Is(err, ErrSemanticConflict) {
		t.Fatalf("stale head error=%v", err)
	}
}
