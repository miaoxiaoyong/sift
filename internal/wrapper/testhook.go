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
	// Self-directed SIGSTOP is delivered asynchronously: Kill can return and
	// subsequent instructions (claim.started, result rename, process exit) still
	// run. Park here and re-STOP after any unexpected CONT so the sync point
	// never advances until the test SIGKILLs the group.
	for {
		_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	}
}
