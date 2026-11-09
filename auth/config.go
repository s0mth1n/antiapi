package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// OAuth client credentials for the Antigravity IDE public client.
// They are intentionally NOT baked into the source tree. Provide them via:
//  1. Environment: ANTIAPI_OAUTH_CLIENT_ID and ANTIAPI_OAUTH_CLIENT_SECRET
//  2. Config file: ~/.antigravity-api/config.json
//     {"oauthClientId": "...", "oauthClientSecret": "..."}
//
// Both values are visible in plain text inside a local Antigravity IDE
// installation; see the README "OAuth client credentials" section.
var (
	clientID     string
	clientSecret string
	configOnce   sync.Once
)

func loadOAuthConfig() {
	clientID = os.Getenv("ANTIAPI_OAUTH_CLIENT_ID")
	clientSecret = os.Getenv("ANTIAPI_OAUTH_CLIENT_SECRET")
	if clientID != "" && clientSecret != "" {
		return
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(homeDir, ".antigravity-api", "config.json"))
	if err != nil {
		return
	}
	var cfg struct {
		OAuthClientID     string `json:"oauthClientId"`
		OAuthClientSecret string `json:"oauthClientSecret"`
	}
	if json.Unmarshal(data, &cfg) == nil {
		if clientID == "" {
			clientID = cfg.OAuthClientID
		}
		if clientSecret == "" {
			clientSecret = cfg.OAuthClientSecret
		}
	}
}

// OAuthClientID returns the configured client ID.
func OAuthClientID() string {
	configOnce.Do(loadOAuthConfig)
	return clientID
}

// OAuthClientSecret returns the configured client secret.
func OAuthClientSecret() string {
	configOnce.Do(loadOAuthConfig)
	return clientSecret
}

// OAuthConfigured reports whether token refresh / login can work.
func OAuthConfigured() bool {
	return OAuthClientID() != "" && OAuthClientSecret() != ""
}

// ErrOAuthNotConfigured is returned when no client credentials are available.
var ErrOAuthNotConfigured = fmt.Errorf("OAuth client credentials not configured: set ANTIAPI_OAUTH_CLIENT_ID and ANTIAPI_OAUTH_CLIENT_SECRET or add them to ~/.antigravity-api/config.json (see README)")
