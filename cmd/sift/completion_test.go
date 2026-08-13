package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCompletionNoArgsPrintsInstallInstructions pins the no-arg surface: it
// must advertise the eval one-liner and the per-shell install paths.
func TestCompletionNoArgsPrintsInstallInstructions(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"sift", "completion"}, &out, io.Discard); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	for _, want := range []string{`eval "$(sift completion zsh)"`, "bash", "zsh", "fish", "sift completion bash >", "~/.config/fish/completions/sift.fish"} {
		if !strings.Contains(got, want) {
			t.Fatalf("completion instructions lack %q:\n%s", want, got)
		}
	}
}

// TestCompletionUsageAndUnknownShell pins the closed argument surface: exactly
// one shell name, from {bash,zsh,fish}.
func TestCompletionUsageAndUnknownShell(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want int
	}{
		{[]string{"sift", "completion", "bash", "extra"}, 2},
		{[]string{"sift", "completion", "powershell"}, 2},
	} {
		var stderr bytes.Buffer
		if code := run(tc.argv, io.Discard, &stderr); code != tc.want {
			t.Fatalf("%v exit = %d, want %d; stderr=%q", tc.argv, code, tc.want, stderr.String())
		}
	}
}

// TestCompletionScriptsCoverEveryCommandAndFlag asserts the scripts are
// generated from the metadata table: every command name and every flag of
// every command appears in each shell's script.
func TestCompletionScriptsCoverEveryCommandAndFlag(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			script := runCapture(t, []string{"sift", "completion", shell})
			for _, m := range commands {
				if !strings.Contains(script, m.name) {
					t.Fatalf("%s script lacks command %q", shell, m.name)
				}
				for _, f := range m.flags {
					needle := f.flag
					if shell == "fish" {
						needle = "-l " + strings.TrimPrefix(f.flag, "--")
					}
					if !strings.Contains(script, needle) {
						t.Fatalf("%s script lacks flag %q of command %q", shell, f.flag, m.name)
					}
				}
				for _, s := range m.subcommands {
					if !strings.Contains(script, s) {
						t.Fatalf("%s script lacks subcommand %q of command %q", shell, s, m.name)
					}
				}
			}
		})
	}
}

// TestCompletionScriptsParse runs bash -n / zsh -n over the generated scripts
// when the shell is available, so an eval'ed script is guaranteed parseable
// (issue #935: `sift completion zsh` 输出可 eval 的脚本).
func TestCompletionScriptsParse(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s not installed", shell)
			}
			var out bytes.Buffer
			if code := run([]string{"sift", "completion", shell}, &out, io.Discard); code != 0 {
				t.Fatalf("completion %s exit = %d", shell, code)
			}
			cmd := exec.Command(shell, "-n")
			cmd.Stdin = bytes.NewReader(out.Bytes())
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s -n rejected the generated script: %v\n%s", shell, err, out.String())
			}
		})
	}
}

// TestZshCompletionEvalsAndRegisters exercises the documented install path
// (`eval "$(sift completion zsh)"`) with a real zsh: after compinit + eval,
// the _sift function must be defined and registered for the sift command.
func TestZshCompletionEvalsAndRegisters(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not installed")
	}
	var out bytes.Buffer
	if code := run([]string{"sift", "completion", "zsh"}, &out, io.Discard); code != 0 {
		t.Fatalf("completion zsh exit = %d", code)
	}
	script := out.String()
	probe := `autoload -U compinit; compinit; eval "$(cat /tmp/sift-zsh-completion-probe)"; print -r -- "registered:${_comps[sift]}:$(whence -w _sift)"`
	if err := os.WriteFile("/tmp/sift-zsh-completion-probe", []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove("/tmp/sift-zsh-completion-probe")
	outBytes, err := exec.Command("zsh", "-fc", probe).Output()
	if err != nil {
		t.Fatalf("zsh eval probe failed: %v", err)
	}
	got := string(outBytes)
	if !strings.Contains(got, "registered:_sift") || !strings.Contains(got, "_sift: function") {
		t.Fatalf("zsh eval did not register the completion: %q", got)
	}
}
