package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/hosting"
	"github.com/miaoxiaoyong/sift/internal/storage"
	"github.com/miaoxiaoyong/sift/internal/version"
)

// freshHome returns a 0700 temp dir suitable for use as SIFT_HOME. It creates
// the directory directly in the OS temp root rather than under t.TempDir's
// per-test subdirectory, so the resolved socket path stays within the Unix
// domain socket length limit for the online test.
func freshHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "sift")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIFT_HOME", home)
	return home
}

// withDatabase provisions a readable sift.db so the doctor's sqlite and
// permission checks do not report errors, leaving only the unavoidable
// unsafe-local warning.
func withDatabase(t *testing.T, home string) {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func installDoctorWrapper(t *testing.T) {
	t.Helper()
	installDoctorWrapperVersion(t, controlplane.Version, controlplane.ProtocolMajor)
}

// installDoctorWrapperVersion installs a wrapper fixture next to the test
// binary reporting the given release version and wire protocol major, so
// in-process doctor probes observe a controlled wrapper pairing.
func installDoctorWrapperVersion(t *testing.T, version string, protocolMajor int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(filepath.Dir(executable), "sift-agent-wrapper")
	old, readErr := os.ReadFile(wrapper)
	oldInfo, statErr := os.Stat(wrapper)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	t.Cleanup(func() {
		if readErr == nil {
			_ = os.WriteFile(wrapper, old, oldInfo.Mode().Perm())
			return
		}
		_ = os.Remove(wrapper)
	})
	content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n'; fi\nif [ \"$1\" = \"--protocol-major\" ]; then printf '%d\\n'; fi\n", version, protocolMajor)
	if err := os.WriteFile(wrapper, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorExitCode extracts the §7 exit status from every shape the doctor
// result can take: a Go int (offline, direct) and a JSON float64 (online, after
// wire decode), plus the degenerate cases that must default to 0.
// TestVersionFlag makes `sift --version` report the release version. The
// wrapper prints the same value via --version and the daemon via the RPC
// envelope / doctor; the release handshake compares them (WBS M8 §8.1).
func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"sift", "--version"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if got := strings.TrimSpace(out.String()); got != version.Release {
		t.Fatalf("--version = %q, want %q", got, version.Release)
	}
	var dashOut bytes.Buffer
	if code := run([]string{"sift", "-version"}, &dashOut, io.Discard); code != 0 {
		t.Fatalf("-version exit code = %d", code)
	}
	if dashOut.String() != out.String() {
		t.Fatalf("-version output differs from --version")
	}
}

func TestVersionFlagRejectsExtraArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"sift", "--version", "extra"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestInstallRequiresArchiveArgument mirrors the daemon/report argument
// discipline: sift install without an archive path is a usage error.
func TestInstallRequiresArchiveArgument(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "install"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage: sift install") {
		t.Fatalf("stderr = %q, want install usage", stderr.String())
	}
}

// TestServiceRequiresAction asserts the hosting dispatch is a usage error
// without an action verb (WBS M8 §8.2 / specs/hosting.md §4).
func TestServiceRequiresAction(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "service"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage: sift service") {
		t.Fatalf("stderr = %q, want service usage", stderr.String())
	}
}

// TestServiceRejectsUnknownAction keeps the four-verb surface closed: an
// unknown verb is a usage error, not a silent default.
func TestServiceRejectsUnknownAction(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "service", "reboot"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
}

// TestServiceInstallRequiresRelease asserts the hosting units refuse to point
// at nothing: with no release installed under bin/current, install reports a
// clear error and exits non-zero instead of writing a unit to a missing binary.
func TestServiceInstallRequiresRelease(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "service", "install"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "install") {
		t.Fatalf("stderr = %q, want a hint to install a release first", stderr.String())
	}
}

// installReleaseLayout provisions bin/<release>/sift + bin/current under home
// so the hosting layer's NewSpec resolves. The hosting layer never runs the
// binary; an executable shell fixture suffices.
func installReleaseLayout(t *testing.T, home string) {
	t.Helper()
	release := version.Release
	dir := filepath.Join(home, "bin", release)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sift"), []byte("#!/bin/sh\necho "+release+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(home, "bin", "current")); err != nil {
		t.Fatal(err)
	}
}

