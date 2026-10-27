package tunnel

import (
	"strings"
	"testing"
)

func TestExtractTunnelURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard trycloudflare log box",
			input:    "2026-10-25T21:30:28Z INF |  https://pvc-faces-aged-poem.trycloudflare.com                                             |",
			expected: "https://pvc-faces-aged-poem.trycloudflare.com",
		},
		{
			name:     "visit it at line",
			input:    "Your quick Tunnel has been created! Visit it at: https://alpha-beta-123.trycloudflare.com",
			expected: "https://alpha-beta-123.trycloudflare.com",
		},
		{
			name:     "no url present",
			input:    "2026-10-25T21:30:28Z INF Requesting new quick Tunnel on trycloudflare.com...",
			expected: "",
		},
		{
			name:     "unrelated link",
			input:    "Visit https://developers.cloudflare.com/cloudflare-one/connections/connect-apps",
			expected: "",
		},
		{
			name:     "control plane api url",
			input:    "2026-10-27T17:15:00Z INF Registered tunnel connection endpoint=https://api.trycloudflare.com",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractTunnelURL(tc.input)
			if got != tc.expected {
				t.Errorf("ExtractTunnelURL(%q) = %q; want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestStart_MissingBinary(t *testing.T) {
	// If cloudflared binary is tested with invalid PATH, Start should return wrapped error
	t.Setenv("PATH", "/nonexistent")
	_, err := Start(t.Context(), 8964)
	if err == nil {
		t.Fatal("expected error when cloudflared is not in PATH, got nil")
	}
	if !strings.Contains(err.Error(), "cloudflared executable not found in PATH") {
		t.Errorf("unexpected error message: %v", err)
	}
}
