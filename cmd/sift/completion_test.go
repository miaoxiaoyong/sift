package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
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

// bashCompletionProbe drives the generated bash _sift handler with a given
// COMP_WORDS / COMP_CWORD and returns the resulting COMPREPLY candidates. It
// is the non-self-referential gold standard for issue #935: it executes the
// real completion logic rather than substring-matching the script text. It is
// skipped when bash is unavailable.
func bashCompletionProbe(t *testing.T, compWords []string, cword int) []string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	var script bytes.Buffer
	if code := run([]string{"sift", "completion", "bash"}, &script, io.Discard); code != 0 {
		t.Fatalf("completion bash exit = %d", code)
	}
	scriptFile, err := os.CreateTemp("", "sift-bash-completion-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(scriptFile.Name()) })
	if _, err := scriptFile.Write(script.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := scriptFile.Close(); err != nil {
		t.Fatal(err)
	}
	var wordsBuf bytes.Buffer
	wordsBuf.WriteString("(")
	for i, w := range compWords {
		if i > 0 {
			wordsBuf.WriteString(" ")
		}
		wordsBuf.WriteString("'")
		wordsBuf.WriteString(w)
		wordsBuf.WriteString("'")
	}
	wordsBuf.WriteString(")")
	probe := fmt.Sprintf("source '%s'; COMP_WORDS=%s; COMP_CWORD=%d; "+
		"cur=\"${COMP_WORDS[COMP_CWORD]}\"; _sift; printf '%%s\\n' \"${COMPREPLY[@]}\"",
		scriptFile.Name(), wordsBuf.String(), cword)
	out, err := exec.Command("bash", "-c", probe).Output()
	if err != nil {
		t.Fatalf("bash completion probe failed: %v\nscript:\n%s", err, script.String())
	}
	var cands []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			cands = append(cands, line)
		}
	}
	return cands
}

// TestCompletionGlobal pins F935-2: the top-level discovery surface must offer
// the `help` verb and the global --help/-h/--version flags in every shell.
// The wanted list is hardcoded (dispatch-layer constructs, not the commands
// table) so the test cannot be satisfied by the old commands-only scripts.
// Bash and fish are exercised with real completion probes; zsh's headless
// completion driver is impractical so it gets targeted structural checks.
func TestCompletionGlobal(t *testing.T) {
	want := []string{"help", "--help", "-h", "--version"}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			script := runCapture(t, []string{"sift", "completion", shell})
			switch shell {
			case "bash":
				// The top-level compgen word list must carry help + the global flags
				// as actual members (precise, not a substring match).
				words := bashCompgenWordsFrom(script, 0)
				if len(words) == 0 {
					t.Fatalf("bash script lacks a top-level compgen word list")
				}
				for _, w := range want {
					if !slices.Contains(words, w) {
						t.Errorf("bash top-level words %v lack %q", words, w)
					}
				}
			case "zsh":
				// `help` is offered as a command, and the global flags live in a
				// globalopts array described at CURRENT==2.
				for _, entry := range []string{"'help:查看帮助'", "'--help:查看帮助'", "'-h:查看帮助'", "'--version:查看版本'"} {
					if !strings.Contains(script, entry) {
						t.Errorf("zsh lacks top-level candidate entry %q", entry)
					}
				}
				if !strings.Contains(script, "_describe 'global option' globalopts") {
					t.Errorf("zsh lacks the global-options _describe at CURRENT==2")
				}
			case "fish":
				// fish offers help as a subcommand candidate and the global
				// flags under __fish_use_subcommand (-l help -s h, -l version).
				if !strings.Contains(script, "-n '__fish_use_subcommand' -a 'help'") {
					t.Errorf("fish lacks the help subcommand candidate")
				}
				if !strings.Contains(script, "-n '__fish_use_subcommand' -l help -s h") {
					t.Errorf("fish lacks the global --help/-h flag")
				}
				if !strings.Contains(script, "-n '__fish_use_subcommand' -l version") {
					t.Errorf("fish lacks the global --version flag")
				}
			}
		})
	}
	// Real bash probe: drive the generated _sift handler and assert the actual
	// COMPREPLY for the first position contains help + the global flags.
	t.Run("bash_probe", func(t *testing.T) {
		cands := bashCompletionProbe(t, []string{"sift", ""}, 1)
		for _, w := range want {
			if !slices.Contains(cands, w) {
				t.Errorf("bash top-level completions %v lack %q", cands, w)
			}
		}
	})
	// Real fish probe: source the script and query `complete -C` (skipped where
	// fish is unavailable).
	t.Run("fish_probe", func(t *testing.T) {
		cands := fishCompletionProbe(t, "sift ")
		for _, w := range want {
			if !slices.Contains(cands, w) {
				t.Errorf("fish top-level completions %v lack %q", cands, w)
			}
		}
	})
}