// TestServiceStatusForegroundReportsSocketVerdict pins the hosting §5 status
// contract on the CLI's no-supervisor report in both states: the output must
// carry present|absent for the operator socket (verifiable with
// `[ -S "$SIFT_HOME/siftd.sock" ]`), not only the static hint.
func TestServiceStatusForegroundReportsSocketVerdict(t *testing.T) {
	for _, want := range []string{"absent", "present"} {
		t.Run(want, func(t *testing.T) {
			home := freshHome(t)
			installReleaseLayout(t, home)
			spec, err := hosting.NewSpecFor(home, "freebsd")
			if err != nil {
				t.Fatal(err)
			}
			if want == "present" {
				sock := filepath.Join(home, "siftd.sock")
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })
			}
			plan, err := spec.Plan(hosting.ActionStatus)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			printForegroundReport(&out, plan)
			if !strings.Contains(out.String(), filepath.Join(home, "siftd.sock")) {
				t.Errorf("status output %q does not name the operator socket", out.String())
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("status output %q lacks the %q verdict", out.String(), want)
			}
		})
	}
}

// TestInstallEndToEnd drives sift install against a real archive whose
// binaries answer --version natively (executable shell fixtures, so the probe
// step runs them).
func TestInstallEndToEnd(t *testing.T) {
	home := freshHome(t)
	archiveDir := t.TempDir()

	release := version.Release
	archive := filepath.Join(archiveDir, "sift_"+release+"_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz")
	buildTestArchive(t, archive, release)

	var out bytes.Buffer
	if code := run([]string{"sift", "install", archive}, &out, io.Discard); code != 0 {
		t.Fatalf("exit code = %d; output=%q", code, out.String())
	}
	current := filepath.Join(home, "bin", "current")
	target, err := os.Readlink(current)
	if err != nil {
		t.Fatalf("current link: %v", err)
	}
	if target != release {
		t.Fatalf("current -> %q, want %q", target, release)
	}
	for _, name := range []string{"sift", "sift-agent-wrapper"} {
		installed := filepath.Join(home, "bin", release, name)
		info, err := os.Stat(installed)
		if err != nil {
			t.Fatalf("%s not installed: %v", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable", installed)
		}
	}
}

// testReleaseBinary returns an executable shell fixture that answers
// --version with release; the install probe executes it natively.
func testReleaseBinary(release string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"" + release + "\"; else exit 1; fi\n")
}

// buildTestArchive writes a release archive (two binaries + manifest.json) for
// the current platform.
func buildTestArchive(t *testing.T, archive, release string) {
	t.Helper()
	content := map[string][]byte{
		"sift":               testReleaseBinary(release),
		"sift-agent-wrapper": testReleaseBinary(release),
	}
	sum := func(b []byte) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	raw, err := json.Marshal(map[string]any{
		"schema_version":  1,
		"release_version": release,
		"artifacts": []any{
			map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "name": "sift", "sha256": sum(content["sift"])},
			map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "name": "sift-agent-wrapper", "sha256": sum(content["sift-agent-wrapper"])},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	content["manifest.json"] = raw
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, data := range content {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonDispatchRejectsArguments is the daemon argument-discipline
// regression: `sift daemon --unexpected` must exit 2 without starting.
func TestDaemonDispatchRejectsArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"sift", "daemon", "--unexpected"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
}

func TestDaemonResolvesWrapperAlongsideSiftExecutable(t *testing.T) {
	home := freshHome(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(filepath.Dir(executable), "sift-agent-wrapper")
	old, readErr := os.ReadFile(wrapper)
	oldInfo, statErr := os.Stat(wrapper)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	t.Cleanup(func() {
		if readErr == nil {
			_ = os.WriteFile(wrapper, old, oldInfo.Mode().Perm())
			return
		}
		_ = os.Remove(wrapper)
	})
	content := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + controlplane.Version + "'; fi\nif [ \"$1\" = \"--protocol-major\" ]; then printf '%d\\n' '" + fmt.Sprint(controlplane.ProtocolMajor) + "'; fi\n")
	if err := os.WriteFile(wrapper, content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wrapper, 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, config.Home{Path: home}) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("runDaemon stopped before serving: %v", err)
		default:
		}
		if _, err := controlplane.OperatorRequest(config.Home{Path: home}, "ops.doctor", map[string]any{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runDaemon did not start serving")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not stop after context cancellation")
	}
}

func TestHookBootstrapRequestRequiresExplicitProject(t *testing.T) {
	method, params, err := request("hooks-bootstrap", []string{"project"})
	if err != nil || method != "ops.hooks-bootstrap" || params["project_id"] != "project" {
		t.Fatalf("bootstrap request = %q %#v %v", method, params, err)
	}
	if _, _, err := request("hooks-bootstrap", nil); err == nil {
		t.Fatal("missing project bootstrap request succeeded")
	}
}

// TestDoctorResultContract verifies that only the closed 0/1/2 exit-code
// domain is healthy. The direct offline representation is int; the decoded
// online representation is float64.
func TestDoctorResultContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result any
		want   int
	}{
		{"offline int clean", map[string]any{"exit_code": int(0)}, 0},
		{"offline int warning", map[string]any{"exit_code": int(1)}, 1},
		{"offline int error", map[string]any{"exit_code": int(2)}, 2},
		{"online float clean", map[string]any{"exit_code": float64(0)}, 0},
		{"online float warning", map[string]any{"exit_code": float64(1)}, 1},
		{"online float error", map[string]any{"exit_code": float64(2)}, 2},
		{"missing exit_code", map[string]any{"checks": nil}, 2},
		{"wrong exit_code type", map[string]any{"exit_code": "2"}, 2},
		{"fractional exit_code", map[string]any{"exit_code": float64(1.5)}, 2},
		{"out of range exit_code", map[string]any{"exit_code": float64(3)}, 2},
		{"not a map", []any{"checks"}, 2},
		{"nil", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorExitCode(tc.result); got != tc.want {
				t.Fatalf("doctorExitCode(%v) = %d, want %d", tc.result, got, tc.want)
			}
		})
	}
}

