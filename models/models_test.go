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
		{"gemini-3.8-flash", "gemini-3.8-flash-high"},
		{"claude-3-7-sonnet", "gemini-3.8-flash-high"},
		{"claude-opus", "gemini-3.8-flash-high"},
		{"gemini-pro", "gemini-3.8-flash-high"},
		{"gpt-oss", "gemini-3.8-flash-high"},
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

func TestCuratedModelsCanonical(t *testing.T) {
	reg := GetRegistry()
	models := reg.GetAllModels()

	foundCanonical := false
	for _, m := range models {
		if m.ID == "gemini-3.8-flash" {
			foundCanonical = true
			if m.ContextWindow != 1048576 {
				t.Errorf("expected 1048576 context window for gemini-3.8-flash, got %d", m.ContextWindow)
			}
			if m.MaxOutputTokens != 65536 {
				t.Errorf("expected 65536 max output tokens for gemini-3.8-flash, got %d", m.MaxOutputTokens)
			}
			break
		}
	}

	if !foundCanonical {
		t.Fatalf("expected gemini-3.8-flash in GetAllModels(), but not found")
	}
}

func TestCuratedModelsContextLimits(t *testing.T) {
	reg := GetRegistry()
	models := reg.GetAllModels()

	expectedCanonical := map[string]int{
		"gemini-3.8-flash": 1048576,
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

func TestGetModel(t *testing.T) {
	reg := GetRegistry()

	m, ok := reg.GetModel("gemini-3.8-flash")
	if !ok {
		t.Fatalf("expected GetModel(gemini-3.8-flash) to succeed")
	}
	if m.ID != "gemini-3.8-flash" {
		t.Errorf("expected ID 'gemini-3.8-flash', got %q", m.ID)
	}

	// Alias check
	mHigh, ok := reg.GetModel("gemini-3.8-flash-high")
	if !ok {
		t.Fatalf("expected GetModel(gemini-3.8-flash-high) to succeed")
	}
	if mHigh.ID != "gemini-3.8-flash" {
		t.Errorf("expected ID 'gemini-3.8-flash', got %q", mHigh.ID)
	}

	// Unknown model
	_, ok = reg.GetModel("nonexistent-model")
	if ok {
		t.Errorf("expected GetModel(nonexistent-model) to return false")
	}
}
