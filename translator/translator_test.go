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
	if model != "gemini-3.8-flash-high" {
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

	respBytes := FormatClaudeNonStream("msg-123", "claude-3-7-sonnet", "", "", toolCalls, nil)

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

func TestPackUnpackThoughtSignature(t *testing.T) {
	callID := "call_abc123"
	sig := "fake-cryptographic-hmac-signature"

	packed := PackCallID(callID, sig)
	if !strings.HasPrefix(packed, callID+"_ts_") {
		t.Fatalf("expected packed ID to start with %s_ts_, got %s", callID, packed)
	}

	unpackedID, unpackedSig := UnpackCallID(packed)
	if unpackedID != callID {
		t.Errorf("expected unpacked ID %s, got %s", callID, unpackedID)
	}
	if unpackedSig != sig {
		t.Errorf("expected unpacked sig %s, got %s", sig, unpackedSig)
	}

	// Call ID containing _ts_ in prefix preserves baseID and unpacked sig correctly
	customID := "call_ts_custom_tool"
	packedCustom := PackCallID(customID, sig)
	unpackedCustomID, unpackedCustomSig := UnpackCallID(packedCustom)
	if unpackedCustomID != customID {
		t.Errorf("expected unpacked custom ID %s, got %s", customID, unpackedCustomID)
	}
	if unpackedCustomSig != sig {
		t.Errorf("expected unpacked custom sig %s, got %s", sig, unpackedCustomSig)
	}

	// Non-packed callID passes through cleanly
	plainID, plainSig := UnpackCallID("call_plain_456")
	if plainID != "call_plain_456" {
		t.Errorf("expected call_plain_456, got %s", plainID)
	}
	if plainSig != "" {
		t.Errorf("expected empty sig, got %s", plainSig)
	}

	// Invalid hex suffix or odd-length hex does not corrupt ID
	for _, raw := range []string{"call_ts_invalid", "call_ts_123", "call_ts_"} {
		id, s := UnpackCallID(raw)
		if id != raw || s != "" {
			t.Errorf("expected %s to pass through unchanged with empty sig, got id=%s, sig=%s", raw, id, s)
		}
	}
}

func TestClaudeStatelessToolNameRecovery(t *testing.T) {
	// Simulate stateless client request with packed ID where toolCallMemory has not seen the ID
	sig := "stateless-thought-token"
	rawID := "call_calc_987"
	packedID := PackCallID(rawID, sig)

	req := &ClaudeRequest{
		Model: "claude-3-5-sonnet",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Calculate 21 * 2"},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type":  "tool_use",
						"id":    packedID,
						"name":  "calculator",
						"input": map[string]any{"expression": "21 * 2"},
					},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": packedID,
						"content":     "42",
					},
				},
			},
		},
	}

	gReq, _ := ConvertClaudeToGoogle(req)
	contents := gReq.Request.Contents

	if len(contents) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(contents))
	}

	// Model turn: must contain FunctionCall with name "calculator" and unpacked ID "call_calc_987"
	modelTurn := contents[1]
	if modelTurn.Role != "model" || len(modelTurn.Parts) == 0 {
		t.Fatalf("invalid model turn: %+v", modelTurn)
	}
	fc := modelTurn.Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("expected functionCall part in model turn")
	}
	if fc.Name != "calculator" || fc.ID != rawID {
		t.Errorf("expected FunctionCall name=calculator, ID=%s, got name=%s, ID=%s", rawID, fc.Name, fc.ID)
	}
	if modelTurn.Parts[0].ThoughtSignature != sig {
		t.Errorf("expected ThoughtSignature=%s, got %s", sig, modelTurn.Parts[0].ThoughtSignature)
	}

	// User turn: must contain FunctionResponse matching name "calculator" (NOT "tool_result") and ID "call_calc_987"
	userTurn := contents[2]
	if userTurn.Role != "user" || len(userTurn.Parts) == 0 {
		t.Fatalf("invalid user turn: %+v", userTurn)
	}
	fr := userTurn.Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("expected functionResponse part in user turn")
	}
	if fr.Name != "calculator" {
		t.Errorf("expected FunctionResponse Name='calculator', got '%s'", fr.Name)
	}
	if fr.ID != rawID {
		t.Errorf("expected FunctionResponse ID='%s', got '%s'", rawID, fr.ID)
	}
}

