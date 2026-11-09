package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"antiapi/auth"
	"antiapi/translator"
	"antiapi/transport"
	"antiapi/tui"
)

const (
	StandardEndpoint = "https://cloudcode-pa.googleapis.com"
	DailyEndpoint    = "https://daily-cloudcode-pa.googleapis.com"
	StreamPath       = "/v1internal:streamGenerateContent?alt=sse"
	RequestTimeout   = 120 * time.Second
)

var (
	validationURLRegex = regexp.MustCompile("https://accounts\\.google\\.com[^\\s\\x22\\x27<>]+")

	endpointCooldownMu sync.RWMutex
	endpointCooldowns  = make(map[string]time.Time)

	endpointAffinityMu sync.RWMutex
	preferredEndpoint  = make(map[string]string)
)

func getPreferredEndpoint(email string) string {
	endpointAffinityMu.RLock()
	defer endpointAffinityMu.RUnlock()
	return preferredEndpoint[email]
}

func setPreferredEndpoint(email, endpoint string) {
	endpointAffinityMu.Lock()
	defer endpointAffinityMu.Unlock()
	preferredEndpoint[email] = endpoint
}

// endpointCooldownKey scopes a cooldown to one account on one endpoint: a 429
// from Google is per-account quota, so it must not bench the endpoint for the
// whole pool.
func endpointCooldownKey(email, endpoint string) string {
	return email + "|" + endpoint
}

func isEndpointInCooldown(email, endpoint string) bool {
	endpointCooldownMu.RLock()
	defer endpointCooldownMu.RUnlock()
	expiry, exists := endpointCooldowns[endpointCooldownKey(email, endpoint)]
	if !exists {
		return false
	}
	return time.Now().Before(expiry)
}

func markEndpointCooldown(email, endpoint string, duration time.Duration) {
	endpointCooldownMu.Lock()
	defer endpointCooldownMu.Unlock()
	endpointCooldowns[endpointCooldownKey(email, endpoint)] = time.Now().Add(duration)
}

func clearEndpointCooldown(email, endpoint string) {
	endpointCooldownMu.Lock()
	defer endpointCooldownMu.Unlock()
	delete(endpointCooldowns, endpointCooldownKey(email, endpoint))
}

// ClearAllEndpointCooldowns flushes all active endpoint backoffs.
func ClearAllEndpointCooldowns() {
	endpointCooldownMu.Lock()
	defer endpointCooldownMu.Unlock()
	endpointCooldowns = make(map[string]time.Time)
}

type SSEEvent struct {
	Data []byte
	Err  error
}

type Client struct {
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: NewStealthTransport(),
			Timeout:   RequestTimeout,
		},
	}
}

func getCapacityFallbackModel(model string) string {
	switch model {
	case "gemini-3.8-flash-high":
		return "gemini-3.8-flash-medium"
	case "gemini-3.8-flash-medium":
		return "gemini-3.8-flash-low"
	case "gemini-3.7-flash-high":
		return "gemini-3.7-flash-medium"
	case "gemini-3.7-flash-medium":
		return "gemini-3.8-flash-high"
	case "gemini-3.1-pro-high", "gemini-pro-agent":
		return "gemini-3.8-flash-high"
	case "claude-opus-4-6-thinking":
		return "claude-sonnet-4-6"
	default:
		return ""
	}
}

func extractModelName(payload any) string {
	if gReq, ok := payload.(*translator.GoogleRequest); ok {
		return gReq.Model
	}
	if reqMap, ok := payload.(map[string]any); ok {
		if m, ok := reqMap["model"].(string); ok {
			return m
		}
	}
	return ""
}

func setModelName(payload any, model string) {
	if gReq, ok := payload.(*translator.GoogleRequest); ok {
		gReq.Model = model
	} else if reqMap, ok := payload.(map[string]any); ok {
		reqMap["model"] = model
	}
}

func deriveConversationKey(payload any) string {
	if gReq, ok := payload.(*translator.GoogleRequest); ok {
		if len(gReq.Request.Contents) > 1 {
			first := gReq.Request.Contents[0]
			var b strings.Builder
			b.WriteString(first.Role)
			for _, p := range first.Parts {
				if p.Text != "" {
					b.WriteString(p.Text)
				}
			}
			h := sha256.Sum256([]byte(b.String()))
			return hex.EncodeToString(h[:8])
		}
	} else if reqMap, ok := payload.(map[string]any); ok {
		if reqObj, ok := reqMap["request"].(map[string]any); ok {
			if contents, ok := reqObj["contents"].([]any); ok && len(contents) > 1 {
				firstBytes, _ := json.Marshal(contents[0])
				h := sha256.Sum256(firstBytes)
				return hex.EncodeToString(h[:8])
			}
		}
	}
	return ""
}

