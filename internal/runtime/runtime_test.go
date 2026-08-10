package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveWrapperUsesDaemonDirectoryAndRequiresExactVersion(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "siftd")
	if err := os.WriteFile(daemon, nil, 0755); err != nil {
		t.Fatal(err)
	}
	writeWrapper(t, dir, "0.1.0", 1)

	got, err := ResolveWrapper(daemon, "0.1.0", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, wrapperName) {
		t.Fatalf("wrapper = %q", got)
	}

	writeWrapper(t, dir, "0.2.0", 1)
	_, err = ResolveWrapper(daemon, "0.1.0", 1)
	if !errors.Is(err, ErrWrapperVersion) {
		t.Fatalf("version mismatch error = %v", err)
	}
}

func TestResolveWrapperProtocolMismatch(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "siftd")
	if err := os.WriteFile(daemon, nil, 0755); err != nil {
		t.Fatal(err)
	}
	// Same SemVer as the daemon but a different wire protocol major: the
	// resolver must probe --protocol-major and fail closed instead of pairing.
	writeWrapper(t, dir, "0.1.0", 2)

	_, err := ResolveWrapper(daemon, "0.1.0", 1)
	if !errors.Is(err, ErrWrapperProtocolMajor) {
		t.Fatalf("protocol mismatch error = %v", err)
	}
}

func TestResolveWrapperDoesNotUsePATH(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "siftd")
	if err := os.WriteFile(daemon, nil, 0755); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	writeWrapper(t, pathDir, "0.1.0", 1)
	t.Setenv("PATH", pathDir)
	if _, err := ResolveWrapper(daemon, "0.1.0", 1); err == nil {
		t.Fatal("wrapper found only through PATH must be rejected")
	}
}

func TestDirectLauncherOnlyInjectsRunDirectory(t *testing.T) {
	t.Setenv("SECRET", "must-not-reach-agent")
	var stdout bytes.Buffer
	worktree := t.TempDir()
	runDir := t.TempDir()
	cmd, err := (DirectLauncher{}).Start(context.Background(), AgentLaunch{
		Executable: "/bin/sh",
		Args:       []string{"-c", `printf '%s|%s' "$SIFT_RUN_DIR" "${SECRET-unset}"`},
		Worktree:   worktree,
		RunDir:     runDir,
		Stdout:     &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), runDir+"|unset"; got != want {
		t.Fatalf("environment = %q, want %q", got, want)
	}
}

func TestQualificationBinaryReplacementBetweenMeasurementAndAgentExecFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("sealed executable images require a Unix executable image")
	}
	agent := filepath.Join(t.TempDir(), "agent")
	old := []byte("#!/bin/sh\nprintf old\n")
	if err := os.WriteFile(agent, old, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(old)
	image, err := MaterializeQualifiedExecutable(Qualification{ExecutablePath: agent, ExecutableSHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseExecutableImage(image)

	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte("#!/bin/sh\nprintf replacement\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, agent); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd, err := (DirectLauncher{}).Start(context.Background(), AgentLaunch{Executable: agent, ExecutableImage: image, Worktree: t.TempDir(), RunDir: t.TempDir(), Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "old" {
		t.Fatalf("agent bytes after path replacement = %q, want old verified image", got)
	}
}

func TestWriteControlFileIsPrivateAndReplacesContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.json")
	if err := WriteControlFile(path, []byte(`{"first":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := WriteControlFile(path, []byte(`{"second":true}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"second":true}` {
		t.Fatalf("contents = %s", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func writeWrapper(t *testing.T, dir, version string, protocolMajor int) {
	t.Helper()
	path := filepath.Join(dir, wrapperName)
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n'; fi\nif [ \"$1\" = \"--protocol-major\" ]; then printf '%d\\n'; fi\n", version, protocolMajor)
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}
