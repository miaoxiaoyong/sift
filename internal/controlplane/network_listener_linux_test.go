//go:build linux

package controlplane

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestV10ZeroNetworkListeners proves that starting `sift daemon` leaves this process
// with no TCP or UDP socket. Unix sockets are checked separately by V10a.
func TestV10ZeroNetworkListeners(t *testing.T) {
	home := testHome(t)
	s, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if got := networkListenerInodes(t); len(got) != 0 {
		t.Fatalf("`sift daemon` opened TCP/UDP listeners: %v", got)
	}
}

func networkListenerInodes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	open := make(map[string]bool)
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		open[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
	}

	var listeners []string
	for _, name := range []string{"tcp", "tcp6", "udp", "udp6"} {
		data, err := os.ReadFile(filepath.Join("/proc/self/net", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || !open[fields[9]] {
				continue
			}
			state, err := strconv.ParseUint(fields[3], 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			// TCP LISTEN is 0A. A bound UDP socket is a listener regardless of
			// its state field, which Linux reports as 07 when unconnected.
			if strings.HasPrefix(name, "tcp") && state != 0x0A {
				continue
			}
			listeners = append(listeners, name+":"+fields[9])
		}
	}
	return listeners
}