func (c *Client) StreamGenerate(ctx context.Context, payload any, sessionID ...string) (<-chan SSEEvent, error) {
	currentModel := extractModelName(payload)
	triedFallback := false

	var sessKey string
	if len(sessionID) > 0 && sessionID[0] != "" {
		sessKey = sessionID[0]
	} else {
		sessKey = deriveConversationKey(payload)
	}

attempt:
	pool := auth.GetPool()
	totalAccounts := pool.Count()

	if totalAccounts == 0 {
		return nil, fmt.Errorf("no accounts available in pool")
	}

	maxAttempts := totalAccounts
	var lastErr error
	saw503Capacity := false
	attemptedAccounts := make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		acc, err := pool.GetAccountForSession(sessKey, attemptedAccounts)
		if err != nil {
			return nil, fmt.Errorf("cannot select account: %w", err)
		}
		attemptedAccounts[acc.Email] = true

		if err := acc.Acquire(ctx); err != nil {
			return nil, fmt.Errorf("concurrency acquire failed for %s: %w", acc.Email, err)
		}

		token, err := acc.GetAccessToken()
		if err != nil {
			acc.Release()
			log.Printf("[Upstream] Failed to get access token for %s: %v", acc.Email, err)
			continue
		}

		var reqBytes []byte
		if gReq, ok := payload.(*translator.GoogleRequest); ok {
			reqCopy := *gReq
			reqCopy.Project = acc.ProjectID
			reqBytes, err = json.Marshal(reqCopy)
		} else if reqMap, ok := payload.(map[string]any); ok {
			reqMap["project"] = acc.ProjectID
			reqBytes, err = json.Marshal(reqMap)
		} else {
			b, marshalErr := json.Marshal(payload)
			if marshalErr != nil {
				acc.Release()
				return nil, fmt.Errorf("failed to marshal payload: %w", marshalErr)
			}
			var reqMap map[string]any
			if unmarshalErr := json.Unmarshal(b, &reqMap); unmarshalErr == nil {
				reqMap["project"] = acc.ProjectID
				reqBytes, _ = json.Marshal(reqMap)
			} else {
				reqBytes = b
			}
		}

		if err != nil {
			acc.Release()
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}

		ua := BuildUserAgent()
		clientVersion := fmt.Sprintf("antigravity-ide/2.9.1 gl-go/%s", runtime.Version())

		endpoints := []string{StandardEndpoint, DailyEndpoint}
		pref := getPreferredEndpoint(acc.Email)
		if pref == DailyEndpoint && !isEndpointInCooldown(acc.Email, DailyEndpoint) {
			endpoints = []string{DailyEndpoint, StandardEndpoint}
		} else if isEndpointInCooldown(acc.Email, StandardEndpoint) && !isEndpointInCooldown(acc.Email, DailyEndpoint) {
			endpoints = []string{DailyEndpoint, StandardEndpoint}
		}
		saw429 := false
		for _, base := range endpoints {
			url := base + StreamPath
			req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBytes))
			if err != nil {
				acc.Release()
				return nil, err
			}

			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", ua)
			req.Header.Set("X-Goog-Api-Client", clientVersion)
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Accept-Language", "en-US,en;q=0.9")

			accountClient := transport.NewAccountClient(acc.Email, RequestTimeout)
			resp, err := accountClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					acc.Release()
					return nil, ctx.Err()
				}
				lastErr = err
				continue
			}

			if resp.StatusCode == http.StatusOK {
				clearEndpointCooldown(acc.Email, base)
				setPreferredEndpoint(acc.Email, base)
				acc.RecordSuccess()
				log.Printf("[Upstream] 200 OK via %s on account %s (Stealth TLS + %s)", base, acc.Email, ua)
				eventCh := make(chan SSEEvent, 64)
				go func() {
					defer acc.Release()
					c.streamReader(ctx, resp.Body, eventCh)
				}()
				return eventCh, nil
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusUnauthorized {
				log.Printf("[Upstream] 401 for %s, refreshing token...", acc.Email)
				if newToken, refErr := acc.Refresh(); refErr == nil {
					token = newToken
				}
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				saw429 = true
				markEndpointCooldown(acc.Email, base, 10*time.Minute)
				log.Printf("[Upstream] 429 Quota Exceeded on %s via %s (placed on 10m cooldown): %s", acc.Email, base, string(body))
				lastErr = fmt.Errorf("quota exceeded on %s: %s", acc.Email, string(body))
				continue // try next endpoint before failing over account
			}

			if resp.StatusCode == http.StatusForbidden {
				log.Printf("[Upstream] 403 Forbidden on %s: %s", acc.Email, string(body))
				if vURL := ExtractValidationURL(body); vURL != "" {
					acc.SetValidationURL(vURL)
					log.Printf("[Upstream] Account %s needs interactive validation: %s", acc.Email, vURL)
				}
				acc.RecordError(http.StatusForbidden)
				lastErr = fmt.Errorf("account %s forbidden: %s", acc.Email, string(body))
				break // failover to next account in pool
			}

			if resp.StatusCode == http.StatusBadRequest {
				log.Printf("[Upstream] 400 Bad Request on %s: %s", acc.Email, string(body))
				acc.Release()
				return nil, fmt.Errorf("upstream rejected request (%d): %s", resp.StatusCode, string(body))
			}

			if resp.StatusCode == http.StatusServiceUnavailable || strings.Contains(string(body), "MODEL_CAPACITY_EXHAUSTED") || strings.Contains(string(body), "No capacity available") || (resp.StatusCode == 503 && strings.Contains(string(body), "UNAVAILABLE")) {
				saw503Capacity = true
				lastErr = fmt.Errorf("upstream model capacity unavailable (503): %s", string(body))
				log.Printf("[Upstream] 503 Capacity Unavailable for %s (MODEL_CAPACITY_EXHAUSTED)", currentModel)

				if !triedFallback {
					fallbackModel := getCapacityFallbackModel(currentModel)
					if fallbackModel != "" {
						triedFallback = true
						tui.SetActivity(fmt.Sprintf("[Failover] %s -> %s (503 capacity)", currentModel, fallbackModel))
						log.Printf("[Failover] Model '%s' capacity exhausted upstream (503) -> immediately falling back to '%s'", currentModel, fallbackModel)
						currentModel = fallbackModel
						setModelName(payload, fallbackModel)
						pool.ClearAllRateLimits()
						acc.Release()
						goto attempt
					}
				}
				break
			}

			lastErr = fmt.Errorf("upstream error (%d): %s", resp.StatusCode, string(body))
			log.Printf("[Upstream] Request to %s failed (%d): %s", base, resp.StatusCode, string(body))
		}
		if saw429 && !saw503Capacity {
			acc.RecordError(http.StatusTooManyRequests)
		}
		acc.Release()
	}

	if saw503Capacity && !triedFallback {
		fallbackModel := getCapacityFallbackModel(currentModel)
		if fallbackModel != "" {
			triedFallback = true
			log.Printf("[Failover] Model '%s' upstream capacity exhausted (503) -> falling back to '%s'", currentModel, fallbackModel)
			currentModel = fallbackModel
			setModelName(payload, fallbackModel)
			pool.ClearAllRateLimits()
			goto attempt
		}
	}

	return nil, lastErr
}

