package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/config"
	"github.com/xsift/sift/internal/controlplane"
	"github.com/xsift/sift/internal/runtime"
)

const attachSession = "sift-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func attachSuccessResponse(session string) controlplane.Response {
	return controlplane.Response{
		OK: true, ProtocolMajor: controlplane.ProtocolMajor, ProtocolMinor: controlplane.ProtocolMinor, ServerVersion: controlplane.Version,
		Result: attachResponse{RunID: "run-1", AttemptNo: 1, Generation: 1, Backend: "tmux", SessionName: session},
	}
}

func installAttachTmux(t *testing.T, exitCode int) (argsPath, callsPath string) {
	t.Helper()
	dir := t.TempDir()
	tmux := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '1\\n' >> \"$0.calls\"\nprintf '%s\\n' \"$@\" > \"$0.args\"\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(tmux, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tmux + ".args", tmux + ".calls"
}

func tmuxCallCount(t *testing.T, callsPath string) int {
	t.Helper()
	b, err := os.ReadFile(callsPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(b)))
}

func TestAttachUsesPrivateSocketAndExactReadOnlyArgv(t *testing.T) {
	home := config.Home{Path: t.TempDir()}
	argsPath, callsPath := installAttachTmux(t, 0)
	if code := runAttach(attachSuccessResponse(attachSession), home, &bytes.Buffer{}, &bytes.Buffer{}, false); code != 0 {
		t.Fatalf("runAttach exit code = %d, want 0", code)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-f", "/dev/null", "-S", runtime.TmuxSocketPath(filepath.Join(home.Path, "tmux.sock")), "attach-session", "-r", "-t", "=" + attachSession}
	if got := strings.Fields(string(args)); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("tmux argv = %#v, want %#v", got, want)
	}
	if calls := tmuxCallCount(t, callsPath); calls != 1 {
		t.Fatalf("tmux calls = %d, want 1", calls)
	}
}

func TestAttachRejectsMalformedDaemonSessionBeforeExec(t *testing.T) {
	home := config.Home{Path: t.TempDir()}
	_, callsPath := installAttachTmux(t, 0)
	for _, session := range []string{
		"",
		"arbitrary-session",
		"sift-" + strings.Repeat("a", 63),
		"sift-" + strings.Repeat("A", 64),
		attachSession + "-suffix",
	} {
		t.Run(session, func(t *testing.T) {
			if code := runAttach(attachSuccessResponse(session), home, &bytes.Buffer{}, &bytes.Buffer{}, false); code != 1 {
				t.Fatalf("runAttach(%q) = %d, want 1", session, code)
			}
			if calls := tmuxCallCount(t, callsPath); calls != 0 {
				t.Fatalf("malformed session invoked tmux %d times", calls)
			}
		})
	}
}

func TestAttachRejectsClosedRPCFailureBeforeExec(t *testing.T) {
	home := config.Home{Path: t.TempDir()}
	_, callsPath := installAttachTmux(t, 0)
	response := attachSuccessResponse(attachSession)
	response.OK = false
	if code := runAttach(response, home, &bytes.Buffer{}, &bytes.Buffer{}, false); code != 1 {
		t.Fatalf("runAttach failed RPC = %d, want 1", code)
	}
	if calls := tmuxCallCount(t, callsPath); calls != 0 {
		t.Fatalf("failed RPC invoked tmux %d times", calls)
	}
}

func TestAttachReturnsTmuxExitCode(t *testing.T) {
	home := config.Home{Path: t.TempDir()}
	_, callsPath := installAttachTmux(t, 7)
	if code := runAttach(attachSuccessResponse(attachSession), home, &bytes.Buffer{}, &bytes.Buffer{}, false); code != 7 {
		t.Fatalf("runAttach exit code = %d, want tmux exit code 7", code)
	}
	if calls := tmuxCallCount(t, callsPath); calls != 1 {
		t.Fatalf("tmux calls = %d, want 1", calls)
	}
}

// TestAttachHumanHint pins the humanized pre-attach line (ux-3): the run id
// and the read-only tmux session are surfaced before tmux takes over.
func TestAttachHumanHint(t *testing.T) {
	home := config.Home{Path: t.TempDir()}
	_, callsPath := installAttachTmux(t, 0)
	var out bytes.Buffer
	if code := runAttach(attachSuccessResponse(attachSession), home, &out, &bytes.Buffer{}, false); code != 0 {
		t.Fatalf("runAttach exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "正在只读连接") || !strings.Contains(out.String(), "run-1") || !strings.Contains(out.String(), "Ctrl-b d") {
		t.Fatalf("human attach hint = %q, want run id + 只读连接 + detach key", out.String())
	}
	if calls := tmuxCallCount(t, callsPath); calls != 1 {
		t.Fatalf("tmux calls = %d, want 1", calls)
	}
}

// TestAttachJSONPrintsEnvelopeWithoutExec pins the --json scripting surface:
// the raw RPC envelope is printed and tmux is never invoked.
func TestAttachJSONPrintsEnvelopeWithoutExec(t *testing.T) {
	home := config.Home{Path: t.TempDir()}
	_, callsPath := installAttachTmux(t, 0)
	var out bytes.Buffer
	if code := runAttach(attachSuccessResponse(attachSession), home, &out, &bytes.Buffer{}, true); code != 0 {
		t.Fatalf("runAttach --json exit code = %d, want 0", code)
	}
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("--json output is not JSON: %v; output=%q", err, out.String())
	}
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("attach --json ok = %v, want true; output=%q", response["ok"], out.String())
	}
	result := response["result"].(map[string]any)
	if result["session_name"] != attachSession {
		t.Fatalf("attach --json session_name = %v", result["session_name"])
	}
	if calls := tmuxCallCount(t, callsPath); calls != 0 {
		t.Fatalf("--json invoked tmux %d times", calls)
	}
}
