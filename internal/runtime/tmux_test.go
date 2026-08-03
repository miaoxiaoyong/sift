package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestTmuxBackendBindingUsesFrozenHostLaunchNotBootstrap(t *testing.T) {
	dir := t.TempDir()
	tmux := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.args\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte(`{"run_id":"replaced","attempt_no":9,"generation":9,"dispatch_id":"replaced"}`), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, "/wrapper path/sift-agent-wrapper", filepath.Join(dir, "tmux.sock"))
	if err != nil {
		t.Fatal(err)
	}
	launch := HostLaunch{Backend: "tmux", RunID: "durable-run", AttemptNo: 2, Generation: 3, DispatchID: "durable-dispatch", WrapperPath: "/wrapper path/sift-agent-wrapper", BootstrapPath: bootstrap}
	if _, err := backend.Spawn(context.Background(), launch); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(tmux + ".args")
	if err != nil {
		t.Fatal(err)
	}
	want, err := TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(args))
	if len(got) < 8 || got[4] != "new-session" || got[7] != want {
		t.Fatalf("tmux argv = %#v, want frozen session %q", got, want)
	}
	if strings.Contains(string(args), "replaced") {
		t.Fatalf("tmux argv trusted replaced bootstrap identity: %q", args)
	}
}

func TestTmuxBackendExistingSessionIsTypedConflictWithoutReuse(t *testing.T) {
	dir := t.TempDir()
	tmux := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$5\" >> \"$0.calls\"\nif [ \"$5\" = new-session ]; then exit 1; fi\nif [ \"$5\" = has-session ]; then exit 0; fi\nexit 99\n"
	if err := os.WriteFile(tmux, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, "/wrapper", filepath.Join(dir, "tmux.sock"))
	if err != nil {
		t.Fatal(err)
	}
	// The bootstrap represents a replaced generation/backend; the host only
	// receives the current durable tmux dispatch and must not adopt the pane.
	bootstrap := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte(`{"generation":99,"backend":"process"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = backend.Spawn(context.Background(), HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 2, DispatchID: "current", WrapperPath: "/wrapper", BootstrapPath: bootstrap})
	var conflict *TmuxSessionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Spawn error = %v, want typed tmux session conflict", err)
	}
	calls, err := os.ReadFile(tmux + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(calls)); strings.Join(got, ",") != "new-session,has-session" {
		t.Fatalf("tmux calls = %q, want only new-session then conflict check", got)
	}
}

func TestTmuxCredentialIsolationAndLiteralWrapperArgv(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("real tmux is not installed")
	}
	dir := t.TempDir()
	t.Setenv("SIFT_RUN_TOKEN", "daemon-run-secret")
	t.Setenv("GITHUB_TOKEN", "daemon-github-secret")
	wrapper := filepath.Join(dir, "wrapper space;$(printf PWN)")
	bootstrap := filepath.Join(dir, "bootstrap space;$(printf PWN).json")
	fixture := "#!/bin/sh\nprintf '%s\\000' \"$0\" \"$@\" > \"$1.argv\"\nenv -0 > \"$1.env\"\n: > \"$1.ready\"\nsleep 5\n"
	if err := os.WriteFile(wrapper, []byte(fixture), 0755); err != nil {
		t.Fatal(err)
	}
	const token = "bootstrap-only-secret"
	if err := os.WriteFile(bootstrap, []byte(`{"run_token":"`+token+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, wrapper, filepath.Join(dir, "tmux.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.command(context.Background(), "kill-server").Run()
	launch := HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: wrapper, BootstrapPath: bootstrap}
	if _, err := backend.Spawn(context.Background(), launch); err != nil {
		t.Fatal(err)
	}
	waitForTmuxFixture(t, bootstrap+".ready")
	argv, err := os.ReadFile(bootstrap + ".argv")
	if err != nil {
		t.Fatal(err)
	}
	gotArgv := strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00")
	if len(gotArgv) != 2 || gotArgv[0] != wrapper || gotArgv[1] != bootstrap {
		t.Fatalf("wrapper argv = %#v, want literal separate wrapper/bootstrap paths", gotArgv)
	}
	env, err := os.ReadFile(bootstrap + ".env")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SIFT_RUN_TOKEN", "GITHUB_TOKEN", "daemon-run-secret", "daemon-github-secret", token} {
		if strings.Contains(string(env), secret) || strings.Contains(string(argv), secret) {
			t.Fatalf("tmux wrapper received secret %q", secret)
		}
	}
	info, err := os.Stat(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("bootstrap permissions = %o, want 0600", info.Mode().Perm())
	}
	for _, name := range []string{"SIFT_RUN_TOKEN", "GITHUB_TOKEN"} {
		show := exec.Command(tmux, "-f", "/dev/null", "-S", backend.SocketPath(), "show-environment", "-g", name)
		show.Env = TmuxClientEnvironment()
		if out, err := show.CombinedOutput(); err == nil || strings.Contains(string(out), "secret") {
			t.Fatalf("tmux server retained %s: %q, %v", name, out, err)
		}
	}
}

func waitForTmuxFixture(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for tmux wrapper fixture %q", path)
}
