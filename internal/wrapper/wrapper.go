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
	"path/filepath"
	"time"

	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/runtime"
)

func Run(ctx context.Context, bootstrapPath string) error {
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
	self, err := os.Executable()
	if err != nil {
		return err
	}
	pid := int64(os.Getpid())
	started := time.Now().UnixMilli()
	instance, session := secret(), secret()
	wi := map[string]any{"pid": pid, "started_at_ms": started, "executable": self, "pgid": pid}
	base := map[string]any{"run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "dispatch_id": b.DispatchID, "wrapper_instance_id": instance, "session_candidate": session, "wrapper_identity": wi}
	if _, err := call(b.RunDir, "claim.acquire", map[string]any{"kind": "bootstrap", "nonce": b.BootstrapNonce}, base); err != nil {
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
	if _, err := call(b.RunDir, "claim.permit_spawn", map[string]any{"kind": "wrapper_session", "session": session}, pp); err != nil {
		return err
	}
	// The gate is adjacent to the sole launcher invocation; no permit replay can
	// cross it after this point.
	var gate runtime.PermitGate
	log, err := os.OpenFile(filepath.Join(b.RunDir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
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
	launch := runtime.AgentLaunch{Executable: b.Agent.Executable, Args: args, Worktree: b.WorktreePath, RunDir: b.RunDir, Stdin: in, Stdout: log, Stderr: log}
	// SpawnOnce does not return the command, so invoke the guarded primitive here
	// to retain the identity needed by claim.started.
	if err := gate.Enter(); err != nil {
		return err
	}
	cmd, err := runtime.DirectLauncher{}.Start(ctx, launch)
	if err != nil {
		return err
	}
	ai := map[string]any{"pid": int64(cmd.Process.Pid), "started_at_ms": time.Now().UnixMilli(), "executable": b.Agent.Executable}
	control["agent_identity"] = ai
	control["updated_at_ms"] = time.Now().UnixMilli()
	digest, err = writeJSON(filepath.Join(b.RunDir, "control.json"), control)
	if err != nil {
		return err
	}
	sp := map[string]any{"run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "agent_identity": ai, "control_digest": digest, "result_digest": nil}
	if _, err = call(b.RunDir, "claim.started", map[string]any{"kind": "wrapper_started", "session": session, "permit": permit}, sp); err != nil {
		return err
	}
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go heartbeat(b, instance, stopHeartbeat)
	waitErr := cmd.Wait()
	exitCode, signal := resultStatus(waitErr)
	result := map[string]any{"schema_version": 1, "run_id": b.RunID, "attempt_no": b.AttemptNo, "generation": b.Generation, "wrapper_instance_id": instance, "agent_identity": ai, "exit_code": exitCode, "signal": signal, "finished_at_ms": time.Now().UnixMilli(), "final_head_sha": headSHA(b.WorktreePath), "control_digest": digest}
	if _, err := writeJSON(filepath.Join(b.RunDir, "result.json"), result); err != nil {
		return err
	}
	return waitErr
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
func call(runDir, method string, auth, params map[string]any) (map[string]any, error) {
	c, err := net.DialTimeout("unix", filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(runDir))), "run.sock"), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
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
		return nil, fmt.Errorf("wrapper: %s rejected: %s", method, r.Error.Code)
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
