package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealth(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var res map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", res["status"])
	}
}

func TestConnectRPCHeartbeat(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/exa.language_server_pb.LanguageServerService/Heartbeat", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var res struct {
		Response struct {
			Healthy bool `json:"healthy"`
		} `json:"response"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !res.Response.Healthy {
		t.Errorf("expected healthy true")
	}
}

func TestConnectRPCServerConfiguration(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/exa.language_server_pb.LanguageServerService/GetServerConfiguration", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var res struct {
		Response struct {
			Version      string   `json:"version"`
			Capabilities []string `json:"serverCapabilities"`
			DefaultModel string   `json:"defaultModel"`
		} `json:"response"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Response.Version != "2.6.0" {
		t.Errorf("expected version '2.6.0', got %q", res.Response.Version)
	}
	if len(res.Response.Capabilities) == 0 {
		t.Errorf("expected non-empty serverCapabilities")
	}
}

func TestConnectRPCUserStatus(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/exa.language_server_pb.LanguageServerService/GetUserStatus", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var res struct {
		UserStatus struct {
			Email string `json:"email"`
		} `json:"userStatus"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.UserStatus.Email == "" {
		t.Errorf("expected non-empty user email")
	}
}

func TestModelsEndpoint(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var res struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Object != "list" || len(res.Data) == 0 {
		t.Errorf("unexpected models response: %v", res)
	}
}

func TestSingleModelEndpoint(t *testing.T) {
	srv := NewServer()

	// Test GET /v1/models/gemini-3.8-flash
	req := httptest.NewRequest(http.MethodGet, "/v1/models/gemini-3.8-flash", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var modelItem struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		OwnedBy       string `json:"owned_by"`
		ContextWindow int    `json:"context_window"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &modelItem); err != nil {
		t.Fatalf("failed to unmarshal model item: %v", err)
	}
	if modelItem.ID != "gemini-3.8-flash" {
		t.Errorf("expected ID 'gemini-3.8-flash', got %q", modelItem.ID)
	}
	if modelItem.Object != "model" {
		t.Errorf("expected object 'model', got %q", modelItem.Object)
	}
	if modelItem.ContextWindow != 1048576 {
		t.Errorf("expected context_window 1048576, got %d", modelItem.ContextWindow)
	}
}

func TestSingleModelNotFound(t *testing.T) {
	srv := NewServer()

	req := httptest.NewRequest(http.MethodGet, "/v1/models/nonexistent-model-xyz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", w.Code)
	}

	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	if errResp.Error.Code != "model_not_found" {
		t.Errorf("expected error code 'model_not_found', got %q", errResp.Error.Code)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type 'invalid_request_error', got %q", errResp.Error.Type)
	}
}

func TestIsFastModeRequest(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		hval     string
		query    string
		expected bool
	}{
		{"standard_request", "", "", "", false},
		{"header_x_antiapi_fast_mode_true", "X-Antiapi-Fast-Mode", "true", "", true},
		{"header_x_antiapi_fast_mode_1", "X-Antiapi-Fast-Mode", "1", "", true},
		{"header_x_fast_mode_true", "X-Fast-Mode", "true", "", true},
		{"header_x_fast_mode_0", "X-Fast-Mode", "0", "", false},
		{"query_fast_1", "", "", "?fast=1", true},
		{"query_fast_true", "", "", "?fast=true", true},
		{"query_fast_mode_1", "", "", "?fast_mode=1", true},
		{"query_fast_false", "", "", "?fast=false", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/v1/chat/completions" + tt.query
			req := httptest.NewRequest(http.MethodPost, url, nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.hval)
			}
			got := isFastModeRequest(req)
			if got != tt.expected {
				t.Errorf("isFastModeRequest() = %v; want %v", got, tt.expected)
			}
		})
	}
}

func TestFastModeAPI(t *testing.T) {
	srv := NewServer()

	// GET initial state
	req := httptest.NewRequest(http.MethodGet, "/api/fast_mode", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var res map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// POST toggle
	req = httptest.NewRequest(http.MethodPost, "/api/fast_mode", strings.NewReader(`{"toggle": true}`))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resPost map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resPost); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resPost["fast_mode"] == res["fast_mode"] {
		t.Errorf("expected toggle to change fast_mode state")
	}

	// Restore to false
	req = httptest.NewRequest(http.MethodPost, "/api/fast_mode", strings.NewReader(`{"fast_mode": false}`))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var resFalse map[string]bool
	json.Unmarshal(w.Body.Bytes(), &resFalse)
	if resFalse["fast_mode"] != false {
		t.Errorf("expected fast_mode == false")
	}
}

func TestTunnelAPI(t *testing.T) {
	srv := NewServer()

	// GET initial state
	req := httptest.NewRequest(http.MethodGet, "/api/tunnel", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var res struct {
		Active bool   `json:"active"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// POST stop
	req = httptest.NewRequest(http.MethodPost, "/api/tunnel", strings.NewReader(`{"action": "stop"}`))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestIsLocalOrigin(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8964":      true,
		"http://127.0.0.1:8964":      true,
		"https://[::1]:9000":         true,
		"https://evil.example.com":   false,
		"http://localhost.evil.com":  false,
		"http://127.0.0.1.evil.com":  false,
		"not a url":                  false,
	}
	for origin, want := range cases {
		if got := isLocalOrigin(origin); got != want {
			t.Errorf("isLocalOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestCORSKeylessReflectsOnlyLocalOrigins(t *testing.T) {
	SetAPIKey("")
	defer SetAPIKey("")

	handler := NewServer().cors(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	// Cross-site origin: no ACAO header, browser will block.
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if acao := rec.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("keyless cross-origin got ACAO %q, want empty", acao)
	}

	// Local origin: reflected.
	req = httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Origin", "http://localhost:8964")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if acao := rec.Header().Get("Access-Control-Allow-Origin"); acao != "http://localhost:8964" {
		t.Errorf("keyless local origin got ACAO %q", acao)
	}
}

func TestCORSWithKeyAllowsWildcard(t *testing.T) {
	SetAPIKey("test-key")
	defer SetAPIKey("")

	handler := NewServer().cors(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Origin", "https://anywhere.example.com")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if acao := rec.Header().Get("Access-Control-Allow-Origin"); acao != "*" {
		t.Errorf("with key configured got ACAO %q, want *", acao)
	}
}