// TestRunDoctorOfflineExitsWithError reproduces the issue #34 baseline: an
// empty SIFT_HOME cannot have a database, so offline doctor must surface the
// sqlite error as a non-zero (2) process exit, not silently exit 0.
func TestRunDoctorOfflineExitsWithError(t *testing.T) {
	freshHome(t) // no sift.db -> sqlite check errors
	var out bytes.Buffer
	code := run([]string{"sift", "doctor", "--offline", "--json"}, &out, io.Discard)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; output:\n%s", code, out.String())
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["exit_code"] != float64(2) {
		t.Fatalf("doctor exit_code = %v, want 2", result["exit_code"])
	}
	if result["offline"] != true {
		t.Fatalf("doctor offline = %v, want true", result["offline"])
	}
}

// TestRunDoctorOfflineExitsWithWarning verifies the warning-only path (exit 1):
// with a healthy database the only remaining finding is the always-on
// unsafe-local warning.
func TestRunDoctorOfflineExitsWithWarning(t *testing.T) {
	installDoctorWrapper(t)
	home := freshHome(t)
	withDatabase(t, home)
	code := run([]string{"sift", "doctor", "--offline", "--json"}, &bytes.Buffer{}, io.Discard)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// TestRunDoctorOnlineExitsWithWarning drives the online path end to end: a live
// daemon returns the doctor result in response.Result, and the process must
// exit with the daemon-computed exit_code (1, unsafe-local warning).
func TestRunDoctorOnlineExitsWithWarning(t *testing.T) {
	installDoctorWrapper(t)
	home := freshHome(t)
	withDatabase(t, home)
	s, err := controlplane.Start(config.Home{Path: home})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))

	var out bytes.Buffer
	code := run([]string{"sift", "doctor", "--json"}, &out, io.Discard)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != true {
		t.Fatalf("response ok = %v, want true", response["ok"])
	}
	result, _ := response["result"].(map[string]any)
	if result["exit_code"] != float64(1) {
		t.Fatalf("doctor exit_code = %v, want 1", result["exit_code"])
	}
	if result["offline"] != false {
		t.Fatalf("doctor offline = %v, want false", result["offline"])
	}
}

// TestRunDoctorOnlineExitsOneWhenDaemonUnavailable confirms the daemon-missing
// path still surfaces as a non-zero process exit.
func TestRunDoctorOnlineExitsOneWhenDaemonUnavailable(t *testing.T) {
	freshHome(t) // no daemon, no token, no socket
	var stderr bytes.Buffer
	code := run([]string{"sift", "doctor", "--json"}, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("daemon unavailable")) {
		t.Fatalf("stderr = %q, want daemon unavailable message", stderr.String())
	}
}