// TestCompletionHelp pins F935-2: every command (at least ps) must offer the
// universal --help/-h flags. The wanted list is hardcoded so the test fails on
// the old flags-only scripts; bash and fish are exercised with real probes.
func TestCompletionHelp(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			script := runCapture(t, []string{"sift", "completion", shell})
			switch shell {
			case "bash":
				// The ps) case compgen word list must carry --help and -h.
				off := strings.Index(script, "ps)")
				if off < 0 {
					t.Fatalf("bash script lacks the ps) case")
				}
				words := bashCompgenWordsFrom(script, off)
				if len(words) == 0 {
					t.Fatalf("bash ps) case has no compgen word list")
				}
				for _, w := range []string{"--help", "-h"} {
					if !slices.Contains(words, w) {
						t.Errorf("bash ps word list %v lacks %q", words, w)
					}
				}
			case "zsh":
				for _, entry := range []string{"'--help[查看帮助]'", "'-h[查看帮助]'"} {
					if !strings.Contains(script, entry) {
						t.Errorf("zsh lacks universal help spec %q", entry)
					}
				}
			case "fish":
				if !strings.Contains(script, "-n '__fish_seen_subcommand_from ps' -l help -s h") {
					t.Errorf("fish ps lacks the --help/-h flag")
				}
			}
		})
	}
	// Real bash probe: the actual COMPREPLY after `sift ps` must contain both
	// --help and -h.
	t.Run("bash_probe", func(t *testing.T) {
		cands := bashCompletionProbe(t, []string{"sift", "ps", ""}, 2)
		for _, w := range []string{"--help", "-h"} {
			if !slices.Contains(cands, w) {
				t.Errorf("bash ps completions %v lack %q", cands, w)
			}
		}
	})
	// Real fish probe: query `complete -C 'sift ps '` (skipped where fish is
	// unavailable).
	t.Run("fish_probe", func(t *testing.T) {
		cands := fishCompletionProbe(t, "sift ps ")
		for _, w := range []string{"--help", "-h"} {
			if !slices.Contains(cands, w) {
				t.Errorf("fish ps completions %v lack %q", cands, w)
			}
		}
	})
}

// bashCompgenWordsFrom returns the space-split word list of the first
// `compgen -W "..."` at or after byte offset `from` in a bash completion
// script. Precise membership (not a substring) so "-h" never matches "--help".
func bashCompgenWordsFrom(script string, from int) []string {
	rest := script[from:]
	idx := strings.Index(rest, "compgen -W \"")
	if idx < 0 {
		return nil
	}
	rest = rest[idx+len("compgen -W \""):]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return nil
	}
	return strings.Fields(rest[:end])
}

// fishCompletionProbe sources the generated fish script and queries
// `complete -C <cmdline>`, returning the candidate (first tab-separated field
// of each offered completion). Skipped when fish is unavailable.
func fishCompletionProbe(t *testing.T, cmdline string) []string {
	t.Helper()
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish not installed")
	}
	var script bytes.Buffer
	if code := run([]string{"sift", "completion", "fish"}, &script, io.Discard); code != 0 {
		t.Fatalf("completion fish exit = %d", code)
	}
	scriptFile, err := os.CreateTemp("", "sift-fish-completion-*.fish")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(scriptFile.Name()) })
	if _, err := scriptFile.Write(script.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := scriptFile.Close(); err != nil {
		t.Fatal(err)
	}
	probe := fmt.Sprintf("source '%s'; complete -C '%s'", scriptFile.Name(), cmdline)
	out, err := exec.Command("fish", "-c", probe).Output()
	if err != nil {
		t.Fatalf("fish completion probe failed: %v\nscript:\n%s", err, script.String())
	}
	var cands []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		cand := line
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			cand = line[:i]
		}
		cands = append(cands, cand)
	}
	return cands
}

// TestCompletionGlobalVerboseQuiet pins issue #939: the universal
// -v/--verbose and -q/--quiet flags are discoverable both at the top level
// (global flags) and on every command (they are accepted everywhere).
func TestCompletionGlobalVerboseQuiet(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			script := runCapture(t, []string{"sift", "completion", shell})
			switch shell {
			case "bash":
				words := bashCompgenWordsFrom(script, 0)
				for _, w := range []string{"-v", "--verbose", "-q", "--quiet"} {
					if !slices.Contains(words, w) {
						t.Errorf("bash top-level words %v lack %q", words, w)
					}
				}
				off := strings.Index(script, "ps)")
				words = bashCompgenWordsFrom(script, off)
				for _, w := range []string{"-v", "--verbose", "-q", "--quiet"} {
					if !slices.Contains(words, w) {
						t.Errorf("bash ps words %v lack %q", words, w)
					}
				}
			case "zsh":
				for _, entry := range []string{"'--verbose:详细输出'", "'-v:详细输出'", "'--quiet:静默输出'", "'-q:静默输出'"} {
					if !strings.Contains(script, entry) {
						t.Errorf("zsh lacks global flag entry %q", entry)
					}
				}
			case "fish":
				for _, line := range []string{"-l verbose -s v", "-l quiet -s q"} {
					if !strings.Contains(script, line) {
						t.Errorf("fish lacks global flag line %q", line)
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
