package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"antiapi/translator"
	"antiapi/upstream"
)

// OpenAI Responses API (/v1/responses) support - the surface Codex CLI and
// newer OpenAI clients speak. Internally normalized to the chat-completions
// pipeline and re-emitted as Responses events/objects.

type ResponsesRequest struct {
	Model              string         `json:"model"`
	Input              any            `json:"input"`
	Instructions       any            `json:"instructions"`
	Stream             bool           `json:"stream"`
	Temperature        *float64       `json:"temperature,omitempty"`
	TopP               *float64       `json:"top_p,omitempty"`
	MaxOutputTokens    *int           `json:"max_output_tokens,omitempty"`
	Tools              []any          `json:"tools,omitempty"`
	ToolChoice         any            `json:"tool_choice,omitempty"`
	Reasoning          map[string]any `json:"reasoning,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	Store              *bool          `json:"store,omitempty"`
	User               string         `json:"user,omitempty"`
}

// responsesToChatRequest normalizes a Responses request into the internal
// chat-completions request shape so the existing pipeline handles it.
func responsesToChatRequest(r *ResponsesRequest) *translator.OpenAIChatRequest {
	req := &translator.OpenAIChatRequest{
		Model:       r.Model,
		Stream:      r.Stream,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		MaxTokens:   r.MaxOutputTokens,
		User:        r.User,
	}

	if r.Reasoning != nil {
		if effort, ok := r.Reasoning["effort"].(string); ok {
			req.ReasoningEffort = effort
		}
	}

	for _, t := range r.Tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		tType, _ := tm["type"].(string)
		if tType != "function" {
			continue
		}
		// Responses flattens name/description/parameters to the top level
		if _, nested := tm["function"]; !nested {
			req.Tools = append(req.Tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tm["name"],
					"description": tm["description"],
					"parameters":  tm["parameters"],
				},
			})
		} else {
			req.Tools = append(req.Tools, tm)
		}
	}

	switch tc := r.ToolChoice.(type) {
	case string:
		req.ToolChoice = tc
	case map[string]any:
		if tcType, _ := tc["type"].(string); tcType == "function" {
			if name, ok := tc["name"].(string); ok {
				req.ToolChoice = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		} else if tcType != "" {
			req.ToolChoice = tcType
		}
	}

	if r.Instructions != nil {
		if text := responsesText(r.Instructions); text != "" {
			req.Messages = append(req.Messages, translator.OpenAIMessage{Role: "system", Content: text})
		}
	}

	switch input := r.Input.(type) {
	case string:
		if input != "" {
			req.Messages = append(req.Messages, translator.OpenAIMessage{Role: "user", Content: input})
		}
	case []any:
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := m["type"].(string)
			switch itemType {
			case "message", "":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				req.Messages = append(req.Messages, translator.OpenAIMessage{
					Role:    role,
					Content: responsesContentToChat(m["content"]),
				})
			case "function_call":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				tc := translator.OpenAIToolCall{ID: callID, Type: "function"}
				tc.Function.Name = name
				tc.Function.Arguments = args
				req.Messages = append(req.Messages, translator.OpenAIMessage{
					Role:      "assistant",
					ToolCalls: []translator.OpenAIToolCall{tc},
				})
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				var output string
				switch out := m["output"].(type) {
				case string:
					output = out
				default:
					b, _ := json.Marshal(out)
					output = string(b)
				}
				req.Messages = append(req.Messages, translator.OpenAIMessage{
					Role:       "tool",
					ToolCallID: callID,
					Content:    output,
				})
			case "reasoning":
				// Prior reasoning turns: replay as assistant reasoning content.
				if summary, ok := m["summary"].([]any); ok {
					var sb strings.Builder
					for _, part := range summary {
						if pm, ok := part.(map[string]any); ok {
							if t, ok := pm["text"].(string); ok {
								sb.WriteString(t)
							}
						}
					}
					if sb.Len() > 0 {
						req.Messages = append(req.Messages, translator.OpenAIMessage{
							Role:             "assistant",
							ReasoningContent: sb.String(),
							Content:          "",
						})
					}
				}
			}
		}
	}
	return req
}

// responsesText extracts plain text from a string-or-parts field (instructions).
func responsesText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
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
	return ""
}

// responsesContentToChat converts Responses message content into the internal
// chat content shape (string or parts array with text/image_url).
func responsesContentToChat(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []any
	var textOnly strings.Builder
	onlyText := true
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		switch t {
		case "input_text", "output_text", "text":
			text, _ := m["text"].(string)
			textOnly.WriteString(text)
			parts = append(parts, map[string]any{"type": "text", "text": text})
		case "input_image":
			urlStr, _ := m["image_url"].(string)
			if urlStr != "" {
				onlyText = false
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": urlStr},
				})
			}
		}
	}
	if onlyText {
		return textOnly.String()
	}
	return parts
}

type responsesUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	InputTokensDetails  struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func responsesUsageFrom(u *translator.GoogleUsageMetadata) *responsesUsage {
	if u == nil {
		return nil
	}
	out := &responsesUsage{
		InputTokens:  u.PromptTokenCount,
		OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		TotalTokens:  u.TotalTokenCount,
	}
	if out.OutputTokens == 0 && u.TotalTokenCount > u.PromptTokenCount {
		out.OutputTokens = u.TotalTokenCount - u.PromptTokenCount
	}
	out.InputTokensDetails.CachedTokens = u.CachedContentTokenCount
	out.OutputTokensDetails.ReasoningTokens = u.ThoughtsTokenCount
	return out
}

func responsesOutputItems(reasoning, text string, toolCalls []translator.OpenAIToolCall) []any {
	var items []any
	if reasoning != "" {
		items = append(items, map[string]any{
			"type": "reasoning",
			"id":   translator.GenerateID("rs"),
			"summary": []any{
				map[string]any{"type": "summary_text", "text": reasoning},
			},
		})
	}
	if text != "" || len(toolCalls) == 0 {
		items = append(items, map[string]any{
			"type":   "message",
			"id":     translator.GenerateID("msg"),
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				},
			},
		})
	}
	for _, tc := range toolCalls {
		items = append(items, map[string]any{
			"type":      "function_call",
			"id":        translator.GenerateID("fc"),
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
			"status":    "completed",
		})
	}
	return items
}

func responsesObject(id, model, status string, output []any, u *translator.GoogleUsageMetadata) map[string]any {
	obj := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
		"error":      nil,
	}
	if output == nil {
		obj["output"] = []any{}
	}
	if usage := responsesUsageFrom(u); usage != nil {
		obj["usage"] = usage
	}
	return obj
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	flusher.Flush()
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	applyAdaptiveJitter(r.Context(), len(bodyBytes))

	var rreq ResponsesRequest
	if err := json.Unmarshal(bodyBytes, &rreq); err != nil {
		writeJSONError(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "Invalid JSON payload: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	req := responsesToChatRequest(&rreq)
	if isFastModeRequest(r) && req.FastMode == nil {
		fast := true
		req.FastMode = &fast
	}

	gReq, resolvedModel := translator.ConvertOpenAIToGoogle(req)
	log.Printf("[Responses] Routing request: client_model='%s' -> antigravity_model='%s'", rreq.Model, resolvedModel)

	sessID := r.Header.Get("X-Session-ID")
	if sessID == "" {
		sessID = rreq.User
	}
	if sessID == "" && rreq.Metadata != nil {
		if uid, ok := rreq.Metadata["user_id"].(string); ok {
			sessID = uid
		}
	}

	eventCh, err := s.upstreamClient.StreamGenerate(r.Context(), gReq, sessID)
	if err != nil {
		statusCode := http.StatusBadGateway
		if strings.Contains(err.Error(), "(400)") {
			statusCode = http.StatusBadRequest
		}
		writeJSONError(w, statusCode, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	respID := translator.GenerateID("resp")

	if rreq.Stream {
		s.streamResponses(w, r, eventCh, respID, resolvedModel)
		return
	}

	// Non-streaming: collect the whole upstream stream, then emit one object.
	var fullText, fullReasoning strings.Builder
	var allToolCalls []translator.OpenAIToolCall
	var lastUsage *translator.GoogleUsageMetadata
	var streamErr error
	for event := range eventCh {
		if event.Err != nil {
			streamErr = event.Err
			break
		}
		parsed, _ := translator.ParseGoogleSSEChunk(event.Data)
		if parsed.Usage != nil {
			lastUsage = parsed.Usage
		}
		fullText.WriteString(parsed.Text)
		fullReasoning.WriteString(parsed.ReasoningText)
		if len(parsed.ToolCalls) > 0 {
			allToolCalls = append(allToolCalls, parsed.ToolCalls...)
		}
	}

	if streamErr != nil && fullText.Len() == 0 && fullReasoning.Len() == 0 && len(allToolCalls) == 0 {
		writeJSONError(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream stream failed: " + streamErr.Error(), "type": "server_error"},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	obj := responsesObject(respID, resolvedModel, "completed",
		responsesOutputItems(fullReasoning.String(), fullText.String(), allToolCalls), lastUsage)
	json.NewEncoder(w).Encode(obj)
}

// streamResponses emits the Responses SSE event sequence Codex CLI expects:
// response.created -> output_item/content_part lifecycle -> response.completed.
func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, eventCh <-chan upstream.SSEEvent, respID, resolvedModel string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": "Streaming unsupported", "type": "server_error"},
		})
		return
	}

	writeSSE(w, flusher, "response.created", map[string]any{
		"type":     "response.created",
		"response": responsesObject(respID, resolvedModel, "in_progress", nil, nil),
	})
	writeSSE(w, flusher, "response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": responsesObject(respID, resolvedModel, "in_progress", nil, nil),
	})

	var fullText, fullReasoning strings.Builder
	var allToolCalls []translator.OpenAIToolCall
	var lastUsage *translator.GoogleUsageMetadata

	outputIndex := 0
	reasoningItemID := translator.GenerateID("rs")
	reasoningOpen := false
	msgItemID := translator.GenerateID("msg")
	msgOpen := false
	textPartOpen := false

	closeReasoning := func() {
		if !reasoningOpen {
			return
		}
		summary := map[string]any{"type": "summary_text", "text": fullReasoning.String()}
		writeSSE(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":          "response.reasoning_summary_text.done",
			"item_id":       reasoningItemID,
			"output_index":  outputIndex,
			"summary_index": 0,
			"text":          fullReasoning.String(),
		})
		writeSSE(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":          "response.reasoning_summary_part.done",
			"item_id":       reasoningItemID,
			"output_index":  outputIndex,
			"summary_index": 0,
			"part":          summary,
		})
		writeSSE(w, flusher, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item": map[string]any{
				"type":    "reasoning",
				"id":      reasoningItemID,
				"summary": []any{summary},
			},
		})
		reasoningOpen = false
		outputIndex++
	}

	openMessage := func() {
		if msgOpen {
			return
		}
		writeSSE(w, flusher, "response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": outputIndex,
			"item": map[string]any{
				"type":    "message",
				"id":      msgItemID,
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		msgOpen = true
		writeSSE(w, flusher, "response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       msgItemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		textPartOpen = true
	}

	for event := range eventCh {
		if event.Err != nil {
			break
		}
		parsed, err := translator.ParseGoogleSSEChunk(event.Data)
		if err != nil {
			continue
		}
		if parsed.Usage != nil {
			lastUsage = parsed.Usage
		}

		if parsed.ReasoningText != "" {
			if !reasoningOpen {
				reasoningOpen = true
				writeSSE(w, flusher, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item":         map[string]any{"type": "reasoning", "id": reasoningItemID, "summary": []any{}},
				})
				writeSSE(w, flusher, "response.reasoning_summary_part.added", map[string]any{
					"type":          "response.reasoning_summary_part.added",
					"item_id":       reasoningItemID,
					"output_index":  outputIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": ""},
				})
			}
			fullReasoning.WriteString(parsed.ReasoningText)
			writeSSE(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
				"type":          "response.reasoning_summary_text.delta",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"delta":         parsed.ReasoningText,
			})
		}

		if parsed.Text != "" {
			if reasoningOpen {
				closeReasoning()
			}
			openMessage()
			fullText.WriteString(parsed.Text)
			writeSSE(w, flusher, "response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       msgItemID,
				"output_index":  outputIndex,
				"content_index": 0,
				"delta":         parsed.Text,
			})
		}

		for _, tc := range parsed.ToolCalls {
			if reasoningOpen {
				closeReasoning()
			}
			if textPartOpen {
				writeSSE(w, flusher, "response.output_text.done", map[string]any{
					"type":          "response.output_text.done",
					"item_id":       msgItemID,
					"output_index":  outputIndex,
					"content_index": 0,
					"text":          fullText.String(),
				})
				writeSSE(w, flusher, "response.content_part.done", map[string]any{
					"type":          "response.content_part.done",
					"item_id":       msgItemID,
					"output_index":  outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": fullText.String(), "annotations": []any{}},
				})
				writeSSE(w, flusher, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":   "message",
						"id":     msgItemID,
						"status": "completed",
						"role":   "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": fullText.String(), "annotations": []any{}}},
					},
				})
				textPartOpen = false
				msgOpen = false
				outputIndex++
			}
			fcItemID := translator.GenerateID("fc")
			writeSSE(w, flusher, "response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]any{
					"type":      "function_call",
					"id":        fcItemID,
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": "",
					"status":    "in_progress",
				},
			})
			writeSSE(w, flusher, "response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      fcItemID,
				"output_index": outputIndex,
				"delta":        tc.Function.Arguments,
			})
			writeSSE(w, flusher, "response.function_call_arguments.done", map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      fcItemID,
				"output_index": outputIndex,
				"arguments":    tc.Function.Arguments,
			})
			writeSSE(w, flusher, "response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]any{
					"type":      "function_call",
					"id":        fcItemID,
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
					"status":    "completed",
				},
			})
			outputIndex++
			allToolCalls = append(allToolCalls, tc)
		}
	}

	if reasoningOpen {
		closeReasoning()
	}
	if textPartOpen {
		writeSSE(w, flusher, "response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       msgItemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"text":          fullText.String(),
		})
		writeSSE(w, flusher, "response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       msgItemID,
			"output_index":  outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": fullText.String(), "annotations": []any{}},
		})
	}
	if msgOpen {
		writeSSE(w, flusher, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item": map[string]any{
				"type":   "message",
				"id":     msgItemID,
				"status": "completed",
				"role":   "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": fullText.String(), "annotations": []any{}}},
			},
		})
	}

	final := responsesObject(respID, resolvedModel, "completed",
		responsesOutputItems(fullReasoning.String(), fullText.String(), allToolCalls), lastUsage)
	writeSSE(w, flusher, "response.completed", map[string]any{
		"type":     "response.completed",
		"response": final,
	})
}
