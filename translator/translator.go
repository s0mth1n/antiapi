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

func maxTokensForModel(model string, requested *int) int {
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
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Tools       []any           `json:"tools,omitempty"`
}

// Anthropic Data Structures
type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []any
}

type ClaudeRequest struct {
	Model       string          `json:"model"`
	Messages    []ClaudeMessage `json:"messages"`
	System      any             `json:"system,omitempty"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Tools       []any           `json:"tools,omitempty"`
}

// Upstream Google Cloud Code Structures
type GoogleInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GooglePart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *GoogleInlineData `json:"inlineData,omitempty"`
	FunctionCall     *GoogleFuncCall   `json:"functionCall,omitempty"`
	FunctionResponse *GoogleFuncResp   `json:"functionResponse,omitempty"`
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

// PackCallID encodes thoughtSignature into the callID if present so stateless clients preserve it across restarts.
func PackCallID(callID, thoughtSig string) string {
	if thoughtSig == "" {
		return callID
	}
	encodedSig := hex.EncodeToString([]byte(thoughtSig))
	return callID + "_ts_" + encodedSig
}

// UnpackCallID extracts the callID and any packed thoughtSignature.
func UnpackCallID(rawID string) (string, string) {
	if idx := strings.LastIndex(rawID, "_ts_"); idx != -1 {
		baseID := rawID[:idx]
		sigHex := rawID[idx+4:]
		if sigBytes, err := hex.DecodeString(sigHex); err == nil && len(sigBytes) > 0 {
			return baseID, string(sigBytes)
		}
		return rawID, ""
	}
	return rawID, ""
}

func RecordToolCall(id, name, args, thoughtSig string) {
	if id == "" {
		return
	}
	baseID, unpackedSig := UnpackCallID(id)
	if thoughtSig == "" && unpackedSig != "" {
		thoughtSig = unpackedSig
	}
	toolCallMu.Lock()
	defer toolCallMu.Unlock()
	toolCallMemory[id] = ToolCallInfo{
		Name:             name,
		Args:             args,
		ThoughtSignature: thoughtSig,
	}
	if baseID != id {
		toolCallMemory[baseID] = ToolCallInfo{
			Name:             name,
			Args:             args,
			ThoughtSignature: thoughtSig,
		}
	}
}

func GetToolCall(id string) (ToolCallInfo, bool) {
	baseID, unpackedSig := UnpackCallID(id)
	toolCallMu.RLock()
	defer toolCallMu.RUnlock()
	info, ok := toolCallMemory[id]
	if !ok && baseID != id {
		info, ok = toolCallMemory[baseID]
	}
	if ok && info.ThoughtSignature == "" && unpackedSig != "" {
		info.ThoughtSignature = unpackedSig
	}
	if !ok && unpackedSig != "" {
		return ToolCallInfo{ThoughtSignature: unpackedSig}, true
	}
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

	maxTokens := maxTokensForModel(resolvedModel, req.MaxTokens)
	gReq.Request.GenerationConfig = map[string]any{
		"maxOutputTokens": maxTokens,
	}
	if req.Temperature != nil {
		gReq.Request.GenerationConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gReq.Request.GenerationConfig["topP"] = *req.TopP
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
				baseID, unpackedSig := UnpackCallID(tc.ID)
				if tc.Function.Name != "" {
					callIDToName[tc.ID] = tc.Function.Name
					if baseID != tc.ID {
						callIDToName[baseID] = tc.Function.Name
					}
				}
				if unpackedSig != "" {
					callIDToThought[tc.ID] = unpackedSig
					if baseID != tc.ID {
						callIDToThought[baseID] = unpackedSig
					}
				}
				if cached, ok := GetToolCall(tc.ID); ok {
					if tc.Function.Name == "" {
						callIDToName[tc.ID] = cached.Name
						if baseID != tc.ID {
							callIDToName[baseID] = cached.Name
						}
					}
					if cached.ThoughtSignature != "" && callIDToThought[tc.ID] == "" {
						callIDToThought[tc.ID] = cached.ThoughtSignature
						if baseID != tc.ID {
							callIDToThought[baseID] = cached.ThoughtSignature
						}
					}
				} else if baseID != tc.ID {
					if cached, ok := GetToolCall(baseID); ok {
						if tc.Function.Name == "" {
							callIDToName[tc.ID] = cached.Name
							callIDToName[baseID] = cached.Name
						}
						if cached.ThoughtSignature != "" && callIDToThought[tc.ID] == "" {
							callIDToThought[tc.ID] = cached.ThoughtSignature
							callIDToThought[baseID] = cached.ThoughtSignature
						}
					}
				}
			}
		}
	}

	var contents []GoogleContent
	var systemParts []GooglePart

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemParts = append(systemParts, GooglePart{Text: extractText(msg.Content)})
			continue
		}

		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}

		var parts []GooglePart
		if msg.Role != "tool" {
			parts = append(parts, extractOpenAIParts(msg.Content)...)
		}

		// Handle tool calls from assistant
		for _, tc := range msg.ToolCalls {
			baseID, unpackedSig := UnpackCallID(tc.ID)
			funcName := tc.Function.Name
			if funcName == "" {
				funcName = callIDToName[tc.ID]
			}
			if funcName == "" && baseID != tc.ID {
				funcName = callIDToName[baseID]
			}
			if funcName == "" {
				funcName = "action_tool"
			}

			var args map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = make(map[string]any)
			}

			callID := baseID
			if callID == "" {
				callID = GenerateID("call")
			}

			thoughtSig := callIDToThought[tc.ID]
			if thoughtSig == "" && baseID != tc.ID {
				thoughtSig = callIDToThought[baseID]
			}
			if thoughtSig == "" && unpackedSig != "" {
				thoughtSig = unpackedSig
			}
			if thoughtSig == "" {
				if cached, ok := GetToolCall(tc.ID); ok {
					thoughtSig = cached.ThoughtSignature
				}
			}
			if thoughtSig == "" && baseID != tc.ID {
				if cached, ok := GetToolCall(baseID); ok {
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
			baseID, unpackedSig := UnpackCallID(msg.ToolCallID)
			funcName := callIDToName[msg.ToolCallID]
			if funcName == "" && baseID != msg.ToolCallID {
				funcName = callIDToName[baseID]
			}
			if funcName == "" {
				if cached, ok := GetToolCall(msg.ToolCallID); ok {
					funcName = cached.Name
				}
			}
			if funcName == "" && baseID != msg.ToolCallID {
				if cached, ok := GetToolCall(baseID); ok {
					funcName = cached.Name
				}
			}
			if funcName == "" {
				funcName = msg.Name
			}
			if funcName == "" {
				funcName = "tool_result"
			}

			callID := baseID
			if callID == "" {
				callID = GenerateID("call")
			}

			thoughtSig := callIDToThought[msg.ToolCallID]
			if thoughtSig == "" && baseID != msg.ToolCallID {
				thoughtSig = callIDToThought[baseID]
			}
			if thoughtSig == "" && unpackedSig != "" {
				thoughtSig = unpackedSig
			}
			if thoughtSig == "" {
				if cached, ok := GetToolCall(msg.ToolCallID); ok {
					thoughtSig = cached.ThoughtSignature
				}
			}
			if thoughtSig == "" && baseID != msg.ToolCallID {
				if cached, ok := GetToolCall(baseID); ok {
					thoughtSig = cached.ThoughtSignature
				}
			}

			rawContent := extractText(msg.Content)
			if thoughtSig != "" {
				var respMap map[string]any
				if err := json.Unmarshal([]byte(rawContent), &respMap); err != nil {
					respMap = map[string]any{"result": rawContent}
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
					Text: fmt.Sprintf("[Tool `%s` returned]:\n%s", funcName, rawContent),
				})
			}
		}

		// Turn merging: if consecutive messages have the same role (e.g. multiple tool responses),
		// merge parts into the last turn to enforce strict user/model alternation required by Google Cloud Code PA.
		if len(parts) > 0 {
			if len(contents) > 0 && contents[len(contents)-1].Role == role {
				contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
			} else {
				contents = append(contents, GoogleContent{
					Role:  role,
					Parts: parts,
				})
			}
		}
	}

	if len(systemParts) > 0 {
		gReq.Request.SystemInstruction = &struct {
			Parts []GooglePart `json:"parts"`
		}{Parts: systemParts}
	}

	// Ensure contents alternation and user-first turn
	gReq.Request.Contents = CleanAndSanitizeContents(contents)
	return gReq, resolvedModel
}

// CleanAndSanitizeContents guarantees strict user/model alternation,
// ensures contents starts with user role, and collapses consecutive same-role turns into unified parts.
func CleanAndSanitizeContents(contents []GoogleContent) []GoogleContent {
	if len(contents) == 0 {
		return []GoogleContent{
			{
				Role:  "user",
				Parts: []GooglePart{{Text: "Hello"}},
			},
		}
	}

	var sanitized []GoogleContent
	for _, c := range contents {
		if len(c.Parts) == 0 {
			continue
		}
		if len(sanitized) > 0 && sanitized[len(sanitized)-1].Role == c.Role {
			sanitized[len(sanitized)-1].Parts = append(sanitized[len(sanitized)-1].Parts, c.Parts...)
		} else {
			sanitized = append(sanitized, c)
		}
	}

	if len(sanitized) == 0 {
		return []GoogleContent{
			{
				Role:  "user",
				Parts: []GooglePart{{Text: "Hello"}},
			},
		}
	}

	if sanitized[0].Role == "model" {
		sanitized = append([]GoogleContent{
			{
				Role:  "user",
				Parts: []GooglePart{{Text: "Hello"}},
			},
		}, sanitized...)
	}

	return sanitized
}

// ConvertClaudeToGoogle converts Anthropic request to Google internal payload
func ConvertClaudeToGoogle(req *ClaudeRequest) (*GoogleRequest, string) {
	resolvedModel := models.GetRegistry().ResolveModel(req.Model)
	project := auth.GetManager().GetProjectID()

	gReq := &GoogleRequest{
		Model:   resolvedModel,
		Project: project,
	}

	maxTokens := maxTokensForModel(resolvedModel, req.MaxTokens)
	gReq.Request.GenerationConfig = map[string]any{
		"maxOutputTokens": maxTokens,
	}
	if req.Temperature != nil {
		gReq.Request.GenerationConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gReq.Request.GenerationConfig["topP"] = *req.TopP
	}

	// Handle Anthropic Tools declaration
	if len(req.Tools) > 0 {
		var funcDecls []map[string]any
		for _, t := range req.Tools {
			if tm, ok := t.(map[string]any); ok {
				name, _ := tm["name"].(string)
				desc, _ := tm["description"].(string)
				decl := map[string]any{
					"name":        name,
					"description": desc,
				}
				if schema, hasSchema := tm["input_schema"]; hasSchema {
					decl["parameters"] = schema
				} else if params, hasParams := tm["parameters"]; hasParams {
					decl["parameters"] = params
				}
				funcDecls = append(funcDecls, decl)
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

	// Pre-scan Claude tool_use calls to populate thought signatures and tool names
	callIDToThought := make(map[string]string)
	callIDToName := make(map[string]string)
	for _, msg := range req.Messages {
		if arr, ok := msg.Content.([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					if t, _ := m["type"].(string); t == "tool_use" {
						id, _ := m["id"].(string)
						name, _ := m["name"].(string)
						if id != "" {
							baseID, unpackedSig := UnpackCallID(id)
							if name != "" {
								callIDToName[id] = name
								if baseID != id {
									callIDToName[baseID] = name
								}
							}
							if unpackedSig != "" {
								callIDToThought[id] = unpackedSig
								if baseID != id {
									callIDToThought[baseID] = unpackedSig
								}
							} else if cached, ok := GetToolCall(id); ok && cached.ThoughtSignature != "" {
								callIDToThought[id] = cached.ThoughtSignature
							} else if baseID != id {
								if cached, ok := GetToolCall(baseID); ok && cached.ThoughtSignature != "" {
									callIDToThought[baseID] = cached.ThoughtSignature
								}
							}
						}
					}
				}
			}
		}
	}

	var contents []GoogleContent
	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		parts := extractClaudeParts(msg.Content, callIDToThought, callIDToName)
		if len(parts) > 0 {
			if len(contents) > 0 && contents[len(contents)-1].Role == role {
				contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
			} else {
				contents = append(contents, GoogleContent{
					Role:  role,
					Parts: parts,
				})
			}
		}
	}

	gReq.Request.Contents = CleanAndSanitizeContents(contents)
	return gReq, resolvedModel
}

func parseDataURL(url string) GooglePart {
	parts := strings.SplitN(url, ";base64,", 2)
	if len(parts) != 2 {
		return GooglePart{Text: "[Image URL: " + url + "]"}
	}
	mime := strings.TrimPrefix(parts[0], "data:")
	if mime == "" {
		mime = "image/jpeg"
	}
	return GooglePart{
		InlineData: &GoogleInlineData{
			MimeType: mime,
			Data:     parts[1],
		},
	}
}

func extractOpenAIParts(content any) []GooglePart {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		if s != "" {
			return []GooglePart{{Text: s}}
		}
		return nil
	}
	if arr, ok := content.([]any); ok {
		var parts []GooglePart
		for _, item := range arr {
			if s, ok := item.(string); ok && s != "" {
				parts = append(parts, GooglePart{Text: s})
			} else if m, ok := item.(map[string]any); ok {
				t, _ := m["type"].(string)
				switch t {
				case "text":
					if textStr, ok := m["text"].(string); ok && textStr != "" {
						parts = append(parts, GooglePart{Text: textStr})
					}
				case "image_url":
					if imgObj, ok := m["image_url"].(map[string]any); ok {
						if urlStr, ok := imgObj["url"].(string); ok {
							if strings.HasPrefix(urlStr, "data:") {
								parts = append(parts, parseDataURL(urlStr))
							} else {
								parts = append(parts, GooglePart{Text: "[Image URL: " + urlStr + "]"})
							}
						}
					}
				}
			}
		}
		return parts
	}
	s := fmt.Sprintf("%v", content)
	if s != "" {
		return []GooglePart{{Text: s}}
	}
	return nil
}

func extractClaudeParts(content any, callIDToThought, callIDToName map[string]string) []GooglePart {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		if s != "" {
			return []GooglePart{{Text: s}}
		}
		return nil
	}
	if arr, ok := content.([]any); ok {
		var parts []GooglePart
		for _, item := range arr {
			if s, ok := item.(string); ok && s != "" {
				parts = append(parts, GooglePart{Text: s})
			} else if m, ok := item.(map[string]any); ok {
				t, _ := m["type"].(string)
				switch t {
				case "text":
					if textStr, ok := m["text"].(string); ok && textStr != "" {
						parts = append(parts, GooglePart{Text: textStr})
					}
				case "image":
					if src, ok := m["source"].(map[string]any); ok {
						srcType, _ := src["type"].(string)
						if srcType == "base64" {
							mediaType, _ := src["media_type"].(string)
							data, _ := src["data"].(string)
							if mediaType == "" {
								mediaType = "image/jpeg"
							}
							parts = append(parts, GooglePart{
								InlineData: &GoogleInlineData{
									MimeType: mediaType,
									Data:     data,
								},
							})
						}
					}
				case "tool_use":
					id, _ := m["id"].(string)
					name, _ := m["name"].(string)
					inputObj, _ := m["input"].(map[string]any)
					if inputObj == nil {
						inputObj = make(map[string]any)
					}
					argsBytes, _ := json.Marshal(inputObj)
					baseID, unpackedSig := UnpackCallID(id)
					thoughtSig := callIDToThought[id]
					if thoughtSig == "" && baseID != id {
						thoughtSig = callIDToThought[baseID]
					}
					if thoughtSig == "" && unpackedSig != "" {
						thoughtSig = unpackedSig
					}
					if thoughtSig == "" {
						if cached, ok := GetToolCall(id); ok {
							thoughtSig = cached.ThoughtSignature
						}
					}
					if thoughtSig == "" && baseID != id {
						if cached, ok := GetToolCall(baseID); ok {
							thoughtSig = cached.ThoughtSignature
						}
					}
					RecordToolCall(id, name, string(argsBytes), thoughtSig)
					if thoughtSig != "" {
						parts = append(parts, GooglePart{
							ThoughtSignature: thoughtSig,
							FunctionCall: &GoogleFuncCall{
								Name: name,
								Args: inputObj,
								ID:   baseID,
							},
						})
					} else {
						parts = append(parts, GooglePart{
							Text: fmt.Sprintf("[Assistant called tool `%s` with arguments: %s]", name, string(argsBytes)),
						})
					}
				case "tool_result":
					toolUseID, _ := m["tool_use_id"].(string)
					resultContent := extractText(m["content"])
					baseID, unpackedSig := UnpackCallID(toolUseID)
					name := callIDToName[toolUseID]
					if name == "" && baseID != toolUseID {
						name = callIDToName[baseID]
					}
					thoughtSig := callIDToThought[toolUseID]
					if thoughtSig == "" && baseID != toolUseID {
						thoughtSig = callIDToThought[baseID]
					}
					if thoughtSig == "" && unpackedSig != "" {
						thoughtSig = unpackedSig
					}
					if cached, ok := GetToolCall(toolUseID); ok {
						if name == "" {
							name = cached.Name
						}
						if thoughtSig == "" {
							thoughtSig = cached.ThoughtSignature
						}
					}
					if (name == "" || thoughtSig == "") && baseID != toolUseID {
						if cached, ok := GetToolCall(baseID); ok {
							if name == "" {
								name = cached.Name
							}
							if thoughtSig == "" {
								thoughtSig = cached.ThoughtSignature
							}
						}
					}
					if name == "" {
						name = "tool_result"
					}
					if thoughtSig != "" {
						var respMap map[string]any
						if err := json.Unmarshal([]byte(resultContent), &respMap); err != nil {
							respMap = map[string]any{"result": resultContent}
						}
						parts = append(parts, GooglePart{
							FunctionResponse: &GoogleFuncResp{
								Name:     name,
								Response: respMap,
								ID:       baseID,
							},
						})
					} else {
						parts = append(parts, GooglePart{
							Text: fmt.Sprintf("[Tool `%s` returned]:\n%s", name, resultContent),
						})
					}
				}
			}
		}
		return parts
	}
	s := fmt.Sprintf("%v", content)
	if s != "" {
		return []GooglePart{{Text: s}}
	}
	return nil
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
	Text             string
	ReasoningText    string
	Thought          string
	ThoughtSignature string
	FinishReason     string
	ToolCalls        []OpenAIToolCall
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
		var reasoningBuilder strings.Builder
		for _, part := range cand.Content.Parts {
			if part.Thought {
				if part.Text != "" {
					reasoningBuilder.WriteString(part.Text)
				}
			} else {
				if part.Text != "" {
					textBuilder.WriteString(part.Text)
				}
			}
			if part.ThoughtSignature != "" {
				res.ThoughtSignature = part.ThoughtSignature
				res.Thought = part.ThoughtSignature
			}
			if part.FunctionCall != nil {
				rawCallID := part.FunctionCall.ID
				if rawCallID == "" {
					rawCallID = GenerateID("call")
				}
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				RecordToolCall(rawCallID, part.FunctionCall.Name, string(argsBytes), part.ThoughtSignature)

				packedID := PackCallID(rawCallID, part.ThoughtSignature)
				tc := OpenAIToolCall{
					ID:   packedID,
					Type: "function",
				}
				tc.Function.Name = part.FunctionCall.Name
				tc.Function.Arguments = string(argsBytes)
				res.ToolCalls = append(res.ToolCalls, tc)
			}
		}
		res.Text = textBuilder.String()
		res.ReasoningText = reasoningBuilder.String()
	}

	return res, nil
}

// FormatOpenAIChunk formats an incremental delta for OpenAI streaming client
func FormatOpenAIChunk(id, model, text, reasoningText, finishReason string, toolCalls []OpenAIToolCall) string {
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
	if reasoningText != "" {
		delta["reasoning_content"] = reasoningText
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
func FormatOpenAINonStream(id, model, content, reasoningContent string, toolCalls []OpenAIToolCall) []byte {
	type Message struct {
		Role             string           `json:"role"`
		Content          *string          `json:"content"`
		ReasoningContent *string          `json:"reasoning_content,omitempty"`
		ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
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

	var rcPtr *string
	if reasoningContent != "" {
		rcPtr = &reasoningContent
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
					Role:             "assistant",
					Content:          contentPtr,
					ReasoningContent: rcPtr,
					ToolCalls:        toolCalls,
				},
				FinishReason: finishReason,
			},
		},
	}

	bytes, _ := json.Marshal(res)
	return bytes
}

// FormatClaudeNonStream formats response for Anthropic clients
func FormatClaudeNonStream(id, model, content string, toolCalls []OpenAIToolCall) []byte {
	type ClaudeContentBlock struct {
		Type  string         `json:"type"`
		Text  string         `json:"text,omitempty"`
		ID    string         `json:"id,omitempty"`
		Name  string         `json:"name,omitempty"`
		Input map[string]any `json:"input,omitempty"`
	}
	type Response struct {
		ID         string               `json:"id"`
		Type       string               `json:"type"`
		Role       string               `json:"role"`
		Content    []ClaudeContentBlock `json:"content"`
		Model      string               `json:"model"`
		StopReason string               `json:"stop_reason"`
	}

	var blocks []ClaudeContentBlock
	if content != "" {
		blocks = append(blocks, ClaudeContentBlock{Type: "text", Text: content})
	}
	for _, tc := range toolCalls {
		var inputMap map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &inputMap); err != nil {
			inputMap = make(map[string]any)
		}
		blocks = append(blocks, ClaudeContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: inputMap,
		})
	}

	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}

	res := Response{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      model,
		StopReason: stopReason,
	}

	bytes, _ := json.Marshal(res)
	return bytes
}
