//go:build !linux && !darwin

package main

import "net"

// daemonPID is a no-op on platforms without a peer-PID socket option; the
// daemon state is still reported from socket presence + connect liveness.
func daemonPID(*net.UnixConn) int { return 0 }
