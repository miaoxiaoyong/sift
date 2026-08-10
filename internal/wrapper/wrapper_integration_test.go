package wrapper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/controlplane"
	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
	"github.com/miaoxiaoyong/sift/internal/worktree"
)

func TestProductionWrapperCrashWindows(t *testing.T) {
	wrapperPath := buildWrapper(t)
	cases := []struct {
		name       string
		bootstrap  bool
		private    bool
		reject     string
		executable string
		wantSpawn  int
		wantResult bool
	}{
		{name: "bootstrap", executable: "/bin/sh"},
		{name: "file", bootstrap: true, private: false, executable: "/bin/sh"},
		{name: "spawn", bootstrap: true, private: true, executable: "/not-an-agent"},
		{name: "acquire", bootstrap: true, private: true, reject: "claim.acquire", executable: "/bin/sh"},
		{name: "permit", bootstrap: true, private: true, reject: "claim.permit_spawn", executable: "/bin/sh"},
		{name: "started", bootstrap: true, private: true, reject: "claim.started", executable: "/bin/sh", wantSpawn: 1},
		{name: "quick-exit", bootstrap: true, private: true, executable: "/bin/sh", wantSpawn: 1, wantResult: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := shortTempDir(t)
			runDir := filepath.Join(root, "runs", "run-1", "attempts", "1")
			if err := os.MkdirAll(runDir, 0700); err != nil {
				t.Fatal(err)
			}
			worktree := t.TempDir()
			bootstrap := filepath.Join(runDir, "bootstrap.json")
			if c.bootstrap {
				data, err := json.Marshal(runtimepkg.Bootstrap{
					SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor,
					DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: "run-1", AttemptNo: 1,
					Generation: 1, DispatchID: "dispatch", BootstrapNonce: "aaaaaaaaaaaaaaaa", RunToken: "bbbbbbbbbbbbbbbb",
					RunDir: runDir, WorktreePath: worktree, Agent: runtimepkg.BootstrapAgent{ID: "agent", Executable: c.executable, Args: []string{"-c", "echo spawned >> \"$SIFT_RUN_DIR/spawn-count\""}, TaskTransport: "stdin"},
					TaskSpecSnapshotID: "task-1", TaskSpec: json.RawMessage(`{}`),
				})
				if err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0600)
				if !c.private {
					mode = 0644
				}
				if err := os.WriteFile(bootstrap, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			var server *wrapperServer
			if c.reject == "claim.started" {
				server = newWrapperServerWaitFor(t, root, c.reject, filepath.Join(runDir, "spawn-count"))
			} else {
				server = newWrapperServer(t, root, c.reject)
			}
			defer server.Close()
			cmd := osexec.Command(wrapperPath, bootstrap)
			out, err := cmd.CombinedOutput()
			if c.wantResult && err != nil {
				t.Fatalf("wrapper failed: %v\n%s", err, out)
			}
			if !c.wantResult && err == nil {
				t.Fatal("wrapper unexpectedly succeeded")
			}
			count := countLines(filepath.Join(runDir, "spawn-count"))
			if count != c.wantSpawn {
				t.Fatalf("agent spawn count = %d, want %d (err=%v, output=%s)", count, c.wantSpawn, err, out)
			}
			_, resultErr := os.Stat(filepath.Join(runDir, "result.json"))
			if (resultErr == nil) != c.wantResult {
				t.Fatalf("result exists=%v, want %v", resultErr == nil, c.wantResult)
			}
		})
	}
}

func TestProductionWrapperPTYRelaysRawOutputToLogAndHost(t *testing.T) {
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", `printf 'pty-raw\n'`})
	server := newWrapperServer(t, root, "")
	defer server.Close()
	out, err := osexec.Command(wrapperPath, bootstrap).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, out)
	}
	if got, want := string(out), "pty-raw\n"; got != want {
		t.Fatalf("host stream = %q, want %q; log=%q", got, want, readFile(t, filepath.Join(runDir, "agent.log")))
	}
	if got, want := readFile(t, filepath.Join(runDir, "agent.log")), "pty-raw\n"; got != want {
		t.Fatalf("agent.log = %q, want %q", got, want)
	}
}

