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
			server := newWrapperServer(t, root, c.reject)
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
	root, runDir, bootstrap := validBootstrap(t, "/bin/sh", []string{"-c", "sleep 2"})
	server := newWrapperServer(t, root, "")
	defer server.Close()
	cmd := osexec.Command(wrapperPath, bootstrap)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(runDir, "control.json"))
		if err == nil && string(data) != "" {
			var control struct {
				AgentIdentity *struct {
					PID int `json:"pid"`
				} `json:"agent_identity"`
			}
			if json.Unmarshal(data, &control) == nil && control.AgentIdentity != nil {
				wrapperPGID, err1 := syscall.Getpgid(executionWrapperPID(t, filepath.Join(runDir, "control.json")))
				agentPGID, err2 := syscall.Getpgid(control.AgentIdentity.PID)
				if err1 != nil || err2 != nil {
					t.Fatalf("get process groups: %v %v", err1, err2)
				}
				if wrapperPGID != agentPGID {
					t.Fatalf("agent pgid=%d, wrapper pgid=%d", agentPGID, wrapperPGID)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wrapper did not publish agent identity")
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
	listener net.Listener
	reject   string
	waitPath string
	once     sync.Once
}

func newWrapperServer(t *testing.T, root, reject string) *wrapperServer {
	return newWrapperServerWaitFor(t, root, reject, "")
}

func newWrapperServerWaitFor(t *testing.T, root, reject, waitPath string) *wrapperServer {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(root, "run.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := &wrapperServer{listener: l, reject: reject, waitPath: waitPath}
	go s.serve()
	return s
}
func (s *wrapperServer) Close() { s.once.Do(func() { _ = s.listener.Close() }) }
func (s *wrapperServer) waitForPath() {
	if s.waitPath == "" {
		return
	}
	deadline := time.Now().Add(time.Second)
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
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			var response any = map[string]any{"ok": true, "result": map[string]any{}}
			if req.Method == s.reject {
				s.waitForPath()
				response = map[string]any{"ok": false, "error": map[string]any{"code": "rejected"}}
			}
			b, _ := json.Marshal(response)
			binary.BigEndian.PutUint32(h[:], uint32(len(b)))
			_, _ = c.Write(append(h[:], b...))
		}()
	}
}
