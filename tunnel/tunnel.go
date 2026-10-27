package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var tunnelURLRegex = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

// ExtractTunnelURL searches raw log lines for a trycloudflare.com URL.
func ExtractTunnelURL(line string) string {
	u := tunnelURLRegex.FindString(line)
	if u != "" && u != "https://api.trycloudflare.com" {
		return u
	}
	return ""
}

func findCloudflared() string {
	if envPath := os.Getenv("ANTIAPI_CLOUDFLARED_PATH"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}
	if cfPath, err := exec.LookPath("cloudflared"); err == nil {
		return cfPath
	}
	if exePath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), "libcloudflared.so")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// Tunnel manages a cloudflared background subprocess exposing local port.
type Tunnel struct {
	cmd     *exec.Cmd
	url     string
	urlCh   chan string
	errCh   chan error
	cancel  context.CancelFunc
	mu      sync.RWMutex
	stopped bool
}

// Start launches a cloudflared tunnel pointing to 127.0.0.1:localPort.
func Start(parentCtx context.Context, localPort int) (*Tunnel, error) {
	ctx, cancel := context.WithCancel(parentCtx)
	var cmd *exec.Cmd

	token := os.Getenv("ANTIAPI_CLOUDFLARED_TOKEN")
	var args []string
	if token != "" {
		args = []string{"tunnel", "--no-autoupdate", "run", "--token", token}
	} else {
		args = []string{"tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", localPort), "--no-autoupdate"}
	}

	if cfPath := findCloudflared(); cfPath != "" {
		cmd = exec.CommandContext(ctx, cfPath, args...)
	} else if nixPath, nErr := exec.LookPath("nix"); nErr == nil {
		nixArgs := append([]string{"shell", "nixpkgs#cloudflared", "-c", "cloudflared"}, args...)
		cmd = exec.CommandContext(ctx, nixPath, nixArgs...)
	} else {
		cancel()
		return nil, errors.New("cloudflared executable not found in PATH or environment (use nix flake devShell or install cloudflared)")
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	t := &Tunnel{
		cmd:    cmd,
		urlCh:  make(chan string, 1),
		errCh:  make(chan error, 1),
		cancel: cancel,
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start cloudflared: %w", err)
	}

	var once sync.Once
	setURL := func(found string) {
		once.Do(func() {
			t.mu.Lock()
			t.url = found
			t.mu.Unlock()
			t.urlCh <- found
		})
	}

	scanLines := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if u := ExtractTunnelURL(line); u != "" {
				setURL(u)
			}
		}
	}

	go scanLines(stderrPipe)
	go scanLines(stdoutPipe)

	go func() {
		waitErr := cmd.Wait()
		t.mu.RLock()
		stopped := t.stopped
		t.mu.RUnlock()
		if !stopped && waitErr != nil {
			select {
			case t.errCh <- waitErr:
			default:
			}
		}
	}()

	return t, nil
}

// WaitURL blocks until the public URL is discovered, an error occurs, or timeout fires.
func (t *Tunnel) WaitURL(timeout time.Duration) (string, error) {
	t.mu.RLock()
	if t.url != "" {
		defer t.mu.RUnlock()
		return t.url, nil
	}
	t.mu.RUnlock()

	select {
	case u := <-t.urlCh:
		return u, nil
	case err := <-t.errCh:
		return "", fmt.Errorf("cloudflared process terminated: %w", err)
	case <-time.After(timeout):
		return "", errors.New("timeout waiting for cloudflared tunnel URL")
	}
}

// URL returns the discovered public URL or empty string.
func (t *Tunnel) URL() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.url
}

// Stop terminates the running cloudflared process.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	t.mu.Unlock()

	t.cancel()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}
