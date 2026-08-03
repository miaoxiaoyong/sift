//go:build linux

package wrapper

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

func enableChildSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// reapExitedChildren collects descendants adopted by the outer supervisor.
// It is called only after its direct execution-wrapper child has exited, so it
// cannot race exec.Cmd.Wait for that child.
func reapExitedChildren() error {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) || pid == 0 {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
