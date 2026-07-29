//go:build linux

package runtime

import (
	"os"
	"strconv"
)

// ProcessExecutable returns the procfs exe path used by PlatformProcessInspector.
func ProcessExecutable(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}
