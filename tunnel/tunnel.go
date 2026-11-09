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

// Manager coordinates the lifecycle of background Cloudflare tunnels.
type Manager struct {
	mu       sync.RWMutex
	tun      *Tunnel
	port     int
	url      string
	starting bool
	onURL    func(url string)
	onStop   func()
}

var (
	globalManager     *Manager
	globalManagerOnce sync.Once
)

// GetManager returns the singleton tunnel manager instance.
func GetManager() *Manager {
	globalManagerOnce.Do(func() {
		globalManager = &Manager{}
	})
	return globalManager
}

// Init configures the manager with the target local port and event callbacks.
func (m *Manager) Init(port int, onURL func(string), onStop func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.port = port
	m.onURL = onURL
	m.onStop = onStop
}

// IsActive returns whether a tunnel is currently running and connected.
func (m *Manager) IsActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tun != nil && !m.tun.stopped && m.url != ""
}

// IsStarting returns whether a tunnel connection is currently being negotiated.
func (m *Manager) IsStarting() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.starting
}

// URL returns the discovered public URL or empty string.
func (m *Manager) URL() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.url
}

// Start launches a cloudflared tunnel pointing to the configured local port.
func (m *Manager) Start() (string, error) {
	m.mu.Lock()
	if m.tun != nil && !m.tun.stopped && m.url != "" {
		u := m.url
		m.mu.Unlock()
		return u, nil
	}
	if m.starting {
		m.mu.Unlock()
		return "", errors.New("tunnel startup already in progress")
	}
	port := m.port
	if port == 0 {
		port = 8964
	}
	m.starting = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.starting = false
		m.mu.Unlock()
	}()

	t, err := Start(context.Background(), port)
	if err != nil {
		return "", err
	}

	url, err := t.WaitURL(30 * time.Second)
	if err != nil {
		t.Stop()
		return "", err
	}

	m.mu.Lock()
	m.tun = t
	m.url = url
	onURL := m.onURL
	m.mu.Unlock()

	if onURL != nil {
		onURL(url)
	}
	return url, nil
}

// Stop terminates any active cloudflared process and notifies listeners.
func (m *Manager) Stop() {
	m.mu.Lock()
	t := m.tun
	m.tun = nil
	m.url = ""
	m.starting = false
	onStop := m.onStop
	m.mu.Unlock()

	if t != nil {
		t.Stop()
	}
	if onStop != nil {
		onStop()
	}
}

// Toggle starts the tunnel if inactive, or stops it if active.
func (m *Manager) Toggle() (bool, string, error) {
	if m.IsActive() {
		m.Stop()
		return false, "", nil
	}
	url, err := m.Start()
	if err != nil {
		return false, "", err
	}
	return true, url, nil
}
