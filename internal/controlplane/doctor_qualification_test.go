package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDoctorQualificationProbeSanitizesEnvironmentAndHonorsContext(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "credential-leaked")
	agent := filepath.Join(dir, "agent")
	body := "#!/bin/sh\nif [ -n \"${SIFT_DOCTOR_SENTINEL+x}\" ]; then echo leaked > " + marker + "; fi\nprintf version\n"
	if err := os.WriteFile(agent, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIFT_DOCTOR_SENTINEL", "daemon-credential")
	if check := qualificationCommandCheck(context.Background(), "agent", agent, nil, nil); check.Level != "ok" {
		t.Fatalf("doctor qualification probe = %#v", check)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor qualification probe inherited sentinel credential: %v", err)
	}

	hanging := filepath.Join(dir, "hanging-agent")
	if err := os.WriteFile(hanging, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if check := qualificationCommandCheck(ctx, "agent", hanging, nil, nil); check.Level != "error" {
		t.Fatalf("doctor hanging qualification probe = %#v, want error", check)
	}
}

// Issue #993: agent-cli probes run under the frozen launch_env, so an agent
// whose shim requires HOME passes doctor exactly when the frozen snapshot
// provides it — the same environment the daemon will launch with.

func TestDoctorQualificationProbeUsesFrozenLaunchEnv(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "agent")
	body := "#!/bin/sh\n: \"${HOME:?HOME: unbound variable}\"\nprintf version\n"
	if err := os.WriteFile(agent, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if check := qualificationCommandCheck(context.Background(), "agent", agent, nil, nil); check.Level != "error" {
		t.Fatalf("probe without frozen env = %#v, want error (HOME unset)", check)
	}
	check := qualificationCommandCheck(context.Background(), "agent", agent, nil, map[string]string{"HOME": dir})
	if check.Level != "ok" {
		t.Fatalf("probe with frozen env = %#v, want ok", check)
	}
}
