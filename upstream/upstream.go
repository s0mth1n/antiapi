package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"antiapi/auth"
)

const (
	DailyEndpoint    = "https://daily-cloudcode-pa.googleapis.com"
	StandardEndpoint = "https://cloudcode-pa.googleapis.com"
	StreamPath       = "/v1internal:streamGenerateContent?alt=sse"
	UserAgent        = "antigravity/hub/2.9.1 darwin/arm64"
	RequestTimeout   = 120 * time.Second
)

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
			Timeout: RequestTimeout,
		},
	}
}

// StreamGenerate sends request to Google backend with automatic multi-account failover
func (c *Client) StreamGenerate(ctx context.Context, payload any) (<-chan SSEEvent, error) {
	pool := auth.GetPool()
	totalAccounts := pool.Count()
	if totalAccounts == 0 {
		return nil, fmt.Errorf("no accounts available in pool")
	}

	maxAttempts := totalAccounts
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, err := pool.NextAvailableAccount()
		if err != nil {
			return nil, fmt.Errorf("cannot select account: %w", err)
		}

		token, err := acc.GetAccessToken()
		if err != nil {
			log.Printf("[Upstream] Failed to get access token for %s: %v", acc.Email, err)
			continue
		}

		// Convert payload to map so we can inject the active account's project ID
		var reqMap map[string]any
		b, _ := json.Marshal(payload)
		json.Unmarshal(b, &reqMap)
		reqMap["project"] = acc.ProjectID

		reqBytes, err := json.Marshal(reqMap)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}

		endpoints := []string{DailyEndpoint, StandardEndpoint}
		for _, base := range endpoints {
			url := base + StreamPath
			req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBytes))
			if err != nil {
				return nil, err
			}

			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", UserAgent)
			req.Header.Set("Accept", "text/event-stream")

			resp, err := c.httpClient.Do(req)
			if err != nil {
				lastErr = err
				continue
			}

			if resp.StatusCode == http.StatusOK {
				acc.ClearRateLimit()
				log.Printf("[Upstream] 200 OK via %s on account %s", base, acc.Email)
				eventCh := make(chan SSEEvent, 64)
				go c.streamReader(ctx, resp.Body, eventCh)
				return eventCh, nil
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusUnauthorized {
				log.Printf("[Upstream] 401 for %s, refreshing token...", acc.Email)
				acc.Refresh()
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				log.Printf("[Upstream] 429 Quota Exceeded on %s: %s", acc.Email, string(body))
				acc.MarkRateLimited(5 * time.Minute)
				lastErr = fmt.Errorf("quota exceeded on %s", acc.Email)
				break // break endpoint loop to try next account in pool
			}

			if resp.StatusCode == http.StatusBadRequest {
				log.Printf("[Upstream] 400 Payload Error: %s", string(body))
				return nil, fmt.Errorf("upstream rejected payload (400): %s", string(body))
			}
			lastErr = fmt.Errorf("upstream error (%d): %s", resp.StatusCode, string(body))
			if resp.StatusCode == 400 {
				log.Printf("[Upstream] 400 Payload was: %s", string(reqBytes))
			}
			log.Printf("[Upstream] Request to %s failed (%d): %s", base, resp.StatusCode, string(body))
		}
	}

	return nil, lastErr
}

func (c *Client) streamReader(ctx context.Context, body io.ReadCloser, ch chan<- SSEEvent) {
	defer body.Close()
	defer close(ch)

	scanner := bufio.NewScanner(body)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			ch <- SSEEvent{Err: ctx.Err()}
			return
		default:
		}

		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "data: ") {
			data := strings.TrimPrefix(trimmed, "data: ")
			ch <- SSEEvent{Data: []byte(data)}
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		ch <- SSEEvent{Err: err}
	}
}
