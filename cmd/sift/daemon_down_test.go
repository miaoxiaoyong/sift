package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/config"
)

// The daemon-down error is contextual (issue #939): an RPC command that
// cannot reach the daemon must point at the right next step — `sift init`
// when no config exists, `sift daemon` / `sift service install` when the
// config is present, and `sift init` (re-generate) when the config is
// invalid. All three branches share reportDaemonUnavailable; these tests pin
// them through the real `sift ps` dispatch path.

// TestDaemonDownNoConfigPromptsInit covers the fresh-home branch: no config
// yet, so the only actionable next step is `sift init`.
func TestDaemonDownNoConfigPromptsInit(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "ps"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"daemon unavailable", "运行 `sift init`"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "sift daemon") {
		t.Fatalf("without a config the hint must not point at sift daemon:\n%s", stderr.String())
	}
}

// TestDaemonDownWithConfigPromptsDaemon covers the configured branch: the
// config exists and loads, so the missing piece is the daemon itself.
func TestDaemonDownWithConfigPromptsDaemon(t *testing.T) {
	home := freshHome(t)
	if err := os.WriteFile(config.ConfigPath(config.Home{Path: home}), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{"sift", "ps"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"daemon 未运行", "运行 `sift daemon`", "sift service install"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "运行 `sift init`") {
		t.Fatalf("with a valid config the hint must not point at sift init:\n%s", stderr.String())
	}
}

// TestDaemonDownInvalidConfigPromptsReinit covers the invalid-config branch:
// the file exists but fails validation, so the next step is regenerating it
// via `sift init`.
func TestDaemonDownInvalidConfigPromptsReinit(t *testing.T) {
	home := freshHome(t)
	if err := os.WriteFile(config.ConfigPath(config.Home{Path: home}), []byte("version: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{"sift", "ps"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"配置无效", "运行 `sift init` 重新生成"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "sift service install") {
		t.Fatalf("an invalid config must not hint at the service path:\n%s", stderr.String())
	}
}

// TestDaemonDownContextSharedAcrossRPCCommands confirms the same contextual
// messaging reaches every RPC command, not just ps: timeline (no config)
// points at init and logs (with config) points at the daemon.
func TestDaemonDownContextSharedAcrossRPCCommands(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "timeline"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("timeline exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "运行 `sift init`") {
		t.Fatalf("timeline stderr lacks the init hint:\n%s", stderr.String())
	}

	home := freshHome(t)
	if err := os.WriteFile(config.ConfigPath(config.Home{Path: home}), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := run([]string{"sift", "logs", "run-1"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("logs exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "运行 `sift daemon`") {
		t.Fatalf("logs stderr lacks the daemon hint:\n%s", stderr.String())
	}
}
