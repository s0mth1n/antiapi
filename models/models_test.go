package models

import (
	"testing"
)

func TestModelResolution(t *testing.T) {
	reg := GetRegistry()

	tests := []struct {
		input    string
		expected string
	}{
		{"gemini-3.8-flash-high", "gemini-3.8-flash-high"},
		{"gemini-3.8-flash-medium", "gemini-3.8-flash-medium"},
		{"gemini-3.8-medium", "gemini-3.8-flash-medium"},
		{"gemini-3.8-flash-low", "gemini-3.8-flash-low"},
		{"gemini-3.8-low", "gemini-3.8-flash-low"},
		{"gemini-3.8-high", "gemini-3.8-flash-high"},
		{"gemini-3.8", "gemini-3.8-flash-high"},
		{"gemini-3.7-flash-high", "gemini-3.7-flash-high"},
		{"gemini-3.7-flash-medium", "gemini-3.7-flash-medium"},
		{"gemini-3.7-flash-low", "gemini-3.7-flash-low"},
		{"gemini-3.7-ultra", "gemini-3.7-flash-high"},
		{"gemini-ultra", "gemini-3.7-flash-high"},
		{"claude-3-7-sonnet", "claude-sonnet-4-6"},
		{"claude-opus", "claude-opus-4-6-thinking"},
		{"gemini-pro", "gemini-pro-agent"},
		{"gpt-oss", "gpt-oss-120b-medium"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := reg.ResolveModel(tt.input)
			if got != tt.expected {
				t.Errorf("ResolveModel(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestCuratedModelsContainsMedium(t *testing.T) {
	reg := GetRegistry()
	models := reg.GetAllModels()

	foundMedium := false
	for _, m := range models {
		if m.ID == "gemini-3.8-flash-medium" {
			foundMedium = true
			break
		}
	}

	if !foundMedium {
		t.Fatalf("expected gemini-3.8-flash-medium in GetAllModels(), but not found")
	}
}

func TestCuratedModelsContextLimits(t *testing.T) {
	reg := GetRegistry()
	models := reg.GetAllModels()

	expectedCanonical := map[string]int{
		"claude-sonnet-4-6": 1048576,
		"claude-opus-4-6":   1048576,
		"gemini-3.8-flash":  1048576,
		"gemini-3.7-flash":  1048576,
	}

	for id, expectedCtx := range expectedCanonical {
		var found bool
		for _, m := range models {
			if m.ID == id {
				found = true
				if m.ContextWindow != expectedCtx {
					t.Errorf("model %s ContextWindow = %d; want %d", id, m.ContextWindow, expectedCtx)
				}
				if m.MaxOutputTokens <= 0 {
					t.Errorf("model %s MaxOutputTokens = %d; want > 0", id, m.MaxOutputTokens)
				}
				break
			}
		}
		if !found {
			t.Errorf("model %s not found in GetAllModels()", id)
		}
	}
}
