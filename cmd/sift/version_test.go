package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/version"
)

// TestVersionUpdateAvailable pins the primary human surface (issue #939): a
// newer release reports the current version plus the "有更新" hint with the
// update command, exactly the human wording from the issue.
func TestVersionUpdateAvailable(t *testing.T) {
	releaseServer(t, "9.9.9", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "version"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"Sift " + version.Release, "有更新 9.9.9", "运行 sift update 升级"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestVersionUpToDate pins the equal-version human surface: 已是最新 with the
// latest value, never an update hint.
func TestVersionUpToDate(t *testing.T) {
	releaseServer(t, version.Release, nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "version"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"Sift " + version.Release, "最新 " + version.Release, "已是最新"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "有更新") {
		t.Fatalf("up-to-date version must not claim an update:\n%s", out.String())
	}
}

// TestVersionLocalNewer mirrors update's honesty rule: when the local build is
// newer than the release latest (the dev default is), version must never claim
// 已是最新.
func TestVersionLocalNewer(t *testing.T) {
	releaseServer(t, "0.0.1", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "version"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"Sift " + version.Release, "比 release 最新 0.0.1 更新"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "已是最新") {
		t.Fatalf("version must not claim 已是最新 when latest < current:\n%s", out.String())
	}
}

// TestVersionJSONContract pins the closed {current, latest, updated} machine
// contract (issue #939): updated means "an update is available", and the
// SIFT_JSON=1 environment equivalent must keep the machine output too.
func TestVersionJSONContract(t *testing.T) {
	releaseServer(t, "9.9.9", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "version", "--json"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out.String())
	}
	if got["current"] != version.Release || got["latest"] != "9.9.9" || got["updated"] != true {
		t.Fatalf("json = %v, want {current:%s latest:9.9.9 updated:true}", got, version.Release)
	}

	// The SIFT_JSON=1 environment equivalent must keep the machine output.
	t.Setenv("SIFT_JSON", "1")
	var envOut bytes.Buffer
	if code := run([]string{"sift", "version"}, &envOut, io.Discard); code != 0 {
		t.Fatalf("SIFT_JSON=1 exit = %d, output=%q", code, envOut.String())
	}
	if err := json.Unmarshal(envOut.Bytes(), &got); err != nil {
		t.Fatalf("SIFT_JSON=1 output is not JSON: %v (%q)", err, envOut.String())
	}
	if got["current"] != version.Release || got["latest"] != "9.9.9" || got["updated"] != true {
		t.Fatalf("SIFT_JSON=1 json = %v, want {current, latest, updated:true}", got)
	}
}

// TestVersionUsageRejectsUnknownFlag keeps the version flag surface closed.
func TestVersionUsageRejectsUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"sift", "version", "--wide"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "sift version") {
		t.Fatalf("stderr = %q, want the usage line", stderr.String())
	}
}

// TestVersionQueryFailureFailsClosed pins the fail-closed behavior shared with
// `sift update --check`: an unreachable/failing latest-release API is an error
// (exit 1), never a fabricated "已是最新".
func TestVersionQueryFailureFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := releaseAPIURL
	releaseAPIURL = srv.URL
	defer func() { releaseAPIURL = old }()

	var stderr bytes.Buffer
	if code := run([]string{"sift", "version"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "查询最新版本失败") {
		t.Fatalf("stderr = %q, want the latest-query failure", stderr.String())
	}
	if strings.Contains(stderr.String(), "已是最新") {
		t.Fatalf("failed query must never claim 已是最新: %q", stderr.String())
	}
}

// TestVersionDispatchesWithoutHome pins that `sift version` works with no
// SIFT_HOME and no config (it needs neither the home nor the daemon); the
// version dispatch happens before home resolution.
func TestVersionDispatchesWithoutHome(t *testing.T) {
	releaseServer(t, "9.9.9", nil, "")
	var out bytes.Buffer
	if code := run([]string{"sift", "version"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "Sift "+version.Release) {
		t.Fatalf("output = %q", out.String())
	}
}
