package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/decode"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

const deadline = 5 * time.Second

type Server struct {
	Home          config.Home
	operatorToken string
	lock          *os.File
	operator      *net.UnixListener
	run           *net.UnixListener
	wg            sync.WaitGroup
	db            *storage.DB
	operations    func(context.Context, string, string, int64) error
}

// Start obtains the process-lifetime mutex, creates the capability token when
// necessary, and binds exactly the two owner-only Unix sockets.
func Start(home config.Home, db ...*storage.DB) (*Server, error) {
	if err := config.EnsureHomeLayout(home); err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(home.Path, "siftd.lock"))
	if err != nil {
		return nil, err
	}
	cleanup := func(e error) (*Server, error) { _ = lock.Close(); return nil, e }
	token, err := loadOperatorToken(filepath.Join(home.Path, "operator.token"))
	if err != nil {
		return cleanup(err)
	}
	s := &Server{Home: home, operatorToken: token, lock: lock}
	if len(db) > 1 {
		return cleanup(errf("at most one storage database may be supplied"))
	}
	if len(db) == 1 {
		s.db = db[0]
	}
	if s.operator, err = bindSocket("siftd.sock", home.Path); err != nil {
		return cleanup(err)
	}
	if s.run, err = bindSocket("run.sock", home.Path); err != nil {
		_ = s.operator.Close()
		_ = os.Remove(socketPath(home.Path, "siftd.sock"))
		return cleanup(err)
	}
	return s, nil
}

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errf("open lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errf("another daemon already holds siftd.lock")
	}
	return f, nil
}

func loadOperatorToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		token := hex.EncodeToString(b)
		tmp, err := os.CreateTemp(filepath.Dir(path), ".operator-token-*")
		if err != nil {
			return "", err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if err := tmp.Chmod(0o600); err == nil {
			_, err = tmp.WriteString(token + "\n")
		}
		if err == nil {
			err = tmp.Sync()
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return "", err
		}
		if err := os.Rename(name, path); err != nil {
			return "", err
		}
		return token, nil
	}
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return "", errf("operator token has unsafe type or permissions")
	}
	token := strings.TrimSuffix(string(data), "\n")
	if !validToken(token) || string(data) != token+"\n" {
		return "", errf("operator token is invalid")
	}
	return token, nil
}

func bindSocket(name, home string) (*net.UnixListener, error) {
	path := socketPath(home, name)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errf("refusing to replace non-socket %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		_ = ln.Close()
		return nil, errf("socket %s permission verification failed", name)
	}
	return ln, nil
}

func (s *Server) Serve(ctx context.Context) error {
	errch := make(chan error, 2)
	s.wg.Add(2)
	go s.accept(s.operator, true, errch)
	go s.accept(s.run, false, errch)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errch:
		return err
	}
}
func (s *Server) accept(ln *net.UnixListener, operator bool, errch chan<- error) {
	defer s.wg.Done()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case errch <- err:
			default:
			}
			return
		}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); defer c.Close(); _ = s.handle(c, operator) }()
	}
}
func (s *Server) Close() error {
	var errs []error
	if s.operator != nil {
		errs = append(errs, s.operator.Close())
		_ = os.Remove(socketPath(s.Home.Path, "siftd.sock"))
	}
	if s.run != nil {
		errs = append(errs, s.run.Close())
		_ = os.Remove(socketPath(s.Home.Path, "run.sock"))
	}
	s.wg.Wait()
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	return errors.Join(errs...)
}

func (s *Server) handle(c *net.UnixConn, operator bool) error {
	if err := checkPeerUID(c); err != nil {
		return err
	}
	_ = c.SetDeadline(time.Now().Add(deadline))
	body, err := readFrame(c)
	if err != nil {
		return err
	}
	var req Request
	if err := decode.Decode(body, &req, decode.Closed); err != nil {
		return writeFrame(c, failure("", "invalid_request", "invalid request", false))
	}
	if code, message := validateEnvelope(req); code != "" {
		return writeFrame(c, failure(req.RequestID, code, message, false))
	}
	if operator {
		return writeFrame(c, s.operatorRequest(req))
	}
	return writeFrame(c, s.runRequest(req))
}