func TestCleanAndSanitizeContents(t *testing.T) {
	raw := []GoogleContent{
		{Role: "model", Parts: []GooglePart{{Text: "leading model"}}},
		{Role: "user", Parts: []GooglePart{{Text: "turn 1"}}},
		{Role: "user", Parts: []GooglePart{{Text: "turn 1 continuation"}}},
		{Role: "model", Parts: []GooglePart{{Text: "response 1"}}},
		{Role: "model", Parts: []GooglePart{{Text: "response 1 extra"}}},
	}

	cleaned := CleanAndSanitizeContents(raw)

	// Cleaned must start with user
	if len(cleaned) == 0 || cleaned[0].Role != "user" {
		t.Fatalf("expected first turn to be user, got %v", cleaned)
	}

	// Roles must strictly alternate
	for i := 1; i < len(cleaned); i++ {
		if cleaned[i].Role == cleaned[i-1].Role {
			t.Errorf("consecutive identical roles at turn %d: %s", i, cleaned[i].Role)
		}
	}
}

func TestOpenAIToolResponseNameMismatchResolution(t *testing.T) {
	// Scenario: Client assistant turn called "tool_call", but client tool turn sent custom name "mcp__canva__search_brand_templates".
	// The translated FunctionResponse must strictly match FunctionCall.Name ("tool_call") to avoid Google Cloud Code PA 400 INVALID_ARGUMENT.
	sig := "authentic-thought-sig"
	rawID := "call_test_123"
	packedID := PackCallID(rawID, sig)

	req := &OpenAIChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Search Canva templates"},
			{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{
					{
						ID:   packedID,
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "tool_call",
							Arguments: `{"arguments":{"user_intent":"Find templates"},"name":"mcp__canva__search_brand_templates"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: packedID,
				Name:       "mcp__canva__search_brand_templates", // Client passes sub-tool name here
				Content:    `{"result": "found 3 templates"}`,
			},
		},
	}

	gReq, _ := ConvertOpenAIToGoogle(req)
	contents := gReq.Request.Contents

	if len(contents) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(contents))
	}

	// Model turn: FunctionCall.Name must be "tool_call"
	modelTurn := contents[1]
	if modelTurn.Role != "model" || len(modelTurn.Parts) == 0 {
		t.Fatalf("invalid model turn: %+v", modelTurn)
	}
	fc := modelTurn.Parts[0].FunctionCall
	if fc == nil || fc.Name != "tool_call" {
		t.Fatalf("expected model turn FunctionCall name 'tool_call', got %+v", fc)
	}

	// User turn: FunctionResponse.Name MUST match FunctionCall.Name ("tool_call"), NOT "mcp__canva__search_brand_templates"
	userTurn := contents[2]
	if userTurn.Role != "user" || len(userTurn.Parts) == 0 {
		t.Fatalf("invalid user turn: %+v", userTurn)
	}
	fr := userTurn.Parts[0].FunctionResponse
	if fr == nil {
		t.Fatalf("expected user turn FunctionResponse part, got %+v", userTurn.Parts[0])
	}
	if fr.Name != "tool_call" {
		t.Errorf("expected FunctionResponse.Name to match 'tool_call', got '%s'", fr.Name)
	}
	if fr.ID != rawID {
		t.Errorf("expected FunctionResponse.ID '%s', got '%s'", rawID, fr.ID)
	}
}

func TestParseGoogleSSEChunkWithUsage(t *testing.T) {
	raw := []byte(`{
		"response": {
			"candidates": [
				{
					"content": {
						"parts": [{"text": "Hello world"}]
					},
					"finishReason": "STOP"
				}
			],
			"usageMetadata": {
				"promptTokenCount": 15,
				"candidatesTokenCount": 2,
				"totalTokenCount": 25,
				"thoughtsTokenCount": 8,
				"cachedContentTokenCount": 5
			}
		}
	}`)

	parsed, err := ParseGoogleSSEChunk(raw)
	if err != nil {
		t.Fatalf("ParseGoogleSSEChunk failed: %v", err)
	}

	if parsed.Usage == nil {
		t.Fatalf("expected parsed.Usage != nil")
	}
	if parsed.Usage.PromptTokenCount != 15 {
		t.Errorf("expected promptTokenCount 15, got %d", parsed.Usage.PromptTokenCount)
	}
	if parsed.Usage.ThoughtsTokenCount != 8 {
		t.Errorf("expected thoughtsTokenCount 8, got %d", parsed.Usage.ThoughtsTokenCount)
	}
	if parsed.Usage.TotalTokenCount != 25 {
		t.Errorf("expected totalTokenCount 25, got %d", parsed.Usage.TotalTokenCount)
	}

	// Test FormatOpenAIUsageChunk
	chunk := FormatOpenAIUsageChunk("chatcmpl-test", "gemini-3.8-flash-high", parsed.Usage)
	if !strings.HasPrefix(chunk, "data: ") {
		t.Fatalf("expected data: prefix, got %s", chunk)
	}
	var chunkData struct {
		Choices []any `json:"choices"`
		Usage   struct {
			PromptTokens            int            `json:"prompt_tokens"`
			CompletionTokens        int            `json:"completion_tokens"`
			TotalTokens             int            `json:"total_tokens"`
			PromptTokensDetails     map[string]any `json:"prompt_tokens_details"`
			CompletionTokensDetails map[string]any `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	jsonBytes := strings.TrimPrefix(chunk, "data: ")
	jsonBytes = strings.TrimSpace(jsonBytes)
	if err := json.Unmarshal([]byte(jsonBytes), &chunkData); err != nil {
		t.Fatalf("failed to unmarshal usage chunk: %v", err)
	}

	if len(chunkData.Choices) != 0 {
		t.Errorf("expected empty choices in usage chunk, got %d", len(chunkData.Choices))
	}
	if chunkData.Usage.PromptTokens != 15 {
		t.Errorf("expected prompt_tokens 15, got %d", chunkData.Usage.PromptTokens)
	}
	if chunkData.Usage.CompletionTokens != 10 { // 2 candidates + 8 thoughts
		t.Errorf("expected completion_tokens 10, got %d", chunkData.Usage.CompletionTokens)
	}
	if chunkData.Usage.TotalTokens != 25 {
		t.Errorf("expected total_tokens 25, got %d", chunkData.Usage.TotalTokens)
	}
	if chunkData.Usage.CompletionTokensDetails["reasoning_tokens"].(float64) != 8 {
		t.Errorf("expected reasoning_tokens 8, got %v", chunkData.Usage.CompletionTokensDetails["reasoning_tokens"])
	}
}

func TestFormatOpenAINonStreamWithUsage(t *testing.T) {
	u := &GoogleUsageMetadata{
		PromptTokenCount:     20,
		CandidatesTokenCount: 10,
		TotalTokenCount:      30,
		ThoughtsTokenCount:   5,
	}
	respBytes := FormatOpenAINonStream("cmpl-1", "gemini-3.8-flash-high", "Test response", "", nil, u)
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("failed to unmarshal non-stream response: %v", err)
	}
	if resp.Usage.PromptTokens != 20 {
		t.Errorf("expected prompt_tokens 20, got %d", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 15 { // 10 candidates + 5 thoughts
		t.Errorf("expected completion_tokens 15, got %d", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("expected total_tokens 30, got %d", resp.Usage.TotalTokens)
	}
}

func TestFormatClaudeNonStreamWithUsage(t *testing.T) {
	u := &GoogleUsageMetadata{
		PromptTokenCount:     50,
		CandidatesTokenCount: 25,
		TotalTokenCount:      75,
	}
	respBytes := FormatClaudeNonStream("msg-1", "claude-sonnet-4-6", "Claude text", "", nil, u)
	var resp struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("failed to unmarshal Claude response: %v", err)
	}
	if resp.Usage.InputTokens != 50 {
		t.Errorf("expected input_tokens 50, got %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 25 {
		t.Errorf("expected output_tokens 25, got %d", resp.Usage.OutputTokens)
	}
}

func TestSanitizeGoogleSchema(t *testing.T) {
	// Recreate schema scenario with const in anyOf and unsupported JSON schema keys
	rawSchema := map[string]any{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"command", "action"},
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Command to run",
			},
			"action": map[string]any{
				"description": "Action selector",
				"anyOf": []any{
					map[string]any{"const": "read", "description": "Read file"},
					map[string]any{"const": "write", "description": "Write file"},
					map[string]any{"const": 42},
				},
			},
			"mode": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "number"},
				},
			},
		},
	}

	sanitized := SanitizeGoogleSchema(rawSchema)
	sMap, ok := sanitized.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", sanitized)
	}

	// Verify unsupported keys stripped
	if _, hasSchema := sMap["$schema"]; hasSchema {
		t.Errorf("expected $schema to be stripped")
	}
	if _, hasAddProps := sMap["additionalProperties"]; hasAddProps {
		t.Errorf("expected additionalProperties to be stripped")
	}

	// Verify required
	req, ok := sMap["required"].([]string)
	if !ok || len(req) != 2 || req[0] != "command" || req[1] != "action" {
		t.Errorf("unexpected required fields: %v", sMap["required"])
	}

	// Verify properties
	props, ok := sMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map")
	}

	actionProp, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("expected action property map")
	}

	anyOf, ok := actionProp["anyOf"].([]any)
	if !ok || len(anyOf) != 3 {
		t.Fatalf("expected 3 anyOf elements, got %v", actionProp["anyOf"])
	}

	// Check const -> enum conversion
	first := anyOf[0].(map[string]any)
	if _, hasConst := first["const"]; hasConst {
		t.Errorf("expected const to be removed from anyOf[0]")
	}
	enums, ok := first["enum"].([]string)
	if !ok || len(enums) != 1 || enums[0] != "read" {
		t.Errorf("expected enum ['read'], got %v", first["enum"])
	}
	if first["type"] != "string" {
		t.Errorf("expected inferred type 'string', got %v", first["type"])
	}

	third := anyOf[2].(map[string]any)
	if third["type"] != "number" && third["type"] != "integer" {
		t.Errorf("expected inferred numeric type, got %v", third["type"])
	}
	enums3, ok := third["enum"].([]string)
	if !ok || len(enums3) != 1 || enums3[0] != "42" {
		t.Errorf("expected enum ['42'], got %v", third["enum"])
	}

	// Verify oneOf -> anyOf
	modeProp, ok := props["mode"].(map[string]any)
	if !ok {
		t.Fatalf("expected mode property map")
	}
	if _, hasOneOf := modeProp["oneOf"]; hasOneOf {
		t.Errorf("expected oneOf to be stripped")
	}
	if _, hasAnyOf := modeProp["anyOf"]; !hasAnyOf {
		t.Errorf("expected oneOf to be converted to anyOf")
	}
}