// TestDoctorHandshakeErrorConsistencyDaemonVersionMismatchExitsTwo drives the real CLI binary over the
// real operator socket against a daemon compiled at a different release. The
// daemon handshake stays fail-closed (unsupported_binary), and the CLI must
// surface that rejection as a synthesized version:daemon error — built from
// the observed response envelope, never by consuming an incompatible success
// result — and exit 2 (config.md §7, control-plane.md §3.4). The CLI is
// built with the release version stamped (internal/version.Release, which
// controlplane.Version derives from), so the wire handshake observes a
// genuine client/daemon binary-major mismatch — no direct operatorRequest
// call and no fabricated boot row.
func TestDoctorHandshakeErrorConsistencyDaemonVersionMismatchExitsTwo(t *testing.T) {
	home := freshHome(t)
	withDatabase(t, home)

	cli := filepath.Join(t.TempDir(), "sift")
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	build := exec.Command("go", "build", "-o", cli, "-ldflags", "-X github.com/miaoxiaoyong/sift/internal/version.Release=2.0.0", "./cmd/sift")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mismatched CLI: %v\n%s", err, output)
	}

	s, err := controlplane.Start(config.Home{Path: home})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))

	cmd := exec.Command(cli, "doctor", "--json")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("mismatched doctor exited 0; output:\n%s", output)
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("exit code = %d, want 2; output:\n%s", code, output)
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal doctor output: %v; output:\n%s", err, output)
	}
	if ok, _ := result["ok"].(bool); ok {
		t.Fatalf("doctor consumed a success response across a major boundary; output:\n%s", output)
	}
	if result["exit_code"] != float64(2) {
		t.Fatalf("doctor exit_code = %v, want 2", result["exit_code"])
	}
	for _, check := range result["checks"].([]any) {
		m := check.(map[string]any)
		if m["id"] != "version:daemon" {
			continue
		}
		if m["level"] != "error" {
			t.Fatalf("version:daemon = %v, want error", m)
		}
		details, _ := m["details"].(map[string]any)
		if details["cli_version"] != "2.0.0" {
			t.Fatalf("cli_version = %v, want the CLI release 2.0.0", details["cli_version"])
		}
		if details["daemon_version"] != controlplane.Version || details["daemon_protocol_major"] != float64(controlplane.ProtocolMajor) {
			t.Fatalf("daemon details = %v, want the actual daemon values observed on the wire", details)
		}
		return
	}
	t.Fatal("missing version:daemon error")
}

// TestDoctorWrapperUniqueOnline drives the online doctor end to end and
// asserts version:wrapper appears exactly once, graded ok by the actual
// daemon-side wrapper probe.
// TestDoctorRejectsInvalidResponseEnvelope proves a malicious or stale Unix
// socket peer cannot make doctor consume either result or error before the
// response envelope, request ID, wire protocol, and server version are
// validated. The valid unsupported_* handshake rejection remains covered by
// TestDoctorDaemonVersionMismatchExitsTwo.
func TestDoctorRejectsInvalidResponseEnvelope(t *testing.T) {
	const poison = "untrusted-response-content"
	for _, tc := range []struct {
		name     string
		response func(controlplane.Request) map[string]any
	}{
		{
			name: "wrong request id",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError("00000000000000000000000000000000", controlplane.ProtocolMajor, controlplane.ProtocolMinor, controlplane.Version, "unsupported_binary", poison)
			},
		},
		{
			name: "incompatible response envelope",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor+1, controlplane.ProtocolMinor, "not-a-canonical-semver", "unsupported_binary", poison)
			},
		},
		{
			name: "ok result error combination",
			response: func(req controlplane.Request) map[string]any {
				response := fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, controlplane.ProtocolMinor, controlplane.Version, "unsupported_binary", poison)
				response["ok"] = true
				response["result"] = map[string]any{"exit_code": 0, "poison": poison}
				return response
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := freshHome(t)
			serveFakeDoctorResponse(t, home, tc.response)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"sift", "doctor", "--json"}, &stdout, &stderr); code == 0 {
				t.Fatalf("doctor exit code = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("doctor consumed invalid response into stdout: %q", stdout.String())
			}
			if bytes.Contains(append(stdout.Bytes(), stderr.Bytes()...), []byte(poison)) {
				t.Fatalf("doctor consumed untrusted result/error: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

// TestDoctorOnlineResultContract exercises the success result over the Unix
// socket. A legal envelope does not make an absent, malformed, fractional, or
// out-of-range doctor exit_code trustworthy.
func TestDoctorOnlineResultContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result map[string]any
		want   int
	}{
		{"clean", map[string]any{"exit_code": 0}, 0},
		{"warning", map[string]any{"exit_code": 1}, 1},
		{"error", map[string]any{"exit_code": 2}, 2},
		{"missing exit_code", map[string]any{}, 2},
		{"wrong exit_code type", map[string]any{"exit_code": "0"}, 2},
		{"fractional exit_code", map[string]any{"exit_code": 1.5}, 2},
		{"out of range exit_code", map[string]any{"exit_code": 3}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := freshHome(t)
			serveFakeDoctorResponse(t, home, func(req controlplane.Request) map[string]any {
				return fakeDoctorSuccess(req.RequestID, tc.result)
			})
			var stdout, stderr bytes.Buffer
			if got := run([]string{"sift", "doctor", "--json"}, &stdout, &stderr); got != tc.want {
				t.Fatalf("doctor exit code = %d, want %d; stdout=%q stderr=%q", got, tc.want, stdout.String(), stderr.String())
			}
		})
	}
}

