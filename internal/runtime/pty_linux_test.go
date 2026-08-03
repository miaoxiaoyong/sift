//go:build linux

package runtime

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNewPTYStartsRealLinuxPTY(t *testing.T) {
	pty, err := NewPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()

	if _, err := unix.IoctlGetTermios(int(pty.Slave.Fd()), unix.TCGETS); err != nil {
		t.Fatalf("PTY slave is not a tty: %v", err)
	}
	winsize, err := unix.IoctlGetWinsize(int(pty.Slave.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatalf("read PTY size: %v", err)
	}
	if winsize.Row != 24 || winsize.Col != 80 {
		t.Fatalf("PTY size = %dx%d, want 80x24", winsize.Col, winsize.Row)
	}

	statusPath := t.TempDir()
	statusFile := statusPath + "/stdio"
	doneFile := statusPath + "/done"
	cmd := exec.Command("/bin/sh", "-c", `if test -t 1 && test -t 2; then printf yes > "$PTY_STDIO"; else printf no > "$PTY_STDIO"; fi; while test ! -e "$PTY_DONE"; do sleep 0.01; done`)
	cmd.Env = append(os.Environ(), "PTY_STDIO="+statusFile, "PTY_DONE="+doneFile)
	cmd.Stdout = pty.Slave
	cmd.Stderr = pty.Slave
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	waitForLinuxPTYFile(t, statusFile)
	if got := strings.TrimSpace(readLinuxPTYFile(t, statusFile)); got != "yes" {
		t.Fatalf("child stdout/stderr tty check = %q, want yes", got)
	}
	ppid, pgid, sid := linuxPTYProcessTopology(t, cmd.Process.Pid)
	parentPGID, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	parentSID, err := unix.Getsid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if ppid != os.Getpid() {
		t.Fatalf("child PPID = %d, want %d", ppid, os.Getpid())
	}
	if pgid != parentPGID {
		t.Fatalf("child PGID = %d, want inherited parent PGID %d", pgid, parentPGID)
	}
	if sid != parentSID {
		t.Fatalf("child SID = %d, want inherited parent SID %d", sid, parentSID)
	}

	if err := os.WriteFile(doneFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func waitForLinuxPTYFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PTY child did not write %s", path)
}

func readLinuxPTYFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func linuxPTYProcessTopology(t *testing.T, pid int) (ppid, pgid, sid int) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 >= len(data) {
		t.Fatalf("invalid /proc/%d/stat: %q", pid, data)
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) < 4 {
		t.Fatalf("short /proc/%d/stat: %q", pid, data)
	}
	parse := func(name, value string) int {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("parse %s from /proc/%d/stat: %v", name, pid, err)
		}
		return parsed
	}
	return parse("ppid", fields[1]), parse("pgid", fields[2]), parse("sid", fields[3])
}
