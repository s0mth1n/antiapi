//go:build darwin

package tui

import (
	"syscall"
	"unsafe"
)

func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&termios)))
	return err == 0
}