// TestDoctorHandshakeErrorConsistency only allows unsupported_* errors when
// the corresponding response version is actually incompatible. A compatible
// peer cannot synthesize a daemon mismatch; matching real handshake rejections
// still become the closed version:daemon error and exit 2.
func TestDoctorHandshakeErrorConsistency(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response func(controlplane.Request) map[string]any
		want     int
		consume  bool
	}{
		{
			name: "compatible protocol cannot claim unsupported protocol",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, controlplane.ProtocolMinor, controlplane.Version, "unsupported_protocol", "forged")
			},
			want: 1,
		},
		{
			name: "compatible binary cannot claim unsupported binary",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, controlplane.ProtocolMinor, controlplane.Version, "unsupported_binary", "forged")
			},
			want: 1,
		},
		{
			name: "incompatible protocol with matching error becomes mismatch",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor+1, controlplane.ProtocolMinor, controlplane.Version, "unsupported_protocol", "protocol mismatch")
			},
			want: 2, consume: true,
		},
		{
			name: "incompatible binary with matching error becomes mismatch",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, controlplane.ProtocolMinor, "2.0.0", "unsupported_binary", "binary mismatch")
			},
			want: 2, consume: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := freshHome(t)
			serveFakeDoctorResponse(t, home, tc.response)
			var stdout, stderr bytes.Buffer
			if got := run([]string{"sift", "doctor", "--json"}, &stdout, &stderr); got != tc.want {
				t.Fatalf("doctor exit code = %d, want %d; stdout=%q stderr=%q", got, tc.want, stdout.String(), stderr.String())
			}
			if tc.consume {
				if !bytes.Contains(stdout.Bytes(), []byte(`"id": "version:daemon"`)) {
					t.Fatalf("missing synthesized version:daemon error: %q", stdout.String())
				}
			} else if stdout.Len() != 0 {
				t.Fatalf("doctor consumed compatible forged handshake error: %q", stdout.String())
			}
		})
	}
}