func TestConvertOpenAIToGoogleWithSanitizedTools(t *testing.T) {
	req := &OpenAIChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hello"},
		},
		Tools: []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "test_tool",
					"description": "A tool with const",
					"parameters": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"command": map[string]any{
								"anyOf": []any{
									map[string]any{"const": "run"},
								},
							},
						},
					},
				},
			},
		},
	}

	gReq, _ := ConvertOpenAIToGoogle(req)
	if gReq == nil {
		t.Fatalf("ConvertOpenAIToGoogle returned nil gReq")
	}

	if len(gReq.Request.Tools) == 0 {
		t.Fatalf("expected tools in Google request")
	}

	toolMap := gReq.Request.Tools[0].(map[string]any)
	funcs := toolMap["functionDeclarations"].([]map[string]any)
	if len(funcs) != 1 {
		t.Fatalf("expected 1 function declaration")
	}

	params := funcs[0]["parameters"].(map[string]any)
	if _, hasAddProps := params["additionalProperties"]; hasAddProps {
		t.Errorf("expected additionalProperties removed")
	}

	props := params["properties"].(map[string]any)
	cmd := props["command"].(map[string]any)
	anyOf := cmd["anyOf"].([]any)
	elem := anyOf[0].(map[string]any)
	if _, hasConst := elem["const"]; hasConst {
		t.Errorf("expected const removed")
	}
	if elem["type"] != "string" {
		t.Errorf("expected inferred type string")
	}
}