func (c *Client) streamReader(ctx context.Context, body io.ReadCloser, ch chan<- SSEEvent) {
	defer body.Close()
	defer close(ch)

	reader := bufio.NewReaderSize(body, 4*1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 && bytes.HasPrefix(trimmed, []byte("data: ")) {
				data := bytes.TrimPrefix(trimmed, []byte("data: "))
				select {
				case <-ctx.Done():
					return
				case ch <- SSEEvent{Data: bytes.Clone(data)}:
				}
			}
		}

		if err != nil {
			if err != io.EOF && !errors.Is(err, context.Canceled) {
				select {
				case <-ctx.Done():
				case ch <- SSEEvent{Err: err}:
				}
			}
			return
		}
	}
}

// ExtractValidationURL pulls the interactive account-verification URL out of a
// Google 403 VALIDATION_REQUIRED error body, when present.
func ExtractValidationURL(body []byte) string {
	var parsed struct {
		Error struct {
			Details []struct {
				Reason   string `json:"reason"`
				Metadata struct {
					ValidationURL  string `json:"validationUrl"`
					ValidationURL2 string `json:"validation_url"`
				} `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		for _, d := range parsed.Error.Details {
			if d.Metadata.ValidationURL != "" {
				return d.Metadata.ValidationURL
			}
			if d.Metadata.ValidationURL2 != "" {
				return d.Metadata.ValidationURL2
			}
		}
	}
	if m := validationURLRegex.Find(body); len(m) > 0 {
		return string(m)
	}
	return ""
}
