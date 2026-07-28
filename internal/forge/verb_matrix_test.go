package forge

import (
	"context"
	"strings"
	"testing"
)

// TestV3AllVerbsDualPlatformMatrix makes the contract matrix visible: every
// port verb has one successful normalization path on each real CLI adapter.
func TestV3AllVerbsDualPlatformMatrix(t *testing.T) {
	for _, kind := range []Kind{KindGitHub, KindGitLab} {
		t.Run(string(kind), func(t *testing.T) {
			project := ProjectRef{Kind: kind, Host: string(kind) + ".example", ProjectKey: "owner/repo"}
			a := NewAdapter(kind, "forge", matrixRunner(kind))
			ctx := context.Background()

			if got, _, err := a.ListIssuesByLabel(ctx, project, "sift", ""); err != nil || len(got) != 1 {
				t.Fatalf("ListIssuesByLabel: %#v %v", got, err)
			}
			if got, err := a.GetIssue(ctx, project, "7"); err != nil || got.ID != "7" {
				t.Fatalf("GetIssue: %#v %v", got, err)
			}
			if got, _, err := a.ListIssueComments(ctx, project, "7", ""); err != nil || len(got) != 1 || got[0].Author == "" {
				t.Fatalf("ListIssueComments: %#v %v", got, err)
			}
			if got, _, err := a.ListLabelEvents(ctx, project, TargetRef{Kind: TargetIssue, ID: "7"}, ""); err != nil || len(got) != 1 || got[0].Actor == "" {
				t.Fatalf("ListLabelEvents: %#v %v", got, err)
			}
			if got, err := a.CommentTarget(ctx, project, TargetRef{Kind: TargetChange, ID: "7"}, "body"); err != nil || got != "9" {
				t.Fatalf("CommentTarget: %q %v", got, err)
			}
			if err := a.SetLabels(ctx, project, TargetRef{Kind: TargetChange, ID: "7"}, []string{"ready"}, []string{"stale"}); err != nil {
				t.Fatalf("SetLabels: %v", err)
			}
			if got, err := a.CreateChange(ctx, project, "branch", "main", "title", "body"); err != nil || got.HeadSHA != "head-7" {
				t.Fatalf("CreateChange: %#v %v", got, err)
			}
			if got, result, err := a.FindChangeForCreateOperation(ctx, project, "sift-op", "branch", "main"); err != nil || result != MarkerHit || got == nil {
				t.Fatalf("FindChangeForCreateOperation: %#v %q %v", got, result, err)
			}
			if got, err := a.GetChange(ctx, project, "7"); err != nil || got.ID != "7" {
				t.Fatalf("GetChange: %#v %v", got, err)
			}
			if got, err := a.GetChangeDiff(ctx, project, "7"); err != nil || got == "" {
				t.Fatalf("GetChangeDiff: %q %v", got, err)
			}
			if got, _, err := a.ListChangeComments(ctx, project, "7", ""); err != nil || len(got) != 1 || got[0].Author == "" {
				t.Fatalf("ListChangeComments: %#v %v", got, err)
			}
			if got, err := a.GetChecks(ctx, project, "head-7"); err != nil || got.Conclusion != "success" {
				t.Fatalf("GetChecks: %#v %v", got, err)
			}
			if proven, evidence := a.ProbeAutoMergeCapability(ctx, project); !proven {
				t.Fatalf("ProbeAutoMergeCapability: %s", evidence)
			}
			if got, err := a.MergeChange(ctx, project, "7", "head-7", "merge"); err != nil || got.State != ChangeMerged {
				t.Fatalf("MergeChange: %#v %v", got, err)
			}
		})
	}
}

func matrixRunner(kind Kind) Runner {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		path := args[1]
		if path == "--help" {
			return []byte("--input file"), nil, nil
		}
		if strings.Contains(path, "/status") {
			return []byte(`{"state":"success","statuses":[{"state":"success"}]}`), nil, nil
		}
		if strings.Contains(path, "/check-runs") {
			return []byte(`{"check_runs":[{"name":"unit","conclusion":"success","html_url":"https://ci/unit"}]}`), nil, nil
		}
		if strings.Contains(path, "/pipelines?") {
			return []byte(`[{"id":1,"web_url":"https://ci/pipeline","status":"success"}]`), nil, nil
		}
		if strings.HasSuffix(path, "/jobs") {
			return []byte(`[{"name":"unit","web_url":"https://ci/job","status":"success"}]`), nil, nil
		}
		if strings.Contains(path, "/reviews") {
			return []byte(`[{"state":"APPROVED"}]`), nil, nil
		}
		if strings.Contains(path, "/approvals") {
			return []byte(`{"approved_by":[{}]}`), nil, nil
		}
		if strings.Contains(path, "/changes") {
			return []byte(`{"changes":[{"diff":"diff --git a/a b/a\n"}]}`), nil, nil
		}
		if strings.Contains(path, "/timeline") {
			return []byte(`[{"id":1,"event":"labeled","created_at":"2026-01-01T00:00:00Z","label":{"name":"ready"},"actor":{"login":"operator"}}]`), nil, nil
		}
		if strings.Contains(path, "/resource_label_events") {
			return []byte(`[{"id":1,"action":"add","created_at":"2026-01-01T00:00:00Z","label":{"name":"ready"},"user":{"username":"operator"}}]`), nil, nil
		}
		if strings.Contains(path, "/notes") || strings.Contains(path, "/comments") {
			if strings.Contains(strings.Join(args, " "), "--method POST") {
				return []byte(`{"id":9}`), nil, nil
			}
			return []byte(`[{"id":9,"body":"body","created_at":"2026-01-01T00:00:00Z","user":{"login":"operator"},"author":{"username":"operator"}}]`), nil, nil
		}
		if strings.Contains(path, "state=all") {
			return []byte(`[` + matrixChange(kind, "sift-op") + `]`), nil, nil
		}
		if strings.HasSuffix(path, "/merge") {
			return []byte(`{"merged":true}`), nil, nil
		}
		if strings.Contains(path, "/labels") {
			return []byte(`{}`), nil, nil
		}
		if strings.Contains(path, "/pulls") || strings.Contains(path, "/merge_requests") {
			return []byte(matrixChange(kind, "")), nil, nil
		}
		if strings.Contains(path, "/issues") {
			issue := matrixIssue(kind)
			if strings.Contains(path, "page=") {
				return []byte("[" + issue + "]"), nil, nil
			}
			return []byte(issue), nil, nil
		}
		return []byte(`{}`), nil, nil
	}
}

func matrixIssue(kind Kind) string {
	if kind == KindGitLab {
		return `{"iid":7,"title":"title","body":"body","web_url":"https://forge/7","state":"opened","updated_at":"2026-01-01T00:00:00Z","author":{"username":"author"},"labels":[{"name":"sift"},{"name":"ready"}]}`
	}
	return `{"number":7,"title":"title","body":"body","html_url":"https://forge/7","state":"open","updated_at":"2026-01-01T00:00:00Z","user":{"login":"author"},"labels":[{"name":"sift"},{"name":"ready"}]}`
}

func matrixChange(kind Kind, body string) string {
	if kind == KindGitLab {
		return `{"iid":7,"web_url":"https://forge/7","state":"merged","merged_at":"2026-01-01T00:00:00Z","diff_refs":{"head_sha":"head-7"},"title":"title","body":"` + body + `","labels":[{"name":"ready"}]}`
	}
	return `{"number":7,"html_url":"https://forge/7","state":"closed","merged_at":"2026-01-01T00:00:00Z","head":{"sha":"head-7"},"body":"` + body + `"}`
}
