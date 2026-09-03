package translator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"antiapi/auth"
	"antiapi/models"
)

func getMaxTokensForModel(model string, requested *int) int {
	maxLimit := 64000
	if strings.Contains(model, "gpt") {
		maxLimit = 16384
	} else if strings.Contains(model, "gemini") {
		maxLimit = 64000
	} else if strings.Contains(model, "claude") {
		maxLimit = 64000
	}
	if requested != nil && *requested > 0 && *requested <= maxLimit {
		return *requested
	}
	return maxLimit
}

// OpenAI Data Structures
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"` // string or []any for multimodal
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type OpenAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Tools       []any           `json:"tools,omitempty"`
}

// Anthropic Data Structures
type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []any
}

type ClaudeRequest struct {
	Model     string          `json:"model"`
	Messages  []ClaudeMessage `json:"messages"`
	System    any             `json:"system,omitempty"`
	Stream    bool            `json:"stream"`
	MaxTokens *int            `json:"max_tokens,omitempty"`
}

// Upstream Google Cloud Code Structures
type GooglePart struct {
	Text             string          `json:"text,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	FunctionCall     *GoogleFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *GoogleFuncResp `json:"functionResponse,omitempty"`
}

type GoogleFuncCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	ID   string         `json:"id,omitempty"`
}

type GoogleFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	ID       string         `json:"id,omitempty"`
}

type GoogleContent struct {
	Role  string       `json:"role"`
	Parts []GooglePart `json:"parts"`
}

type GoogleRequest struct {
	Model   string `json:"model"`
	Project string `json:"project"`
	Request struct {
		Contents          []GoogleContent `json:"contents"`
		SystemInstruction *struct {
			Parts []GooglePart `json:"parts"`
		} `json:"systemInstruction,omitempty"`
		GenerationConfig map[string]any `json:"generationConfig,omitempty"`
		Tools            []any          `json:"tools,omitempty"`
	} `json:"request"`
}

func GenerateID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// Global cache for tracking function call names and Google cryptographic thought signatures
type ToolCallInfo struct {
	Name             string
	Args             string
	ThoughtSignature string
}

var (
	toolCallMu     sync.RWMutex
	toolCallMemory = make(map[string]ToolCallInfo)
)

func RecordToolCall(id, name, args, thoughtSig string) {
	if id == "" {
		return
	}
	toolCallMu.Lock()
	defer toolCallMu.Unlock()
	toolCallMemory[id] = ToolCallInfo{
		Name:             name,
		Args:             args,
		ThoughtSignature: thoughtSig,
	}
}

func GetToolCall(id string) (ToolCallInfo, bool) {
	toolCallMu.RLock()
	defer toolCallMu.RUnlock()
	info, ok := toolCallMemory[id]
	return info, ok
}

// ConvertOpenAIToGoogle converts OpenAI request to Google internal payload
func ConvertOpenAIToGoogle(req *OpenAIChatRequest) (*GoogleRequest, string) {
	resolvedModel := models.GetRegistry().ResolveModel(req.Model)
	project := auth.GetManager().GetProjectID()

	gReq := &GoogleRequest{
		Model:   resolvedModel,
		Project: project,
	}

	maxTokens := getMaxTokensForModel(resolvedModel, req.MaxTokens)
	gReq.Request.GenerationConfig = map[string]any{
		"maxOutputTokens": maxTokens,
	}
	if req.Temperature != nil {
		gReq.Request.GenerationConfig["temperature"] = *req.Temperature
	}

	// 1. Process Tools declaration
	if len(req.Tools) > 0 {
		var funcDecls []map[string]any
		for _, t := range req.Tools {
			if tm, ok := t.(map[string]any); ok {
				tType, _ := tm["type"].(string)
				if tType == "function" || tType == "" {
					if fn, ok := tm["function"].(map[string]any); ok {
						decl := map[string]any{
							"name":        fn["name"],
							"description": fn["description"],
						}
						if params, hasParams := fn["parameters"]; hasParams {
							decl["parameters"] = params
						}
						funcDecls = append(funcDecls, decl)
					}
				}
			}
		}
		if len(funcDecls) > 0 {
			gReq.Request.Tools = []any{
				map[string]any{
					"functionDeclarations": funcDecls,
				},
			}
		}
	}

	// 2. Pre-scan messages to map tool_call_id -> function name and cached thought signatures
	callIDToName := make(map[string]string)
	callIDToThought := make(map[string]string)

	for _, msg := range req.Messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" {
				if tc.Function.Name != "" {
					callIDToName[tc.ID] = tc.Function.Name
				}
				if cached, ok := GetToolCall(tc.ID); ok {
					if tc.Function.Name == "" {
						callIDToName[tc.ID] = cached.Name
					}
					if cached.ThoughtSignature != "" {
						callIDToThought[tc.ID] = cached.ThoughtSignature
					}
				}
			}
		}
	}

	var contents []GoogleContent
	var systemParts []GooglePart

	for _, msg := range req.Messages {
		text := extractText(msg.Content)

		if msg.Role == "system" {
			systemParts = append(systemParts, GooglePart{Text: text})
			continue
		}

		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}

		parts := []GooglePart{}
		if text != "" {
			parts = append(parts, GooglePart{Text: text})
		}

		// Handle tool calls from assistant
		for _, tc := range msg.ToolCalls {
			funcName := tc.Function.Name
			if funcName == "" {
				funcName = callIDToName[tc.ID]
			}
			if funcName == "" {
				funcName = "action_tool"
			}

			var args map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = make(map[string]any)
			}

			callID := tc.ID
			if callID == "" {
				callID = GenerateID("call")
			}

			thoughtSig := callIDToThought[callID]
			if thoughtSig == "" {
				if cached, ok := GetToolCall(callID); ok {
					thoughtSig = cached.ThoughtSignature
				}
			}

			// Store in memory
			RecordToolCall(callID, funcName, tc.Function.Arguments, thoughtSig)

			// If we have the authentic thoughtSignature, output native functionCall part
			if thoughtSig != "" {
				parts = append(parts, GooglePart{
					ThoughtSignature: thoughtSig,
					FunctionCall: &GoogleFuncCall{
						Name: funcName,
						Args: args,
						ID:   callID,
					},
				})
			} else {
				// Graceful text representation fallback so Google doesn't fail with "missing thought_signature"
				parts = append(parts, GooglePart{
					Text: fmt.Sprintf("[Assistant called tool `%s` with arguments: %s]", funcName, tc.Function.Arguments),
				})
			}
		}

		// Handle tool response from user/tool
		if msg.Role == "tool" {
			funcName := msg.Name
			if funcName == "" {
				funcName = callIDToName[msg.ToolCallID]
			}
			if funcName == "" {
				if cached, ok := GetToolCall(msg.ToolCallID); ok {
					funcName = cached.Name
				}
			}
			if funcName == "" {
				funcName = "tool_result"
			}

			callID := msg.ToolCallID
			if callID == "" {
				callID = GenerateID("call")
			}

			thoughtSig := callIDToThought[callID]
			if thoughtSig == "" {
				if cached, ok := GetToolCall(callID); ok {
					thoughtSig = cached.ThoughtSignature
				}
			}

			if thoughtSig != "" {
				var respMap map[string]any
				if err := json.Unmarshal([]byte(text), &respMap); err != nil {
					respMap = map[string]any{"result": text}
				}
				parts = append(parts, GooglePart{
					FunctionResponse: &GoogleFuncResp{
						Name:     funcName,
						Response: respMap,
						ID:       callID,
					},
				})
			} else {
				// Text representation fallback
				parts = append(parts, GooglePart{
					Text: fmt.Sprintf("[Tool `%s` returned]:\n%s", funcName, text),
				})
			}
		}

		if len(parts) > 0 {
			contents = append(contents, GoogleContent{
				Role:  role,
				Parts: parts,
			})
		}
	}

	if len(systemParts) > 0 {
		gReq.Request.SystemInstruction = &struct {
			Parts []GooglePart `json:"parts"`
		}{Parts: systemParts}
	}

	// If contents is empty, add a placeholder
	if len(contents) == 0 {
		contents = append(contents, GoogleContent{
			Role:  "user",
			Parts: []GooglePart{{Text: "Hello"}},
		})
	}

	gReq.Request.Contents = contents
	return gReq, resolvedModel
}

// ConvertClaudeToGoogle converts Anthropic request to Google internal payload
func ConvertClaudeToGoogle(req *ClaudeRequest) (*GoogleRequest, string) {
	resolvedModel := models.GetRegistry().ResolveModel(req.Model)
	project := auth.GetManager().GetProjectID()

	gReq := &GoogleRequest{
		Model:   resolvedModel,
		Project: project,
	}

	maxTokens := getMaxTokensForModel(resolvedModel, req.MaxTokens)
	gReq.Request.GenerationConfig = map[string]any{
		"maxOutputTokens": maxTokens,
	}

	if req.System != nil {
		sysText := extractText(req.System)
		if sysText != "" {
			gReq.Request.SystemInstruction = &struct {
				Parts []GooglePart `json:"parts"`
			}{
				Parts: []GooglePart{{Text: sysText}},
			}
		}
	}

	var contents []GoogleContent
	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		text := extractText(msg.Content)
		if text != "" {
			contents = append(contents, GoogleContent{
				Role:  role,
				Parts: []GooglePart{{Text: text}},
			})
		}
	}

	if len(contents) == 0 {
		contents = append(contents, GoogleContent{
			Role:  "user",
			Parts: []GooglePart{{Text: "Hello"}},
		})
	}

	gReq.Request.Contents = contents
	return gReq, resolvedModel
}

func extractText(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		var sb strings.Builder
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return fmt.Sprintf("%v", content)
}

type ParsedChunk struct {
	Text         string
	Thought      string
	FinishReason string
	ToolCalls    []OpenAIToolCall
}

// ParseGoogleSSEChunk extracts text, thought, finish reason, and function calls
func ParseGoogleSSEChunk(raw []byte) (ParsedChunk, error) {
	var chunk struct {
		Response struct {
			Candidates []struct {
				Content struct {
					Role  string       `json:"role"`
					Parts []GooglePart `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
		} `json:"response"`
	}

	res := ParsedChunk{}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return res, err
	}

	if len(chunk.Response.Candidates) > 0 {
		cand := chunk.Response.Candidates[0]
		res.FinishReason = cand.FinishReason

		var textBuilder strings.Builder
		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				textBuilder.WriteString(part.Text)
			}
			if part.ThoughtSignature != "" {
				res.Thought = part.ThoughtSignature
			}
			if part.FunctionCall != nil {
				callID := part.FunctionCall.ID
				if callID == "" {
					callID = GenerateID("call")
				}
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				RecordToolCall(callID, part.FunctionCall.Name, string(argsBytes), part.ThoughtSignature)

				tc := OpenAIToolCall{
					ID:   callID,
					Type: "function",
				}
				tc.Function.Name = part.FunctionCall.Name
				tc.Function.Arguments = string(argsBytes)
				res.ToolCalls = append(res.ToolCalls, tc)
			}
		}
		res.Text = textBuilder.String()
	}

	return res, nil
}

