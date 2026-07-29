package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCaptureIncludesConfigHooksPathAndDirectory(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	hooksDir := filepath.Join(repo, "custom-hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("echo one\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.hooksPath", hooksDir)
	first, err := Capture(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if first.CoreHooksPathValue == nil || *first.CoreHooksPathValue != hooksDir {
		t.Fatalf("hooks path = %#v", first.CoreHooksPathValue)
	}
	if first.EffectiveHooksPath != hooksDir {
		t.Fatalf("effective path = %q", first.EffectiveHooksPath)
	}
	if first.GitConfigDigest == "" || first.DirectoryDigest == "" || first.Digest == "" {
		t.Fatalf("incomplete snapshot: %#v", first)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("echo two\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := Capture(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest == first.Digest || second.DirectoryDigest == first.DirectoryDigest {
		t.Fatal("directory mutation did not change fingerprint")
	}
}
