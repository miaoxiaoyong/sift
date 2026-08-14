package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/version"
)

// updateArchiveName returns the goreleaser per-combo archive name for a
// release on the current platform.
func updateArchiveName(release string) string {
	return fmt.Sprintf("sift_%s_%s_%s.tar.gz", release, runtime.GOOS, runtime.GOARCH)
}

// checksumLine returns the goreleaser-format checksums.txt entry.
func checksumLine(archiveName string, archiveBytes []byte) string {
	sum := sha256.Sum256(archiveBytes)
	return fmt.Sprintf("%x  %s\n", sum, archiveName)
}

// swapRestartDaemon replaces the update command's daemon-restart seam for the
// duration of a test and returns a restore func. It mirrors the endpoint
// rewiring pattern so the auto-restart can be asserted without a real
// platform supervisor.
func swapRestartDaemon(fn func(config.Home) (string, error)) func() {
	prev := restartRunningDaemon
	restartRunningDaemon = fn
	return func() { restartRunningDaemon = prev }
}

// releaseServer serves the GitHub release API and download endpoints over
// httptest and rewires the package endpoints for the duration of the test.
// archives maps archive filenames to bytes (served under /download/v<ver>/),
// checksums is served verbatim for every /download/.../checksums.txt request.
func releaseServer(t *testing.T, tag string, archives map[string][]byte, checksums string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name": "v%s"}`, tag)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		if parts[1] == "checksums.txt" {
			_, _ = io.WriteString(w, checksums)
			return
		}
		if b, ok := archives[parts[1]]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	oldAPI, oldBase := releaseAPIURL, releaseDownloadBaseURL
	releaseAPIURL, releaseDownloadBaseURL = srv.URL+"/releases/latest", srv.URL+"/download"
	t.Cleanup(func() {
		srv.Close()
		releaseAPIURL, releaseDownloadBaseURL = oldAPI, oldBase
	})
}

