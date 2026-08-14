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

// Issue #993: the qualification probe runs under the agent's frozen
// launch_env — exactly the environment production launch will use.

func TestQualificationProbeUsesFrozenLaunchEnv(t *testing.T) {
	dir := t.TempDir()
	observed := filepath.Join(dir, "observed")
	agent := filepath.Join(dir, "agent")
	body := "#!/bin/sh\nprintf '%s|%s|%s' \"$HOME\" \"$PATH\" \"${SIFT_QUALIFICATION_SENTINEL-unset}\" > " + observed + "\nprintf version\n"
	if err := os.WriteFile(agent, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIFT_QUALIFICATION_SENTINEL", "daemon-credential")
	t.Setenv("HOME", "/daemon-home")
	frozen := map[string]string{"HOME": "/frozen/home", "PATH": "/frozen/bin:/usr/bin"}
	if _, err := BuildQualification(QualificationInput{AgentID: "agent", TaskTransport: "stdin", Executable: agent, LaunchEnv: frozen, ProbeTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "/frozen/home|/frozen/bin:/usr/bin|unset"; got != want {
		t.Fatalf("probe environment = %q, want %q", got, want)
	}
}

func TestFrozenEnvListIsSortedAndDeterministic(t *testing.T) {
	if got := FrozenEnvList(nil); got != nil {
		t.Fatalf("FrozenEnvList(nil) = %#v, want nil", got)
	}
	got := FrozenEnvList(map[string]string{"PATH": "/a:/b", "HOME": "/h"})
	if len(got) != 2 || got[0] != "HOME=/h" || got[1] != "PATH=/a:/b" {
		t.Fatalf("FrozenEnvList = %#v, want key-sorted K=V entries", got)
	}
}
