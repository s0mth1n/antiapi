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
