//go:build sift_test

package wrapper

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"
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
	if release := os.Getenv("SIFT_WRAPPER_TEST_RELEASE"); release != "" {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(release); err == nil {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		return fmt.Errorf("wrapper test pause timed out waiting for %s", release)
	}
	// Self-directed SIGSTOP is delivered asynchronously: Kill can return and
	// subsequent instructions (claim.started, result rename, process exit) still
	// run. Park here and re-STOP after any unexpected CONT so the sync point
	// never advances until the test SIGKILLs the group.
	for {
		_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	}
}

// dumpForTest appends the live handoff tuple at a boundary so crash-harness
// tests can replay the exact same RPC parameters after killing the wrapper.
func dumpForTest(point string, fields map[string]any) {
	path := os.Getenv("SIFT_WRAPPER_TEST_DUMP")
	if path == "" {
		return
	}
	b, err := json.Marshal(map[string]any{"point": point, "fields": fields})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