func TestQualificationBinaryReplacementBetweenMeasurementAndAgentExecFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("sealed executable images require a Unix executable image")
	}
	root := shortTempDir(t)
	runDir := filepath.Join(root, "runs", "run-1", "attempts", "1")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "agent")
	old := []byte("#!/bin/sh\nprintf old > \"$SIFT_RUN_DIR/executed-bytes\"\n")
	if err := os.WriteFile(agent, old, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(old)
	bootstrapData, err := json.Marshal(runtimepkg.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", BootstrapNonce: "aaaaaaaaaaaaaaaa", RunToken: "bbbbbbbbbbbbbbbb", RunDir: runDir, WorktreePath: t.TempDir(), Agent: runtimepkg.BootstrapAgent{ID: "agent", Executable: agent, ExecutableSHA256: hex.EncodeToString(sum[:]), TaskTransport: "stdin"}, TaskSpecSnapshotID: "task-1", TaskSpec: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(runDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, bootstrapData, 0600); err != nil {
		t.Fatal(err)
	}
	server := newWrapperServer(t, root, "")
	defer server.Close()
	ready := filepath.Join(root, "qualified-image-ready")
	release := filepath.Join(root, "qualified-image-release")
	cmd := osexec.Command(buildWrapperWithTags(t, "sift_test"), bootstrap)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = append(os.Environ(), "SIFT_WRAPPER_TEST_PAUSE=after-qualified-executable-open", "SIFT_WRAPPER_TEST_READY="+ready, "SIFT_WRAPPER_TEST_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForWrapperFile(t, ready)

	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("#!/bin/sh\nprintf replacement > \"$SIFT_RUN_DIR/executed-bytes\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, agent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper failed after executing sealed image: %v\n%s", err, output.String())
	}
	if got := readFile(t, filepath.Join(runDir, "executed-bytes")); got != "old" {
		t.Fatalf("agent bytes after wrapper-check-to-exec replacement = %q, want old verified image", got)
	}
	if _, err := os.Stat(filepath.Join(runDir, "qualification-invalid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed image unexpectedly invalidated qualification: %v", err)
	}
}

func TestQualificationBinaryInPlaceMutationBetweenMaterializationAndAgentExecFailsClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("in-place mutation coverage is Darwin-specific")
	}
	root := shortTempDir(t)
	runDir := filepath.Join(root, "runs", "run-1", "attempts", "1")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "agent")
	old := []byte("#!/bin/sh\nprintf old > \"$SIFT_RUN_DIR/executed-bytes\"\n")
	if err := os.WriteFile(agent, old, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(old)
	bootstrapData, err := json.Marshal(runtimepkg.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", BootstrapNonce: "aaaaaaaaaaaaaaaa", RunToken: "bbbbbbbbbbbbbbbb", RunDir: runDir, WorktreePath: t.TempDir(), Agent: runtimepkg.BootstrapAgent{ID: "agent", Executable: agent, ExecutableSHA256: hex.EncodeToString(sum[:]), TaskTransport: "stdin"}, TaskSpecSnapshotID: "task-1", TaskSpec: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(runDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, bootstrapData, 0600); err != nil {
		t.Fatal(err)
	}
	server := newWrapperServer(t, root, "")
	defer server.Close()
	ready := filepath.Join(root, "qualified-image-ready")
	release := filepath.Join(root, "qualified-image-release")
	cmd := osexec.Command(buildWrapperWithTags(t, "sift_test"), bootstrap)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = append(os.Environ(), "SIFT_WRAPPER_TEST_PAUSE=after-qualified-executable-open", "SIFT_WRAPPER_TEST_READY="+ready, "SIFT_WRAPPER_TEST_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForWrapperFile(t, ready)

	originalInfo, err := os.Stat(agent)
	if err != nil {
		t.Fatal(err)
	}
	mutated, err := os.OpenFile(agent, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutated.Write([]byte("#!/bin/sh\nprintf replacement > \"$SIFT_RUN_DIR/executed-bytes\"\n")); err != nil {
		_ = mutated.Close()
		t.Fatal(err)
	}
	if err := mutated.Close(); err != nil {
		t.Fatal(err)
	}
	mutatedInfo, err := os.Stat(agent)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(originalInfo, mutatedInfo) {
		t.Fatal("mutation unexpectedly replaced the original inode")
	}
	if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper failed after executing sealed image: %v\n%s", err, output.String())
	}
	if got := readFile(t, filepath.Join(runDir, "executed-bytes")); got != "old" {
		t.Fatalf("agent bytes after in-place mutation = %q, want old verified image", got)
	}
	if _, err := os.Stat(filepath.Join(runDir, "qualification-invalid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed image unexpectedly invalidated qualification: %v", err)
	}
}

// TestBackendV2HandoffResponseReplayAndSpawnOnce runs the same wrapper through
// both real hosts. Dropping acquire, permit, and started replies models a
// response loss after the daemon's durable commit; replay must retain the
// original handoff tuple and PermitGate must still invoke the launcher once.
func TestBackendV2HandoffResponseReplayAndSpawnOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Fatal("V2 backend handoff harness requires Unix process groups")
	}
	tmux, err := osexec.LookPath("tmux")
	if err != nil {
		t.Fatalf("V2 tmux backend is required: %v", err)
	}
	wrapperPath := buildWrapper(t)
	for _, backend := range []string{"process", "tmux"} {
		t.Run("V2/"+backend+"/acquire-permit-started-replay/PASS", func(t *testing.T) {
			root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", "echo spawned >> \"$SIFT_RUN_DIR/spawn-count\""})
			server := newWrapperServer(t, root, "")
			server.dropFirstResponse("claim.acquire", "claim.permit_spawn", "claim.started")
			defer server.Close()
			if backend == "process" {
				cmd := osexec.Command(wrapperPath, bootstrap)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("process wrapper replay: %v\\n%s", err, out)
				}
			} else {
				host, err := runtimepkg.NewTmuxBackend(tmux, wrapperPath, filepath.Join(root, "tmux.sock"), staticTmuxBindingVerifier)
				if err != nil {
					t.Fatal(err)
				}
				defer killTmuxServer(t, tmux, host.SocketPath())
				launch := runtimepkg.HostLaunch{Backend: "tmux", RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: wrapperPath, BootstrapPath: bootstrap}
				if _, err := host.Spawn(context.Background(), launch); err != nil {
					t.Fatalf("tmux wrapper replay: %v", err)
				}
				name, err := runtimepkg.TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
				if err != nil {
					t.Fatal(err)
				}
				waitForTmuxSessionGone(t, tmux, host.SocketPath(), name)
			}
			if got := countLines(filepath.Join(runDir, "spawn-count")); got != 1 {
				t.Fatalf("%s spawnCalls=%d, want 1", backend, got)
			}
			for _, method := range []string{"claim.acquire", "claim.permit_spawn", "claim.started"} {
				params := server.replayedParams(method)
				if len(params) != 2 || string(params[0]) != string(params[1]) {
					t.Fatalf("%s %s replay params=%q, want exactly identical pair", backend, method, params)
				}
			}
		})
	}
}

