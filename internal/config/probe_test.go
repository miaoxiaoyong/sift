package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Process-level probes: any failure refuses startup (config.md §5.1).
// Project-level probes: failure isolates only that project (§5.2).

func TestRunProcessProbesAnyFailureFails(t *testing.T) {
	probes := []Probe{
		{Name: "ok", Run: func(context.Context) error { return nil }},
		{Name: "boom", Run: func(context.Context) error { return errors.New("nope") }},
		{Name: "later", Run: func(context.Context) error { return nil }},
	}
	out := RunProcessProbes(context.Background(), probes)
	if !AnyFailed(out) {
		t.Fatal("any failure must fail the set")
	}
	if out[1].Err == nil {
		t.Fatal("boom outcome must carry error")
	}
}

func TestRunProcessProbesAllPass(t *testing.T) {
	probes := []Probe{
		{Name: "a", Run: func(context.Context) error { return nil }},
		{Name: "b", Run: func(context.Context) error { return nil }},
	}
	if AnyFailed(RunProcessProbes(context.Background(), probes)) {
		t.Fatal("all-pass must not fail")
	}
}

func TestAgentExecutableProbeMissingBinary(t *testing.T) {
	cfg := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: sift-no-such-binary-zzz\n")
	diag := NewDiagnostics()
	p := AgentExecutableProbe(cfg, diag)
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("missing agent executable must fail probe")
	}
}

func TestAgentExecutableProbeResolvesPath(t *testing.T) {
	cfg := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\n")
	diag := NewDiagnostics()
	p := AgentExecutableProbe(cfg, diag)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("echo must resolve: %v", err)
	}
	if diag.AgentPaths["a"] == "" {
		t.Fatal("resolved path must be recorded")
	}
}

func TestBrainExecutableProbeDeterministicModeSkipped(t *testing.T) {
	cfg := mustCfg(t, "version: 1\n") // brain.executable defaults to "" (deterministic)
	diag := NewDiagnostics()
	if err := BrainExecutableProbe(cfg, diag).Run(context.Background()); err != nil {
		t.Fatalf("deterministic brain must not be probed: %v", err)
	}
}

func TestTmuxProbeOnlyWhenUsed(t *testing.T) {
	noTmux := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\n    backend: process\n")
	if err := TmuxProbe(noTmux, NewDiagnostics()).Run(context.Background()); err != nil {
		t.Fatalf("tmux probe must be skipped when unused: %v", err)
	}
	withTmux := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\n    backend: tmux\n")
	if !usesTmux(withTmux) {
		t.Fatal("usesTmux must report true for a tmux-backed agent")
	}
	// When tmux is actually selected, the probe must attempt a PATH lookup;
	// its outcome depends on whether tmux is installed in the environment, so
	// assert against the real PATH rather than assuming absence.
	err := TmuxProbe(withTmux, NewDiagnostics()).Run(context.Background())
	if _, lookupErr := exec.LookPath("tmux"); lookupErr != nil {
		if err == nil {
			t.Fatal("tmux selected and absent must fail")
		}
	} else if err != nil {
		t.Fatalf("tmux selected and present must resolve: %v", err)
	}
}

func TestTmuxProbeDoesNotInvokeTmuxForProcessOnly(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\n: > "+marker+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\n    backend: process\n")
	if err := TmuxProbe(cfg, NewDiagnostics()).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("process-only configuration invoked tmux: %v", err)
	}
}

func TestTmuxProbeRejectsOldAndIncompatibleTmux(t *testing.T) {
	cfg := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\n    backend: tmux\n")
	for _, tc := range []struct {
		name   string
		script string
	}{
		{name: "old", script: "#!/bin/sh\nif [ \"$1\" = -V ]; then echo 'tmux 3.1'; exit 0; fi\nexit 0\n"},
		{name: "incompatible", script: "#!/bin/sh\nif [ \"$1\" = -V ]; then echo 'tmux 3.6'; exit 0; fi\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(tc.script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			if err := TmuxProbe(cfg, NewDiagnostics()).Run(context.Background()); err == nil {
				t.Fatal("old or incompatible tmux unexpectedly passed startup probe")
			}
		})
	}
}

func TestTmuxProbeFreezesResolvedRealpath(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("real tmux is not installed")
	}
	cfg := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\n    backend: tmux\n")
	dir := t.TempDir()
	link := filepath.Join(dir, "tmux")
	if err := os.Symlink(realTmux, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	diag := NewDiagnostics()
	if err := TmuxProbe(cfg, diag).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realTmux)
	if err != nil {
		t.Fatal(err)
	}
	if diag.TmuxPath != want {
		t.Fatalf("frozen tmux path = %q, want resolved realpath %q", diag.TmuxPath, want)
	}
}

func TestProjectProbesIsolateFailingProject(t *testing.T) {
	goodRepo := tTempDir(t)
	badRepo := goodRepo + "/does-not-exist"
	yaml := "version: 1\nagents:\n  - id: a\n    executable: echo\n" +
		"projects:\n" +
		"  - id: good\n    repo: " + goodRepo + "\n    forge: {kind: github, project: o/r}\n" +
		"  - id: bad\n    repo: " + badRepo + "\n    forge: {kind: github, project: o/r2}\n"
	cfg := mustCfg(t, yaml)
	out := RunProjectProbes(context.Background(), cfg, []ProjectProbe{ProjectRepoProbe(), ProjectAgentsProbe(cfg)})
	failed := FailedProjects(out)
	if len(failed) != 1 || failed[0] != "bad" {
		t.Fatalf("only project bad must fail, got %v", failed)
	}
}

func TestProjectProbesDisabledProjectSkipped(t *testing.T) {
	cfg := mustCfg(t, "version: 1\nagents:\n  - id: a\n    executable: echo\nprojects:\n  - id: off\n    repo: /no/such\n    forge: {kind: github, project: o/r}\n    enabled: false\n")
	out := RunProjectProbes(context.Background(), cfg, []ProjectProbe{ProjectRepoProbe()})
	if len(out) != 0 {
		t.Fatalf("disabled project must be skipped, got %v", out)
	}
}

func mustCfg(t *testing.T, yaml string) *Config {
	t.Helper()
	snap, err := mustLoadYAMLOrErr(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	return snap.Config
}

func tTempDir(t *testing.T) string {
	t.Helper()
	h := tempHome(t) // misuse: just need a throwaway dir that exists
	return h.Path
}
