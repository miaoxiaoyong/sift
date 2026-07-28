package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type contractFixtureSet struct {
	kind                                                     Kind
	project                                                  ProjectRef
	newAdapter                                               func(Runner) *Adapter
	change, review, checks, jobs, labels                     []byte
	unknownChange, unknownReview, unknownChecks, unknownJobs []byte
}

// Dedicated dual-platform contract regressions use recorded-style fixtures,
// rather than the broad behavior suite's generated responses.
func TestContractRegressionSuite(t *testing.T) {
	cases := []struct {
		name string
		f    contractFixtureSet
	}{
		{"github", contractFixtureSet{
			kind: KindGitHub, project: ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"}, newAdapter: func(r Runner) *Adapter { return NewGitHub("gh", r) },
			change: v3Fixture(t, "github", "change_open.json"), review: v3Fixture(t, "github", "reviews_approved.json"), checks: v3Fixture(t, "github", "checks_contract.json"), labels: v3Fixture(t, "github", "labels_readback.json"),
			unknownChange: v3Fixture(t, "github", "change_unknown.json"), unknownReview: v3Fixture(t, "github", "reviews_unknown.json"), unknownChecks: v3Fixture(t, "github", "checks_unknown.json"),
		}},
		{"gitlab", contractFixtureSet{
			kind: KindGitLab, project: ProjectRef{Kind: KindGitLab, Host: "gitlab.example", ProjectKey: "owner/repo"}, newAdapter: func(r Runner) *Adapter { return NewGitLab("glab", r) },
			change: v3Fixture(t, "gitlab", "change_draft.json"), review: v3Fixture(t, "gitlab", "approvals_approved.json"), checks: v3Fixture(t, "gitlab", "pipelines_contract.json"), jobs: v3Fixture(t, "gitlab", "jobs_contract.json"), labels: v3Fixture(t, "gitlab", "labels_readback.json"),
			unknownChange: v3Fixture(t, "gitlab", "change_unknown.json"), unknownReview: v3Fixture(t, "gitlab", "approvals_unknown.json"), unknownChecks: v3Fixture(t, "gitlab", "pipelines_unknown.json"), unknownJobs: v3Fixture(t, "gitlab", "jobs_unknown.json"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a := tc.f.newAdapter(contractRunner(t, tc.f))
			t.Run("GetChange_review_normalization", func(t *testing.T) {
				c, err := a.GetChange(ctx, tc.f.project, "7")
				if err != nil || c.ReviewState != Approved {
					t.Fatalf("change=%+v err=%v", c, err)
				}
			})
			t.Run("GetChecks_checks_statuses_or_jobs", func(t *testing.T) {
				s, err := a.GetChecks(ctx, tc.f.project, "head-7")
				if err != nil || s.Conclusion != "failure" || len(s.FailedJobs) != 1 {
					t.Fatalf("checks=%+v err=%v", s, err)
				}
				if tc.f.kind == KindGitLab && s.FailedJobs[0].AllowFailure {
					t.Fatal("allow_failure job reported as required failure")
				}
			})
			t.Run("SetLabels_readback", func(t *testing.T) {
				if err := a.SetLabels(ctx, tc.f.project, TargetRef{Kind: TargetIssue, ID: "42"}, []string{"ready", "ready"}, []string{"stale", "stale"}); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("unknown_fails_closed", func(t *testing.T) {
				unknown := tc.f
				unknown.change, unknown.review, unknown.checks, unknown.jobs = unknown.unknownChange, unknown.unknownReview, unknown.unknownChecks, unknown.unknownJobs
				run := contractRunner(t, unknown)
				if tc.f.kind == KindGitHub {
					baseRun := run
					run = func(ctx context.Context, cli string, args []string, input []byte) ([]byte, []byte, error) {
						if strings.HasSuffix(args[1], "/status") {
							return []byte(`{"state":"success","statuses":[]}`), nil, nil
						}
						return baseRun(ctx, cli, args, input)
					}
				}
				a := tc.f.newAdapter(run)
				neutral, err := a.getChange(ctx, tc.f.project, "7", false)
				if err != nil || neutral.Mergeability != MergeabilityUnknown {
					t.Fatalf("unknown mergeability=%+v err=%v", neutral, err)
				}
				c, err := a.GetChange(ctx, tc.f.project, "7")
				if tc.f.kind == KindGitHub {
					if !errors.Is(err, ErrContractViolation) {
						t.Fatalf("unknown review err=%v", err)
					}
				} else if err != nil || c.ReviewState != ReviewUnknown {
					t.Fatalf("unknown approvals change=%+v err=%v", c, err)
				}
				s, err := a.GetChecks(ctx, tc.f.project, "head-7")
				if err != nil || s.Conclusion != "unknown" {
					t.Fatalf("unknown checks=%+v err=%v", s, err)
				}
			})
		})
	}
}

func contractRunner(t *testing.T, f contractFixtureSet) Runner {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		path := args[1]
		switch {
		case strings.Contains(path, "/reviews"), strings.Contains(path, "/approvals"):
			return f.review, nil, nil
		case strings.Contains(path, "/check-runs"):
			return f.checks, nil, nil
		case strings.HasSuffix(path, "/status"):
			return v3Fixture(t, "github", "statuses_contract.json"), nil, nil
		case strings.HasSuffix(path, "/jobs"):
			return f.jobs, nil, nil
		case strings.Contains(path, "/pipelines"):
			return f.checks, nil, nil
		case strings.HasSuffix(path, "/pulls/7"), strings.HasSuffix(path, "/merge_requests/7"):
			return f.change, nil, nil
		case strings.HasSuffix(path, "/issues/42"), strings.HasSuffix(path, "/merge_requests/42"):
			return f.labels, nil, nil
		default:
			return []byte(`{}`), nil, nil
		}
	}
}

func TestRetryAtParsing(t *testing.T) {
	a := NewGitHub("gh", func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return nil, []byte("HTTP 429; X-RateLimit-Reset: 1893456000"), errors.New("exit status 1")
	})
	_, err := a.GetIssue(context.Background(), ProjectRef{Kind: KindGitHub, Host: "github.com", ProjectKey: "owner/repo"}, "7")
	var classified *ClassifiedError
	if !errors.As(err, &classified) || !errors.Is(err, ErrRateLimited) || !classified.RetryAt.Equal(time.Unix(1893456000, 0)) {
		t.Fatalf("err=%v retry_at=%v", err, classified.RetryAt)
	}
}
