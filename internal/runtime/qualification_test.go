package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQualificationEnvironmentIsCredentialFree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "credential-leaked")
	agent := filepath.Join(t.TempDir(), "agent")
	body := "#!/bin/sh\nif [ -n \"${SIFT_QUALIFICATION_SENTINEL+x}\" ]; then echo leaked > " + marker + "; fi\nprintf version\n"
	if err := os.WriteFile(agent, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIFT_QUALIFICATION_SENTINEL", "daemon-credential")
	if _, err := BuildQualification(QualificationInput{AgentID: "agent", TaskTransport: "stdin", Executable: agent, ProbeTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("qualification probe inherited sentinel credential: %v", err)
	}
}

func TestQualificationTimeoutIsBounded(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := BuildQualification(QualificationInput{AgentID: "agent", TaskTransport: "stdin", Executable: agent, Context: context.Background(), ProbeTimeout: 25 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("qualification timeout error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("qualification probe exceeded bounded timeout: %s", elapsed)
	}
}
