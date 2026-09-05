package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	OAuthCallbackPort = 51121
	OAuthRedirectURI  = "http://127.0.0.1:51121/oauth-callback"
	OAuthAuthURL      = "https://accounts.google.com/o/oauth2/v2/auth"
	OAuthUserInfoURL  = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
)

var OAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

type PKCESession struct {
	Verifier  string
	ExpiresAt time.Time
}

var (
	pkceMu    sync.RWMutex
	pkceStore = make(map[string]PKCESession)

	listenerMu     sync.Mutex
	listenerActive bool
)

func generateRandomString(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func GenerateAuthURL() (string, string) {
	state := generateRandomString(16)
	verifier := generateRandomString(32)

	h := sha256.New()
	h.Write([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h.Sum(nil))

	pkceMu.Lock()
	pkceStore[state] = PKCESession{
		Verifier:  verifier,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	pkceMu.Unlock()

	params := url.Values{}
	params.Set("client_id", ClientID)
	params.Set("redirect_uri", OAuthRedirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(OAuthScopes, " "))
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")

	return fmt.Sprintf("%s?%s", OAuthAuthURL, params.Encode()), state
}

func ExchangeCode(code, state string) (refreshToken, accessToken, email string, err error) {
	pkceMu.Lock()
	session, ok := pkceStore[state]
	if ok {
		delete(pkceStore, state)
	}
	pkceMu.Unlock()

	if !ok || time.Now().After(session.ExpiresAt) {
		return "", "", "", fmt.Errorf("invalid or expired OAuth session")
	}

	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", ClientID)
	form.Set("client_secret", ClientSecret)
	form.Set("redirect_uri", OAuthRedirectURI)
	form.Set("grant_type", "authorization_code")
	form.Set("code_verifier", session.Verifier)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(TokenURL, form)
	if err != nil {
		return "", "", "", fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", "", fmt.Errorf("invalid token JSON: %w", err)
	}

	// Fetch user email using newly acquired token
	userReq, _ := http.NewRequest("GET", OAuthUserInfoURL, nil)
	userReq.Header.Set("Authorization", "Bearer "+res.AccessToken)
	userResp, err := client.Do(userReq)
	if err == nil && userResp.StatusCode == http.StatusOK {
		var uInfo struct {
			Email string `json:"email"`
		}
		if json.NewDecoder(userResp.Body).Decode(&uInfo) == nil && uInfo.Email != "" {
			email = uInfo.Email
		}
		userResp.Body.Close()
	}

	if email == "" {
		email = "user-" + generateRandomString(6) + "@gmail.com"
	}

	return res.RefreshToken, res.AccessToken, email, nil
}

// StartOAuthCallbackListener launches temporary listener on port 51121 to capture authorization code
func StartOAuthCallbackListener() {
	listenerMu.Lock()
	if listenerActive {
		listenerMu.Unlock()
		return
	}
	listenerActive = true
	listenerMu.Unlock()

	mux := http.NewServeMux()
	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", OAuthCallbackPort),
		Handler: mux,
	}

	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		if code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}

		refresh, _, email, err := ExchangeCode(code, state)
		if err != nil {
			http.Error(w, "Authentication failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		GetPool().AddAccount(email, refresh, DefaultProject)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html><body style="background:#090d16;color:#f0f4fc;font-family:sans-serif;padding:3rem;text-align:center;">
			<h1 style="color:#10b981;">Successfully Authenticated</h1>
			<p>Account <b>%s</b> added to Antigravity pool.</p>
			<p style="color:#8b9bb4;margin-top:1rem;">You may now close this window.</p>
			<script>setTimeout(() => window.location.href='http://localhost:8964/', 2000);</script>
		</body></html>`, email)

		log.Printf("[OAuth] Successfully authenticated new account: %s", email)
	})

	go func() {
		defer func() {
			listenerMu.Lock()
			listenerActive = false
			listenerMu.Unlock()
		}()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[OAuth] Callback listener closed: %v", err)
		}
	}()
}