func TestProductionWrapperReplaysLostPermitResponseWithSameParameters(t *testing.T) {
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", "echo spawned >> \"$SIFT_RUN_DIR/spawn-count\""})
	server := newWrapperServerDroppingFirstPermit(t, root)
	defer server.Close()
	out, err := osexec.Command(wrapperPath, bootstrap).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed after permit replay: %v\n%s", err, out)
	}
	if count := countLines(filepath.Join(runDir, "spawn-count")); count != 1 {
		t.Fatalf("agent spawn count = %d, want 1", count)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.permitParams) != 2 {
		t.Fatalf("permit requests = %d, want 2", len(server.permitParams))
	}
	if string(server.permitParams[0]) != string(server.permitParams[1]) {
		t.Fatalf("lost permit replay changed parameters: %s != %s", server.permitParams[0], server.permitParams[1])
	}
}

func TestProductionWrapperReapsTERMIgnoringGroupAfterStartedFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", "trap '' TERM; (trap '' TERM; while :; do :; done) & echo $! > \"$SIFT_RUN_DIR/descendant.pid\"; wait"})
	server := newWrapperServerWaitFor(t, root, "claim.started", filepath.Join(runDir, "descendant.pid"))
	defer server.Close()
	cmd := osexec.Command(wrapperPath, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := executionWrapperPGID(t, filepath.Join(runDir, "control.json"))
	assertProcessInGroup(t, filepath.Join(runDir, "descendant.pid"), pgid)
	if err := cmd.Wait(); err == nil {
		t.Fatal("wrapper unexpectedly succeeded")
	}
	assertGroupAbsent(t, pgid)
}

func TestProductionWrapperPTYRelayEOFThenSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", `exec 1>&- 2>&-; : > "$SIFT_RUN_DIR/pty-eof"; (trap '' TERM; while :; do :; done) & echo $! > "$SIFT_RUN_DIR/descendant.pid"; trap '' TERM; while :; do :; done`})
	server := newWrapperServer(t, root, "")
	defer server.Close()
	cmd := osexec.Command(wrapperPath, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := executionWrapperPGID(t, filepath.Join(runDir, "control.json"))
	waitForWrapperFile(t, filepath.Join(runDir, "pty-eof"))
	assertProcessInGroup(t, filepath.Join(runDir, "descendant.pid"), pgid)
	// All processes closed the slave before the barrier above, so the relay has
	// observed PTY EOF before the supervisor is signalled.
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("wrapper unexpectedly succeeded")
	}
	assertGroupAbsent(t, pgid)
	if _, err := os.Stat(filepath.Join(runDir, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful result exists after signal: %v", err)
	}
}

