package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSplitGlobalFlags pins the strip contract: every accepted spelling of
// -v/--verbose and -q/--quiet is consumed in any position, everything else is
// preserved verbatim.
func TestSplitGlobalFlags(t *testing.T) {
	got, verbose, quiet := splitGlobalFlags([]string{"-v", "update", "--quiet", "--check", "-q", "x", "-verbose"})
	if !verbose || !quiet {
		t.Fatalf("verbose=%v quiet=%v, want both true", verbose, quiet)
	}
	want := []string{"update", "--check", "x"}
	if !slices.Equal(got, want) {
		t.Fatalf("rest = %v, want %v", got, want)
	}
	got, verbose, quiet = splitGlobalFlags([]string{"ps", "--verbose", "-q"})
	if !verbose || !quiet || !slices.Equal(got, []string{"ps"}) {
		t.Fatalf("second case: rest=%v verbose=%v quiet=%v", got, verbose, quiet)
	}
}

// TestGlobalFlagsAcceptedWithoutBreakingCommands is the acceptance core: the
// flags must parse globally on commands without verbose/quiet semantics and
// leave their behavior byte-identical (same exit code, stdout and stderr) to
// running the command without the flags. Commands with their own flag parsers
// (kill) and commands that reach the daemon (ps/timeline/metrics) are both
// included so the strip-before-dispatch claim is exercised end to end.
func TestGlobalFlagsAcceptedWithoutBreakingCommands(t *testing.T) {
	freshHome(t)
	cases := [][]string{
		{"sift", "ps"},
		{"sift", "status"},
		{"sift", "timeline"},
		{"sift", "metrics"},
		// update and version implement -v/-q semantics (see
		// TestUpdateVerboseProgress / TestUpdateQuietSilencesSuccess and the
		// version tests), so they are deliberately not in this strict list.
		{"sift", "doctor", "--offline"},
		{"sift", "project", "list"},
		{"sift", "agent", "list"},
		{"sift", "completion", "bash"},
		{"sift", "service", "status"},
		{"sift", "kill", "--expected-version", "1", "--request-key", "k", "run-123"},
	}
	for _, base := range cases {
		base := base
		t.Run(base[1], func(t *testing.T) {
			var out1, err1 bytes.Buffer
			code1 := run(base, &out1, &err1)
			variants := [][]string{
				append(slices.Clone(base), "-v", "-q"),
				append(slices.Clone(base), "--verbose", "--quiet"),
				append([]string{"sift", "-v", "-q"}, base[1:]...),
				append([]string{"sift", "-q"}, base[1:]...),
			}
			for _, argv := range variants {
				var out, errBuf bytes.Buffer
				if code := run(argv, &out, &errBuf); code != code1 {
					t.Errorf("%v exit = %d, want %d (baseline %v)", argv, code, code1, base)
				}
				if out.String() != out1.String() || errBuf.String() != err1.String() {
					t.Errorf("%v changed behavior vs %v:\n--- stdout ---\n%q\n--- stderr ---\n%q", argv, base, out.String(), errBuf.String())
				}
			}
		})
	}
}

// TestUpdateVerboseProgress pins the -v semantics on a long operation (issue
// #939): update prints per-step download/verify progress lines.
func TestUpdateVerboseProgress(t *testing.T) {
	freshHome(t)
	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "-v"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	for _, want := range []string{"正在升级…", "下载 sift_9.9.9_", "下载 checksums.txt", "校验 sha256", "校验通过", "已升级到 9.9.9"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("verbose update lacks %q:\n%s", want, out.String())
		}
	}
}

// TestUpdateQuietSilencesSuccess pins the -q semantics: success/progress prose
// disappears (empty stdout) while the exit code stays 0 and errors still reach
// stderr.
func TestUpdateQuietSilencesSuccess(t *testing.T) {
	home := freshHome(t)
	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "-q"}, &out, io.Discard); code != 0 {
		t.Fatalf("quiet update exit = %d, want 0; output=%q", code, out.String())
	}
	if out.String() != "" {
		t.Fatalf("quiet update must print nothing on success, got %q", out.String())
	}
	if _, err := os.Readlink(filepath.Join(home, "bin", "current")); err != nil {
		t.Fatalf("quiet update still must install: %v", err)
	}

	// --check -q: the compare message is success prose too.
	var checkOut bytes.Buffer
	if code := run([]string{"sift", "update", "--check", "-q"}, &checkOut, io.Discard); code != 0 {
		t.Fatalf("quiet --check exit = %d, want 0", code)
	}
	if checkOut.String() != "" {
		t.Fatalf("quiet --check must print nothing, got %q", checkOut.String())
	}

	// An error is never silenced by -q: it still goes to stderr.
	real := sha256.Sum256(archive)
	real[0] ^= 0xff // corrupt the expected checksum
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, fmt.Sprintf("%x  %s\n", real, name))
	var errOut bytes.Buffer
	if code := run([]string{"sift", "update", "-q"}, io.Discard, &errOut); code != 1 {
		t.Fatalf("quiet failed update exit = %d, want 1; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "校验失败") {
		t.Fatalf("quiet must not swallow errors, stderr = %q", errOut.String())
	}
}

// TestUpdateVerboseKeepsJSONClean pins that -v progress never corrupts the
// --json machine output (verbose lines are gated on human mode).
func TestUpdateVerboseKeepsJSONClean(t *testing.T) {
	freshHome(t)
	archive := testUpdateArchive(t, "9.9.9")
	name := updateArchiveName("9.9.9")
	releaseServer(t, "9.9.9", map[string][]byte{name: archive}, checksumLine(name, archive))

	var out bytes.Buffer
	if code := run([]string{"sift", "update", "--json", "-v"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d, output=%q", code, out.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--json -v output is not pure JSON: %v (%q)", err, out.String())
	}
	if got["updated"] != true {
		t.Fatalf("json = %v, want updated:true", got)
	}
}

// TestGlobalFlagsAroundVersionCommand pins both flag positions for version:
// before the verb (`sift -v version`) and after (`sift version -v`) behave
// identically to a plain `sift version`.
func TestGlobalFlagsAroundVersionCommand(t *testing.T) {
	releaseServer(t, "9.9.9", nil, "")
	baseline := runCapture(t, []string{"sift", "version"})
	for _, argv := range [][]string{
		{"sift", "version", "-v"},
		{"sift", "version", "--verbose"},
		{"sift", "-v", "version"},
		{"sift", "--verbose", "version"},
	} {
		if got := runCapture(t, argv); got != baseline {
			t.Fatalf("%v output differs from `sift version`:\n%q", argv, got)
		}
	}
}

// TestBareGlobalFlagsFallBackToOverview pins the flag-only invocations
// (`sift -v` / `sift -q`): they consume the flags and fall back to the
// default overview command, with -q silencing it.
func TestBareGlobalFlagsFallBackToOverview(t *testing.T) {
	freshHome(t)
	var out bytes.Buffer
	if code := run([]string{"sift", "-v"}, &out, io.Discard); code != 0 {
		t.Fatalf("sift -v exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Sift ") || !strings.Contains(out.String(), "运行 sift help") {
		t.Fatalf("sift -v must render the overview:\n%s", out.String())
	}
	var quietOut bytes.Buffer
	if code := run([]string{"sift", "-q"}, &quietOut, io.Discard); code != 0 {
		t.Fatalf("sift -q exit = %d, want 0", code)
	}
	if quietOut.String() != "" {
		t.Fatalf("sift -q must print nothing, got %q", quietOut.String())
	}
}
