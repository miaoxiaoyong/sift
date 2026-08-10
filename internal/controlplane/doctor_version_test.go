package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/version"
)

// releaseWrapperFixture writes an executable wrapper script next to daemonPath
// that answers --version with release.
func releaseWrapperFixture(t *testing.T, dir, release string) {
	t.Helper()
	path := filepath.Join(dir, "sift-agent-wrapper")
	content := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"" + release + "\"; else exit 1; fi\n"
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorReportsReleaseVersion(t *testing.T) {
	home := testHome(t)
	result := doctor(context.Background(), true, home)
	checks := doctorChecks(t, result)
	check := checks["version"]
	if check.Level != "ok" {
		t.Fatalf("version check = %#v", check)
	}
	if check.Details["release_version"] != version.Release {
		t.Fatalf("release_version = %v, want %q", check.Details["release_version"], version.Release)
	}
}

func TestWrapperVersionChecksMatch(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "sift")
	if err := os.WriteFile(daemon, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	releaseWrapperFixture(t, dir, version.Release)
	checks := wrapperVersionChecks(context.Background(), daemon)
	if len(checks) != 1 || checks[0].ID != "version:wrapper" || checks[0].Level != "ok" {
		t.Fatalf("checks = %#v", checks)
	}
	if checks[0].Details["wrapper_version"] != version.Release {
		t.Fatalf("wrapper_version = %v", checks[0].Details["wrapper_version"])
	}
}

func TestWrapperVersionChecksMismatchIsError(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "sift")
	if err := os.WriteFile(daemon, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	releaseWrapperFixture(t, dir, "9.9.9-other")
	checks := wrapperVersionChecks(context.Background(), daemon)
	if len(checks) != 1 || checks[0].Level != "error" {
		t.Fatalf("checks = %#v", checks)
	}
	if !strings.Contains(checks[0].Message, runtime.ErrWrapperVersion.Error()) {
		t.Fatalf("message = %q, want wrapper version mismatch", checks[0].Message)
	}
}

func TestWrapperVersionChecksMissingIsWarning(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "sift")
	if err := os.WriteFile(daemon, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	checks := wrapperVersionChecks(context.Background(), daemon)
	if len(checks) != 1 || checks[0].Level != "warning" {
		t.Fatalf("checks = %#v", checks)
	}
	if !strings.Contains(checks[0].Message, "not installed next to") {
		t.Fatalf("message = %q", checks[0].Message)
	}
}

func TestWrapperVersionChecksNonExecutableIsWarning(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "sift")
	if err := os.WriteFile(daemon, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sift-agent-wrapper"), []byte("#!/bin/sh\necho x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := wrapperVersionChecks(context.Background(), daemon)
	if len(checks) != 1 || checks[0].Level != "warning" {
		t.Fatalf("checks = %#v", checks)
	}
}
