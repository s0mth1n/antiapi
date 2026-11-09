package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// apiKey is the optional shared secret protecting the gateway. Empty means open
// (backwards-compatible local-only default). Set with -api-key or ANTIAPI_API_KEY.
var apiKey string

// SetAPIKey configures the shared secret required on API endpoints.
func SetAPIKey(key string) {
	apiKey = strings.TrimSpace(key)
}

// APIKeyConfigured reports whether endpoint authentication is active.
func APIKeyConfigured() bool {
	return apiKey != ""
}

func init() {
	SetAPIKey(os.Getenv("ANTIAPI_API_KEY"))
}

// requestAuthorized checks the key against Authorization: Bearer, x-api-key, or
// a ?key= query parameter (needed for browser dashboard and OAuth redirects).
func requestAuthorized(r *http.Request) bool {
	if apiKey == "" {
		return true
	}
	given := ""
	if h := r.Header.Get("Authorization"); h != "" {
		given = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if given == "" {
		given = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if given == "" {
		given = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	if given == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(apiKey)) == 1
}

// requireAPIKey rejects unauthenticated requests on API-bearing paths.
// /health stays open for liveness probes; OPTIONS preflights pass so CORS works.
func (s *Server) requireAPIKey(w http.ResponseWriter, r *http.Request) bool {
	if apiKey == "" || r.Method == http.MethodOptions {
		return true
	}
	if r.URL.Path == "/health" {
		return true
	}
	if requestAuthorized(r) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": "Invalid or missing API key. Pass Authorization: Bearer <key> or x-api-key.",
			"type":    "authentication_error",
			"code":    "invalid_api_key",
		},
	})
	w.Write(body)
	return false
}
