package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// runCapture runs one CLI invocation in-process and returns stdout, failing
// the test on a non-zero exit.
func runCapture(t *testing.T, argv []string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := run(argv, &out, &errBuf); code != 0 {
		t.Fatalf("%v exit code = %d, stderr=%q", argv, code, errBuf.String())
	}
	return out.String()
}

// TestHelpConsistencyAcrossSurfaces pins the #935 core acceptance: for every
// documented command, `sift help <cmd>`, `sift <cmd> --help`, `sift <cmd> -h`
// and `sift <cmd> -help` render identical Chinese help (one-liner + usage +
// flags + examples) and never reach the daemon — `sift ps --help` on a fresh
// home must not report "daemon unavailable".
func TestHelpConsistencyAcrossSurfaces(t *testing.T) {
	freshHome(t)
	for _, m := range commands {
		t.Run(m.name, func(t *testing.T) {
			want := runCapture(t, []string{"sift", "help", m.name})
			for _, argv := range [][]string{
				{"sift", m.name, "--help"},
				{"sift", m.name, "-h"},
				{"sift", m.name, "-help"},
			} {
				if got := runCapture(t, argv); got != want {
					t.Fatalf("%v output differs from `sift help %s`:\n--- %v ---\n%s\n--- sift help ---\n%s", argv, m.name, argv, got, want)
				}
			}
			for _, token := range []string{m.summary, "用法：" + m.usage, "示例："} {
				if !strings.Contains(want, token) {
					t.Fatalf("`sift help %s` lacks %q:\n%s", m.name, token, want)
				}
			}
			if strings.Contains(want, "daemon unavailable") || strings.Contains(want, "Usage of") {
				t.Fatalf("`sift help %s` leaked a daemon error or raw flag.Usage:\n%s", m.name, want)
			}
			// Every documented flag appears in the help.
			for _, f := range m.flags {
				if !strings.Contains(want, f.flag) {
					t.Fatalf("`sift help %s` lacks flag %q:\n%s", m.name, f.flag, want)
				}
			}
		})
	}
}

// TestHelpSubcommandFlagsRendersParentHelp pins the deeper interception: a
// help flag anywhere after the verb (`sift project add --help`) renders the
// parent command's help instead of erroring or parsing flags.
func TestHelpSubcommandFlagsRendersParentHelp(t *testing.T) {
	freshHome(t)
	for _, argv := range [][]string{
		{"sift", "project", "add", "--help"},
		{"sift", "service", "status", "-h"},
		{"sift", "report", "progress", "--help"},
	} {
		out := runCapture(t, argv)
		if !strings.Contains(out, "用法：sift "+argv[1]) {
			t.Fatalf("%v did not render %s help:\n%s", argv, argv[1], out)
		}
	}
}

// TestTopLevelHelpListsEveryCommand ensures the metadata table is the single
// source of truth: the top-level listing is derived from it, so a command
// added to the table always appears (and completion/status joined the list
// per issue #935).
func TestTopLevelHelpListsEveryCommand(t *testing.T) {
	freshHome(t)
	help := runCapture(t, []string{"sift", "help"})
	if !strings.Contains(help, "命令参考") {
		t.Fatalf("help lacks 命令参考:\n%s", help)
	}
	for _, m := range commands {
		if !strings.Contains(help, m.brief) {
			t.Fatalf("top-level help lacks %q:\n%s", m.brief, help)
		}
	}
	if !strings.Contains(help, "completion") || !strings.Contains(help, "status") {
		t.Fatalf("help must list completion and status:\n%s", help)
	}
}

// TestTopLevelHelpCommonFlows pins issue #939: the top-level help ends with
// the 常用流程 examples section — the beginner three-step flow (init → daemon
// → ps) plus the maintenance verbs (project list, update) — placed after the
// command list.
func TestTopLevelHelpCommonFlows(t *testing.T) {
	freshHome(t)
	help := runCapture(t, []string{"sift", "help"})
	for _, want := range []string{"常用流程：", "入门：", "维护：", "sift init", "sift daemon", "sift ps", "sift project list", "sift update"} {
		if !strings.Contains(help, want) {
			t.Fatalf("top-level help lacks %q:\n%s", want, help)
		}
	}
	// The section sits after the command list (the last listed command) and
	// closes the help text.
	if i := strings.Index(help, "常用流程："); i < 0 || i < strings.Index(help, "completion") {
		t.Fatalf("常用流程 must come after the command list:\n%s", help)
	}
	lines := strings.Split(strings.TrimSpace(help), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); !strings.HasPrefix(last, "sift update") {
		t.Fatalf("top-level help must end with the 常用流程 section:\n%s", help)
	}
}

// TestHelpCommandUnknownKeepsUsageExit pins the unchanged usage-class exit for
// an unknown `sift help <cmd>`.
func TestHelpCommandUnknownKeepsUsageExit(t *testing.T) {
	freshHome(t)
	var stderr bytes.Buffer
	if code := run([]string{"sift", "help", "frobnicate"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
}

// TestControlHelpExamplesParse pins F935-1: every kill/retry help example must
// parse with the real flag parser, so copy-pasting a documented example never
// exits 2 before reaching the daemon. stdlib flag stops at the first
// positional, so the example must place flags before <run-id>.
func TestControlHelpExamplesParse(t *testing.T) {
	for _, name := range []string{"kill", "retry"} {
		name := name
		t.Run(name, func(t *testing.T) {
			meta, ok := commandsByName[name]
			if !ok {
				t.Fatalf("missing command %q in metadata table", name)
			}
			for _, ex := range meta.examples {
				parts := strings.Fields(ex)
				// Each example starts with "sift <command>"; drop those two words
				// and feed the rest to request(), which drives the real flag
				// parser for kill/retry.
				if len(parts) < 2 || parts[0] != "sift" || parts[1] != name {
					t.Fatalf("%s example %q must start with \"sift %s\"", name, ex, name)
				}
				if _, _, err := request(name, parts[2:]); err != nil {
					t.Errorf("%s example %q does not parse: %v", name, ex, err)
				}
			}
		})
	}
}

// TestMetadataTableIsClosed guards the table itself: every command has a
// summary, usage and at least one example, and every subcommand listed has a
// description (completion renders it).
func TestMetadataTableIsClosed(t *testing.T) {
	for _, m := range commands {
		if m.name == "" || m.summary == "" || m.usage == "" || len(m.examples) == 0 {
			t.Fatalf("command %q is missing summary/usage/examples: %+v", m.name, m)
		}
		for _, s := range m.subcommands {
			if m.subDescriptions[s] == "" {
				t.Fatalf("command %q subcommand %q lacks a description", m.name, s)
			}
		}
	}
}
