package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func staticBindingVerifier(context.Context, HostLaunch) error { return nil }

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
	backend, err := NewTmuxBackend(tmux, "/wrapper path/sift-agent-wrapper", filepath.Join(dir, "tmux.sock"), staticBindingVerifier)
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

func TestTmuxBackendReclaimConvergesToVerifiedBinding(t *testing.T) {
	dir := t.TempDir()
	tmux := filepath.Join(dir, "tmux")
	wrapper := filepath.Join(dir, "wrapper")
	bootstrap := filepath.Join(dir, "bootstrap.json")
	launch := HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 2, DispatchID: "current", WrapperPath: wrapper, BootstrapPath: bootstrap}
	name, err := TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(name, "sift-")
	if err := os.WriteFile(bootstrap, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// new-session creates the wrapper, then always loses its response. Concurrent
	// reclaimers must validate that one session rather than starting another.
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf 'wrapper\\n' >> \"$1.wrapper\"\nprintf 'agent\\n' >> \"$1.agent\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nstate=\"$0.state\"\ncase \"$5\" in\nnew-session) if mkdir \"$state\" 2>/dev/null; then \"${12}\" \"${13}\" & fi; exit 1 ;;\nhas-session) test -d \"$state\" ;;\nshow-environment) printf 'SIFT_TMUX_BINDING=" + digest + "\\n' ;;\nlist-panes) printf '0\\n' ;;\nshow-options) printf 'off\\n' ;;\n*) exit 99 ;;\nesac\n"
	if err := os.WriteFile(tmux, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, wrapper, filepath.Join(dir, "tmux.sock"), staticBindingVerifier)
	if err != nil {
		t.Fatal(err)
	}

	const reclaimers = 16
	start := make(chan struct{})
	errs := make(chan error, reclaimers)
	for range reclaimers {
		go func() {
			<-start
			_, err := backend.Spawn(context.Background(), launch)
			errs <- err
		}()
	}
	close(start)
	for range reclaimers {
		if err := <-errs; err != nil {
			t.Fatalf("reclaim Spawn error = %v", err)
		}
	}
	waitForTmuxFixture(t, bootstrap+".agent")
	wrapperStarts, err := os.ReadFile(bootstrap + ".wrapper")
	if err != nil || string(wrapperStarts) != "wrapper\n" {
		t.Fatalf("wrapper starts = %q, %v; want exactly one", wrapperStarts, err)
	}
	agentStarts, err := os.ReadFile(bootstrap + ".agent")
	if err != nil || string(agentStarts) != "agent\n" {
		t.Fatalf("agent starts = %q, %v; want exactly one", agentStarts, err)
	}
}

func TestTmuxBackendReclaimRevalidatesDurableBindingBeforeConvergence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift bool
	}{
		{name: "durable_claim_drift", drift: true},
		{name: "unchanged_binding_converges", drift: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmux := filepath.Join(dir, "tmux")
			wrapper := filepath.Join(dir, "wrapper")
			bootstrap := filepath.Join(dir, "bootstrap.json")
			launch := HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: wrapper, BootstrapPath: bootstrap, OperationID: "op", LeaseOwner: "owner", LeaseExpiresAtMS: 100}
			name, err := TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
			if err != nil {
				t.Fatal(err)
			}
			digest := strings.TrimPrefix(name, "sift-")
			script := "#!/bin/sh\ncase \"$5\" in\nnew-session) exit 1 ;;\nhas-session) exit 0 ;;\nshow-environment) printf 'SIFT_TMUX_BINDING=" + digest + "\\n' ;;\nlist-panes) printf '0\\n' ;;\nshow-options) printf 'off\\n' ;;\n*) exit 99 ;;\nesac\n"
			if err := os.WriteFile(tmux, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bootstrap, nil, 0600); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var claimValid atomic.Bool
			claimValid.Store(!tc.drift)
			leaseErr := errors.New("durable launch binding moved")
			verifier := func(context.Context, HostLaunch) error {
				close(entered)
				<-release
				if !claimValid.Load() {
					return leaseErr
				}
				return nil
			}
			backend, err := NewTmuxBackend(tmux, wrapper, filepath.Join(dir, "tmux.sock"), verifier)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := backend.Spawn(context.Background(), launch)
				result <- err
			}()
			<-entered
			if tc.drift {
				claimValid.Store(false)
			}
			close(release)
			err = <-result
			if tc.drift {
				var conflict *TmuxSessionConflictError
				if !errors.As(err, &conflict) || !errors.Is(err, leaseErr) {
					t.Fatalf("drifted Spawn error = %v, want typed conflict wrapping lease error", err)
				}
			} else if err != nil {
				t.Fatalf("unchanged Spawn error = %v, want reclaim convergence", err)
			}
		})
	}
}

