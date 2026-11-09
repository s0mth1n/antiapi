package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withAPIKey(t *testing.T, key string) {
	t.Helper()
	SetAPIKey(key)
	t.Cleanup(func() { SetAPIKey("") })
}

func TestAPIKeyOpenByDefault(t *testing.T) {
	withAPIKey(t, "")
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("expected open access without configured key, got 401")
	}
}

func TestAPIKeyRequired(t *testing.T) {
	withAPIKey(t, "secret-key")
	srv := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without key, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected json content type, got %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 body is not valid json: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("401 body missing error object: %v", body)
	}
}

func TestAPIKeyAcceptedViaBearerXAPIKeyAndQuery(t *testing.T) {
	withAPIKey(t, "secret-key")
	srv := NewServer()

	cases := []struct {
		name  string
		setup func(r *http.Request)
		path  string
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret-key") }, "/v1/models"},
		{"x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "secret-key") }, "/v1/models"},
		{"query", func(r *http.Request) {}, "/v1/models?key=secret-key"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		tc.setup(req)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s: expected authorized request, got 401", tc.name)
		}
	}
}

func TestAPIKeyHealthAndPreflightExempt(t *testing.T) {
	withAPIKey(t, "secret-key")
	srv := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected /health to stay open, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("expected OPTIONS preflight to pass, got 401")
	}
}

func TestAPIKeyWrongKeyRejected(t *testing.T) {
	withAPIKey(t, "secret-key")
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong key, got %d", w.Code)
	}
}

func TestInjectDashboardKey(t *testing.T) {
	html := []byte("<html><head><title>x</title></head><body></body></html>")
	out := injectDashboardKey(html, "k\"ey")
	if !strings.Contains(string(out), `window.ANTIAPI_API_KEY="k\"ey"`) {
		t.Errorf("key not injected with safe escaping: %s", out)
	}
	if !strings.Contains(string(out), "</head>") {
		t.Errorf("closing head tag missing after injection")
	}
}

func TestTunnelWarningWithoutKey(t *testing.T) {
	withAPIKey(t, "")
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/tunnel", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Tunnel inactive: no warning expected
	if _, ok := body["warning"]; ok {
		t.Errorf("unexpected warning while tunnel inactive")
	}
}
