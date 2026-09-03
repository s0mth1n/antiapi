package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// StealthTransport provides browser-grade TLS fingerprint emulation (Chrome / Electron)
// matching the JA3/JA4 signatures expected by Google's backend edge proxies.
type StealthTransport struct {
	h2Transport  *http2.Transport
	h1Transport  *http.Transport
	useStealth   bool
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
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}

			rawConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
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
		DisableCompression: false,
		AllowHTTP:          false,
	}

	st.h1Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	return st
}

func (st *StealthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !st.useStealth || req.URL.Scheme != "https" {
		return st.h1Transport.RoundTrip(req)
	}

	// For Google's HTTPS endpoints, route through HTTP/2 with Chrome TLS handshake
	resp, err := st.h2Transport.RoundTrip(req)
	if err == nil {
		return resp, nil
	}

	// If HTTP/2 failed with a transport-level error, attempt HTTP/1.1 fallback
	log.Printf("[Transport] HTTP/2 stealth attempt note: %v. Falling back to HTTP/1.1", err)
	return st.h1Transport.RoundTrip(req)
}