// FormatOpenAIChunk formats an incremental delta for OpenAI streaming client
func FormatOpenAIChunk(id, model, text, finishReason string, toolCalls []OpenAIToolCall) string {
	type Choice struct {
		Index        int            `json:"index"`
		Delta        map[string]any `json:"delta"`
		FinishReason *string        `json:"finish_reason"`
	}
	type Chunk struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []Choice `json:"choices"`
	}

	delta := make(map[string]any)
	if text != "" {
		delta["content"] = text
	}
	if len(toolCalls) > 0 {
		type TCDelta struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		tcDeltas := []TCDelta{}
		for i, tc := range toolCalls {
			t := TCDelta{
				Index: i,
				ID:    tc.ID,
				Type:  "function",
			}
			t.Function.Name = tc.Function.Name
			t.Function.Arguments = tc.Function.Arguments
			tcDeltas = append(tcDeltas, t)
		}
		delta["tool_calls"] = tcDeltas
	}

	var fr *string
	if finishReason != "" {
		mapped := "stop"
		if strings.ToUpper(finishReason) == "MAX_TOKENS" {
			mapped = "length"
		}
		if len(toolCalls) > 0 || finishReason == "tool_calls" {
			mapped = "tool_calls"
		}
		fr = &mapped
	} else if len(toolCalls) > 0 {
		mapped := "tool_calls"
		fr = &mapped
	}

	c := Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: fr,
			},
		},
	}

	bytes, _ := json.Marshal(c)
	return "data: " + string(bytes) + "\n\n"
}

