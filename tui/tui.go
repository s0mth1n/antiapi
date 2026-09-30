package tui

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"antiapi/auth"
)

// ANSI color and style tokens
const (
	Reset     = "\033[0m"
	Bold      = "\033[1m"
	Dim       = "\033[2m"
	Cyan      = "\033[36m"
	HiCyan    = "\033[96m"
	Green     = "\033[32m"
	HiGreen   = "\033[92m"
	Yellow    = "\033[33m"
	HiYellow  = "\033[93m"
	Red       = "\033[31m"
	HiRed     = "\033[91m"
	Blue      = "\033[34m"
	HiBlue    = "\033[94m"
	Magenta   = "\033[35m"
	HiMagenta = "\033[95m"
	HiWhite   = "\033[97m"
	DarkGray  = "\033[90m"
)

const CardWidth = 68

type MetricsProvider interface {
	GetMetrics() (total, s2xx, s4xx, s5xx uint64, inFlight int64, avgMs float64)
}

type TUI struct {
	port            int
	metricsProvider MetricsProvider
	startTime       time.Time

	mu                sync.RWMutex
	lastActivity      string
	lastRenderedLines int
	isInitialized     bool
	stopCh            chan struct{}
}

var (
	globalTUI *TUI
	globalMu  sync.RWMutex
)

func IsTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return err == 0
}

func Init(port int, mp MetricsProvider) *TUI {
	globalMu.Lock()
	defer globalMu.Unlock()

	globalTUI = &TUI{
		port:            port,
		metricsProvider: mp,
		startTime:       time.Now(),
		lastActivity:    fmt.Sprintf("Gateway ready • Port %d", port),
		stopCh:          make(chan struct{}),
	}
	return globalTUI
}

func SetActivity(msg string) {
	globalMu.RLock()
	t := globalTUI
	globalMu.RUnlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	t.lastActivity = msg
	t.mu.Unlock()
	t.Render()
}

func (t *TUI) Start() {
	// Hide cursor during in-place rendering
	os.Stdout.WriteString("\033[?25l")

	// Initial render
	t.Render()

	// Periodic update ticker (1s) to update uptime & metrics smoothly
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-t.stopCh:
				return
			case <-ticker.C:
				t.Render()
			}
		}
	}()
}

func (t *TUI) Stop() {
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
	// Restore cursor and move cleanly past the card
	os.Stdout.WriteString("\n\033[?25h\n")
}

func (t *TUI) Render() {
	t.mu.Lock()
	defer t.mu.Unlock()

	var lines []string

	// 1. Top border (Visual width: 68)
	lines = append(lines, fmt.Sprintf("%s┌─ AntiAPI Gateway %sv1.1.0%s %s┐%s",
		HiCyan, Bold+HiWhite, HiCyan, strings.Repeat("─", 41), Reset))

	// 2. Empty row
	lines = append(lines, formatRow("", CardWidth))

	// 3. Endpoints
	lines = append(lines, formatRow(fmt.Sprintf("%s➜%s  %sDashboard:%s  http://localhost:%d/",
		Bold+HiCyan, Reset, Bold+HiWhite, Reset, t.port), CardWidth))
	lines = append(lines, formatRow(fmt.Sprintf("%s➜%s  %sOpenAI:%s     http://localhost:%d/v1",
		Bold+HiCyan, Reset, Bold+HiWhite, Reset, t.port), CardWidth))
	lines = append(lines, formatRow(fmt.Sprintf("%s➜%s  %sClaude:%s     http://localhost:%d/v1/messages",
		Bold+HiCyan, Reset, Bold+HiWhite, Reset, t.port), CardWidth))
	lines = append(lines, formatRow(fmt.Sprintf("%s➜%s  %sModels:%s     http://localhost:%d/v1/models",
		Bold+HiCyan, Reset, Bold+HiWhite, Reset, t.port), CardWidth))

	// 4. Empty row
	lines = append(lines, formatRow("", CardWidth))

	// 5. Account pool
	accounts := auth.GetPool().GetAllAccounts()
	if len(accounts) == 0 {
		lines = append(lines, formatRow(fmt.Sprintf("%s●%s  %sPool:%s       0 accounts active",
			HiYellow, Reset, Bold+HiWhite, Reset), CardWidth))
	} else {
		lines = append(lines, formatRow(fmt.Sprintf("%s●%s  %sPool:%s       %d accounts active",
			HiGreen, Reset, Bold+HiWhite, Reset, len(accounts)), CardWidth))
		maxShow := 3
		for i, acc := range accounts {
			if i >= maxShow {
				remaining := len(accounts) - maxShow
				lines = append(lines, formatRow(fmt.Sprintf("   %s• +%d more accounts in dashboard%s",
					DarkGray, remaining, Reset), CardWidth))
				break
			}
			email := acc.Email
			if len(email) > 24 {
				email = email[:21] + "..."
			}
			quota := getAccountQuotaStr(acc)
			lines = append(lines, formatRow(fmt.Sprintf("   • %-24s (%s)", email, quota), CardWidth))
		}
	}

	// 6. Failover feature
	lines = append(lines, formatRow(fmt.Sprintf("%s●%s  %sFailover:%s   503 capacity auto-reroute enabled",
		HiGreen, Reset, Bold+HiWhite, Reset), CardWidth))

	// 7. Empty row
	lines = append(lines, formatRow("", CardWidth))

	// 8. Live Activity
	act := t.lastActivity
	lines = append(lines, formatRow(fmt.Sprintf("%sActivity:%s   %s", Bold+HiWhite, Reset, act), CardWidth))

	// 9. Metrics & Uptime
	uptime := time.Since(t.startTime).Round(time.Second)
	var total, s2xx, s4xx, s5xx uint64
	var inFlight int64
	if t.metricsProvider != nil {
		total, s2xx, s4xx, s5xx, inFlight, _ = t.metricsProvider.GetMetrics()
	}
	metricsLine := fmt.Sprintf("%sStatus:%s     Ready • %d reqs (%s%d OK%s, %s%d ERR%s) • %d in-flight • %s",
		Bold+HiWhite, Reset, total,
		HiGreen, s2xx, Reset,
		HiRed, s4xx+s5xx, Reset,
		inFlight, formatDuration(uptime))
	lines = append(lines, formatRow(metricsLine, CardWidth))

	// 10. Bottom border (Visual width: 68)
	lines = append(lines, fmt.Sprintf("%s└%s┘%s",
		HiCyan, strings.Repeat("─", CardWidth-2), Reset))

	var buf bytes.Buffer

	// If already rendered, move cursor back up to the top line of the previous render
	if t.isInitialized && t.lastRenderedLines > 1 {
		fmt.Fprintf(&buf, "\033[%dA\r", t.lastRenderedLines-1)
	} else {
		t.isInitialized = true
	}

	totalLines := len(lines)
	for i, line := range lines {
		if i < totalLines-1 {
			buf.WriteString(line)
			buf.WriteString("\033[K\n")
		} else {
			// Last line: clear to end of line, but DO NOT emit newline!
			// Omitting the newline keeps the cursor on the bottom border and prevents viewport scrolling.
			buf.WriteString(line)
			buf.WriteString("\033[K")
		}
	}

	// If the new frame has fewer lines than previous frame, clear leftover lines below
	if t.lastRenderedLines > totalLines {
		diff := t.lastRenderedLines - totalLines
		for d := 0; d < diff; d++ {
			buf.WriteString("\n\033[K")
		}
		fmt.Fprintf(&buf, "\033[%dA\r", diff)
	}

	t.lastRenderedLines = totalLines
	os.Stdout.Write(buf.Bytes())
}

