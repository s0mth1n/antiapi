package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"antiapi/auth"
	"antiapi/transport"
)

const (
	CloudCodeBaseURL = "https://cloudcode-pa.googleapis.com"
	FallbackBaseURL  = "https://daily-cloudcode-pa.googleapis.com"
)

type ModelQuota struct {
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time,omitempty"`
}

type ModelInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Tier      string     `json:"tier"`
	QuotaInfo ModelQuota `json:"quota_info"`
}

type Registry struct {
	mu           sync.RWMutex
	models       map[string]ModelInfo
	lastUpdated  time.Time
	defaultModel string
}

var (
	globalRegistry *Registry
	once           sync.Once
)

func GetRegistry() *Registry {
	once.Do(func() {
		globalRegistry = &Registry{
			models:       make(map[string]ModelInfo),
			defaultModel: "gemini-3.7-flash-high",
		}
		globalRegistry.initDefaults()
	})
	return globalRegistry
}

// CuratedSupportedModels defines the clean, official models exposed via /v1/models
var CuratedSupportedModels = []ModelInfo{
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Tier: "Claude"},
	{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6 (Thinking)", Tier: "Claude"},
	{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)", Tier: "Gemini Flash"},
	{ID: "gemini-3.7-flash-high", Name: "Gemini 3.7 Flash (High)", Tier: "Gemini Flash"},
	{ID: "gemini-3.7-flash-medium", Name: "Gemini 3.7 Flash (Medium)", Tier: "Gemini Flash"},
	{ID: "gemini-3.6-flash-medium", Name: "Gemini 3.6 Flash (Medium)", Tier: "Gemini Flash"},
	{ID: "gemini-3.1-pro-high", Name: "Gemini 3.1 Pro (High)", Tier: "Gemini Pro"},
	{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)", Tier: "GPT"},
}

func (r *Registry) initDefaults() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range CuratedSupportedModels {
		m.QuotaInfo.RemainingFraction = 1.0
		r.models[m.ID] = m
	}
}

