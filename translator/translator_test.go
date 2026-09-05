package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConsecutiveToolTurnsMerging(t *testing.T) {
	// Setup OpenAI request with consecutive tool responses
	req := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Run tool 1 and tool 2"},
			{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "tool_a", Arguments: `{"arg": 1}`},
					},
					{
						ID:   "call_2",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "tool_b", Arguments: `{"arg": 2}`},
					},
				},
			},
			{Role: "tool", ToolCallID: "call_1", Name: "tool_a", Content: `{"result": "success_a"}`},
			{Role: "tool", ToolCallID: "call_2", Name: "tool_b", Content: `{"result": "success_b"}`},
		},
	}

	gReq, _ := ConvertOpenAIToGoogle(req)
	contents := gReq.Request.Contents

	// Google requires strict user/model alternation.
	// Contents must have:
	// Turn 0: user (Text: "Run tool 1 and tool 2")
	// Turn 1: model (Tool calls)
	// Turn 2: user (Merged tool responses for call_1 and call_2)
	if len(contents) != 3 {
		t.Fatalf("expected 3 merged turns, got %d", len(contents))
	}

	if contents[0].Role != "user" {
		t.Errorf("expected contents[0].Role == 'user', got '%s'", contents[0].Role)
	}
	if contents[1].Role != "model" {
		t.Errorf("expected contents[1].Role == 'model', got '%s'", contents[1].Role)
	}
	if contents[2].Role != "user" {
		t.Errorf("expected contents[2].Role == 'user', got '%s'", contents[2].Role)
	}

	// Turn 2 should have both tool responses
	if len(contents[2].Parts) != 2 {
		t.Errorf("expected contents[2] to have 2 parts, got %d", len(contents[2].Parts))
	}
}

func TestClaudeToolsAndMessages(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-3-5-sonnet",
		Tools: []any{
			map[string]any{
				"name":        "search_weather",
				"description": "Get current weather for location",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]string{"type": "string"},
					},
					"required": []string{"location"},
				},
			},
		},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "What is the weather in Tokyo?"},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_123",
						"name":  "search_weather",
						"input": map[string]any{"location": "Tokyo"},
					},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_123",
						"content":     "22C and sunny",
					},
				},
			},
		},
	}

	gReq, model := ConvertClaudeToGoogle(req)
	if model != "claude-sonnet-4-6" && model != "claude-3-5-sonnet" {
		t.Errorf("unexpected resolved model: %s", model)
	}

	// Verify Tools declarations
	if len(gReq.Request.Tools) == 0 {
		t.Fatal("expected tools declaration in Google request")
	}

	// Verify contents turn structure
	contents := gReq.Request.Contents
	if len(contents) != 3 {
		t.Fatalf("expected 3 alternating contents, got %d", len(contents))
	}

	if contents[0].Role != "user" || contents[1].Role != "model" || contents[2].Role != "user" {
		t.Errorf("expected user -> model -> user alternation, got %s -> %s -> %s",
			contents[0].Role, contents[1].Role, contents[2].Role)
	}
}

func TestVisionDataURLParsing(t *testing.T) {
	req := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "Describe this image"},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
						},
					},
				},
			},
		},
	}

	gReq, _ := ConvertOpenAIToGoogle(req)
	if len(gReq.Request.Contents) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(gReq.Request.Contents))
	}

	parts := gReq.Request.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + inlineData), got %d", len(parts))
	}

	if parts[0].Text != "Describe this image" {
		t.Errorf("expected text 'Describe this image', got '%s'", parts[0].Text)
	}

	if parts[1].InlineData == nil {
		t.Fatal("expected InlineData to be populated")
	}

	if parts[1].InlineData.MimeType != "image/png" {
		t.Errorf("expected mimeType 'image/png', got '%s'", parts[1].InlineData.MimeType)
	}

	if !strings.HasPrefix(parts[1].InlineData.Data, "iVBORw0KGgo") {
		t.Errorf("unexpected base64 data: %s", parts[1].InlineData.Data)
	}
}

func TestReasoningTokensSeparation(t *testing.T) {
	// Raw SSE chunk with thought: true
	raw := []byte(`{
		"response": {
			"candidates": [
				{
					"content": {
						"role": "model",
						"parts": [
							{
								"text": "Let me think through this carefully...",
								"thought": true
							},
							{
								"text": "Here is the final answer."
							}
						]
					},
					"finishReason": "STOP"
				}
			]
		}
	}`)

	parsed, err := ParseGoogleSSEChunk(raw)
	if err != nil {
		t.Fatalf("ParseGoogleSSEChunk failed: %v", err)
	}

	if parsed.ReasoningText != "Let me think through this carefully..." {
		t.Errorf("expected ReasoningText 'Let me think through this carefully...', got '%s'", parsed.ReasoningText)
	}

	if parsed.Text != "Here is the final answer." {
		t.Errorf("expected Text 'Here is the final answer.', got '%s'", parsed.Text)
	}

	// Verify FormatOpenAIChunk formats reasoning_content
	chunk := FormatOpenAIChunk("chatcmpl-1", "gemini-2.5-pro", parsed.Text, parsed.ReasoningText, parsed.FinishReason, nil)
	if !strings.Contains(chunk, `"reasoning_content":"Let me think through this carefully..."`) {
		t.Errorf("expected chunk to contain reasoning_content, got: %s", chunk)
	}
	if !strings.Contains(chunk, `"content":"Here is the final answer."`) {
		t.Errorf("expected chunk to contain content, got: %s", chunk)
	}
}

func TestFormatClaudeNonStreamToolCalls(t *testing.T) {
	toolCalls := []OpenAIToolCall{
		{
			ID:   "toolu_999",
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      "execute_query",
				Arguments: `{"sql": "SELECT 1;"}`,
			},
		},
	}

	respBytes := FormatClaudeNonStream("msg-123", "claude-3-7-sonnet", "", toolCalls)

	var resp map[string]any
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("failed to unmarshal Claude response: %v", err)
	}

	if resp["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason == 'tool_use', got '%v'", resp["stop_reason"])
	}

	contentArr, ok := resp["content"].([]any)
	if !ok || len(contentArr) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(contentArr))
	}

	block, ok := contentArr[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content block map")
	}

	if block["type"] != "tool_use" {
		t.Errorf("expected block type 'tool_use', got '%v'", block["type"])
	}
	if block["name"] != "execute_query" {
		t.Errorf("expected block name 'execute_query', got '%v'", block["name"])
	}
}
