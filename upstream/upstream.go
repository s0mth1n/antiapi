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
	"runtime"
	"strings"
	"time"

	"antiapi/auth"
)

const (
	DailyEndpoint    = "https://daily-cloudcode-pa.googleapis.com"
	StandardEndpoint = "https://cloudcode-pa.googleapis.com"
	StreamPath       = "/v1internal:streamGenerateContent?alt=sse"
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
			Transport: NewStealthTransport(),
			Timeout:   RequestTimeout,
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

		if err := acc.Acquire(ctx); err != nil {
			return nil, fmt.Errorf("concurrency acquire failed for %s: %w", acc.Email, err)
		}

		token, err := acc.GetAccessToken()
		if err != nil {
			acc.Release()
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
			acc.Release()
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}

		ua := BuildUserAgent()
		clientVersion := fmt.Sprintf("antigravity-ide/2.9.1 gl-go/%s", runtime.Version())

		endpoints := []string{DailyEndpoint, StandardEndpoint}
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

			resp, err := c.httpClient.Do(req)
			if err != nil {
				lastErr = err
				continue
			}

			if resp.StatusCode == http.StatusOK {
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
				acc.Refresh()
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				log.Printf("[Upstream] 429 Quota Exceeded on %s: %s", acc.Email, string(body))
				acc.RecordError(http.StatusTooManyRequests)
				lastErr = fmt.Errorf("quota exceeded on %s", acc.Email)
				break // break endpoint loop to try next account in pool
			}

			if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
				log.Printf("[Upstream] %d Error on %s: %s", resp.StatusCode, acc.Email, string(body))
				acc.RecordError(resp.StatusCode)
				acc.Release()
				return nil, fmt.Errorf("upstream rejected request (%d): %s", resp.StatusCode, string(body))
			}

			lastErr = fmt.Errorf("upstream error (%d): %s", resp.StatusCode, string(body))
			log.Printf("[Upstream] Request to %s failed (%d): %s", base, resp.StatusCode, string(body))
		}
		acc.Release()
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
