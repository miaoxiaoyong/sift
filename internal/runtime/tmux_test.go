package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTmuxSessionIdentityIsDigestOnlyAndDeterministic(t *testing.T) {
	name, err := TmuxSessionName("run with spaces;$(touch pwned)", 2, 7, "dispatch/secret-ish")
	if err != nil {
		t.Fatal(err)
	}
	again, err := TmuxSessionName("run with spaces;$(touch pwned)", 2, 7, "dispatch/secret-ish")
	if err != nil || name != again {
		t.Fatalf("session identity is not deterministic: %q/%q (%v)", name, again, err)
	}
	if !strings.HasPrefix(name, "sift-") || len(name) != len("sift-")+64 {
		t.Fatalf("session name = %q, want sift- plus 64 hex digits", name)
	}
	if strings.Contains(name, "run") || strings.ContainsAny(name, "/ ;$()") {
		t.Fatalf("session name contains launch input: %q", name)
	}
	if _, err := TmuxSessionName("", 1, 1, "dispatch"); err == nil {
		t.Fatal("incomplete launch identity unexpectedly accepted")
	}
}

func TestTmuxBackendPassesWrapperAndBootstrapAsSeparateArgv(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	tmux := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\n"
	if err := os.WriteFile(tmux, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_FILE", argsFile)
	bootstrap := filepath.Join(dir, "bootstrap with spaces;$(not-a-command).json")
	contents := `{"schema_version":2,"run_id":"run;name","attempt_no":1,"generation":1,"dispatch_id":"dispatch"}`
	if err := os.WriteFile(bootstrap, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, "/wrapper path/sift-agent-wrapper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Spawn(context.Background(), bootstrap); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 9 || lines[0] != "new-session" || lines[1] != "-d" || lines[2] != "-s" || lines[4] != "-e" || lines[6] != "--" || lines[7] != "/wrapper path/sift-agent-wrapper" || lines[8] != bootstrap {
		t.Fatalf("tmux argv = %#v", lines)
	}
}
