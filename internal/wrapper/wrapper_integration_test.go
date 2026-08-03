package wrapper

import (
	"bufio"
	"encoding/binary"
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
			runDir := filepath.Join(root, "runs", "run-1", "1")
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
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", `trap '' TERM; (trap '' TERM; while :; do :; done) & echo $! > "$SIFT_RUN_DIR/descendant.pid"; printf log-trigger; while :; do :; done`})
	if err := os.Symlink("/dev/full", filepath.Join(runDir, "agent.log")); err != nil {
		t.Fatal(err)
	}
	server := newWrapperServer(t, root, "")
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
	var result struct {
		ExitCode      *int   `json:"exit_code"`
		FailureReason string `json:"failure_reason"`
	}
	data := []byte(readFile(t, filepath.Join(runDir, "result.json")))
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode relay failure result: %v", err)
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

	server.confirmStarted()
	server.waitForStartedConfirmation(t)
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
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	path := filepath.Join(t.TempDir(), "sift-agent-wrapper")
	cmd := osexec.Command("go", "build", "-o", path, "./cmd/sift-agent-wrapper")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wrapper: %v\n%s", err, out)
	}
	return path
}

func validBootstrap(t *testing.T, executable string, args []string) (string, string, string) {
	t.Helper()
	root := shortTempDir(t)
	runDir := filepath.Join(root, "runs", "run-1", "1")
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
	permitParams        []json.RawMessage
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
	s := &wrapperServer{listener: l, reject: reject, waitPath: waitPath, dropFirstPermit: dropFirstPermit}
	if startedBarrier {
		s.startedReceipt = make(chan struct{})
		s.startedRelease = make(chan struct{})
		s.startedConfirmation = make(chan struct{})
	}
	go s.serve()
	return s
}
func (s *wrapperServer) Close() { s.once.Do(func() { _ = s.listener.Close() }) }
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
			if req.Method == "claim.permit_spawn" {
				s.mu.Lock()
				s.permitParams = append(s.permitParams, append(json.RawMessage(nil), req.Params...))
				drop := s.dropFirstPermit && len(s.permitParams) == 1
				s.mu.Unlock()
				if drop {
					return // The daemon committed but its response was lost in transit.
				}
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
