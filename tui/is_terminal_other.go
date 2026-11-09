//go:build !linux && !darwin

package tui

func isTerminal(fd uintptr) bool {
	return true
}
