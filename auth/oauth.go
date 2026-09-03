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
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

type PKCESession struct {
	Verifier  string
	ExpiresAt time.Time
}

var (
	pkceStore = make(map[string]PKCESession)
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

	pkceStore[state] = PKCESession{
		Verifier:  verifier,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

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
	session, ok := pkceStore[state]
	if !ok || time.Now().After(session.ExpiresAt) {
		return "", "", "", fmt.Errorf("invalid or expired OAuth session")
	}
	delete(pkceStore, state)

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
		return "", "", "", fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", "", fmt.Errorf("failed to parse tokens: %w", err)
	}

	// Fetch user email
	userReq, _ := http.NewRequest("GET", OAuthUserInfoURL, nil)
	userReq.Header.Set("Authorization", "Bearer "+res.AccessToken)
	userResp, err := client.Do(userReq)
	if err == nil && userResp.StatusCode == http.StatusOK {
		var uInfo struct {
			Email string `json:"email"`
		}
		json.NewDecoder(userResp.Body).Decode(&uInfo)
		userResp.Body.Close()
		email = uInfo.Email
	}

	if email == "" {
		email = "google-user"
	}

	return res.RefreshToken, res.AccessToken, email, nil
}

// StartOAuthCallbackListener launches temporary listener on port 51121 to capture authorization code
func StartOAuthCallbackListener() {
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
			<h1 style="color:#10b981;">✓ Successfully Authenticated</h1>
			<p>Account <b>%s</b> added to Antigravity pool.</p>
			<p style="color:#8b9bb4;margin-top:1rem;">You may now close this window.</p>
			<script>setTimeout(() => window.location.href='http://localhost:8964/', 2000);</script>
		</body></html>`, email)

		log.Printf("[OAuth] Successfully authenticated new account: %s", email)
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[OAuth] Callback listener closed: %v", err)
		}
	}()
}