func BenchmarkFormatOpenAIChunk(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = FormatOpenAIChunk("chatcmpl-test-12345", "gemini-3.8-flash-high", "Hello world this is a streaming token test.", "", "", nil)
	}
}

func BenchmarkParseGoogleSSEChunk(b *testing.B) {
	raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello world"}]}}]}}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = ParseGoogleSSEChunk(raw)
	}
}

func TestFastModeLogic(t *testing.T) {
	SetGlobalFastMode(false)
	defer SetGlobalFastMode(false)

	// 1. Default request: includeThoughts is enabled by default on standard model
	reqDefault := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	gReq, _ := ConvertOpenAIToGoogle(reqDefault)
	tc, ok := gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["includeThoughts"] != true {
		t.Fatalf("expected includeThoughts == true for default request, got %v", tc)
	}

	// 2. Explicit FastMode in request
	fastTrue := true
	reqFast := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		FastMode: &fastTrue,
	}
	gReq, _ = ConvertOpenAIToGoogle(reqFast)
	tc, ok = gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["thinkingBudget"] != 0 {
		t.Fatalf("expected thinkingBudget == 0 for FastMode=true, got %v", tc)
	}
	if gReq.Request.SystemInstruction == nil || len(gReq.Request.SystemInstruction.Parts) == 0 {
		t.Fatalf("expected SystemInstruction with fast directive for FastMode=true")
	}
	sysText := gReq.Request.SystemInstruction.Parts[len(gReq.Request.SystemInstruction.Parts)-1].Text
	if !strings.Contains(sysText, "Respond immediately and concisely") {
		t.Fatalf("expected fast directive in SystemInstruction, got %q", sysText)
	}

	// 3. Global FastMode enabled
	SetGlobalFastMode(true)
	reqUnderGlobal := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	gReq, _ = ConvertOpenAIToGoogle(reqUnderGlobal)
	tc, ok = gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["thinkingBudget"] != 0 {
		t.Fatalf("expected thinkingBudget == 0 when GlobalFastMode is true, got %v", tc)
	}

	// 4. Global FastMode overridden by request FastMode=false
	fastFalse := false
	reqOverride := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		FastMode: &fastFalse,
	}
	gReq, _ = ConvertOpenAIToGoogle(reqOverride)
	tc, ok = gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["includeThoughts"] != true {
		t.Fatalf("expected includeThoughts == true when request explicitly sets FastMode=false, got %v", tc)
	}

	SetGlobalFastMode(false)

	// 5. Reasoning effort 'none'
	reqReasoningNone := &OpenAIChatRequest{
		Model:           "gemini-3.8-flash",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: "none",
	}
	gReq, _ = ConvertOpenAIToGoogle(reqReasoningNone)
	tc, ok = gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["thinkingBudget"] != 0 {
		t.Fatalf("expected thinkingBudget == 0 for reasoning_effort='none', got %v", tc)
	}

	// 6. Fast alias model 'gemini-3.8-flash-fast'
	reqAliasFast := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash-fast",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	gReq, _ = ConvertOpenAIToGoogle(reqAliasFast)
	tc, ok = gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["thinkingBudget"] != 0 {
		t.Fatalf("expected thinkingBudget == 0 for model 'gemini-3.8-flash-fast', got %v", tc)
	}

	// 7. Claude request with fast_mode
	cReqFast := &ClaudeRequest{
		Model:    "claude-3-7-sonnet",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
		FastMode: &fastTrue,
	}
	gReqClaude, _ := ConvertClaudeToGoogle(cReqFast)
	tc, ok = gReqClaude.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["thinkingBudget"] != 0 {
		t.Fatalf("expected thinkingBudget == 0 for Claude request with FastMode=true, got %v", tc)
	}
}

