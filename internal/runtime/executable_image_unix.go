//go:build linux || darwin

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

func ReleaseExecutableImage(image *os.File) {
	if image == nil {
		return
	}
	name := image.Name()
	_ = image.Close()
	if runtime.GOOS == "darwin" {
		_ = os.Remove(name)
		_ = os.Remove(filepath.Dir(name))
	}
}

// executableImagePath exposes an inherited Linux image to exec without
// resolving its original pathname. Darwin executes its private copy by path.
func executableImagePath(image *os.File) (string, error) {
	if image == nil {
		return "", fmt.Errorf("runtime: executable image is nil")
	}
	if info, err := image.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("runtime: executable image is unavailable")
	}
	if runtime.GOOS == "darwin" {
		return image.Name(), nil
	}
	if _, err := unix.FcntlInt(image.Fd(), unix.F_SETFD, 0); err != nil {
		return "", fmt.Errorf("runtime: preserve executable image across exec: %w", err)
	}
	return fmt.Sprintf("/proc/self/fd/%d", image.Fd()), nil
}
