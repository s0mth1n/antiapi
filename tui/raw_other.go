//go:build !linux && !darwin

package tui

type termState struct{}

func enableCBreak(fd uintptr) (*termState, error) {
	return nil, nil
}

func restoreTerminal(fd uintptr, state *termState) error {
	return nil
}
