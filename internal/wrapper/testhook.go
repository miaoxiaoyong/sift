//go:build sift_test

package wrapper

import (
	"os"
	"syscall"
)

func pauseForTest(point string) error {
	if os.Getenv("SIFT_WRAPPER_TEST_PAUSE") != point {
		return nil
	}
	if path := os.Getenv("SIFT_WRAPPER_TEST_READY"); path != "" {
		if err := os.WriteFile(path, []byte(point), 0600); err != nil {
			return err
		}
	}
	return syscall.Kill(os.Getpid(), syscall.SIGSTOP)
}
