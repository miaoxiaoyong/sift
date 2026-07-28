package forge

import (
	"context"
	"errors"
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

// TestV3RecordedMutationAndCommentMatrix keeps the three mutation/comment
// verbs on real CLI adapters backed by checked-in CLI response fixtures. In
// particular, all failure cases below must stop at the Forge boundary.
func TestV3RecordedMutationAndCommentMatrix(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind          Kind
		project       ProjectRef
		newAdapter    func(string, Runner) *Adapter
		commentID     string
		create        []byte
		reread        []byte
		comments      []byte
		missingActors []byte
	}{
		{
			name:          "github",
			kind:          KindGitHub,
			project:       ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"},
			newAdapter:    NewGitHub,
			commentID:     "9001",
			create:        v3Fixture(t, "github", "change_created.json"),
			comments:      v3Fixture(t, "github", "change_comments.json"),
			missingActors: v3Fixture(t, "github", "comments_missing_actor.json"),
		},
		{
			name:          "gitlab",
			kind:          KindGitLab,
			project:       ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "owner/repo"},
			newAdapter:    NewGitLab,
			commentID:     "9002",
			create:        v3Fixture(t, "gitlab", "change_created_missing_head.json"),
			reread:        v3Fixture(t, "gitlab", "change_created_reread.json"),
			comments:      v3Fixture(t, "gitlab", "change_comments.json"),
			missingActors: v3Fixture(t, "gitlab", "notes_missing_actor.json"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("CommentTarget_recorded_response_and_missing_id_fail_closed", func(t *testing.T) {
				a := tc.newAdapter("forge", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
					if !strings.Contains(args[1], "/merge_requests/7/notes") && !strings.Contains(args[1], "/issues/7/comments") {
						t.Fatalf("comment path=%q", args[1])
					}
					return v3Fixture(t, tc.name, "comment_created.json"), nil, nil
				})
				id, err := a.CommentTarget(ctx, tc.project, TargetRef{Kind: TargetChange, ID: "7"}, "Sift decision")
				if err != nil || id != tc.commentID {
					t.Fatalf("comment id=%q err=%v", id, err)
				}

				a = tc.newAdapter("forge", constRunner(v3Fixture(t, tc.name, "comment_created_missing_id.json")))
				if _, err := a.CommentTarget(ctx, tc.project, TargetRef{Kind: TargetChange, ID: "7"}, "Sift decision"); !errors.Is(err, ErrContractViolation) {
					t.Fatalf("missing comment id err=%v, want ErrContractViolation", err)
				}
			})

			t.Run("CreateChange_recorded_response", func(t *testing.T) {
				calls := 0
				a := tc.newAdapter("forge", func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
					calls++
					if calls == 1 {
						return tc.create, nil, nil
					}
					if tc.reread == nil || !strings.HasSuffix(args[1], "/73") {
						t.Fatalf("unexpected create follow-up path=%q", args[1])
					}
					return tc.reread, nil, nil
				})
				change, err := a.CreateChange(ctx, tc.project, "sift/79", "main", "Sift change", "body")
				if err != nil || change.ID != "73" || change.HeadSHA != "created-head-73" {
					t.Fatalf("change=%+v err=%v", change, err)
				}
				if tc.kind == KindGitLab && calls != 2 {
					t.Fatalf("GitLab missing-head create calls=%d, want reread", calls)
				}
			})

			t.Run("ListChangeComments_recorded_response_and_actor_fail_closed", func(t *testing.T) {
				a := tc.newAdapter("forge", constRunner(tc.comments))
				comments, _, err := a.ListChangeComments(ctx, tc.project, "7", "")
				if err != nil || len(comments) != 1 || comments[0].Author != "maintainer" {
					t.Fatalf("comments=%+v err=%v", comments, err)
				}

				a = tc.newAdapter("forge", constRunner(tc.missingActors))
				comments, _, err = a.ListChangeComments(ctx, tc.project, "7", "")
				if err != nil || len(comments) != 1 || comments[0].Author == "" {
					t.Fatalf("missing actor must be discarded: comments=%+v err=%v", comments, err)
				}
			})
		})
	}

	t.Run("gitlab_create_reread_still_missing_head_fails_closed", func(t *testing.T) {
		project := ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "owner/repo"}
		calls := 0
		a := NewGitLab("glab", func(_ context.Context, _ string, _ []string, _ []byte) ([]byte, []byte, error) {
			calls++
			if calls == 1 {
				return v3Fixture(t, "gitlab", "change_created_missing_head.json"), nil, nil
			}
			return v3Fixture(t, "gitlab", "change_created_reread_missing_head.json"), nil, nil
		})
		if _, err := a.CreateChange(context.Background(), project, "sift/79", "main", "Sift change", "body"); !errors.Is(err, ErrContractViolation) {
			t.Fatalf("missing head after reread err=%v, want ErrContractViolation", err)
		}
		if calls != 2 {
			t.Fatalf("calls=%d, want create plus reread", calls)
		}
	})
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