// testUpdateArchive builds a valid release archive for the current platform
// and returns its bytes for the download endpoint.
func testUpdateArchive(t *testing.T, release string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	buildTestArchive(t, path, release)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpdateCheckNewer(t *testing.T) {
	freshHome(t)
	releaseServer(t, "9.9.9", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--check"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"当前 " + version.Release, "最新 9.9.9", "有可用更新"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestUpdateCheckUpToDate(t *testing.T) {
	freshHome(t)
	releaseServer(t, version.Release, nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--check"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if want := "已是最新 " + version.Release; !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestUpdateEndToEnd(t *testing.T) {
	home := freshHome(t)
	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{
		"当前 " + version.Release + " → 最新 9.9.9，正在升级…",
		"已升级到 9.9.9",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	target, err := os.Readlink(filepath.Join(home, "bin", "current"))
	if err != nil || target != "9.9.9" {
		t.Fatalf("current -> %q (%v), want 9.9.9", target, err)
	}
	for _, bin := range []string{"sift", "sift-agent-wrapper"} {
		if info, err := os.Stat(filepath.Join(home, "bin", "9.9.9", bin)); err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s not installed executably: %v", bin, err)
		}
	}
	// The installed release answers --version with the new version (the
	// fixture binaries echo their manifest release, like real ldflags builds).
	installed, err := exec.Command(filepath.Join(home, "bin", "current", "sift"), "--version").Output()
	if err != nil {
		t.Fatalf("installed sift --version: %v", err)
	}
	if string(installed) != "9.9.9\n" {
		t.Fatalf("installed sift --version = %q, want 9.9.9", string(installed))
	}
}

func TestUpdateDaemonHintWhenSocketPresent(t *testing.T) {
	home := freshHome(t)
	listener, err := net.Listen("unix", filepath.Join(home, "siftd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(filepath.Join(home, "siftd.sock")) })

	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))
	// The auto-restart is attempted but fails (no real supervisor wired), so
	// the command must fall back to the manual restart hint.
	restore := swapRestartDaemon(func(config.Home) (string, error) { return "launchd", fmt.Errorf("not loaded") })
	defer restore()
	var out bytes.Buffer
	if code := run([]string{"sift", "update"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "sift service restart") {
		t.Fatalf("output lacks the daemon restart hint:\n%s", out.String())
	}
}

// TestUpdateAutoRestartsDaemon confirms a successful upgrade auto-restarts the
// running daemon and reports the backend, instead of only hinting.
func TestUpdateAutoRestartsDaemon(t *testing.T) {
	home := freshHome(t)
	listener, err := net.Listen("unix", filepath.Join(home, "siftd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(filepath.Join(home, "siftd.sock")) })

	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var called int
	restore := swapRestartDaemon(func(config.Home) (string, error) {
		called++
		return "launchd", nil
	})
	defer restore()

	var out bytes.Buffer
	if code := run([]string{"sift", "update"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if called != 1 {
		t.Fatalf("restart called %d times, want 1", called)
	}
	if !strings.Contains(out.String(), "已重启守护进程（launchd）") {
		t.Fatalf("output lacks the auto-restart success line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "sift service restart") {
		t.Fatalf("output should not contain the manual hint on success:\n%s", out.String())
	}
}

// TestUpdateNoRestartFlagSkipsRestart confirms --no-restart suppresses the
// auto-restart entirely (but still upgrades the binary).
func TestUpdateNoRestartFlagSkipsRestart(t *testing.T) {
	home := freshHome(t)
	listener, err := net.Listen("unix", filepath.Join(home, "siftd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(filepath.Join(home, "siftd.sock")) })

	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var called int
	restore := swapRestartDaemon(func(config.Home) (string, error) { called++; return "launchd", nil })
	defer restore()

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--no-restart"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if called != 0 {
		t.Fatalf("restart called %d times, want 0 under --no-restart", called)
	}
	if !strings.Contains(out.String(), "已升级到 9.9.9") {
		t.Fatalf("output lacks upgrade confirmation:\n%s", out.String())
	}
}

func TestUpdateSHA256FailClosed(t *testing.T) {
	home := freshHome(t)
	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	real := sha256.Sum256(archive)
	real[0] ^= 0xff // corrupt the expected checksum
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, fmt.Sprintf("%x  %s\n", real, name))

	var stderr bytes.Buffer
	if code := run([]string{"sift", "update"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "校验失败") {
		t.Fatalf("stderr = %q, want the fail-closed checksum message", stderr.String())
	}
	// Fail-closed means nothing was installed and no download residue
	// survived: install never ran, so bin/ must not exist at all.
	if _, err := os.Stat(filepath.Join(home, "bin")); !os.IsNotExist(err) {
		t.Fatalf("bin/ must not exist after a failed checksum: %v", err)
	}
}

func TestUpdateForceReinstallsSameVersion(t *testing.T) {
	home := freshHome(t)
	release := version.Release // in-process current; keeps comparisons hermetic
	archive := testUpdateArchive(t, release)
	name := updateArchiveName(release)
	releaseServer(t, release, map[string][]byte{name: archive}, checksumLine(name, archive))

	// Same version is already current: plain update is a no-op.
	var out bytes.Buffer
	if code := run([]string{"sift", "update"}, &out, io.Discard); code != 0 {
		t.Fatalf("plain update exit = %d, output=%q", code, out.String())
	}
	if want := "已是最新 " + release; !strings.Contains(out.String(), want) {
		t.Fatalf("plain update = %q, want %q", out.String(), want)
	}
	// --force installs the same version through Install's documented
	// remove-first path.
	out.Reset()
	if code := run([]string{"sift", "update", "--force"}, &out, io.Discard); code != 0 {
		t.Fatalf("force update exit = %d, output=%q", code, out.String())
	}
	if want := "已升级到 " + release; !strings.Contains(out.String(), want) {
		t.Fatalf("force update = %q, want %q", out.String(), want)
	}
	target, err := os.Readlink(filepath.Join(home, "bin", "current"))
	if err != nil || target != release {
		t.Fatalf("current -> %q (%v), want %q", target, err, release)
	}
	// A second --force walks the remove-first path again.
	out.Reset()
	if code := run([]string{"sift", "update", "--force"}, &out, io.Discard); code != 0 {
		t.Fatalf("second force update exit = %d, output=%q", code, out.String())
	}
	if want := "已升级到 " + release; !strings.Contains(out.String(), want) {
		t.Fatalf("second force update = %q, want %q", out.String(), want)
	}
}

func TestUpdatePinnedVersion(t *testing.T) {
	freshHome(t)
	// The pinned version (with the v prefix install.sh also accepts) is newer
	// than the dev default, so it must upgrade without querying the API.
	archive := testUpdateArchive(t, "9.8.7")
	name := updateArchiveName("9.8.7")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--version", "v9.8.7"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "已升级到 9.8.7") {
		t.Fatalf("output = %q, want a pinned upgrade", out.String())
	}
}

func TestUpdatePinnedOlderInstalls(t *testing.T) {
	home := freshHome(t)
	// Pin semantics (install.sh contract): `--version X` installs X even when
	// X is older than the running release — no newer gate, no "已是最新"
	// shortcut, and the latest-release API is never consulted (the server's
	// 9.9.9 tag must be irrelevant).
	release := "0.0.9"
	archive := testUpdateArchive(t, release)
	name := updateArchiveName(release)
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--version", release}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"当前 " + version.Release + " → 目标 " + release, "已升级到 " + release} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	target, err := os.Readlink(filepath.Join(home, "bin", "current"))
	if err != nil || target != release {
		t.Fatalf("current -> %q (%v), want %q", target, err, release)
	}
}

func TestUpdatePinnedEqualInstalls(t *testing.T) {
	home := freshHome(t)
	// Pinning the running version installs it too: the pin contract bypasses
	// the latest-vs-current newer gate (no "已是最新" shortcut).
	release := version.Release
	archive := testUpdateArchive(t, release)
	name := updateArchiveName(release)
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--version", release}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if want := "已升级到 " + release; !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want %q (pinned equal installs)", out.String(), want)
	}
	target, err := os.Readlink(filepath.Join(home, "bin", "current"))
	if err != nil || target != release {
		t.Fatalf("current -> %q (%v), want %q", target, err, release)
	}
}

func TestUpdateCheckOlderLatestNotUpToDate(t *testing.T) {
	freshHome(t)
	// latest < current: --check must not claim "已是最新" (the local build is
	// newer than the release latest).
	releaseServer(t, "0.0.1", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--check"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"当前 " + version.Release, "比 release 最新 0.0.1 更新（本地更新）"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "已是最新") {
		t.Fatalf("--check must not claim 已是最新 when latest < current: %q", out.String())
	}
}

func TestUpdateOlderLatestNoOpNotUpToDate(t *testing.T) {
	freshHome(t)
	// Tracking the latest release while the local build is newer is a no-op,
	// but the message must not claim "已是最新".
	releaseServer(t, "0.0.1", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "update"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"当前 " + version.Release, "比 release 最新 0.0.1 更新（本地更新）"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "已是最新") {
		t.Fatalf("update must not claim 已是最新 when latest < current: %q", out.String())
	}
}

func TestUpdateJSONCheck(t *testing.T) {
	freshHome(t)
	releaseServer(t, "9.9.9", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--check", "--json"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out.String())
	}
	if got["current"] != version.Release || got["latest"] != "9.9.9" || got["updated"] != false {
		t.Fatalf("json = %v, want {current, latest, updated:false}", got)
	}
}

func TestUpdateJSONFullUpgrade(t *testing.T) {
	freshHome(t)
	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))
	t.Setenv("SIFT_JSON", "1") // the environment equivalent must keep machine output

	var out bytes.Buffer
	if code := run([]string{"sift", "update"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out.String())
	}
	if got["current"] != version.Release || got["latest"] != "9.9.9" || got["updated"] != true {
		t.Fatalf("json = %v, want {current, latest, updated:true}", got)
	}
}

func TestUpdateUsageAndValidation(t *testing.T) {
	freshHome(t)
	cases := []struct {
		args     []string
		wantCode int
		wantErr  string
	}{
		{[]string{"sift", "update", "--bogus"}, 2, "usage: sift update"},
		{[]string{"sift", "update", "extra"}, 2, "usage: sift update"},
		{[]string{"sift", "update", "--version", "nope"}, 1, "不是合法 SemVer"},
		{[]string{"sift", "update", "--version", "v"}, 1, "不是合法 SemVer"},
		{[]string{"sift", "update", "--check", "--version", "v"}, 1, "不是合法 SemVer"},
	}
	for _, tc := range cases {
		var stderr bytes.Buffer
		if code := run(tc.args, io.Discard, &stderr); code != tc.wantCode {
			t.Errorf("%v exit = %d, want %d (stderr=%q)", tc.args, code, tc.wantCode, stderr.String())
			continue
		}
		if !strings.Contains(stderr.String(), tc.wantErr) {
			t.Errorf("%v stderr = %q, want %q", tc.args, stderr.String(), tc.wantErr)
		}
	}
}