func TestTmuxBackendRejectsExistingSessionWithoutExactLiveBinding(t *testing.T) {
	for _, tc := range []struct {
		name, binding, panes, remain string
	}{
		{name: "binding_mismatch", binding: "wrong", panes: "0\\n", remain: "off\\n"},
		{name: "multiple_panes", panes: "0\\n0\\n", remain: "off\\n"},
		{name: "dead_pane", panes: "1\\n", remain: "off\\n"},
		{name: "remain_on_exit", panes: "0\\n", remain: "on\\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmux := filepath.Join(dir, "tmux")
			wrapper := filepath.Join(dir, "wrapper")
			bootstrap := filepath.Join(dir, "bootstrap.json")
			launch := HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 2, DispatchID: "current", WrapperPath: wrapper, BootstrapPath: bootstrap}
			name, err := TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
			if err != nil {
				t.Fatal(err)
			}
			binding := tc.binding
			if binding == "" {
				binding = strings.TrimPrefix(name, "sift-")
			}
			script := "#!/bin/sh\ncase \"$5\" in\nnew-session) exit 1 ;;\nhas-session) exit 0 ;;\nshow-environment) printf 'SIFT_TMUX_BINDING=" + binding + "\\n' ;;\nlist-panes) printf '" + tc.panes + "' ;;\nshow-options) printf '" + tc.remain + "' ;;\n*) exit 99 ;;\nesac\n"
			if err := os.WriteFile(tmux, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bootstrap, nil, 0600); err != nil {
				t.Fatal(err)
			}
			backend, err := NewTmuxBackend(tmux, wrapper, filepath.Join(dir, "tmux.sock"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.Spawn(context.Background(), launch)
			var conflict *TmuxSessionConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("Spawn error = %v, want typed tmux session conflict", err)
			}
		})
	}
}

func TestTmuxBackendRejectsRealSessionWithSecondWindow(t *testing.T) {
	tmux := requireRealTmux(t)
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "wrapper")
	bootstrap := filepath.Join(dir, "bootstrap.json")
	fixture := "#!/bin/sh\n: > \"$1.ready\"\nsleep 5\n"
	if err := os.WriteFile(wrapper, []byte(fixture), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrap, nil, 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, wrapper, filepath.Join(dir, "tmux.sock"), staticBindingVerifier)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.command(context.Background(), "kill-server").Run()
	launch := HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: wrapper, BootstrapPath: bootstrap}
	if _, err := backend.Spawn(context.Background(), launch); err != nil {
		t.Fatal(err)
	}
	waitForTmuxFixture(t, bootstrap+".ready")
	name, err := TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := backend.command(context.Background(), "new-window", "-d", "-t", "="+name, "--", "/bin/sh", "-c", "sleep 5").CombinedOutput(); err != nil {
		t.Fatalf("new-window output=%q: %v", out, err)
	}
	_, err = backend.Spawn(context.Background(), launch)
	var conflict *TmuxSessionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("second-window Spawn error = %v, want typed tmux session conflict", err)
	}
}

func TestTmuxCredentialIsolationAndLiteralWrapperArgv(t *testing.T) {
	tmux := requireRealTmux(t)
	dir := t.TempDir()
	t.Setenv("SIFT_RUN_TOKEN", "daemon-run-secret")
	t.Setenv("GITHUB_TOKEN", "daemon-github-secret")
	wrapper := filepath.Join(dir, "wrapper space;$(printf PWN)")
	bootstrap := filepath.Join(dir, "bootstrap space;$(printf PWN).json")
	fixture := "#!/bin/sh\nprintf 'wrapper\\n' >> \"$1.starts\"\nprintf '%s\\000' \"$0\" \"$@\" > \"$1.argv\"\nenv -0 > \"$1.env\"\n: > \"$1.ready\"\nsleep 5\n"
	if err := os.WriteFile(wrapper, []byte(fixture), 0755); err != nil {
		t.Fatal(err)
	}
	const token = "bootstrap-only-secret"
	if err := os.WriteFile(bootstrap, []byte(`{"run_token":"`+token+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := NewTmuxBackend(tmux, wrapper, filepath.Join(dir, "tmux.sock"), staticBindingVerifier)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.command(context.Background(), "kill-server").Run()
	launch := HostLaunch{Backend: "tmux", RunID: "run", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: wrapper, BootstrapPath: bootstrap}
	if _, err := backend.Spawn(context.Background(), launch); err != nil {
		t.Fatal(err)
	}
	waitForTmuxFixture(t, bootstrap+".ready")
	if _, err := backend.Spawn(context.Background(), launch); err != nil {
		t.Fatalf("reclaim verified tmux session: %v", err)
	}
	starts, err := os.ReadFile(bootstrap + ".starts")
	if err != nil || string(starts) != "wrapper\n" {
		t.Fatalf("wrapper starts after reclaim = %q, %v; want exactly one", starts, err)
	}
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

func requireRealTmux(t *testing.T) string {
	t.Helper()
	tmux, err := exec.LookPath("tmux")
	if err == nil {
		return tmux
	}
	if os.Getenv("SIFT_REQUIRE_TMUX") != "" {
		t.Fatalf("real tmux is required but unavailable: %v", err)
	}
	t.Skip("real tmux is not installed")
	return ""
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