// FormatOpenAINonStream formats full response for non-streaming clients
func FormatOpenAINonStream(id, model, content string, toolCalls []OpenAIToolCall) []byte {
	type Message struct {
		Role      string           `json:"role"`
		Content   *string          `json:"content"`
		ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`
	}
	type Choice struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	type Response struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []Choice `json:"choices"`
	}

	var contentPtr *string
	if content != "" || len(toolCalls) == 0 {
		contentPtr = &content
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	res := Response{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index: 0,
				Message: Message{
					Role:      "assistant",
					Content:   contentPtr,
					ToolCalls: toolCalls,
				},
				FinishReason: finishReason,
			},
		},
	}

	bytes, _ := json.Marshal(res)
	return bytes
}

// FormatClaudeNonStream formats response for Anthropic clients
func FormatClaudeNonStream(id, model, content string) []byte {
	type ContentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type Response struct {
		ID         string         `json:"id"`
		Type       string         `json:"type"`
		Role       string         `json:"role"`
		Content    []ContentBlock `json:"content"`
		Model      string         `json:"model"`
		StopReason string         `json:"stop_reason"`
	}

	res := Response{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Content:    []ContentBlock{{Type: "text", Text: content}},
		Model:      model,
		StopReason: "end_turn",
	}

	bytes, _ := json.Marshal(res)
	return bytes
}
