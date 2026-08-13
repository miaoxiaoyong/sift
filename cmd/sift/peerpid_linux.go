//go:build linux

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// daemonPID reports the PID of the process serving the other end of the
// operator socket — the sift daemon — via SO_PEERCRED (Linux). It returns 0
// when the peer credential is unavailable.
func daemonPID(conn *net.UnixConn) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}
