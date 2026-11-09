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

type ModelPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type ModelInfo struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Tier            string        `json:"tier"`
	ContextWindow   int           `json:"context_window,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	QuotaInfo       ModelQuota    `json:"quota_info"`
	Pricing         *ModelPricing `json:"pricing,omitempty"`
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
			defaultModel: "gemini-3.8-flash-high",
		}
		globalRegistry.initDefaults()
	})
	return globalRegistry
}

// CuratedSupportedModels defines the clean, official models exposed via /v1/models
var CuratedSupportedModels = []ModelInfo{
	{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Tier: "Gemini Flash", ContextWindow: 1048576, MaxOutputTokens: 65536, Pricing: &ModelPricing{Prompt: "0.00000075", Completion: "0.00000375"}},
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
		if resp != nil {
			resp.Body.Close()
		}
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
		if id == "gemini-3.8-flash-high" && m.QuotaInfo != nil {
			if flashInfo, ok := r.models["gemini-3.8-flash"]; ok {
				flashInfo.QuotaInfo.RemainingFraction = m.QuotaInfo.RemainingFraction
				flashInfo.QuotaInfo.ResetTime = m.QuotaInfo.ResetTime
				r.models["gemini-3.8-flash"] = flashInfo
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

	aliases := map[string]string{
		"gemini-3.8":              "gemini-3.8-flash-high",
		"gemini-3.8-flash":        "gemini-3.8-flash-high",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
		"gemini-3.8-high":         "gemini-3.8-flash-high",
		"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
		"gemini-3.8-medium":       "gemini-3.8-flash-medium",
		"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
		"gemini-3.8-low":          "gemini-3.8-flash-low",
		"gemini-3.8-flash-fast":   "gemini-3.8-flash-low",
		"gemini-3.8-fast":         "gemini-3.8-flash-low",
	}

	if target, ok := aliases[cleaned]; ok {
		return target
	}

	if _, ok := r.models[cleaned]; ok {
		return cleaned
	}

	log.Printf("[Models] Model requested '%s' routed to default '%s'", userModel, r.defaultModel)
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

// GetModel returns information for a specific model ID or alias
func (r *Registry) GetModel(id string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cleaned := strings.TrimSpace(strings.ToLower(id))
	if cleaned == "" {
		return ModelInfo{}, false
	}

	for _, cur := range CuratedSupportedModels {
		if strings.EqualFold(cur.ID, cleaned) {
			if m, ok := r.models[cur.ID]; ok {
				return m, true
			}
			return cur, true
		}
	}

	if m, ok := r.models[cleaned]; ok {
		return m, true
	}

	if cleaned == "gemini-3.8-flash-high" {
		if m, ok := r.models["gemini-3.8-flash"]; ok {
			return m, true
		}
	}

	return ModelInfo{}, false
}
