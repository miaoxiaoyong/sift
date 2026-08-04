//go:build darwin

package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

type PTY struct {
	Master *os.File
	Slave  *os.File
}

// NewPTY uses the Darwin PTY ioctls directly. In particular, it does not use
// a helper that creates a session or assigns a controlling terminal in the
// Agent child.
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
	if err := ioctlNoArg(masterFD, unix.TIOCPTYUNLK); err != nil {
		return nil, fmt.Errorf("runtime: unlock PTY: %w", err)
	}
	if err := ioctlNoArg(masterFD, unix.TIOCPTYGRANT); err != nil {
		return nil, fmt.Errorf("runtime: grant PTY: %w", err)
	}
	var name [128]byte
	if err := ioctlPtr(masterFD, unix.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		return nil, fmt.Errorf("runtime: identify PTY slave: %w", err)
	}
	slaveName := strings.TrimRight(string(name[:]), "\x00")
	slaveFD, err := unix.Open(slaveName, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("runtime: open PTY slave: %w", err)
	}
	closeSlave := true
	defer func() {
		if closeSlave {
			_ = unix.Close(slaveFD)
		}
	}()
	termios, err := unix.IoctlGetTermios(slaveFD, unix.TIOCGETA)
	if err != nil {
		return nil, fmt.Errorf("runtime: read PTY mode: %w", err)
	}
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag |= unix.CS8
	if err := unix.IoctlSetTermios(slaveFD, unix.TIOCSETA, termios); err != nil {
		return nil, fmt.Errorf("runtime: set PTY mode: %w", err)
	}
	if err := unix.IoctlSetWinsize(slaveFD, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		return nil, fmt.Errorf("runtime: set PTY size: %w", err)
	}
	closeMaster = false
	closeSlave = false
	return &PTY{Master: os.NewFile(uintptr(masterFD), "/dev/ptmx"), Slave: os.NewFile(uintptr(slaveFD), slaveName)}, nil
}

// ioctlNoArg is kept as a small compatibility wrapper because x/sys/unix does
// not expose the Darwin PTY unlock/grant requests as typed operations.
func ioctlNoArg(fd int, req uintptr) error {
	//lint:ignore SA1019 Darwin's PTY requests have no x/sys wrapper.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ioctlPtr is needed for TIOCPTYGNAME, for which x/sys/unix does not expose a
// typed Darwin wrapper.
func ioctlPtr(fd int, req uintptr, arg unsafe.Pointer) error {
	//lint:ignore SA1019 Darwin's arbitrary-pointer ioctl has no x/sys wrapper.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
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
