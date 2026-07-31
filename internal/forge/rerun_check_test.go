package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRerunCheckExactHeadDualPlatform(t *testing.T) {
	for _, kind := range []Kind{KindGitHub, KindGitLab} {
		t.Run(string(kind), func(t *testing.T) {
			var calls [][]string
			runner := func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
				calls = append(calls, append([]string(nil), args...))
				path := args[1]
				if kind == KindGitHub {
					if strings.HasSuffix(path, "/rerequest") {
						return []byte(`{}`), nil, nil
					}
					return []byte(`{"id":77,"head_sha":"head-frozen"}`), nil, nil
				}
				if strings.HasSuffix(path, "/retry") {
					return []byte(`{"id":78,"pipeline":{"sha":"head-frozen"}}`), nil, nil
				}
				return []byte(`{"id":77,"pipeline":{"sha":"head-frozen"}}`), nil, nil
			}
			p := ProjectRef{Kind: kind, Host: string(kind) + ".example", ProjectKey: "owner/repo"}
			if err := NewAdapter(kind, "forge", runner).RerunCheck(context.Background(), p, "77", "head-frozen"); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 {
				t.Fatalf("calls=%d, want verify+rerun", len(calls))
			}
			joined := strings.Join(calls[1], " ")
			want := "/check-runs/77/rerequest"
			if kind == KindGitLab {
				want = "/jobs/77/retry"
			}
			if !strings.Contains(joined, want) || !strings.Contains(joined, "--method POST --input -") {
				t.Fatalf("mutation args=%q", joined)
			}
		})
	}
}

func TestRerunCheckHeadMismatchNeverMutates(t *testing.T) {
	for _, kind := range []Kind{KindGitHub, KindGitLab} {
		t.Run(string(kind), func(t *testing.T) {
			calls := 0
			a := NewAdapter(kind, "forge", func(_ context.Context, _ string, _ []string, _ []byte) ([]byte, []byte, error) {
				calls++
				if kind == KindGitHub {
					return []byte(`{"id":77,"head_sha":"other"}`), nil, nil
				}
				return []byte(`{"id":77,"pipeline":{"sha":"other"}}`), nil, nil
			})
			p := ProjectRef{Kind: kind, Host: "forge.example", ProjectKey: "owner/repo"}
			err := a.RerunCheck(context.Background(), p, "77", "head-frozen")
			if !errors.Is(err, ErrSemanticConflict) || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
