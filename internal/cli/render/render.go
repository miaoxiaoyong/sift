// Package render contains the small, dependency-free presentation layer for
// the sift CLI. It deliberately knows nothing about RPC or command schemas.
package render

import (
	"fmt"
	"strings"
)

// Status returns a status icon and Chinese label for a check level.
func Status(level string) string {
	switch strings.ToLower(level) {
	case "ok", "success", "healthy":
		return "✓"
	case "warning", "warn":
		return "⚠"
	case "error", "failed":
		return "✗"
	default:
		return "ℹ"
	}
}

// Duration renders a millisecond duration with the largest human-friendly
// unit: 340ms, 1.5s, 45s, 3m, 1h5m.
func Duration(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		m := ms / 60_000
		s := (ms % 60_000) / 1000
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		h := ms / 3_600_000
		m := (ms % 3_600_000) / 60_000
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// Table renders headers and rows as a left-aligned table with a two-space
// gutter. Column widths use display width so CJK labels align with ASCII
// columns. It is deterministic and emits no control sequences.
func Table(headers []string, rows [][]string) string {
	cols := len(headers)
	if cols == 0 {
		return ""
	}
	widths := make([]int, cols)
	measure := func(cells []string) {
		for i := 0; i < cols && i < len(cells); i++ {
			if w := DisplayWidth(cells[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(headers)
	for _, r := range rows {
		measure(r)
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(cell)
			if i < cols-1 {
				for j := 0; j < widths[i]+2-DisplayWidth(cell); j++ {
					b.WriteByte(' ')
				}
			}
		}
		b.WriteByte('\n')
	}
	writeRow(headers)
	for _, r := range rows {
		writeRow(r)
	}
	return b.String()
}

// DisplayWidth reports the terminal display width of s: CJK and full-width
// runes count as 2, everything else as 1.
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
			r >= 0x2E80 && r <= 0x303E, // CJK radicals, punctuation
			r >= 0x3041 && r <= 0x33FF, // Kana, CJK symbols
			r >= 0x3400 && r <= 0x4DBF, // CJK extension A
			r >= 0x4E00 && r <= 0x9FFF, // CJK unified
			r >= 0xA000 && r <= 0xA4CF, // Yi
			r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
			r >= 0xF900 && r <= 0xFAFF, // CJK compatibility
			r >= 0xFE30 && r <= 0xFE4F, // CJK compatibility forms
			r >= 0xFF00 && r <= 0xFF60, // full-width forms
			r >= 0xFFE0 && r <= 0xFFE6: // full-width signs
			w += 2
		default:
			w++
		}
	}
	return w
}
