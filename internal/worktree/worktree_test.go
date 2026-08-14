package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/attempt"
)

func TestManagerCreatesIsolatedWorktreeAndReadsBaseOnly(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	if err := os.Mkdir(filepath.Join(repo, ".sift"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, ".sift", "policy.yaml"), "base-policy")
	write(t, filepath.Join(repo, ".sift", "context.md"), "base-context")
	write(t, filepath.Join(repo, "README"), "base")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")

	m, err := NewManager(repo, filepath.Join(t.TempDir(), "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(context.Background(), "run-1", 1, base, "sift/run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, ".sift", "policy.yaml")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, ".sift", "policy.yaml"), "agent-policy")
	write(t, filepath.Join(wt.Path, ".sift", "context.md"), "agent-context")
	for _, tc := range []struct{ name, want string }{
		{name: ".sift/policy.yaml", want: "base-policy"},
		{name: ".sift/context.md", want: "base-context"},
	} {
		got, err := m.ReadBaseFile(context.Background(), base, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Fatalf("base %s = %q, want %q", tc.name, got, tc.want)
		}
	}
	if err := m.Remove(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
}

func TestReadResultPreservesFailureReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"run_id":"run-1","attempt_no":1,"generation":1,"wrapper_instance_id":"wrapper","agent_identity":{"pid":42,"started_at_ms":100,"executable":"/agent"},"exit_code":1,"signal":null,"failure_reason":"agent_log_relay_failed","finished_at_ms":200}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.FailureReason != "agent_log_relay_failed" {
		t.Fatalf("failure reason = %q, want agent_log_relay_failed", result.FailureReason)
	}
}

func TestEvaluateSuccessRequiresMatchingIdentityHeadAndCommit(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "test")
	write(t, filepath.Join(repo, "file"), "base")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	m, err := NewManager(repo, filepath.Join(t.TempDir(), "wt"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(context.Background(), "run-2", 1, base, "sift/run-2")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "file"), "change")
	runGit(t, wt.Path, "add", "file")
	runGit(t, wt.Path, "commit", "-qm", "change")
	head := runGit(t, wt.Path, "rev-parse", "HEAD")
	exit := 0
	id := attempt.Identity{PID: 42, StartedAtMS: 100, Executable: "/agent"}
	ready, err := m.EvaluateSuccess(context.Background(), wt, "run-2", attempt.Result{ExitCode: &exit, FinalHeadSHA: head, Digest: "digest", Agent: id}, id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.FinalHeadSHA != head {
		t.Fatal("head was not frozen")
	}
	bad := id
	bad.PID++
	if _, err := m.EvaluateSuccess(context.Background(), wt, "run-2", attempt.Result{ExitCode: &exit, FinalHeadSHA: head, Digest: "digest", Agent: bad}, id); !errors.Is(err, ErrEvidenceRejected) {
		t.Fatalf("identity error = %v", err)
	}
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
