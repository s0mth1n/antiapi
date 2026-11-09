package transport

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// BuildUserAgent dynamically generates a platform-matching user agent.
func BuildUserAgent() string {
	if custom := os.Getenv("ANTIGRAVITY_USER_AGENT"); custom != "" {
		return custom
	}
	osName := runtime.GOOS
	arch := runtime.GOARCH
	if osName == "android" {
		osName = "linux"
		arch = "amd64"
	}
	return fmt.Sprintf("antigravity/hub/2.9.1 %s/%s", osName, arch)
}

// SetStandardHeaders applies authentic Antigravity 2.0 / CLI metadata headers
func SetStandardHeaders(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", BuildUserAgent())
	req.Header.Set("X-Goog-Api-Client", fmt.Sprintf("antigravity-ide/2.9.1 gl-go/%s", runtime.Version()))
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
}

// StealthTransport provides browser-grade TLS fingerprint emulation (Chrome / Electron)
// matching the JA3/JA4 signatures expected by Google's backend edge proxies.
type StealthTransport struct {
	h2Transport   *http2.Transport
	h1Transport   *http.Transport
	useStealth    bool
	clientHelloID utls.ClientHelloID
}

// NewStealthTransport initializes a stealth transport mimicking Chromium/Electron.
func NewStealthTransport() http.RoundTripper {
	stealthEnv := strings.ToLower(strings.TrimSpace(os.Getenv("ANTIAPI_STEALTH_TLS")))
	if stealthEnv == "0" || stealthEnv == "false" || stealthEnv == "off" {
		log.Printf("[Transport] Stealth TLS disabled by environment; using Go standard crypto/tls")
		return http.DefaultTransport
	}

	st := &StealthTransport{
		useStealth:    true,
		clientHelloID: utls.HelloChrome_Auto,
	}

	st.h2Transport = &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			dialer := &net.Dialer{
				Timeout:       5 * time.Second,
				KeepAlive:     15 * time.Second,
				FallbackDelay: 200 * time.Millisecond,
			}

			var rawConn net.Conn
			proxyFunc := http.ProxyFromEnvironment
			dummyReq := &http.Request{URL: &url.URL{Scheme: "https", Host: addr}}
			proxyURL, proxyErr := proxyFunc(dummyReq)
			if proxyErr == nil && proxyURL != nil {
				proxyConn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
				if err != nil {
					return nil, fmt.Errorf("failed to dial proxy %s: %w", proxyURL.Host, err)
				}
				connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
				if proxyURL.User != nil {
					basicAuth := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.String()))
					connectReq = fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", addr, addr, basicAuth)
				}
				if _, err := proxyConn.Write([]byte(connectReq)); err != nil {
					proxyConn.Close()
					return nil, fmt.Errorf("proxy CONNECT write failed: %w", err)
				}
				resp, err := http.ReadResponse(bufio.NewReader(proxyConn), dummyReq)
				if err != nil {
					proxyConn.Close()
					return nil, fmt.Errorf("proxy CONNECT response failed: %w", err)
				}
				if resp.StatusCode != http.StatusOK {
					proxyConn.Close()
					return nil, fmt.Errorf("proxy CONNECT returned status %d", resp.StatusCode)
				}
				rawConn = proxyConn
			} else {
				var err error
				rawConn, err = dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
			}

			uConn := utls.UClient(rawConn, &utls.Config{
				ServerName: host,
			}, st.clientHelloID)

			if err := uConn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("utls handshake failed: %w", err)
			}

			return uConn, nil
		},
		DisableCompression:         false,
		AllowHTTP:                  false,
		ReadIdleTimeout:            120 * time.Second,
		PingTimeout:                5 * time.Second,
		StrictMaxConcurrentStreams: false,
	}

	st.h1Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:       5 * time.Second,
			KeepAlive:     30 * time.Second,
			FallbackDelay: 200 * time.Millisecond,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	return st
}

func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

func (st *StealthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !st.useStealth || req.URL.Scheme != "https" {
		return st.h1Transport.RoundTrip(req)
	}

	resp, err := st.h2Transport.RoundTrip(req)
	if err == nil {
		return resp, nil
	}

	if req.Context().Err() != nil {
		return nil, req.Context().Err()
	}

	// If connection was closed or broken, close idle connections and retry once with fresh H2
	if isConnectionClosedError(err) && req.GetBody != nil {
		st.h2Transport.CloseIdleConnections()
		if b, getErr := req.GetBody(); getErr == nil {
			req.Body = b
			resp2, err2 := st.h2Transport.RoundTrip(req)
			if err2 == nil {
				return resp2, nil
			}
			err = err2
		}
	}

	// Rewind request body if consumed by H2 transport
	if req.GetBody != nil {
		if b, getErr := req.GetBody(); getErr == nil {
			req.Body = b
		}
	}

	log.Printf("[Transport] HTTP/2 stealth attempt note: %v. Falling back to HTTP/1.1", err)
	return st.h1Transport.RoundTrip(req)
}

var (
	sharedTransport     http.RoundTripper
	sharedTransportOnce sync.Once

	accountTransportsMu sync.RWMutex
	accountTransports   = make(map[string]http.RoundTripper)
)

// GetAccountTransport returns an isolated StealthTransport instance per account
// so different Google accounts never multiplex over the same HTTP/2 TCP connection.
func GetAccountTransport(accountID string) http.RoundTripper {
	if accountID == "" {
		return GetSharedTransport()
	}
	accountTransportsMu.RLock()
	t, ok := accountTransports[accountID]
	accountTransportsMu.RUnlock()
	if ok {
		return t
	}

	accountTransportsMu.Lock()
	defer accountTransportsMu.Unlock()
	if t, ok := accountTransports[accountID]; ok {
		return t
	}
	t = NewStealthTransport()
	accountTransports[accountID] = t
	return t
}

// GetSharedTransport returns a singleton StealthTransport instance so TLS
// handshakes and HTTP/2 multiplexed connections are amortized across all requests.
func GetSharedTransport() http.RoundTripper {
	sharedTransportOnce.Do(func() {
		sharedTransport = NewStealthTransport()
	})
	return sharedTransport
}

// NewClient returns a configured http.Client with Chrome TLS emulation and timeout,
// backed by the shared persistent StealthTransport connection pool.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: GetSharedTransport(),
		Timeout:   timeout,
	}
}

// NewAccountClient returns an http.Client backed by an isolated StealthTransport
// specific to the given account ID, preventing cross-account HTTP/2 stream multiplexing.
func NewAccountClient(accountID string, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: GetAccountTransport(accountID),
		Timeout:   timeout,
	}
}

// PrewarmConnection proactively initiates an HTTP/2 handshake in the background
// so the initial user completion has zero TLS handshake latency.
func PrewarmConnection(baseURL string) {
	go func() {
		client := NewClient(5 * time.Second)
		prewarmOn(client, baseURL)
	}()
}

// PrewarmAccountConnection warms the per-account transport that completions
// actually use. PrewarmConnection alone only warms the shared transport,
// which never serves a completion.
func PrewarmAccountConnection(accountID, baseURL string) {
	go func() {
		client := NewAccountClient(accountID, 5*time.Second)
		prewarmOn(client, baseURL)
	}()
}

func prewarmOn(client *http.Client, baseURL string) {
	req, err := http.NewRequest(http.MethodHead, baseURL, nil)
	if err != nil {
		return
	}
	SetStandardHeaders(req, "")
	resp, err := client.Do(req)
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}