// TestDoctorProtocolMinorNegative proves the client-side mirror of the V0
// closed contract: a fake peer answering with protocol_minor=-1 is not an
// "older compatible" daemon. Neither its success result nor its ordinary
// error may be consumed; only the canonical unsupported_protocol handshake
// rejection remains observable.
func TestDoctorProtocolMinorNegative(t *testing.T) {
	const poison = "negative-minor-content"
	for _, tc := range []struct {
		name     string
		response func(controlplane.Request) map[string]any
	}{
		{
			name: "success result with negative minor",
			response: func(req controlplane.Request) map[string]any {
				response := fakeDoctorSuccess(req.RequestID, map[string]any{"exit_code": 0, "poison": poison})
				response["protocol_minor"] = -1
				return response
			},
		},
		{
			name: "ordinary error with negative minor",
			response: func(req controlplane.Request) map[string]any {
				return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, -1, controlplane.Version, "unauthorized", poison)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := freshHome(t)
			serveFakeDoctorResponse(t, home, tc.response)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"sift", "doctor", "--json"}, &stdout, &stderr); code == 0 {
				t.Fatalf("doctor exit code = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("doctor consumed negative-minor response into stdout: %q", stdout.String())
			}
			if bytes.Contains(append(stdout.Bytes(), stderr.Bytes()...), []byte(poison)) {
				t.Fatalf("doctor consumed untrusted result/error: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func fakeDoctorSuccess(requestID string, result map[string]any) map[string]any {
	return map[string]any{
		"protocol_major": controlplane.ProtocolMajor,
		"protocol_minor": controlplane.ProtocolMinor,
		"server_version": controlplane.Version,
		"request_id":     requestID,
		"ok":             true,
		"result":         result,
	}
}

func fakeDoctorError(requestID string, protocolMajor, protocolMinor int, serverVersion, code, message string) map[string]any {
	return map[string]any{
		"protocol_major": protocolMajor,
		"protocol_minor": protocolMinor,
		"server_version": serverVersion,
		"request_id":     requestID,
		"ok":             false,
		"error": map[string]any{
			"code": code, "message": message, "retryable": false, "details": map[string]any{},
		},
	}
}

func serveFakeDoctorResponse(t *testing.T, home string, response func(controlplane.Request) map[string]any) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "operator.token"), []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(home, "siftd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(header[:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		var request controlplane.Request
		if err := json.Unmarshal(body, &request); err != nil {
			return
		}
		out, err := json.Marshal(response(request))
		if err != nil {
			return
		}
		binary.BigEndian.PutUint32(header[:], uint32(len(out)))
		if _, err := conn.Write(header[:]); err == nil {
			_, _ = conn.Write(out)
		}
	}()
}

func TestDoctorWrapperUniqueOnline(t *testing.T) {
	installDoctorWrapper(t)
	home := freshHome(t)
	withDatabase(t, home)

	s, err := controlplane.Start(config.Home{Path: home})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))

	var out bytes.Buffer
	if code := run([]string{"sift", "doctor", "--json"}, &out, io.Discard); code != 1 {
		t.Fatalf("exit code = %d, want 1 (unsafe-local warning only); output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result, _ := response["result"].(map[string]any)
	var wrappers []map[string]any
	for _, check := range result["checks"].([]any) {
		m := check.(map[string]any)
		if m["id"] == "version:wrapper" {
			wrappers = append(wrappers, m)
		}
	}
	if len(wrappers) != 1 {
		t.Fatalf("version:wrapper count = %d, want exactly 1: %v", len(wrappers), wrappers)
	}
	check := wrappers[0]
	if check["level"] != "ok" {
		t.Fatalf("version:wrapper = %v, want ok", check)
	}
	details, _ := check["details"].(map[string]any)
	if details["wrapper_version"] != controlplane.Version || details["wrapper_protocol_major"] != float64(controlplane.ProtocolMajor) {
		t.Fatalf("details = %v, want actual probed wrapper values", details)
	}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("socket %s not created", path)
}

// TestRequestMetricsMaps verifies the metrics command builds the closed ops.metrics
// param set, including the --project scope.
func TestRequestMetricsMaps(t *testing.T) {
	method, params, err := request("metrics", []string{})
	if err != nil || method != "ops.metrics" {
		t.Fatalf("metrics default = %q %v err=%v", method, params, err)
	}
	if _, ok := params["project_id"]; !ok {
		t.Fatalf("metrics params missing project_id: %v", params)
	}
	method, params, err = request("metrics", []string{"--project", "proj-1"})
	if err != nil || params["project_id"] != "proj-1" {
		t.Fatalf("metrics --project = %q %v err=%v", method, params, err)
	}
}

// TestRequestTimelineMaps verifies the timeline command builds the closed
// ops.timeline param set with keyset/type filters.
func TestRequestTimelineMaps(t *testing.T) {
	method, params, err := request("timeline", []string{"--run", "run-1", "--type", "report.progress", "--after-seq", "5", "--limit", "10"})
	if err != nil || method != "ops.timeline" {
		t.Fatalf("timeline = %q %v err=%v", method, params, err)
	}
	if params["run_id"] != "run-1" || params["type"] != "report.progress" || params["after_seq"] != int64(5) || params["limit"] != 10 {
		t.Fatalf("timeline params = %v", params)
	}
}

// startServerWithDB opens a real database under home and starts the operator
// server bound to it, for online metrics/timeline/ps assertions.
func startServerWithDB(t *testing.T, home string) *controlplane.Server {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SeedProjectForTest(context.Background(), "cfg-cli", "proj-cli", 1_700_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(context.Background(), "runCLI", "proj-cli", "cfg-cli", "issue-1", 1_700_000_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(context.Background(), storage.EventCmd{RunID: "runCLI", Type: "report.progress", Source: storage.SourceAgent, PayloadJSON: []byte("{}"), OccurredAtMS: 1_700_000_000_000, RecordedAtMS: 1_700_000_000_000}); err != nil {
		t.Fatal(err)
	}
	s, err := controlplane.Start(config.Home{Path: home}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))
	return s
}

// TestRunMetricsOnline prints the nine-series report over a real daemon.
func TestRunMetricsOnline(t *testing.T) {
	home := freshHome(t)
	startServerWithDB(t, home)
	var out bytes.Buffer
	code := run([]string{"sift", "metrics"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	metrics := result["metrics"].(map[string]any)
	// The north-star and false-release series are present with coverage notes.
	w := metrics["weighted_attention_per_merged_change"].(map[string]any)
	if _, ok := w["coverage"]; !ok {
		t.Fatalf("weighted attention missing coverage: %v", w)
	}
}

// TestRunTimelineOnline prints the persisted event stream over a real daemon.
func TestRunTimelineOnline(t *testing.T) {
	home := freshHome(t)
	startServerWithDB(t, home)
	var out bytes.Buffer
	code := run([]string{"sift", "timeline", "--run", "runCLI"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	events := result["events"].([]any)
	if len(events) == 0 {
		t.Fatal("timeline returned no events")
	}
}

// TestRunPSOnline prints persisted runs over a real daemon.
func TestRunPSOnline(t *testing.T) {
	home := freshHome(t)
	startServerWithDB(t, home)
	var out bytes.Buffer
	code := run([]string{"sift", "ps"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d; output:\n%s", code, out.String())
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	runs := result["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
}

// seedCLIDurableChannelFailure drives the production Channel write ports to a
// durable, alert-raising failure. It mirrors the controlplane acceptance
// fixture so the thin client can assert the projection end-to-end over the
// real unix socket (with full JSON round-trip) without exporting a test-only
// helper across packages.
func seedCLIDurableChannelFailure(t *testing.T, db *storage.DB) {
	t.Helper()
	ctx := context.Background()
	const now = int64(1_700_000_000_000)
	if err := db.SeedProjectForTest(ctx, "cfg-ch", "project-ch", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedForgeRunForTest(ctx, "run-ch", "project-ch", "cfg-ch", "42", now); err != nil {
		t.Fatal(err)
	}
	const (
		batchID    = "daily:project-ch:Asia/Shanghai:1785286800000:ops-slack"
		deliveryID = batchID + ":publish:1"
		batchKey   = "attention-batch:" + deliveryID
	)
	payload, err := json.Marshal(map[string]any{
		"batch_id": batchID, "batch_kind": "daily_summary",
		"channel":     json.RawMessage(`{"id":"ops-slack","type":"webhook","target_ref":"secret_ref:SIFT_CHANNEL_OPS_SLACK","renderer":"plain-v1","capabilities":["text"]}`),
		"delivery_id": deliveryID, "delivery_kind": "attention_batch",
		"due_at_ms": int64(1_785_286_800_000),
		"forge_alert_target": map[string]any{
			"forge_kind": "github", "forge_host": "github.com",
			"forge_project_key": "owner/project-ch", "target_kind": "issue", "target_id": "42",
		},
		"members": []any{}, "project_id": "project-ch", "rendered_text": "channel failure fixture",
		"scope": "day", "scope_id": "Asia/Shanghai:1785286800000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueChannelPublish(ctx,
		storage.Operation{Key: batchKey, Kind: storage.OperationChannelPublish, Payload: payload},
		deliveryID, now); err != nil {
		t.Fatalf("enqueue channel publish: %v", err)
	}
	db.SetChannelPolicy(3, 3)
	for i, ec := range []storage.ErrorClass{storage.ErrorTransient, storage.ErrorTransient, storage.ErrorRateLimited} {
		attemptAt := now + int64(i+1)
		claim, err := db.ClaimOutboxOperationKind(ctx, "channel", storage.OperationChannelPublish, attemptAt, 10_000)
		if err != nil || claim == nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if err := db.CompleteOutboxAttempt(ctx, *claim, storage.CompleteOutcome{
			State: storage.OperationRetryable, ErrorClass: ec, ErrorSummary: "err",
			NowMS: attemptAt + 1, ChannelFailureAlertAfter: 3, MaxAttempts: 3,
		}); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}
}

// startServerWithChannelFailure opens a database seeded with a durable
// Channel failure and serves a real daemon so a CLI command can talk to it
// over the operator socket.
func startServerWithChannelFailure(t *testing.T, home string) {
	t.Helper()
	db, err := storage.Open(context.Background(), storage.OpenConfig{Path: filepath.Join(home, "sift.db"), BinaryVersion: controlplane.Version, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	seedCLIDurableChannelFailure(t, db)
	s, err := controlplane.Start(config.Home{Path: home}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	waitSocket(t, filepath.Join(home, "siftd.sock"))
}

// channelDeliveryFromCLI runs one CLI command, parses its JSON response and
// returns the surfaced channel_deliveries projection. It checks the RPC-level
// `ok` flag rather than the process exit code: `sift doctor` legitimately
// exits 0/1/2 to project health while still returning a successful RPC.
func channelDeliveryFromCLI(t *testing.T, command string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	args := []string{"sift", command}
	if command == "doctor" {
		args = append(args, "--json")
	}
	run(args, &out, io.Discard)
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("sift %s unmarshal: %v; output:\n%s", command, err, out.String())
	}
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("sift %s RPC ok=false; output:\n%s", command, out.String())
	}
	result := response["result"].(map[string]any)
	deliveries := result["channel_deliveries"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("sift %s channel_deliveries = %d rows, want 1", command, len(deliveries))
	}
	return deliveries[0].(map[string]any)
}

// TestRunPSDoctorOnlineExposeChannelDeliveries verifies the thin client
// surfaces the durable Channel delivery/episode/alert/generated_not_delivered
// projection over the real operator socket (full JSON round-trip) for both
// `sift ps` and `sift doctor`, closing the CLI half of #715 note 4 / #782.
func TestRunPSDoctorOnlineExposeChannelDeliveries(t *testing.T) {
	home := freshHome(t)
	startServerWithChannelFailure(t, home)

	for _, command := range []string{"ps", "doctor"} {
		row := channelDeliveryFromCLI(t, command)
		for _, key := range []string{
			"delivery_id", "channel_id", "operation_key", "state", "attempt_count",
			"consecutive_failures", "episode_state", "last_error_class",
			"alert_operation_key", "alert_state", "generated_not_delivered",
		} {
			if _, ok := row[key]; !ok {
				t.Errorf("sift %s channel delivery missing key %q (row=%v)", command, key, row)
			}
		}
		// JSON round-trip coerces integers to float64.
		if row["attempt_count"] != float64(3) {
			t.Errorf("sift %s attempt_count = %v, want 3", command, row["attempt_count"])
		}
		if row["consecutive_failures"] != float64(3) {
			t.Errorf("sift %s consecutive_failures = %v, want 3", command, row["consecutive_failures"])
		}
		if row["state"] != "failed" {
			t.Errorf("sift %s state = %v, want failed", command, row["state"])
		}
		if row["episode_state"] != "ended_failed" {
			t.Errorf("sift %s episode_state = %v, want ended_failed", command, row["episode_state"])
		}
		if row["last_error_class"] != string(storage.ErrorRateLimited) {
			t.Errorf("sift %s last_error_class = %v, want %s", command, row["last_error_class"], storage.ErrorRateLimited)
		}
		if row["alert_state"] != "pending" {
			t.Errorf("sift %s alert_state = %v, want pending", command, row["alert_state"])
		}
		if row["generated_not_delivered"] != true {
			t.Errorf("sift %s generated_not_delivered = %v, want true", command, row["generated_not_delivered"])
		}
	}
}
