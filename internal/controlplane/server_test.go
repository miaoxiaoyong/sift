package controlplane

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/schema"
)

// TestV12ZeroConfigStartsDaemon verifies the executable startup path accepts an
// absent config.yaml, not merely that the config decoder can construct defaults.
func TestV12ZeroConfigStartsDaemon(t *testing.T) {
	home := testHome(t)
	snapshot, err := config.Load(home, time.Now())
	if err != nil {
		t.Fatalf("zero-config load: %v", err)
	}
	if snapshot.Source.Present || len(snapshot.Config.Agents) != 0 || len(snapshot.Config.Projects) != 0 {
		t.Fatalf("zero-config snapshot = %+v", snapshot)
	}
	s, err := Start(home)
	if err != nil {
		t.Fatalf("zero-config daemon start: %v", err)
	}
	defer s.Close()
}

func TestV10aEndpointCapabilitiesAndSockets(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home.Path, "siftd.sock"))
	for _, name := range []string{"siftd.sock", "run.sock"} {
		info, err := os.Stat(filepath.Join(home.Path, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", name, info.Mode().Perm())
		}
	}
	// No token is never accepted for an operator endpoint.
	response := call(t, filepath.Join(home.Path, "siftd.sock"), Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.doctor", Auth: Auth{Kind: "operator"}, Params: map[string]any{}})
	if response.Error == nil || response.Error.Code != "unauthorized" {
		t.Fatalf("missing token response = %#v", response)
	}
	// Operator methods do not exist on run.sock, even with a syntactically valid token.
	response = call(t, filepath.Join(home.Path, "run.sock"), Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.doctor", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{}})
	if response.Error == nil || response.Error.Code != "unknown_method" {
		t.Fatalf("ops on run socket = %#v", response)
	}
	// A run token cannot claim wrapper handoff authority.
	response = call(t, filepath.Join(home.Path, "run.sock"), Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "claim.acquire", Auth: Auth{Kind: "run_token", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Params: map[string]any{}})
	if response.Error == nil || response.Error.Code != "unauthorized" {
		t.Fatalf("run token claim = %#v", response)
	}
	response = call(t, filepath.Join(home.Path, "siftd.sock"), Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.doctor", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{}})
	if !response.OK {
		t.Fatalf("valid operator doctor = %#v", response)
	}
	if response.Result.(map[string]any)["security_posture"] != "unsafe-local" {
		t.Fatal("doctor did not report unsafe-local")
	}
}

func TestDoctorOnlineHandshakeMismatchReportsDaemonVersion(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	response := s.operatorRequest(Request{
		ProtocolMajor: ProtocolMajor + 1, ClientVersion: "2.0.0",
		RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.doctor",
		Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{},
	})
	if !response.OK {
		t.Fatalf("mismatched doctor request = %#v", response)
	}
	result := response.Result.(map[string]any)
	for _, check := range result["checks"].([]doctorCheck) {
		if check.ID == "version:daemon" {
			if check.Level != "error" {
				t.Fatalf("daemon check = %#v", check)
			}
			return
		}
	}
	t.Fatal("missing version:daemon check")
}

// TestV10bUnsafeLocalAttackReproduces verifies the deliberately unclosed V0
// boundary as an Agent would exploit it: same-UID code reads operator.token
// and uses it to invoke an operator RPC successfully.
func TestOperatorKillAndRetryDelegateToTerminationCoordinator(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var gotMethod, gotRun string
	var gotVersion int64
	s.SetOperatorAction(func(_ context.Context, method, run string, version int64) error {
		gotMethod, gotRun, gotVersion = method, run, version
		return nil
	})
	response := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.kill", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"run_id": "run", "expected_version": float64(3), "request_key": "request"}})
	if !response.OK || gotMethod != "ops.kill" || gotRun != "run" || gotVersion != 3 {
		t.Fatalf("response=%#v action=%q %q %d", response, gotMethod, gotRun, gotVersion)
	}
}

func TestHookLegacyBootstrapOperatorEndpointIsClosedAndDelegated(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got string
	s.SetHookBootstrap(func(_ context.Context, projectID string) error {
		got = projectID
		return nil
	})
	response := s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.hooks-bootstrap", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"project_id": "project"}})
	if !response.OK || got != "project" {
		t.Fatalf("bootstrap response=%#v project=%q", response, got)
	}
	response = s.operatorRequest(Request{RequestID: "0123456789abcdef0123456789abcdef", Method: "ops.hooks-bootstrap", Auth: Auth{Kind: "operator", Token: s.operatorToken}, Params: map[string]any{"project_id": "project", "unexpected": true}})
	if response.OK || response.Error == nil || response.Error.Code != "invalid_request" {
		t.Fatalf("open bootstrap params = %#v", response)
	}
}

func TestV10bUnsafeLocalAttackReproduces(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	path := filepath.Join(home.Path, "siftd.sock")
	waitSocket(t, path)

	// This read intentionally models an untrusted same-UID Agent, not daemon
	// internals. V10b requires this attack to succeed until M8 closes it.
	data, err := os.ReadFile(filepath.Join(home.Path, "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	token := string(data[:len(data)-1])
	response := call(t, path, Request{ProtocolMajor: 1, ProtocolMinor: 0, ClientVersion: Version, RequestID: "fedcba9876543210fedcba9876543210", Method: "ops.doctor", Auth: Auth{Kind: "operator", Token: token}, Params: map[string]any{}})
	if !response.OK {
		t.Fatalf("same-UID agent operator RPC = %#v", response)
	}
	result := response.Result.(map[string]any)
	if result["security_posture"] != "unsafe-local" {
		t.Fatalf("security posture = %v", result["security_posture"])
	}
	checks := result["checks"].([]any)
	for _, check := range checks {
		item := check.(map[string]any)
		if item["id"] == "operator-token-readable-by-agent" && item["level"] == "warning" {
			return
		}
	}
	t.Fatalf("doctor did not report unsafe-local: %#v", checks)
}

func TestSecondDaemonRefusesLock(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Start(home); err == nil {
		t.Fatal("second daemon unexpectedly started")
	}
}

func testHome(t *testing.T) config.Home {
	t.Helper()
	path, err := os.MkdirTemp("", "sift-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return config.Home{Path: path}
}

func call(t *testing.T, path string, request Request) Response {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := writeFrame(c, request); err != nil {
		t.Fatal(err)
	}
	b, err := readFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := schema.Decode(b, &response, schema.Closed); err != nil {
		t.Fatal(err)
	}
	return response
}
func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("socket %s not created", path)
}