func TestHermesReasoningEffortLevels(t *testing.T) {
	cases := []struct {
		effort         string
		expectedModel  string
		expectedBudget int
	}{
		{"minimal", "gemini-3.8-flash-low", 0},
		{"none", "gemini-3.8-flash-low", 0},
		{"low", "gemini-3.8-flash-low", 0},
		{"med", "gemini-3.8-flash-medium", 512},
		{"medium", "gemini-3.8-flash-medium", 512},
		{"high", "gemini-3.8-flash-high", 8192},
		{"xhigh", "gemini-3.8-flash-high", -1},
		{"max", "gemini-3.8-flash-high", -1},
		{"ultra", "gemini-3.8-flash-high", -1},
	}

	for _, c := range cases {
		t.Run("effort_"+c.effort, func(t *testing.T) {
			req := &OpenAIChatRequest{
				Model:           "gemini-3.8-flash",
				Messages:        []OpenAIMessage{{Role: "user", Content: "test"}},
				ReasoningEffort: c.effort,
			}
			gReq, resolved := ConvertOpenAIToGoogle(req)
			if gReq.Model != c.expectedModel {
				t.Errorf("effort %s: expected model %s, got %s (resolved %s)", c.effort, c.expectedModel, gReq.Model, resolved)
			}
			tc, ok := gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
			if !ok {
				t.Fatalf("effort %s: missing thinkingConfig", c.effort)
			}
			if tc["thinkingBudget"] != c.expectedBudget {
				t.Errorf("effort %s: expected budget %d, got %v", c.effort, c.expectedBudget, tc["thinkingBudget"])
			}
			if c.expectedBudget != 0 && tc["includeThoughts"] != true {
				t.Errorf("effort %s: expected includeThoughts == true", c.effort)
			}
		})
	}
}

