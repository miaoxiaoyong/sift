// Package render contains the small, dependency-free presentation layer for
// the sift CLI. It deliberately knows nothing about RPC or command schemas.
package render

import "strings"

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
