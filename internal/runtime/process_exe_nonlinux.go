//go:build !linux

package runtime

import "os"

// ProcessExecutable falls back to the invoking executable outside Linux.
func ProcessExecutable(int) (string, error) {
	return os.Executable()
}
