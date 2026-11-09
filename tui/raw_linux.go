//go:build linux

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
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old))); err != 0 {
		return nil, err
	}
	cbreak := old
	// Only disable canonical mode (ICANON) and local echo (ECHO) for responsive key input.
	// Keep OPOST and ONLCR enabled to guarantee standard carriage return/newline output and prevent staircase rendering.
	// Keep ISIG enabled so standard signals (SIGINT, SIGQUIT) are preserved.
	cbreak.Lflag &^= syscall.ICANON | syscall.ECHO
	cbreak.Lflag |= syscall.ISIG
	cbreak.Oflag |= syscall.OPOST | syscall.ONLCR
	cbreak.Cc[syscall.VMIN] = 1
	cbreak.Cc[syscall.VTIME] = 0
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&cbreak))); err != 0 {
		return nil, err
	}
	return &termState{termios: old}, nil
}

func restoreTerminal(fd uintptr, state *termState) error {
	if state == nil {
		return nil
	}
	restored := state.termios
	// Ensure standard canonical mode, echo, signals, and output formatting are restored
	restored.Lflag |= syscall.ICANON | syscall.ECHO | syscall.ISIG
	restored.Oflag |= syscall.OPOST | syscall.ONLCR
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&restored))); err != 0 {
		return err
	}
	return nil
}