func TestReasoningObjectUnpacking(t *testing.T) {
	req := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "test"}},
		Reasoning: map[string]any{
			"effort": "high",
		},
	}
	gReq, _ := ConvertOpenAIToGoogle(req)
	if gReq.Model != "gemini-3.8-flash-high" {
		t.Errorf("expected model gemini-3.8-flash-high, got %s", gReq.Model)
	}
	tc, ok := gReq.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tc["includeThoughts"] != true || tc["thinkingBudget"] != 8192 {
		t.Errorf("expected includeThoughts:true and thinkingBudget:8192, got %v", tc)
	}

	// 1. Explicit reasoning enabled: false
	reqDisabled := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "test"}},
		Reasoning: map[string]any{
			"enabled": false,
		},
	}
	gReqDisabled, _ := ConvertOpenAIToGoogle(reqDisabled)
	if gReqDisabled.Model != "gemini-3.8-flash-low" {
		t.Errorf("expected model gemini-3.8-flash-low for reasoning.enabled=false, got %s", gReqDisabled.Model)
	}
	tcDis, ok := gReqDisabled.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok || tcDis["thinkingBudget"] != 0 {
		t.Errorf("expected thinkingBudget:0 for reasoning.enabled=false, got %v", tcDis)
	}

	// 2. Boolean reasoning: false
	reqBoolFalse := &OpenAIChatRequest{
		Model:     "gemini-3.8-flash",
		Messages:  []OpenAIMessage{{Role: "user", Content: "test"}},
		Reasoning: false,
	}
	gReqBool, _ := ConvertOpenAIToGoogle(reqBoolFalse)
	if gReqBool.Model != "gemini-3.8-flash-low" {
		t.Errorf("expected model gemini-3.8-flash-low for reasoning=false, got %s", gReqBool.Model)
	}

	// 3. Thinking object with type: disabled
	reqThinkDis := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "test"}},
		Thinking: map[string]any{
			"type": "disabled",
		},
	}
	gReqThinkDis, _ := ConvertOpenAIToGoogle(reqThinkDis)
	if gReqThinkDis.Model != "gemini-3.8-flash-low" {
		t.Errorf("expected model gemini-3.8-flash-low for thinking.type=disabled, got %s", gReqThinkDis.Model)
	}

	// 4. ExtraBody with reasoning
	reqExtra := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "test"}},
		ExtraBody: map[string]any{
			"reasoning": map[string]any{"enabled": false},
		},
	}
	gReqExtra, _ := ConvertOpenAIToGoogle(reqExtra)
	if gReqExtra.Model != "gemini-3.8-flash-low" {
		t.Errorf("expected model gemini-3.8-flash-low for extra_body reasoning disabled, got %s", gReqExtra.Model)
	}

	// 5. Reasoning max_tokens: 0
	reqMax0 := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "test"}},
		Reasoning: map[string]any{
			"max_tokens": float64(0),
		},
	}
	gReqMax0, _ := ConvertOpenAIToGoogle(reqMax0)
	if gReqMax0.Model != "gemini-3.8-flash-low" {
		t.Errorf("expected model gemini-3.8-flash-low for max_tokens=0, got %s", gReqMax0.Model)
	}

	// 6. Assistant message with reasoning_content preserved in Google contents
	reqHistory := &OpenAIChatRequest{
		Model: "gemini-3.8-flash",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!", ReasoningContent: "Deliberating greeting"},
			{Role: "user", Content: "How are you?"},
		},
	}
	gReqHist, _ := ConvertOpenAIToGoogle(reqHistory)
	if len(gReqHist.Request.Contents) < 2 {
		t.Fatalf("expected at least 2 contents turns, got %d", len(gReqHist.Request.Contents))
	}
	foundThought := false
	for _, c := range gReqHist.Request.Contents {
		if c.Role == "model" {
			for _, p := range c.Parts {
				if p.Thought && p.Text == "Deliberating greeting" {
					foundThought = true
				}
			}
		}
	}
	if !foundThought {
		t.Errorf("expected reasoning_content to be translated into thought part in model turn")
	}

	// 7. Claude disabled thinking routes to -low and injects fastDirective
	cReqDisabled := &ClaudeRequest{
		Model:    "claude-3-7-sonnet",
		Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
		Thinking: map[string]any{"type": "disabled"},
	}
	gReqClaudeDis, resolvedClaude := ConvertClaudeToGoogle(cReqDisabled)
	if resolvedClaude != "gemini-3.8-flash-low" {
		t.Errorf("expected claude disabled thinking to resolve to gemini-3.8-flash-low, got %s", resolvedClaude)
	}
	if gReqClaudeDis.Model != "gemini-3.8-flash-low" {
		t.Errorf("expected google model gemini-3.8-flash-low, got %s", gReqClaudeDis.Model)
	}
	tcClaudeDis := gReqClaudeDis.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if tcClaudeDis["thinkingBudget"] != 0 {
		t.Errorf("expected thinkingBudget 0, got %v", tcClaudeDis)
	}
}

