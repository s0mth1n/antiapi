package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"antiapi/auth"
)

type mockMetricsProvider struct{}

func (m *mockMetricsProvider) GetMetrics() (total, s2xx, s4xx, s5xx uint64, inFlight int64, avgMs float64) {
	return 25, 24, 1, 0, 1, 14.2
}

func TestIsInternalPoll(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"/api/status", true},
		{"/api/logs", true},
		{"/health", true},
		{"/quota", true},
		{"/api/show", true},
		{"/v1/chat/completions", false},
		{"/v1/messages", false},
		{"/v1/models", false},
	}

	for _, tc := range tests {
		got := IsInternalPoll(tc.path)
		if got != tc.expected {
			t.Errorf("IsInternalPoll(%q) = %v; want %v", tc.path, got, tc.expected)
		}
	}
}

func TestBorderAndRow_VisualWidth(t *testing.T) {
	// Top border
	top := fmt.Sprintf("%s┌─ AntiAPI Gateway %sv1.1.0%s %s┐%s",
		HiCyan, Bold+HiWhite, HiCyan, strings.Repeat("─", 41), Reset)
	if w := visualWidth(top); w != CardWidth {
		t.Fatalf("top border visualWidth = %d; want %d", w, CardWidth)
	}

	// Bottom border
	bottom := fmt.Sprintf("%s└%s┘%s",
		HiCyan, strings.Repeat("─", CardWidth-2), Reset)
	if w := visualWidth(bottom); w != CardWidth {
		t.Fatalf("bottom border visualWidth = %d; want %d", w, CardWidth)
	}

	// Plain row
	row1 := formatRow("Hello world", CardWidth)
	if w := visualWidth(row1); w != CardWidth {
		t.Fatalf("formatRow plain visualWidth = %d; want %d", w, CardWidth)
	}

	// Row with colors and UTF-8 symbols
	row2 := formatRow(fmt.Sprintf("%s➜%s  %sDashboard:%s  http://localhost:8964/",
		Bold+HiCyan, Reset, Bold+HiWhite, Reset), CardWidth)
	if w := visualWidth(row2); w != CardWidth {
		t.Fatalf("formatRow symbols visualWidth = %d; want %d", w, CardWidth)
	}

	// Long row (needs truncation)
	longContent := strings.Repeat("A very long line of text that exceeds the card width ", 5)
	row3 := formatRow(longContent, CardWidth)
	if w := visualWidth(row3); w != CardWidth {
		t.Fatalf("formatRow truncated visualWidth = %d; want %d", w, CardWidth)
	}
}

func TestGetAccountQuotaStr(t *testing.T) {
	acc := &auth.Account{
		Email: "test@example.com",
		Quotas: map[string]float64{
			"gemini-flash": 0.85,
			"gemini-pro":   1.0,
		},
	}
	s := getAccountQuotaStr(acc)
	if !strings.Contains(s, "Flash 85%") {
		t.Errorf("unexpected quota str: %s", s)
	}

	// Test 429 rate limit
	acc.RateLimitedUntil = time.Now().Add(45 * time.Second)
	s2 := getAccountQuotaStr(acc)
	if !strings.Contains(s2, "429 wait") {
		t.Errorf("expected 429 wait in quota str, got: %s", s2)
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(35 * time.Second); got != "35s" {
		t.Errorf("formatDuration(35s) = %s; want 35s", got)
	}
	if got := formatDuration(2*time.Minute + 15*time.Second); got != "2m15s" {
		t.Errorf("formatDuration(2m15s) = %s; want 2m15s", got)
	}
	if got := formatDuration(1*time.Hour + 5*time.Minute); got != "1h05m" {
		t.Errorf("formatDuration(1h5m) = %s; want 1h05m", got)
	}
}

func TestTUI_Lifecycle(t *testing.T) {
	app := Init(8964, &mockMetricsProvider{})
	if app == nil {
		t.Fatal("expected non-nil TUI")
	}

	SetActivity("Testing activity update")
	SetTunnelURL("https://sample-node-123.trycloudflare.com")
	app.Render()
	app.Stop()
}