// formatRow ensures that visual character width between borders is always exactly (width - 4).
func formatRow(content string, width int) string {
	targetInner := width - 4 // left border is "│ ", right border is " │"
	visLen := visualWidth(content)
	if visLen > targetInner {
		content = truncateVisual(content, targetInner)
		visLen = targetInner
	}
	pad := targetInner - visLen
	if pad < 0 {
		pad = 0
	}
	return fmt.Sprintf("%s│%s %s%s %s│%s", HiCyan, Reset, content, strings.Repeat(" ", pad), HiCyan, Reset)
}

func getAccountQuotaStr(acc *auth.Account) string {
	if acc == nil {
		return "unknown"
	}
	if !acc.RateLimitedUntil.IsZero() && time.Now().Before(acc.RateLimitedUntil) {
		rem := time.Until(acc.RateLimitedUntil).Round(time.Second)
		return fmt.Sprintf("429 wait %s", formatDuration(rem))
	}
	if len(acc.Quotas) > 0 {
		var parts []string
		for k, v := range acc.Quotas {
			short := k
			if strings.Contains(strings.ToLower(k), "flash") {
				short = "Flash"
			} else if strings.Contains(strings.ToLower(k), "pro") {
				short = "Pro"
			}
			parts = append(parts, fmt.Sprintf("%s %.0f%%", short, v*100))
			if len(parts) >= 2 {
				break
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " • ")
		}
	}
	return "Quotas: ready"
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func visualWidth(s string) int {
	width := 0
	inEscape := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEscape = true
			i += 2
			continue
		}
		if inEscape {
			b := s[i]
			if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
				inEscape = false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\n' || r == '\r' {
			i += size
			continue
		}
		width++
		i += size
	}
	return width
}

func truncateVisual(s string, maxLen int) string {
	width := 0
	inEscape := false
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEscape = true
			out.WriteByte(s[i])
			out.WriteByte(s[i+1])
			i += 2
			continue
		}
		if inEscape {
			b := s[i]
			out.WriteByte(b)
			if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
				inEscape = false
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if width+1 > maxLen {
			out.WriteString(Reset)
			break
		}
		out.WriteString(s[i : i+size])
		width++
		i += size
	}
	return out.String()
}

// IsInternalPoll returns true for background health checks and dashboard polling routes.
func IsInternalPoll(path string) bool {
	switch path {
	case "/api/status", "/api/logs", "/health", "/quota", "/api/show", "/api/tags", "/version", "/props", "/v1/props", "/favicon.ico":
		return true
	default:
		return strings.HasPrefix(path, "/api/logs") || strings.HasPrefix(path, "/api/status")
	}
}
