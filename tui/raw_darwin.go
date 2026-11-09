//go:build darwin

package tui

import (
	"syscall"
	"unsafe"
)

type termState struct {
	termios syscall.Termios
}

func enableCBreak(fd uintptr) (*termState, error) {
	var old syscall.Termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&old))); err != 0 {
		return nil, err
	}
	cbreak := old
	cbreak.Lflag &^= syscall.ICANON | syscall.ECHO
	cbreak.Lflag |= syscall.ISIG
	cbreak.Oflag |= syscall.OPOST | syscall.ONLCR
	cbreak.Cc[syscall.VMIN] = 1
	cbreak.Cc[syscall.VTIME] = 0
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&cbreak))); err != 0 {
		return nil, err
	}
	return &termState{termios: old}, nil
}

func restoreTerminal(fd uintptr, state *termState) error {
	if state == nil {
		return nil
	}
	restored := state.termios
	restored.Lflag |= syscall.ICANON | syscall.ECHO | syscall.ISIG
	restored.Oflag |= syscall.OPOST | syscall.ONLCR
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&restored))); err != 0 {
		return err
	}
	return nil
}