func TestOpenAIStopSequences(t *testing.T) {
	// String stop
	reqStr := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		Stop:     "\n\n",
	}
	gReqStr, _ := ConvertOpenAIToGoogle(reqStr)
	stops, ok := gReqStr.Request.GenerationConfig["stopSequences"].([]string)
	if !ok || len(stops) != 1 || stops[0] != "\n\n" {
		t.Errorf("expected stopSequences [\n\n], got %v", gReqStr.Request.GenerationConfig["stopSequences"])
	}

	// Slice stop
	reqSlice := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		Stop:     []any{"END", "STOP"},
	}
	gReqSlice, _ := ConvertOpenAIToGoogle(reqSlice)
	stopsSlice, ok := gReqSlice.Request.GenerationConfig["stopSequences"].([]string)
	if !ok || len(stopsSlice) != 2 || stopsSlice[0] != "END" || stopsSlice[1] != "STOP" {
		t.Errorf("expected stopSequences [END, STOP], got %v", gReqSlice.Request.GenerationConfig["stopSequences"])
	}
}

func TestOpenAIResponseFormat(t *testing.T) {
	// json_object
	reqObj := &OpenAIChatRequest{
		Model:          "gemini-3.8-flash",
		Messages:       []OpenAIMessage{{Role: "user", Content: "hi"}},
		ResponseFormat: map[string]any{"type": "json_object"},
	}
	gReqObj, _ := ConvertOpenAIToGoogle(reqObj)
	if mime := gReqObj.Request.GenerationConfig["responseMimeType"]; mime != "application/json" {
		t.Errorf("expected responseMimeType application/json, got %v", mime)
	}

	// json_schema
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city": map[string]any{"type": "string"},
		},
	}
	reqSchema := &OpenAIChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"schema": schema,
			},
		},
	}
	gReqSchema, _ := ConvertOpenAIToGoogle(reqSchema)
	if mime := gReqSchema.Request.GenerationConfig["responseMimeType"]; mime != "application/json" {
		t.Errorf("expected responseMimeType application/json, got %v", mime)
	}
	if gReqSchema.Request.GenerationConfig["responseSchema"] == nil {
		t.Errorf("expected responseSchema to be set")
	}
}

func TestOpenAIToolChoice(t *testing.T) {
	cases := []struct {
		name         string
		toolChoice   any
		expectedMode string
		allowedFns   []string
	}{
		{"none", "none", "NONE", nil},
		{"auto", "auto", "AUTO", nil},
		{"required", "required", "ANY", nil},
		{"named_function", map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "lookup_stock",
			},
		}, "ANY", []string{"lookup_stock"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &OpenAIChatRequest{
				Model:      "gemini-3.8-flash",
				Messages:   []OpenAIMessage{{Role: "user", Content: "hi"}},
				ToolChoice: tc.toolChoice,
			}
			gReq, _ := ConvertOpenAIToGoogle(req)
			tcMap, ok := gReq.Request.ToolConfig.(map[string]any)
			if !ok {
				t.Fatalf("expected ToolConfig map, got %v", gReq.Request.ToolConfig)
			}
			fcc, ok := tcMap["functionCallingConfig"].(map[string]any)
			if !ok {
				t.Fatalf("expected functionCallingConfig map, got %v", tcMap)
			}
			if fcc["mode"] != tc.expectedMode {
				t.Errorf("expected mode %s, got %v", tc.expectedMode, fcc["mode"])
			}
			if len(tc.allowedFns) > 0 {
				fns, ok := fcc["allowedFunctionNames"].([]string)
				if !ok || len(fns) != len(tc.allowedFns) || fns[0] != tc.allowedFns[0] {
					t.Errorf("expected allowedFunctionNames %v, got %v", tc.allowedFns, fcc["allowedFunctionNames"])
				}
			}
		})
	}
}