func TestProductionWrapperRecordsAgentLogRelayFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/dev/full is Linux-specific")
	}
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", `trap '' TERM; (trap '' TERM; while :; do :; done) & echo $! > "$SIFT_RUN_DIR/descendant.pid"; while test ! -f "$SIFT_RUN_DIR/log-trigger"; do sleep 0.01; done; printf log-trigger; while :; do :; done`})
	if err := os.Symlink("/dev/full", filepath.Join(runDir, "agent.log")); err != nil {
		t.Fatal(err)
	}
	server := newWrapperServerWithStartedBarrier(t, root)
	defer server.Close()
	cmd := osexec.Command(wrapperPath, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := executionWrapperPGID(t, filepath.Join(runDir, "control.json"))
	assertProcessInGroup(t, filepath.Join(runDir, "descendant.pid"), pgid)
	server.waitForStartedReceipt(t) // Agent has not yet been allowed to write.
	if err := os.WriteFile(filepath.Join(runDir, "log-trigger"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// relayFailure records only a private pre-termination diagnostic. Its
	// presence proves the relay has failed while claim.started remains blocked;
	// result.json must still be unavailable to the production decoder.
	waitForWrapperFile(t, relayFailurePath(runDir))
	waitForPendingReaper(t, filepath.Join(runDir, "reaper-result.json"))
	if _, err := worktree.ReadResult(filepath.Join(runDir, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal result was consumable before group reaping: %v", err)
	}
	server.confirmStarted()
	if err := cmd.Wait(); err == nil {
		t.Fatal("wrapper unexpectedly succeeded")
	}
	assertGroupAbsent(t, pgid)
	result, err := worktree.ReadResult(filepath.Join(runDir, "result.json"))
	if err != nil {
		t.Fatalf("read published relay failure result: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode == 0 || result.FailureReason != agentLogRelayFailure {
		t.Fatalf("result = %+v, want non-success %q failure", result, agentLogRelayFailure)
	}
}

func TestProductionWrapperReapsTERMIgnoringAgentOnTerminationSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", "trap '' TERM; while :; do :; done"})
	server := newWrapperServer(t, root, "")
	defer server.Close()
	cmd := osexec.Command(wrapperPath, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := executionWrapperPGID(t, filepath.Join(runDir, "control.json"))
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(runDir, "control.json")); err == nil {
			var control struct {
				AgentIdentity any `json:"agent_identity"`
			}
			if json.Unmarshal(data, &control) == nil && control.AgentIdentity != nil {
				started = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("wrapper did not publish agent identity")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("wrapper unexpectedly succeeded")
	}
	assertGroupAbsent(t, pgid)
}

func executionWrapperPGID(t *testing.T, controlPath string) int {
	t.Helper()
	return executionWrapperPID(t, controlPath)
}

func executionWrapperPID(t *testing.T, controlPath string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(controlPath)
		if err == nil {
			var control struct {
				WrapperIdentity struct {
					PID int `json:"pid"`
				} `json:"wrapper_identity"`
			}
			if json.Unmarshal(data, &control) == nil && control.WrapperIdentity.PID > 0 {
				return control.WrapperIdentity.PID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("execution wrapper did not publish identity")
	return 0
}

func assertProcessInGroup(t *testing.T, pidPath string, pgid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			got, err := syscall.Getpgid(pid)
			if err != nil {
				t.Fatalf("get descendant process group: %v", err)
			}
			if got != pgid {
				t.Fatalf("descendant pgid=%d, want wrapper pgid=%d", got, pgid)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("TERM-ignoring descendant was not started")
}

func assertGroupAbsent(t *testing.T, pgid int) {
	t.Helper()
	if err := syscall.Kill(-pgid, 0); err != syscall.ESRCH {
		t.Fatalf("process group %d exists at wrapper return: %v", pgid, err)
	}
}

func TestReapProcessGroupRecordsFailure(t *testing.T) {
	result := filepath.Join(t.TempDir(), "reaper-result.json")
	if err := ReapProcessGroup(0, result); err == nil {
		t.Fatal("reaper unexpectedly succeeded")
	}
	if err := waitForReaper(result); err == nil {
		t.Fatal("supervisor did not observe reaper failure")
	}
}

func TestProductionTmuxWrapperKeepsAgentInWrapperProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	tmux := requireRealTmux(t)
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", `if test -t 1; then echo yes > "$SIFT_RUN_DIR/pty-active"; else echo no > "$SIFT_RUN_DIR/pty-active"; fi; echo "$$" > "$SIFT_RUN_DIR/agent-pid"; while test ! -f "$SIFT_RUN_DIR/finish"; do sleep 0.01; done`})
	server := newWrapperServerWithStartedBarrier(t, root)
	defer server.Close()
	backend, err := runtimepkg.NewTmuxBackend(tmux, wrapperPath, filepath.Join(root, "tmux.sock"), staticTmuxBindingVerifier)
	if err != nil {
		t.Fatal(err)
	}
	defer killTmuxServer(t, tmux, backend.SocketPath())
	launch := runtimepkg.HostLaunch{Backend: "tmux", RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: wrapperPath, BootstrapPath: bootstrap}
	if _, err := backend.Spawn(context.Background(), launch); err != nil {
		t.Fatalf("tmux wrapper host: %v", err)
	}

	waitForWrapperFile(t, filepath.Join(runDir, "pty-active"))
	wrapperPID := executionWrapperPID(t, filepath.Join(runDir, "control.json"))
	agentPID := readPIDFile(t, filepath.Join(runDir, "agent-pid"))
	server.waitForStartedReceipt(t)
	assertAgentTopology(t, wrapperPID, agentPID)
	if got := strings.TrimSpace(readFile(t, filepath.Join(runDir, "pty-active"))); got != "yes" {
		t.Fatalf("agent stdout is tty = %q, want yes", got)
	}

	server.confirmStarted()
	server.waitForStartedConfirmation(t)
	waitForWrapperFile(t, filepath.Join(runDir, "heartbeat"))
	assertAgentTopology(t, wrapperPID, agentPID)
	if err := os.WriteFile(filepath.Join(runDir, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForWrapperFile(t, filepath.Join(runDir, "result.json"))
	name, err := runtimepkg.TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	waitForTmuxSessionGone(t, tmux, backend.SocketPath(), name)
}

// TestProductionTmuxWrapperCrashWindows repeats the wrapper handoff failure
// matrix through the real tmux host. Each early failure proves the named
// wrapper boundary, not merely that tmux eventually removed a session.
func TestProductionTmuxWrapperCrashWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	tmux := requireRealTmux(t)
	wrapperPath := buildWrapper(t)
	cases := []struct {
		name, reject, executable, wantRequest, wantDiagnostic string
		bootstrap, private, wantResult                        bool
		wantSpawn                                             int
	}{
		{name: "bootstrap", executable: "/bin/sh", wantDiagnostic: "no such file"},
		{name: "file", bootstrap: true, executable: "/bin/sh", wantDiagnostic: "unsafe bootstrap file"},
		{name: "spawn", bootstrap: true, private: true, executable: "/not-an-agent", wantDiagnostic: "runtime: launch agent"},
		{name: "acquire", bootstrap: true, private: true, reject: "claim.acquire", executable: "/bin/sh", wantRequest: "claim.acquire"},
		{name: "permit", bootstrap: true, private: true, reject: "claim.permit_spawn", executable: "/bin/sh", wantRequest: "claim.permit_spawn"},
		{name: "started", bootstrap: true, private: true, reject: "claim.started", executable: "/bin/sh", wantSpawn: 1},
		{name: "quick-exit", bootstrap: true, private: true, executable: "/bin/sh", wantSpawn: 1, wantResult: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := shortTempDir(t)
			runDir := filepath.Join(root, "runs", "run-1", "attempts", "1")
			if err := os.MkdirAll(runDir, 0700); err != nil {
				t.Fatal(err)
			}
			bootstrap := filepath.Join(runDir, "bootstrap.json")
			if c.bootstrap {
				data, err := json.Marshal(runtimepkg.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", BootstrapNonce: "aaaaaaaaaaaaaaaa", RunToken: "bbbbbbbbbbbbbbbb", RunDir: runDir, WorktreePath: t.TempDir(), Agent: runtimepkg.BootstrapAgent{ID: "agent", Executable: c.executable, Args: []string{"-c", "echo spawned >> \"$SIFT_RUN_DIR/spawn-count\""}, TaskTransport: "stdin"}, TaskSpecSnapshotID: "task-1", TaskSpec: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0600)
				if !c.private {
					mode = 0644
				}
				if err := os.WriteFile(bootstrap, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			waitPath := ""
			if c.name == "started" {
				// claim.started races Agent.Start. Delay its intentional rejection
				// until the real shell fixture has recorded the spawn side effect.
				waitPath = filepath.Join(runDir, "spawn-count")
			}
			server := newWrapperServerWaitFor(t, root, c.reject, waitPath)
			defer server.Close()
			hostWrapperPath := wrapperPath
			diagnostic := ""
			if c.wantDiagnostic != "" {
				diagnostic = filepath.Join(root, "wrapper-diagnostic")
				hostWrapperPath = instrumentTmuxWrapper(t, wrapperPath, filepath.Join(root, "wrapper-started"), diagnostic)
			}
			backend, err := runtimepkg.NewTmuxBackend(tmux, hostWrapperPath, filepath.Join(root, "tmux.sock"), staticTmuxBindingVerifier)
			if err != nil {
				t.Fatal(err)
			}
			defer killTmuxServer(t, tmux, backend.SocketPath())
			launch := runtimepkg.HostLaunch{Backend: "tmux", RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", WrapperPath: hostWrapperPath, BootstrapPath: bootstrap}
			if _, err := backend.Spawn(context.Background(), launch); err != nil {
				t.Fatalf("tmux wrapper host: %v", err)
			}
			name, err := runtimepkg.TmuxSessionName(launch.RunID, launch.AttemptNo, launch.Generation, launch.DispatchID)
			if err != nil {
				t.Fatal(err)
			}
			waitForTmuxSessionGone(t, tmux, backend.SocketPath(), name)
			if c.wantDiagnostic != "" {
				waitForWrapperFile(t, filepath.Join(root, "wrapper-started"))
				if got := readFile(t, diagnostic); !strings.Contains(strings.ToLower(got), c.wantDiagnostic) {
					t.Fatalf("wrapper diagnostic = %q, want substring %q", got, c.wantDiagnostic)
				}
			}
			if c.wantRequest != "" && server.requestCount(c.wantRequest) != 1 {
				t.Fatalf("%s requests = %d, want 1", c.wantRequest, server.requestCount(c.wantRequest))
			}
			if count := countLines(filepath.Join(runDir, "spawn-count")); count != c.wantSpawn {
				t.Fatalf("agent spawn count = %d, want %d", count, c.wantSpawn)
			}
			_, resultErr := os.Stat(filepath.Join(runDir, "result.json"))
			if (resultErr == nil) != c.wantResult {
				t.Fatalf("result exists=%v, want %v", resultErr == nil, c.wantResult)
			}
		})
	}
}

func TestProductionWrapperKeepsAgentInWrapperProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups differ on Windows")
	}
	wrapperPath := buildWrapper(t)
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", `if test -t 1; then echo yes > "$SIFT_RUN_DIR/pty-active"; else echo no > "$SIFT_RUN_DIR/pty-active"; fi; echo "$$" > "$SIFT_RUN_DIR/agent-pid"; while test ! -f "$SIFT_RUN_DIR/finish"; do sleep 0.01; done`})
	server := newWrapperServerWithStartedBarrier(t, root)
	defer server.Close()
	cmd := osexec.Command(wrapperPath, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waitForWrapperFile(t, filepath.Join(runDir, "pty-active"))
	wrapperPID := executionWrapperPID(t, filepath.Join(runDir, "control.json"))
	agentPID := readPIDFile(t, filepath.Join(runDir, "agent-pid"))
	server.waitForStartedReceipt(t)
	assertAgentTopology(t, wrapperPID, agentPID)
	if got := strings.TrimSpace(readFile(t, filepath.Join(runDir, "pty-active"))); got != "yes" {
		t.Fatalf("agent stdout is tty = %q, want yes", got)
	}

	heartbeat := filepath.Join(runDir, "heartbeat")
	if _, err := os.Stat(heartbeat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-start heartbeat was pre-stored: %v", err)
	}
	server.confirmStarted()
	server.waitForStartedConfirmation(t) // server has written the response
	waitForWrapperFile(t, heartbeat)     // wrapper returned from call and ran post-claim.started code
	assertAgentTopology(t, wrapperPID, agentPID)
	if err := os.WriteFile(filepath.Join(runDir, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper failed: %v", err)
	}
}

func waitForWrapperFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file was not written: %s", path)
}

func waitForPendingReaper(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var result struct {
				Pending bool `json:"pending"`
			}
			if json.Unmarshal(data, &result) == nil && result.Pending {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("reaper was not pending: %s", path)
}

func readPIDFile(t *testing.T, paths ...string) int {
	t.Helper()
	for _, path := range paths {
		waitForNonEmptyFile(t, path)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, paths[0])))
	if err != nil {
		t.Fatalf("parse pid %s: %v", paths[0], err)
	}
	return pid
}

func waitForNonEmptyFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file was not populated: %s", path)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertAgentTopology(t *testing.T, wrapperPID, agentPID int) {
	t.Helper()
	out, err := osexec.Command("ps", "-o", "ppid=", "-o", "pgid=", "-p", strconv.Itoa(agentPID)).Output()
	if err != nil {
		t.Fatalf("read agent OS topology: %v", err)
	}
	facts := strings.Fields(string(out))
	if len(facts) != 2 {
		t.Fatalf("agent OS topology = %q, want PPID and PGID", out)
	}
	ppid, err := strconv.Atoi(facts[0])
	if err != nil {
		t.Fatalf("parse agent PPID: %v", err)
	}
	agentPGID, err := strconv.Atoi(facts[1])
	if err != nil {
		t.Fatalf("parse agent PGID: %v", err)
	}
	wrapperPGID, err := syscall.Getpgid(wrapperPID)
	if err != nil {
		t.Fatalf("get wrapper PGID: %v", err)
	}
	if ppid != wrapperPID {
		t.Fatalf("agent PPID=%d, want wrapper PID=%d", ppid, wrapperPID)
	}
	if wrapperPGID != wrapperPID {
		t.Fatalf("wrapper PGID=%d, want wrapper PID=%d", wrapperPGID, wrapperPID)
	}
	if agentPGID != wrapperPGID {
		t.Fatalf("agent PGID=%d, wrapper PGID=%d", agentPGID, wrapperPGID)
	}
	if agentPID == agentPGID {
		t.Fatalf("agent PID=%d unexpectedly leads its process group", agentPID)
	}
}

func buildWrapper(t *testing.T) string {
	return buildWrapperWithTags(t)
}

func buildWrapperWithTags(t *testing.T, tags ...string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	path := filepath.Join(t.TempDir(), "sift-agent-wrapper")
	args := []string{"build", "-o", path}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, "./cmd/sift-agent-wrapper")
	cmd := osexec.Command("go", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wrapper: %v\n%s", err, out)
	}
	return path
}

func validBootstrap(t *testing.T, executable string, args []string) (string, string, string) {
	t.Helper()
	root := shortTempDir(t)
	runDir := filepath.Join(root, "runs", "run-1", "attempts", "1")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(runtimepkg.Bootstrap{SchemaVersion: 2, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, DaemonVersion: controlplane.Version, WrapperVersion: controlplane.Version, RunID: "run-1", AttemptNo: 1, Generation: 1, DispatchID: "dispatch", BootstrapNonce: "aaaaaaaaaaaaaaaa", RunToken: "bbbbbbbbbbbbbbbb", RunDir: runDir, WorktreePath: t.TempDir(), Agent: runtimepkg.BootstrapAgent{Executable: executable, Args: args, TaskTransport: "stdin"}, TaskSpecSnapshotID: "task-1", TaskSpec: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runDir, "bootstrap.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return root, runDir, path
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp", "sift-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}

func countLines(path string) int {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		return -1
	}
	defer f.Close()
	n := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		n++
	}
	return n
}

type wrapperServer struct {
	listener            net.Listener
	reject              string
	waitPath            string
	dropFirstPermit     bool
	dropFirst           map[string]bool
	permitParams        []json.RawMessage
	paramsByMethod      map[string][]json.RawMessage
	requests            map[string]int
	startedReceipt      chan struct{}
	startedRelease      chan struct{}
	startedConfirmation chan struct{}
	mu                  sync.Mutex
	once                sync.Once
	startedReceiptOnce  sync.Once
	startedReleaseOnce  sync.Once
	startedConfirmOnce  sync.Once
}

func newWrapperServer(t *testing.T, root, reject string) *wrapperServer {
	return newWrapperServerWithPermitLoss(t, root, reject, "", false)
}

func newWrapperServerDroppingFirstPermit(t *testing.T, root string) *wrapperServer {
	return newWrapperServerWithPermitLoss(t, root, "", "", true)
}

func newWrapperServerWaitFor(t *testing.T, root, reject, waitPath string) *wrapperServer {
	return newWrapperServerWithPermitLoss(t, root, reject, waitPath, false)
}

func newWrapperServerWithStartedBarrier(t *testing.T, root string) *wrapperServer {
	return newWrapperServerWithOptions(t, root, "", "", false, true)
}

func (s *wrapperServer) waitForStartedReceipt(t *testing.T) {
	t.Helper()
	select {
	case <-s.startedReceipt:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive claim.started")
	}
}

func (s *wrapperServer) confirmStarted() {
	s.startedReleaseOnce.Do(func() { close(s.startedRelease) })
}

func (s *wrapperServer) waitForStartedConfirmation(t *testing.T) {
	t.Helper()
	select {
	case <-s.startedConfirmation:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not confirm claim.started")
	}
}

func newWrapperServerWithPermitLoss(t *testing.T, root, reject, waitPath string, dropFirstPermit bool) *wrapperServer {
	return newWrapperServerWithOptions(t, root, reject, waitPath, dropFirstPermit, false)
}

func newWrapperServerWithOptions(t *testing.T, root, reject, waitPath string, dropFirstPermit, startedBarrier bool) *wrapperServer {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(root, "run.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := &wrapperServer{listener: l, reject: reject, waitPath: waitPath, dropFirstPermit: dropFirstPermit, requests: make(map[string]int), paramsByMethod: make(map[string][]json.RawMessage)}
	if startedBarrier {
		s.startedReceipt = make(chan struct{})
		s.startedRelease = make(chan struct{})
		s.startedConfirmation = make(chan struct{})
	}
	go s.serve()
	return s
}
func (s *wrapperServer) Close() { s.once.Do(func() { _ = s.listener.Close() }) }
func (s *wrapperServer) requestCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[method]
}

func (s *wrapperServer) dropFirstResponse(methods ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropFirst == nil {
		s.dropFirst = make(map[string]bool)
	}
	for _, method := range methods {
		s.dropFirst[method] = true
	}
}

func (s *wrapperServer) replayedParams(method string) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.paramsByMethod[method]...)
}
func (s *wrapperServer) waitForPath() {
	if s.waitPath == "" {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.waitPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *wrapperServer) serve() {
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var h [4]byte
			if _, err := io.ReadFull(c, h[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(h[:]))
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
			var req struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(body, &req)
			s.mu.Lock()
			s.requests[req.Method]++
			s.paramsByMethod[req.Method] = append(s.paramsByMethod[req.Method], append(json.RawMessage(nil), req.Params...))
			drop := s.dropFirst[req.Method]
			if drop {
				delete(s.dropFirst, req.Method)
			}
			if req.Method == "claim.permit_spawn" {
				s.permitParams = append(s.permitParams, append(json.RawMessage(nil), req.Params...))
				drop = drop || (s.dropFirstPermit && len(s.permitParams) == 1)
			}
			s.mu.Unlock()
			if drop {
				return // The daemon committed but its response was lost in transit.
			}
			var confirmStarted func()
			if req.Method == "claim.started" && s.startedReceipt != nil {
				s.startedReceiptOnce.Do(func() { close(s.startedReceipt) })
				<-s.startedRelease
				confirmStarted = func() { s.startedConfirmOnce.Do(func() { close(s.startedConfirmation) }) }
			}
			var response any = map[string]any{"ok": true, "result": map[string]any{}}
			if req.Method == s.reject {
				s.waitForPath()
				response = map[string]any{"ok": false, "error": map[string]any{"code": "rejected"}}
			}
			b, _ := json.Marshal(response)
			binary.BigEndian.PutUint32(h[:], uint32(len(b)))
			if _, err := c.Write(append(h[:], b...)); err == nil && confirmStarted != nil {
				confirmStarted()
			}
		}()
	}
}

func instrumentTmuxWrapper(t *testing.T, wrapper, startedPath, diagnosticPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux-wrapper-probe")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\\"'\\\"'") + "'" }
	script := "#!/bin/sh\n: > " + quote(startedPath) + "\nexec " + quote(wrapper) + " \"$@\" 2> " + quote(diagnosticPath) + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func staticTmuxBindingVerifier(context.Context, runtimepkg.HostLaunch) error { return nil }

func requireRealTmux(t *testing.T) string {
	t.Helper()
	tmux, err := osexec.LookPath("tmux")
	if err == nil {
		return tmux
	}
	if os.Getenv("SIFT_REQUIRE_TMUX") != "" {
		t.Fatalf("real tmux is required but unavailable: %v", err)
	}
	t.Skip("real tmux is not installed")
	return ""
}

func waitForTmuxSessionGone(t *testing.T, tmux, socket, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd := osexec.Command(tmux, "-f", "/dev/null", "-S", socket, "has-session", "-t", "="+name)
		cmd.Env = runtimepkg.TmuxClientEnvironment()
		if err := cmd.Run(); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tmux session %q did not exit", name)
}

func killTmuxServer(t *testing.T, tmux, socket string) {
	t.Helper()
	cmd := osexec.Command(tmux, "-f", "/dev/null", "-S", socket, "kill-server")
	cmd.Env = runtimepkg.TmuxClientEnvironment()
	_ = cmd.Run()
}
