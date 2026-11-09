package server

import (
	"encoding/json"
	"strings"
	"testing"

	"antiapi/translator"
)

func TestResponsesToChatRequest_StringInput(t *testing.T) {
	r := &ResponsesRequest{
		Model:        "gpt-5",
		Input:        "hello",
		Instructions: "You are helpful.",
		Stream:       true,
	}
	req := responsesToChatRequest(r)
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "You are helpful." {
		t.Errorf("instructions not mapped to system message: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "hello" {
		t.Errorf("input not mapped to user message: %+v", req.Messages[1])
	}
	if !req.Stream {
		t.Error("stream flag lost")
	}
}

func TestResponsesToChatRequest_ItemsInput(t *testing.T) {
	var r ResponsesRequest
	err := json.Unmarshal([]byte(`{
		"model": "gpt-5",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"what is 2+2"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"4"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"NYC\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"},
			{"type":"message","role":"developer","content":"be terse"}
		],
		"reasoning": {"effort":"high"},
		"max_output_tokens": 512
	}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	req := responsesToChatRequest(&r)
	if req.ReasoningEffort != "high" {
		t.Errorf("reasoning effort = %q", req.ReasoningEffort)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 512 {
		t.Errorf("max_output_tokens not mapped: %+v", req.MaxTokens)
	}
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "assistant", "tool", "system"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	if req.Messages[2].ToolCalls[0].ID != "call_1" || req.Messages[2].ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("function_call not mapped: %+v", req.Messages[2].ToolCalls)
	}
	if req.Messages[3].ToolCallID != "call_1" || req.Messages[3].Content != "sunny" {
		t.Errorf("function_call_output not mapped: %+v", req.Messages[3])
	}
}

func TestResponsesToChatRequest_FlatTools(t *testing.T) {
	var r ResponsesRequest
	err := json.Unmarshal([]byte(`{
		"model": "gpt-5",
		"input": "x",
		"tools": [{"type":"function","name":"read_file","description":"Read a file","parameters":{"type":"object"}}],
		"tool_choice": {"type":"function","name":"read_file"}
	}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	req := responsesToChatRequest(&r)
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %+v", req.Tools)
	}
	tool := req.Tools[0].(map[string]any)
	fn, ok := tool["function"].(map[string]any)
	if !ok || fn["name"] != "read_file" {
		t.Errorf("flat tool not normalized: %+v", tool)
	}
	tc, ok := req.ToolChoice.(map[string]any)
	if !ok {
		t.Fatalf("tool_choice = %#v", req.ToolChoice)
	}
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "read_file" {
		t.Errorf("tool_choice not normalized: %+v", tc)
	}
}

func TestResponsesUsageFrom(t *testing.T) {
	u := &translator.GoogleUsageMetadata{
		PromptTokenCount:        100,
		CandidatesTokenCount:    20,
		ThoughtsTokenCount:      30,
		TotalTokenCount:         150,
		CachedContentTokenCount: 40,
	}
	got := responsesUsageFrom(u)
	if got.InputTokens != 100 || got.OutputTokens != 50 || got.TotalTokens != 150 {
		t.Errorf("usage = %+v", got)
	}
	if got.InputTokensDetails.CachedTokens != 40 || got.OutputTokensDetails.ReasoningTokens != 30 {
		t.Errorf("usage details = %+v", got)
	}
}

func TestResponsesOutputItems(t *testing.T) {
	tc := translator.OpenAIToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = "get_weather"
	tc.Function.Arguments = `{"city":"NYC"}`
	items := responsesOutputItems("thought", "answer", []translator.OpenAIToolCall{tc})
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if items[0].(map[string]any)["type"] != "reasoning" ||
		items[1].(map[string]any)["type"] != "message" ||
		items[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("item order wrong: %+v", items)
	}
	fc := items[2].(map[string]any)
	if fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Errorf("function_call item = %+v", fc)
	}
	if !strings.Contains(fc["arguments"].(string), "NYC") {
		t.Errorf("arguments lost: %+v", fc)
	}
}

func TestResponsesContentToChat_TextOnly(t *testing.T) {
	var content any
	if err := json.Unmarshal([]byte(`[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]`), &content); err != nil {
		t.Fatal(err)
	}
	got := responsesContentToChat(content)
	if got != "ab" {
		t.Errorf("got %#v", got)
	}
}
