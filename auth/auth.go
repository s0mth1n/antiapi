package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"antiapi/transport"
)

const (
	TokenURL       = "https://oauth2.googleapis.com/token"
	ProjectURL     = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	DefaultProject = "aicode-consumers"
)

type TokenManager struct {
	mu           sync.RWMutex
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	ProjectID    string    `json:"project_id"`
	Email        string    `json:"email"`
}

var (
	globalManager *TokenManager
	once          sync.Once
)

func GetManager() *TokenManager {
	once.Do(func() {
		globalManager = &TokenManager{
			ProjectID: DefaultProject,
		}
		if err := globalManager.AutoDiscover(); err != nil {
			log.Printf("[Auth] Auto-discovery note: %v", err)
		}
	})
	return globalManager
}

// AutoDiscover searches known storage locations for refresh tokens
func (tm *TokenManager) AutoDiscover() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// 1. Env variable
	if envToken := os.Getenv("ANTIGRAVITY_REFRESH_TOKEN"); envToken != "" {
		tm.RefreshToken = strings.TrimSpace(envToken)
		log.Printf("[Auth] Loaded refresh token from ANTIGRAVITY_REFRESH_TOKEN")
		return nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	// 2. ~/.anti-api/accounts.json
	antiApiPath := filepath.Join(homeDir, ".anti-api", "accounts.json")
	if data, err := os.ReadFile(antiApiPath); err == nil {
		type Account struct {
			Email        string `json:"email"`
			RefreshToken string `json:"refreshToken"`
			AccessToken  string `json:"accessToken"`
			ProjectID    string `json:"projectId"`
		}
		type Container struct {
			Accounts []Account `json:"accounts"`
		}
		var c Container
		if err := json.Unmarshal(data, &c); err == nil && len(c.Accounts) > 0 {
			tm.RefreshToken = c.Accounts[0].RefreshToken
			tm.AccessToken = c.Accounts[0].AccessToken
			tm.Email = c.Accounts[0].Email
			if c.Accounts[0].ProjectID != "" {
				tm.ProjectID = c.Accounts[0].ProjectID
			}
			log.Printf("[Auth] Auto-discovered account %s from %s", tm.Email, antiApiPath)
			return nil
		}
	}

	// 3. Scan Antigravity IDE state database files (Linux & macOS)
	dbCandidates := []string{
		filepath.Join(homeDir, "Library", "Application Support", "Antigravity", "User", "globalStorage", "state.vscdb"),
		filepath.Join(homeDir, "Library", "Application Support", "Code", "User", "globalStorage", "state.vscdb"),
		filepath.Join(homeDir, ".config", "Antigravity", "User", "globalStorage", "state.vscdb"),
		filepath.Join(homeDir, ".antigravity-ide", "User", "globalStorage", "state.vscdb"),
		filepath.Join(homeDir, ".config", "Code", "User", "globalStorage", "state.vscdb"),
	}

	reRefreshToken := regexp.MustCompile(`1//[0-9a-zA-Z_-]{30,}`)
	for _, dbPath := range dbCandidates {
		if content, err := os.ReadFile(dbPath); err == nil {
			if match := reRefreshToken.Find(content); match != nil {
				tm.RefreshToken = string(match)
				log.Printf("[Auth] Auto-discovered refresh token from IDE storage: %s", dbPath)
				return nil
			}
		}
	}

	return fmt.Errorf("no Antigravity credentials found. Provide ANTIGRAVITY_REFRESH_TOKEN or login")
}

// GetAccessToken returns a valid access token, refreshing if expired
func (tm *TokenManager) GetAccessToken() (string, error) {
	tm.mu.RLock()
	valid := tm.AccessToken != "" && time.Now().Add(2*time.Minute).Before(tm.ExpiresAt)
	token := tm.AccessToken
	tm.mu.RUnlock()

	if valid {
		return token, nil
	}

	return tm.Refresh()
}

func (tm *TokenManager) Refresh() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token available")
	}
	if !OAuthConfigured() {
		return "", ErrOAuthNotConfigured
	}

	form := url.Values{}
	form.Set("client_id", OAuthClientID())
	form.Set("client_secret", OAuthClientSecret())
	form.Set("refresh_token", tm.RefreshToken)
	form.Set("grant_type", "refresh_token")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(TokenURL, form)
	if err != nil {
		return "", fmt.Errorf("failed to contact token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token refresh failed (%d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("invalid token response: %w", err)
	}

	if res.AccessToken == "" {
		return "", fmt.Errorf("no access token in response: %s", string(body))
	}

	tm.AccessToken = res.AccessToken
	expiresIn := res.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	tm.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)

	log.Printf("[Auth] Refreshed Google access token successfully (expires in %ds)", expiresIn)

	// Attempt to resolve ProjectID if default
	if tm.ProjectID == DefaultProject || tm.ProjectID == "" {
		go tm.resolveProjectID(tm.AccessToken)
	}

	return tm.AccessToken, nil
}

func (tm *TokenManager) resolveProjectID(token string) {
	reqBody, _ := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideType": "ANTIGRAVITY",
		},
	})
	req, _ := http.NewRequest("POST", ProjectURL, bytes.NewBuffer(reqBody))
	transport.SetStandardHeaders(req, token)

	client := transport.NewClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	var result struct {
		CloudAICompanionProject string `json:"cloudaicompanionProject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.CloudAICompanionProject != "" {
		tm.mu.Lock()
		tm.ProjectID = result.CloudAICompanionProject
		tm.mu.Unlock()
		log.Printf("[Auth] Resolved Antigravity project ID: %s", result.CloudAICompanionProject)
	}
}

func (tm *TokenManager) GetProjectID() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.ProjectID != "" {
		return tm.ProjectID
	}
	return DefaultProject
}

func (tm *TokenManager) GetAccountEmail() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.Email
}