func TestClaudeStopSequencesAndToolChoice(t *testing.T) {
	req := &ClaudeRequest{
		Model:         "claude-3-7-sonnet",
		Messages:      []ClaudeMessage{{Role: "user", Content: "hi"}},
		StopSequences: []string{"Human:", "Assistant:"},
		ToolChoice: map[string]any{
			"type": "tool",
			"name": "get_weather",
		},
	}
	gReq, _ := ConvertClaudeToGoogle(req)
	stops, ok := gReq.Request.GenerationConfig["stopSequences"].([]string)
	if !ok || len(stops) != 2 || stops[0] != "Human:" || stops[1] != "Assistant:" {
		t.Errorf("expected stopSequences, got %v", gReq.Request.GenerationConfig["stopSequences"])
	}

	tcMap, ok := gReq.Request.ToolConfig.(map[string]any)
	if !ok {
		t.Fatalf("expected ToolConfig map, got %v", gReq.Request.ToolConfig)
	}
	fcc, ok := tcMap["functionCallingConfig"].(map[string]any)
	if !ok || fcc["mode"] != "ANY" {
		t.Errorf("expected mode ANY, got %v", fcc)
	}
	fns, ok := fcc["allowedFunctionNames"].([]string)
	if !ok || len(fns) != 1 || fns[0] != "get_weather" {
		t.Errorf("expected allowedFunctionNames [get_weather], got %v", fcc["allowedFunctionNames"])
	}
}

func TestOpenAITelemetry(t *testing.T) {
	// Non-streaming response telemetry
	respBytes := FormatOpenAINonStream("cmpl-123", "gemini-3.8-flash", "hello", "reasoning steps", nil, nil)
	var resp map[string]any
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("failed to unmarshal non-stream response: %v", err)
	}

	if fp, ok := resp["system_fingerprint"].(string); !ok || fp != "fp_antiapi" {
		t.Errorf("expected system_fingerprint 'fp_antiapi', got %v", resp["system_fingerprint"])
	}

	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices array")
	}
	choice0, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("expected choice map")
	}
	if lp, exists := choice0["logprobs"]; !exists || lp != nil {
		t.Errorf("expected logprobs null, got %v", lp)
	}
	if ref, exists := choice0["refusal"]; !exists || ref != nil {
		t.Errorf("expected refusal null in choice, got %v", ref)
	}

	// Streaming chunk telemetry
	chunkStr := FormatOpenAIChunk("cmpl-123", "gemini-3.8-flash", "delta text", "", "", nil)
	if !strings.Contains(chunkStr, `"system_fingerprint":"fp_antiapi"`) {
		t.Errorf("expected streaming chunk to contain system_fingerprint 'fp_antiapi', got %s", chunkStr)
	}
}

func TestClaudeThinkingAndCacheTokens(t *testing.T) {
	u := &GoogleUsageMetadata{
		PromptTokenCount:        100,
		CandidatesTokenCount:    40,
		ThoughtsTokenCount:      20,
		TotalTokenCount:         160,
		CachedContentTokenCount: 80,
	}

	respBytes := FormatClaudeNonStream("msg-456", "claude-3-7-sonnet", "final answer", "detailed thinking", nil, u)
	var resp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			Thinking string `json:"thinking,omitempty"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("failed to unmarshal Claude response: %v", err)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "detailed thinking" {
		t.Errorf("expected block 0 to be thinking with 'detailed thinking', got %+v", resp.Content[0])
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "final answer" {
		t.Errorf("expected block 1 to be text with 'final answer', got %+v", resp.Content[1])
	}

	if resp.Usage.CacheReadInputTokens != 80 {
		t.Errorf("expected cache_read_input_tokens 80, got %d", resp.Usage.CacheReadInputTokens)
	}
	if resp.Usage.CacheCreationInputTokens != 0 {
		t.Errorf("expected cache_creation_input_tokens 0, got %d", resp.Usage.CacheCreationInputTokens)
	}
	if resp.Usage.OutputTokens != 60 { // 40 candidates + 20 thoughts
		t.Errorf("expected output_tokens 60, got %d", resp.Usage.OutputTokens)
	}
}
