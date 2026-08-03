//go:build darwin

package runtime

import "strings"

func darwinSystemExecutable(path string) bool {
	return strings.HasPrefix(path, "/bin/") || strings.HasPrefix(path, "/usr/bin/") || strings.HasPrefix(path, "/System/Library/")
}
