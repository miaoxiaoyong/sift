// Package render contains the small, dependency-free presentation layer for
// the sift CLI. It deliberately knows nothing about RPC or command schemas.
package render

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// IsTTY reports whether w is a terminal. A non-terminal never receives ANSI
// escape sequences.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

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

// Section writes a section heading.
func Section(w io.Writer, title string) { fmt.Fprintf(w, "\n%s\n", title) }

// StatusLine writes one check line. Details, when present, are printed on a
// following indented line as supplied by the caller.
func StatusLine(w io.Writer, level, message string) {
	fmt.Fprintf(w, "%s %s\n", Status(level), message)
}

// Table writes a simple left-aligned table without external dependencies.
func Table(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len([]rune(h))
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len([]rune(cell)) > widths[i] {
				widths[i] = len([]rune(cell))
			}
		}
	}
	writeRow := func(row []string) {
		for i, cell := range row {
			if i > 0 {
				io.WriteString(w, "  ")
			}
			fmt.Fprintf(w, "%-*s", widths[i], cell)
		}
		io.WriteString(w, "\n")
	}
	writeRow(headers)
	for _, row := range rows {
		writeRow(row)
	}
}
