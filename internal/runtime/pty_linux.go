//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// PTY is the wrapper-owned terminal pair used for Agent output. The slave is
// passed to the Agent as stdout/stderr; the wrapper alone reads the master.
type PTY struct {
	Master *os.File
	Slave  *os.File
}

// NewPTY allocates and configures a terminal without making the Agent a
// session or process-group leader. The fixed size is part of the runtime
// protocol and deliberately does not depend on a tmux pane.
func NewPTY() (*PTY, error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("runtime: open PTY master: %w", err)
	}
	closeMaster := true
	defer func() {
		if closeMaster {
			_ = unix.Close(masterFD)
		}
	}()
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		return nil, fmt.Errorf("runtime: unlock PTY: %w", err)
	}
	ptyNumber, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		return nil, fmt.Errorf("runtime: identify PTY slave: %w", err)
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", ptyNumber), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("runtime: open PTY slave: %w", err)
	}
	closeSlave := true
	defer func() {
		if closeSlave {
			_ = unix.Close(slaveFD)
		}
	}()

	termios, err := unix.IoctlGetTermios(slaveFD, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("runtime: read PTY mode: %w", err)
	}
	// cfmakeraw(3), written out to keep the child free of terminal setup
	// syscalls and to preserve the exact bytes emitted by the Agent.
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag |= unix.CS8
	if err := unix.IoctlSetTermios(slaveFD, unix.TCSETS, termios); err != nil {
		return nil, fmt.Errorf("runtime: set PTY mode: %w", err)
	}
	if err := unix.IoctlSetWinsize(slaveFD, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		return nil, fmt.Errorf("runtime: set PTY size: %w", err)
	}

	closeMaster = false
	closeSlave = false
	return &PTY{Master: os.NewFile(uintptr(masterFD), "/dev/ptmx"), Slave: os.NewFile(uintptr(slaveFD), fmt.Sprintf("/dev/pts/%d", ptyNumber))}, nil
}

func (p *PTY) CloseSlave() error {
	if p == nil || p.Slave == nil {
		return nil
	}
	err := p.Slave.Close()
	p.Slave = nil
	return err
}

func (p *PTY) CloseMaster() error {
	if p == nil || p.Master == nil {
		return nil
	}
	err := p.Master.Close()
	p.Master = nil
	return err
}

func (p *PTY) Close() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.CloseSlave(), p.CloseMaster())
}