// SetOperatorAction installs the daemon-owned implementation for kill and retry.
// Keeping the callback at the socket boundary prevents the thin CLI from ever
// acquiring database access.
func (s *Server) SetOperatorAction(action func(context.Context, string, string, int64) error) {
	s.operations = action
}

func (s *Server) operatorRequest(req Request) Response {
	if !strings.HasPrefix(req.Method, "ops.") {
		return failure(req.RequestID, "unknown_method", "method is not registered on this socket", false)
	}
	if req.Auth.Kind != "operator" || !matchesToken(s.operatorToken, req.Auth.Token) || req.Auth.Nonce != "" || req.Auth.Session != "" || req.Auth.Permit != "" {
		return failure(req.RequestID, "unauthorized", "credential rejected", false)
	}
	switch req.Method {
	case "ops.ps":
		if !onlyKeys(req.Params, "run_id", "project_id", "status", "limit", "after_run_id") {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		return success(req.RequestID, map[string]any{"runs": []any{}, "next_after_run_id": nil, "attention_remaining": map[string]int{"low": 3, "normal": 5, "high": 5}})
	case "ops.logs":
		if !onlyKeys(req.Params, "run_id", "attempt_no", "offset", "limit") {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		return failure(req.RequestID, "not_found", "run not found", false)
	case "ops.worktree":
		if !onlyKeys(req.Params, "run_id") {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		return failure(req.RequestID, "not_found", "run not found", false)
	case "ops.doctor":
		if !onlyKeys(req.Params) {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		return success(req.RequestID, doctor(context.Background(), false, s.Home))
	case "ops.kill", "ops.retry":
		if !onlyKeys(req.Params, "run_id", "expected_version", "request_key") {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		runID, ok := req.Params["run_id"].(string)
		version, vok := req.Params["expected_version"].(float64)
		key, kok := req.Params["request_key"].(string)
		if !ok || runID == "" || !vok || version < 1 || version != float64(int64(version)) || !kok || key == "" {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		if s.operations == nil {
			return failure(req.RequestID, "unavailable", "runtime termination is unavailable", true)
		}
		if err := s.operations(context.Background(), req.Method, runID, int64(version)); err != nil {
			if errors.Is(err, storage.ErrRejectedStale) {
				return failure(req.RequestID, "stale", "run or attempt changed", false)
			}
			return failure(req.RequestID, "termination_failed", "controlled termination was not accepted", true)
		}
		return success(req.RequestID, map[string]any{"accepted": true, "state": "terminating"})
	default:
		return failure(req.RequestID, "unknown_method", "unknown method", false)
	}
}
func (s *Server) runRequest(req Request) Response {
	if strings.HasPrefix(req.Method, "ops.") {
		return failure(req.RequestID, "unknown_method", "method is not registered on this socket", false)
	}
	switch req.Method {
	case "report.submit":
		if req.Auth.Kind != "run_token" || !validToken(req.Auth.Token) {
			return failure(req.RequestID, "unauthorized", "credential rejected", false)
		}
		return failure(req.RequestID, "not_found", "run not found", false)
	case "claim.acquire", "claim.permit_spawn", "claim.started":
		return s.handoffRequest(req)
	default:
		return failure(req.RequestID, "unknown_method", "unknown method", false)
	}
}

func onlyKeys(m map[string]any, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
func readFrame(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxFrame {
		return nil, errf("invalid frame length")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("response too large")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if _, err = w.Write(h[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// OfflineDoctor performs only read-only local diagnostics; it never creates a
// home directory, token, socket, lock, or database.
func OfflineDoctor(home config.Home) map[string]any {
	return doctor(context.Background(), true, home)
}
