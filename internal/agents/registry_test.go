package agents

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKnownRegistered guards against drift between the detection order
// (Known) and the profile map (registry): every detected name must have a
// profile so the wizard never shows an empty characteristic row.
func TestKnownRegistered(t *testing.T) {
	for _, name := range Known() {
		if _, ok := registry[name]; !ok {
			t.Fatalf("Known() lists %q but the registry has no profile", name)
		}
	}
}

// TestForEveryKnownProfileComplete pins the acceptance criteria: every built
// -in profile carries strengths, a context magnitude, a cost tier, a speed
// tier and a Chinese note.
func TestForEveryKnownProfileComplete(t *testing.T) {
	for _, name := range Known() {
		c := For(name)
		if len(c.Strengths) == 0 {
			t.Fatalf("%s: no strengths", name)
		}
		if c.Context == "" {
			t.Fatalf("%s: empty context", name)
		}
		if _, ok := CostLabel[c.Cost]; !ok {
			t.Fatalf("%s: unrecognized cost %q", name, c.Cost)
		}
		if _, ok := SpeedLabel[c.Speed]; !ok {
			t.Fatalf("%s: unrecognized speed %q", name, c.Speed)
		}
		if c.Notes == "" {
			t.Fatalf("%s: empty notes", name)
		}
		for _, s := range c.Strengths {
			if _, ok := StrengthLabel[s]; !ok {
				t.Fatalf("%s: unrecognized strength tag %q", name, s)
			}
		}
	}
}

func TestForUnknownAgentReturnsGeneric(t *testing.T) {
	c := For("mystery-tool")
	if c.Notes != generic.Notes || c.Context != generic.Context || c.Cost != generic.Cost || c.Speed != generic.Speed {
		t.Fatalf("For(unknown) = %#v, want the generic profile", c)
	}
	if len(c.Strengths) != 1 || c.Strengths[0] != "coding" {
		t.Fatalf("generic strengths = %#v", c.Strengths)
	}
}

func TestForUsesBaseName(t *testing.T) {
	if got, want := For("/usr/local/bin/claude").Notes, For("claude").Notes; got != want {
		t.Fatalf("For(path) = %q, want the claude profile %q", got, want)
	}
}

// TestSummary pins the wizard display fragment (Chinese tags joined with ·
// then context · cost · speed).
func TestSummary(t *testing.T) {
	c := For("claude")
	want := "编码·推理·长上下文 · 200K · 中 · 中"
	if got := c.Summary(); got != want {
		t.Fatalf("claude Summary = %q, want %q", got, want)
	}
	if got := For("unknown-tool").Summary(); got != "编码 · — · 中 · 中" {
		t.Fatalf("generic Summary = %q", got)
	}
}

func writeProbeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestProbeVersion exercises the real exec path with fake version binaries:
// semver token extraction, the "version" keyword form, multi-line output,
// empty output and failing binaries.
func TestProbeVersion(t *testing.T) {
	bin := t.TempDir()
	writeProbeScript(t, bin, "semver", "#!/bin/sh\necho '2.1.218 (Claude Code)'\n")
	writeProbeScript(t, bin, "keyword", "#!/bin/sh\necho 'Claude Code version 2.0.0'\n")
	writeProbeScript(t, bin, "multiline", "#!/bin/sh\necho '3.15.6'\necho a1f686545fd0ce8917bbd2449f733551a9bce420\necho arm64\n")
	writeProbeScript(t, bin, "vprefix", "#!/bin/sh\necho 'aider v0.73.0'\n")
	writeProbeScript(t, bin, "blank", "#!/bin/sh\nexit 0\n")
	writeProbeScript(t, bin, "fails", "#!/bin/sh\nexit 1\n")

	for _, tt := range []struct{ exe, want string }{
		{filepath.Join(bin, "semver"), "2.1.218"},
		{filepath.Join(bin, "keyword"), "2.0.0"},
		{filepath.Join(bin, "multiline"), "3.15.6"},
		{filepath.Join(bin, "vprefix"), "v0.73.0"},
		{filepath.Join(bin, "blank"), ""},
		{filepath.Join(bin, "fails"), ""},
	} {
		if got := ProbeVersion(tt.exe); got != tt.want {
			t.Fatalf("ProbeVersion(%s) = %q, want %q", tt.exe, got, tt.want)
		}
	}
}

func TestVersionToken(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"codex-cli 0.145.0", "0.145.0"},
		{"2.1.218 (Claude Code)", "2.1.218"},
		{"1.2.3-beta.1", "1.2.3-beta.1"},
		{"0.84.1", "0.84.1"},
		{"build20240801", "build20240801"}, // no dotted digits → whole token
		{"unknown", "unknown"},
	} {
		if got := versionToken(tt.in); got != tt.want {
			t.Fatalf("versionToken(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
