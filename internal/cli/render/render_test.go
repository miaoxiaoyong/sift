package render

import "testing"

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
