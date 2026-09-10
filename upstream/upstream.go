package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"time"

	"antiapi/auth"
	"antiapi/translator"
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
	attemptedAccounts := make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		acc, err := pool.NextAvailableAccount(attemptedAccounts)
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

		endpoints := []string{DailyEndpoint, StandardEndpoint}
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

			resp, err := c.httpClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					acc.Release()
					return nil, ctx.Err()
				}
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
				if newToken, refErr := acc.Refresh(); refErr == nil {
					token = newToken
				}
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				saw429 = true
				log.Printf("[Upstream] 429 Quota Exceeded on %s via %s: %s", acc.Email, base, string(body))
				lastErr = fmt.Errorf("quota exceeded on %s: %s", acc.Email, string(body))
				continue // try next endpoint before failing over account
			}

			if resp.StatusCode == http.StatusForbidden {
				log.Printf("[Upstream] 403 Forbidden on %s: %s", acc.Email, string(body))
				acc.RecordError(http.StatusForbidden)
				lastErr = fmt.Errorf("account %s forbidden: %s", acc.Email, string(body))
				break // failover to next account in pool
			}

			if resp.StatusCode == http.StatusBadRequest {
				log.Printf("[Upstream] 400 Bad Request on %s: %s", acc.Email, string(body))
				acc.Release()
				return nil, fmt.Errorf("upstream rejected request (%d): %s", resp.StatusCode, string(body))
			}

			lastErr = fmt.Errorf("upstream error (%d): %s", resp.StatusCode, string(body))
			log.Printf("[Upstream] Request to %s failed (%d): %s", base, resp.StatusCode, string(body))
		}
		if saw429 {
			acc.RecordError(http.StatusTooManyRequests)
		}
		acc.Release()
	}

	return nil, lastErr
}

func (c *Client) streamReader(ctx context.Context, body io.ReadCloser, ch chan<- SSEEvent) {
	defer body.Close()
	defer close(ch)

	reader := bufio.NewReaderSize(body, 64*1024)

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
