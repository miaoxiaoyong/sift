//go:build darwin

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// daemonPID reports the PID of the process serving the other end of the
// operator socket — the sift daemon — via LOCAL_PEERPID (the macOS peer-cred
// struct carries no PID). It returns 0 when the option is unavailable.
func daemonPID(conn *net.UnixConn) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	return pid
}
