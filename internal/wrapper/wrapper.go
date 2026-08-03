// Package wrapper implements the per-attempt, one-shot wrapper state machine.
package wrapper

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/runtime"
)

// Run supervises the execution wrapper from outside its process group. This
// lets it wait for the reaper's confirmation after the execution wrapper is
// necessarily killed with the rest of that group.
func Run(ctx context.Context, bootstrapPath string) error {
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stopSignals()

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("wrapper: locate supervisor executable: %w", err)
	}
	cmd := exec.Command(self, "--run", bootstrapPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("wrapper: start execution wrapper: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("wrapper: forward termination signal: %w", err)
		}
		waitErr = <-waited
	}
	reaperResult := filepath.Join(filepath.Dir(bootstrapPath), "reaper-result.json")
	if !wasKilled(waitErr) {
		if _, err := os.Stat(reaperResult); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return waitErr
			}
			return fmt.Errorf("wrapper: stat reaper result: %w", err)
		}
	}
	if err := waitForReaper(reaperResult); err != nil {
		return err
	}
	return waitErr
}

func wasKilled(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// RunExecution executes the Agent in the child process group owned by Run.
func RunExecution(ctx context.Context, bootstrapPath string) error {
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stopSignals()
	// The wrapper is the process-group leader even when invoked outside the
	// production backend, so every Agent descendant has one supervision scope.
	if err := syscall.Setpgid(0, 0); err != nil && err != syscall.EPERM {
		return fmt.Errorf("wrapper: create process group: %w", err)
	}
	b, err := readBootstrap(bootstrapPath)
	if err != nil {
		return err
	}
	if err := os.Remove(bootstrapPath); err != nil {
		return fmt.Errorf("wrapper: unlink bootstrap: %w", err)
	}
	if b.SchemaVersion != 2 || b.ProtocolMajor != controlplane.ProtocolMajor || b.ProtocolMinor != controlplane.ProtocolMinor || b.DaemonVersion != controlplane.Version || b.WrapperVersion != controlplane.Version {
		return errors.New("wrapper: incompatible bootstrap")
	}
	if err := os.Remove(filepath.Join(b.RunDir, "reaper-result.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wrapper: clear reaper result: %w", err)
	}
	self, err := runtime.ProcessExecutable(os.Getpid())
	if err != nil {
		return err
	}
	pid := int64(os.Getpid())
	// Align with PlatformProcessInspector's procfs identity so recovery
	// liveness checks can match the persisted fields on Linux.
	started := runtime.ProcessStartedAtMS(os.Getpid())
	instance, session := secret(), secret()
	wi := map[string]any{"pid": pid, "started_at_ms": started, "executable": self, "pgid": pid}
	base := map[string]any{"run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "dispatch_id": b.DispatchID, "wrapper_instance_id": instance, "session_candidate": session, "wrapper_identity": wi}
	if _, err := call(ctx, b.RunDir, "claim.acquire", map[string]any{"kind": "bootstrap", "nonce": b.BootstrapNonce}, base); err != nil {
		return err
	}
	nonce := secret()
	control := map[string]any{"schema_version": 1, "run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "wrapper_identity": wi, "agent_identity": nil, "worktree_path": b.WorktreePath, "task_spec_snapshot_id": b.TaskSpecSnapshotID, "control_nonce": nonce, "run_token": b.RunToken, "updated_at_ms": time.Now().UnixMilli()}
	digest, err := writeJSON(filepath.Join(b.RunDir, "control.json"), control)
	if err != nil {
		return err
	}
	permit := secret()
	pp := map[string]any{"run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "wrapper_identity": wi, "control_digest": digest, "control_nonce_hash": hash(nonce), "permit_candidate": permit}
	if err := pauseForTest("before-permit-rpc"); err != nil {
		return err
	}
	// Establish the authoritative PTY before accepting the one-shot permit.
	// A lost response is not a new permit request: replay the exact candidate
	// and params with a new envelope request ID until the bounded deadline.
	var gate runtime.PermitGate
	log, err := os.OpenFile(filepath.Join(b.RunDir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	pty, err := runtime.NewPTY()
	if err != nil {
		return err
	}
	defer pty.Close()
	if _, err := callPermit(ctx, b.RunDir, map[string]any{"kind": "wrapper_session", "session": session}, pp); err != nil {
		return err
	}
	task := filepath.Join(b.RunDir, "task.json")
	if _, err := writeJSON(task, map[string]any{"schema_version": 1, "task_spec_snapshot_id": b.TaskSpecSnapshotID, "task_spec": json.RawMessage(b.TaskSpec)}); err != nil {
		return err
	}
	var stdin *os.File
	if b.Agent.TaskTransport == "stdin" {
		stdin, err = os.Open(task)
		if err != nil {
			return err
		}
		defer stdin.Close()
	}
	args := append([]string(nil), b.Agent.Args...)
	if b.Agent.TaskTransport == "file" {
		replaced := 0
		for i := range args {
			if args[i] == "{task_file}" {
				args[i], replaced = task, replaced+1
			}
		}
		if replaced != 1 {
			return errors.New("wrapper: file transport requires exactly one {task_file} argument")
		}
	} else if b.Agent.TaskTransport != "stdin" {
		return errors.New("wrapper: unsupported task transport")
	}
	var in = stdin
	launch := runtime.AgentLaunch{Executable: b.Agent.Executable, Args: args, Worktree: b.WorktreePath, RunDir: b.RunDir, Stdin: in, Stdout: pty.Slave, Stderr: pty.Slave}
	cmd, err := gate.StartOnce(ctx, runtime.DirectLauncher{}, launch)
	if err != nil {
		return err
	}
	// Only the Agent child owns the slave after Start. The wrapper reads the
	// master and keeps the persisted log ahead of the host observation stream.
	if err := pty.CloseSlave(); err != nil {
		return errors.Join(err, terminateAndReap(cmd, b.RunDir))
	}
	relayDone := make(chan error, 1)
	go func() { relayDone <- relayPTY(pty.Master, log, os.Stdout) }()
	relayConsumed := false
	defer func() {
		_ = pty.CloseMaster()
		if relayConsumed {
			return
		}
		select {
		case <-relayDone:
		case <-time.After(time.Second):
		}
	}()
	ai := map[string]any{"pid": int64(cmd.Process.Pid), "started_at_ms": time.Now().UnixMilli(), "executable": b.Agent.Executable}
	control["agent_identity"] = ai
	control["updated_at_ms"] = time.Now().UnixMilli()
	digest, err = writeJSON(filepath.Join(b.RunDir, "control.json"), control)
	if err != nil {
		return errors.Join(err, terminateAndReap(cmd, b.RunDir))
	}
	sp := map[string]any{"run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "agent_identity": ai, "control_digest": digest, "result_digest": nil}
	if err := pauseForTest("before-started-rpc"); err != nil {
		return err
	}
	if _, err = call(ctx, b.RunDir, "claim.started", map[string]any{"kind": "wrapper_started", "session": session, "permit": permit}, sp); err != nil {
		return errors.Join(err, terminateAndReap(cmd, b.RunDir))
	}
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go heartbeat(b, instance, stopHeartbeat)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	relayFinished := false
	select {
	case waitErr = <-waited:
	case relayErr := <-relayDone:
		relayFinished = true
		relayConsumed = true
		if relayErr != nil {
			return relayFailure(relayErr, b, instance, ai, digest)
		}
		select {
		case waitErr = <-waited:
		case <-ctx.Done():
			return errors.Join(ctx.Err(), terminateProcessGroup(b.RunDir))
		}
	case <-ctx.Done():
		return errors.Join(ctx.Err(), terminateProcessGroup(b.RunDir))
	}
	if !relayFinished {
		if err := waitForPTYRelay(relayDone, pty.Master); err != nil {
			relayConsumed = true
			return relayFailure(err, b, instance, ai, digest)
		}
		relayConsumed = true
	}
	exitCode, signal := resultStatus(waitErr)
	result := map[string]any{"schema_version": 1, "run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "agent_identity": ai, "exit_code": exitCode, "signal": signal, "finished_at_ms": time.Now().UnixMilli(), "final_head_sha": headSHA(b.WorktreePath), "control_digest": digest}
	if err := pauseForTest("before-result-rename"); err != nil {
		return err
	}
	if _, err := writeJSON(filepath.Join(b.RunDir, "result.json"), result); err != nil {
		return err
	}
	if err := pauseForTest("after-result-rename"); err != nil {
		return err
	}
	return waitErr
}

const (
	terminationGrace = 500 * time.Millisecond
	reaperGrace      = 5 * time.Second
)

// terminateAndReap bounds shutdown of the wrapper process group.
func terminateAndReap(cmd *exec.Cmd, runDir string) error {
	go cmd.Wait()
	return terminateProcessGroup(runDir)
}

func terminateProcessGroup(runDir string) error {
	pgid, err := wrapperProcessGroup()
	if err != nil {
		return fmt.Errorf("wrapper: find process group for reaper: %w", err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("wrapper: terminate process group: %w", err)
	}
	timer := time.NewTimer(terminationGrace)
	defer timer.Stop()
	// Reaping the direct child is not proof that its descendants are gone.
	// Always complete the bounded grace period and then kill the whole group.
	// The wrapper itself is in that group, so a helper must confirm absence.
	<-timer.C
	if err := startProcessGroupReaper(pgid, runDir); err != nil {
		return fmt.Errorf("wrapper: start process group reaper: %w", err)
	}
	return nil
}

func wrapperProcessGroup() (int, error) {
	pid := os.Getpid()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0, err
	}
	if pgid != pid {
		return 0, errors.New("wrapper: process group is not self-led")
	}
	return pgid, nil
}

func startProcessGroupReaper(pgid int, runDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	resultPath := filepath.Join(runDir, "reaper-result.json")
	if err := runtime.WriteControlFile(resultPath, []byte(`{"pending":true}`)); err != nil {
		return fmt.Errorf("wrapper: record pending reaper: %w", err)
	}
	reaper := exec.Command(self, "--reap-process-group", strconv.Itoa(pgid), resultPath)
	reaper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return reaper.Start()
}

// ReapProcessGroup sends SIGKILL to pgid, then proves its absence with bounded
// kill(-pgid, 0) probes. It runs in a separate process group because SIGKILL
// necessarily terminates the wrapper which owns the target group.
func ReapProcessGroup(pgid int, resultPath string) (err error) {
	defer func() {
		if resultPath == "" {
			return
		}
		result := struct {
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
		}{OK: err == nil}
		if err != nil {
			result.Error = err.Error()
		}
		data, _ := json.Marshal(result)
		if writeErr := runtime.WriteControlFile(resultPath, data); writeErr != nil && err == nil {
			err = fmt.Errorf("wrapper: record reaper result: %w", writeErr)
		}
	}()
	if pgid <= 0 {
		return errors.New("wrapper: invalid process group")
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("wrapper: kill process group: %w", err)
	}
	deadline := time.Now().Add(reaperGrace)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wrapper: probe process group: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("wrapper: process group remained after SIGKILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForReaper(path string) error {
	deadline := time.Now().Add(reaperGrace + terminationGrace)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var result struct {
				OK      bool   `json:"ok"`
				Pending bool   `json:"pending"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				return fmt.Errorf("wrapper: decode reaper result: %w", err)
			}
			if result.Pending {
				time.Sleep(10 * time.Millisecond)
				continue
			} else if !result.OK {
				return fmt.Errorf("wrapper: process group reaper failed: %s", result.Error)
			} else {
				return nil
			}
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wrapper: read reaper result: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("wrapper: process group reaper did not confirm completion")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readBootstrap(path string) (runtime.Bootstrap, error) {
	var b runtime.Bootstrap
	info, err := os.Lstat(path)
	if err != nil {
		return b, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return b, errors.New("wrapper: unsafe bootstrap file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(data, &b); err != nil {
		return b, err
	}
	return b, nil
}
func writeJSON(path string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if err = runtime.WriteControlFile(path, b); err != nil {
		return "", err
	}
	return hashBytes(b), nil
}
func secret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func hash(s string) string      { return hashBytes([]byte(s)) }
func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type rpcError struct{ code string }

func (e *rpcError) Error() string { return "wrapper: RPC rejected: " + e.code }

func callPermit(ctx context.Context, runDir string, auth, params map[string]any) (map[string]any, error) {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		result, err := call(ctx, runDir, "claim.permit_spawn", auth, params)
		if err == nil {
			return result, nil
		}
		var rejected *rpcError
		if errors.As(err, &rejected) {
			return nil, err
		}
		backoff := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			backoff.Stop()
			return nil, ctx.Err()
		case <-deadline.C:
			backoff.Stop()
			return nil, err
		case <-backoff.C:
		}
	}
}

func call(ctx context.Context, runDir, method string, auth, params map[string]any) (map[string]any, error) {
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(runDir))), "run.sock"))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopCancel()
	req := map[string]any{"protocol_major": 1, "protocol_minor": 0, "client_version": controlplane.Version, "request_id": secret()[:32], "method": method, "auth": auth, "params": params}
	b, _ := json.Marshal(req)
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if _, err = c.Write(append(h[:], b...)); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint32(h[:]))
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, err
	}
	var r struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		if r.Error == nil {
			return nil, errors.New("wrapper: malformed RPC error")
		}
		return nil, &rpcError{code: r.Error.Code}
	}
	return r.Result, nil
}

func heartbeat(b runtime.Bootstrap, instance string, stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if _, err := writeJSON(filepath.Join(b.RunDir, "heartbeat"), map[string]any{"schema_version": 1, "run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "observed_at_ms": time.Now().UnixMilli()}); err != nil {
			return
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

const agentLogRelayFailure = "agent_log_relay_failed"

type relayError struct {
	code string
	err  error
}

func (e *relayError) Error() string { return e.err.Error() }
func (e *relayError) Unwrap() error { return e.err }

func relayFailure(err error, b runtime.Bootstrap, instance string, ai map[string]any, controlDigest string) error {
	var relayErr *relayError
	if errors.As(err, &relayErr) && relayErr.code == agentLogRelayFailure {
		exitCode := 1
		result := map[string]any{"schema_version": 1, "run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "agent_identity": ai, "exit_code": exitCode, "signal": nil, "failure_reason": agentLogRelayFailure, "finished_at_ms": time.Now().UnixMilli(), "final_head_sha": headSHA(b.WorktreePath), "control_digest": controlDigest}
		if _, writeErr := writeJSON(filepath.Join(b.RunDir, "result.json"), result); writeErr != nil {
			err = errors.Join(err, fmt.Errorf("wrapper: record agent log relay failure: %w", writeErr))
		}
	}
	return errors.Join(err, terminateProcessGroup(b.RunDir))
}

func relayPTY(master *os.File, log io.Writer, host io.Writer) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			if writeErr := writeAll(log, buf[:n]); writeErr != nil {
				return &relayError{code: agentLogRelayFailure, err: fmt.Errorf("wrapper: write agent log: %w", writeErr)}
			}
			// The durable log is authoritative. A closed host stream or pane is
			// observational only and must not alter the Agent result.
			_, _ = host.Write(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return fmt.Errorf("wrapper: read PTY master: %w", err)
		}
	}
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func waitForPTYRelay(done <-chan error, master *os.File) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		// A descendant that retained the slave must not turn a fast Agent exit
		// into a hung wrapper. Closing the master ends observation only; it does
		// not decide process ownership or completion.
		_ = master.Close()
		return <-done
	}
}

func resultStatus(err error) (*int, any) {
	if err == nil {
		n := 0
		return &n, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		n := exit.ExitCode()
		return &n, nil
	}
	n := 1
	return &n, nil
}
func headSHA(worktree string) string {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return string(out[:len(out)-1])
}