// FetchLiveModels queries Google Cloud Code for the real available models & quota
func (r *Registry) FetchLiveModels() error {
	acc, err := auth.GetPool().NextAvailableAccount()
	if err != nil {
		return fmt.Errorf("cannot fetch models without account: %w", err)
	}

	token, err := acc.GetAccessToken()
	if err != nil {
		return fmt.Errorf("cannot fetch models without access token: %w", err)
	}

	project := acc.ProjectID
	if project == "" {
		project = auth.DefaultProject
	}
	reqData, _ := json.Marshal(map[string]string{"project": project})

	makeReq := func(baseURL string) (*http.Response, error) {
		req, _ := http.NewRequest("POST", baseURL+"/v1internal:fetchAvailableModels", bytes.NewBuffer(reqData))
		transport.SetStandardHeaders(req, token)
		client := transport.NewClient(10 * time.Second)
		return client.Do(req)
	}

	resp, err := makeReq(CloudCodeBaseURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		resp, err = makeReq(FallbackBaseURL)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	var upstreamResp struct {
		Models map[string]struct {
			QuotaInfo *struct {
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&upstreamResp); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for id, m := range upstreamResp.Models {
		// Only update quota for official curated models, never expose internal junk models (chat_*, tab_*, *_low, *_tiered)
		info, exists := r.models[id]
		if exists && m.QuotaInfo != nil {
			info.QuotaInfo.RemainingFraction = m.QuotaInfo.RemainingFraction
			info.QuotaInfo.ResetTime = m.QuotaInfo.ResetTime
			r.models[id] = info
		}
		if id == "gemini-pro-agent" {
			if proInfo, hasPro := r.models["gemini-3.1-pro-high"]; hasPro && m.QuotaInfo != nil {
				proInfo.QuotaInfo.RemainingFraction = m.QuotaInfo.RemainingFraction
				proInfo.QuotaInfo.ResetTime = m.QuotaInfo.ResetTime
				r.models["gemini-3.1-pro-high"] = proInfo
			}
		}
	}
	r.lastUpdated = time.Now()
	log.Printf("[Models] Updated %d models from Google upstream", len(r.models))
	return nil
}

// ResolveModel maps any user/client model name to a valid Antigravity model ID
func (r *Registry) ResolveModel(userModel string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cleaned := strings.TrimSpace(strings.ToLower(userModel))
	if cleaned == "" {
		return r.defaultModel
	}

	// 1. Exact alias mappings FIRST
	aliases := map[string]string{
		// Gemini 3.8 / 3.7
		"gemini-3.8":              "gemini-3.8-flash-high",
		"gemini-3.8-flash":        "gemini-3.8-flash-high",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
		"gemini-3.7":              "gemini-3.7-flash-medium",
		"gemini-3.7-flash":        "gemini-3.7-flash-medium",
		"gemini-3.7-flash-medium": "gemini-3.7-flash-medium",
		"gemini-3.7-flash-high":   "gemini-3.7-flash-high",
		"gemini-3.7-flash-low":    "gemini-3.7-flash-low",

		// Gemini 3.6
		"gemini-3.6":              "gemini-3.6-flash-medium",
		"gemini-3.6-flash":        "gemini-3.6-flash-medium",
		"gemini-3.6-flash-high":   "gemini-3.6-flash-high",
		"gemini-3.6-flash-medium": "gemini-3.6-flash-medium",
		"gemini-3.6-flash-low":    "gemini-3.6-flash-low",

		// Gemini Pro - Maps to native Google "gemini-pro-agent"
		"gemini-pro":          "gemini-pro-agent",
		"gemini-3-pro-high":   "gemini-pro-agent",
		"gemini-3.1-pro":      "gemini-pro-agent",
		"gemini-3.1-pro-high": "gemini-pro-agent",
		"gemini-3.1-pro-low":  "gemini-3.1-pro-low",

		// Claude
		"claude-sonnet-4-6":          "claude-sonnet-4-6",
		"claude-sonnet-4.6":          "claude-sonnet-4-6",
		"claude-sonnet-4.6-thinking": "claude-sonnet-4-6",
		"claude-3-7-sonnet":          "claude-sonnet-4-6",
		"claude-3.7-sonnet":          "claude-sonnet-4-6",
		"claude-3-7-sonnet-20250219": "claude-sonnet-4-6",
		"claude-3-5-sonnet":          "claude-sonnet-4-6",
		"claude-3.5-sonnet":          "claude-sonnet-4-6",
		"claude-3-5-sonnet-20241022": "claude-sonnet-4-6",
		"claude-3-5-sonnet-latest":   "claude-sonnet-4-6",
		"claude-sonnet":              "claude-sonnet-4-6",
		"claude-opus-4-6":            "claude-opus-4-6-thinking",
		"claude-opus-4.6":            "claude-opus-4-6-thinking",
		"claude-opus-4-6-thinking":   "claude-opus-4-6-thinking",
		"claude-3-opus":              "claude-opus-4-6-thinking",
		"claude-opus":                "claude-opus-4-6-thinking",

		// GPT & OSS
		"gpt-oss":             "gpt-oss-120b-medium",
		"gpt-oss-120b":        "gpt-oss-120b-medium",
		"gpt-oss-120b-medium": "gpt-oss-120b-medium",
		"gpt-4o":              "claude-sonnet-4-6",
		"gpt-4":               "claude-sonnet-4-6",
	}

	if target, ok := aliases[cleaned]; ok {
		return target
	}

	// 2. Direct match
	if _, ok := r.models[cleaned]; ok {
		return cleaned
	}

	// 3. Fuzzy match
	if strings.Contains(cleaned, "opus") {
		return "claude-opus-4-6-thinking"
	}
	if strings.Contains(cleaned, "claude") || strings.Contains(cleaned, "sonnet") {
		return "claude-sonnet-4-6"
	}
	if strings.Contains(cleaned, "gpt-oss") {
		return "gpt-oss-120b-medium"
	}
	if strings.Contains(cleaned, "pro") {
		return "gemini-pro-agent"
	}
	if strings.Contains(cleaned, "flash") || strings.Contains(cleaned, "gemini") {
		return "gemini-3.7-flash-high"
	}

	log.Printf("[Models] Unknown model requested '%s'; falling back gracefully to '%s'", userModel, r.defaultModel)
	return r.defaultModel
}

func (r *Registry) GetAllModels() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ModelInfo, 0, len(CuratedSupportedModels))
	for _, cur := range CuratedSupportedModels {
		if m, ok := r.models[cur.ID]; ok {
			result = append(result, m)
		} else {
			result = append(result, cur)
		}
	}
	return result
}
