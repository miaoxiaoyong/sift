package render

import (
	"strings"
	"testing"
)

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		level, want string
	}{
		{"ok", "✓"},
		{"warning", "⚠"},
		{"error", "✗"},
		{"unknown", "ℹ"},
	} {
		if got := Status(tc.level); got != tc.want {
			t.Fatalf("Status(%q) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestDuration(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{340, "340ms"},
		{1500, "1.5s"},
		{45000, "45.0s"},
		{180_000, "3m"},
		{390_000, "6m30s"},
		{3_900_000, "1h5m"},
	} {
		if got := Duration(tc.ms); got != tc.want {
			t.Fatalf("Duration(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	if got := DisplayWidth("abc"); got != 3 {
		t.Fatalf("DisplayWidth(abc) = %d, want 3", got)
	}
	if got := DisplayWidth("运行"); got != 4 {
		t.Fatalf("DisplayWidth(运行) = %d, want 4", got)
	}
	if got := DisplayWidth("✓"); got != 1 {
		t.Fatalf("DisplayWidth(✓) = %d, want 1", got)
	}
}

func TestTable(t *testing.T) {
	out := Table([]string{"运行 ID", "项目"}, [][]string{{"run-1", "proj-1"}, {"run-22", "proj-2"}})
	if !strings.Contains(out, "运行 ID") || !strings.Contains(out, "run-22") {
		t.Fatalf("table = %q", out)
	}
	// The CJK header and ASCII values must align: both rows share a gutter.
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table lines = %d, want 3; out=%q", len(lines), out)
	}
	for _, line := range lines {
		if !strings.Contains(line, "  ") {
			t.Fatalf("table line %q lacks column gutter", line)
		}
	}
}
