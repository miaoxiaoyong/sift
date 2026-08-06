//go:build linux

package launchworker

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// enableTestChildSubreaper keeps crash-harness zombies out of the container
// init. The harness SIGKILLs the outer wrapper after killing its execution
// group, so its descendants must be adopted and reaped by this test process
// before kill(-pgid, 0) can prove the group is absent.
func enableTestChildSubreaper(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatalf("enable crash-harness child subreaper: %v", err)
	}
}

func reapTestProcessGroup(t *testing.T, pgid int) {
	t.Helper()
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) || pid == 0 {
			return
		}
		if err != nil {
			t.Fatalf("reap crash-harness process group %d: %v", pgid, err)
		}
	}
}
