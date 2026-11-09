package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"antiapi/transport"
)

type Account struct {
	ID                 string             `json:"id"`
	Email              string             `json:"email"`
	RefreshToken       string             `json:"refreshToken"`
	AccessToken        string             `json:"accessToken"`
	ExpiresAt          time.Time          `json:"expiresAt"`
	ProjectID          string             `json:"projectId"`
	RateLimitedUntil   time.Time          `json:"rateLimitedUntil,omitempty"`
	QuarantinedUntil   time.Time          `json:"quarantinedUntil,omitempty"`
	ValidationURL      string             `json:"validationUrl,omitempty"`
	ConsecutiveErrors  int                `json:"consecutiveErrors,omitempty"`
	Quotas             map[string]float64 `json:"quotas,omitempty"` // Tier -> percentage
	LastQuotaSummary   *UserQuotaSummary  `json:"-"`
	LastQuotaFetchTime time.Time          `json:"-"`
	sem                chan struct{}      `json:"-"`
	semOnce            sync.Once          `json:"-"`
	mu                 sync.RWMutex
}

func (a *Account) getSem() chan struct{} {
	a.semOnce.Do(func() {
		maxConc := 4
		if envVal := os.Getenv("ANTIAPI_MAX_CONCURRENCY"); envVal != "" {
			if parsed, err := strconv.Atoi(envVal); err == nil && parsed > 0 {
				maxConc = parsed
			}
		}
		a.sem = make(chan struct{}, maxConc)
	})
	return a.sem
}

func (a *Account) AvailableSlots() int {
	sem := a.getSem()
	return cap(sem) - len(sem)
}

func (a *Account) Acquire(ctx context.Context) error {
	sem := a.getSem()
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Account) Release() {
	sem := a.getSem()
	select {
	case <-sem:
	default:
	}
}

func (a *Account) IsRateLimited() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().Before(a.RateLimitedUntil)
}

func (a *Account) IsQuarantined() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().Before(a.QuarantinedUntil)
}

func (a *Account) IsAvailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	return !now.Before(a.RateLimitedUntil) && !now.Before(a.QuarantinedUntil)
}

// HasHealthyQuota returns false if known remaining quota for any tier is dangerously low (< 5%)
func (a *Account) HasHealthyQuota() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.LastQuotaSummary == nil {
		return true // No data yet, assume available
	}
	for _, g := range a.LastQuotaSummary.Groups {
		for _, b := range g.Buckets {
			if b.RemainingFraction > 0 && b.RemainingFraction < 0.05 {
				return false
			}
		}
	}
	return true
}

func (a *Account) ClearRateLimit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.RateLimitedUntil = time.Time{}
	a.QuarantinedUntil = time.Time{}
}

func (a *Account) RecordSuccess() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ConsecutiveErrors = 0
	a.RateLimitedUntil = time.Time{}
	a.QuarantinedUntil = time.Time{}
	a.ValidationURL = ""
}

// SetValidationURL records the interactive verification URL Google returned
// with a 403 VALIDATION_REQUIRED so UIs can link the user straight to it.
func (a *Account) SetValidationURL(url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ValidationURL = url
}

// GetValidationURL returns the pending interactive verification URL, if any.
func (a *Account) GetValidationURL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ValidationURL
}

func (a *Account) RecordError(statusCode int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if statusCode == http.StatusTooManyRequests { // 429
		a.RateLimitedUntil = time.Now().Add(5 * time.Minute)
		log.Printf("[Pool] Account %s rate-limited for 5m on 429 (until %s)", a.Email, a.RateLimitedUntil.Format(time.Kitchen))
		return
	}

	if statusCode == http.StatusForbidden { // 403 (Validation Required or Revoked)
		a.QuarantinedUntil = time.Now().Add(15 * time.Minute)
		log.Printf("[Pool] Account %s quarantined for 15m on 403 validation (until %s)", a.Email, a.QuarantinedUntil.Format(time.Kitchen))
		return
	}
}

func (a *Account) GetAccessToken() (string, error) {
	a.mu.RLock()
	valid := a.AccessToken != "" && time.Now().Add(2*time.Minute).Before(a.ExpiresAt)
	token := a.AccessToken
	a.mu.RUnlock()

	if valid {
		return token, nil
	}

	return a.Refresh()
}

func (a *Account) Refresh() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-checked locking: check if refreshed by another concurrent caller while waiting for lock
	if a.AccessToken != "" && time.Now().Add(2*time.Minute).Before(a.ExpiresAt) {
		return a.AccessToken, nil
	}

	if a.RefreshToken == "" {
		return "", fmt.Errorf("account %s has no refresh token", a.Email)
	}

	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("client_secret", ClientSecret)
	form.Set("refresh_token", a.RefreshToken)
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
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("invalid token response: %w", err)
	}

	a.AccessToken = res.AccessToken
	expiresIn := res.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	a.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	log.Printf("[Pool] Refreshed token for %s (expires in %ds)", a.Email, expiresIn)

	if a.ProjectID == "" || a.ProjectID == DefaultProject {
		go a.resolveProjectID()
	}
	if strings.Contains(a.Email, "account") || strings.Contains(a.Email, "user-") || strings.Contains(a.Email, "ide") {
		go a.resolveUserInfo()
	}

	return a.AccessToken, nil
}

func (a *Account) resolveUserInfo() {
	userReq, _ := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	userReq.Header.Set("Authorization", "Bearer "+a.AccessToken)
	client := &http.Client{Timeout: 10 * time.Second}
	userResp, err := client.Do(userReq)
	if err == nil && userResp.StatusCode == http.StatusOK {
		var uInfo struct {
			Email string `json:"email"`
		}
		if json.NewDecoder(userResp.Body).Decode(&uInfo) == nil && uInfo.Email != "" {
			a.mu.Lock()
			a.Email = uInfo.Email
			a.ID = uInfo.Email
			a.mu.Unlock()
			log.Printf("[Pool] Resolved real email for account: %s", uInfo.Email)
			if globalPool != nil {
				globalPool.Save()
			}
		}
		userResp.Body.Close()
	}
}

func (a *Account) resolveProjectID() {
	reqBody, _ := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideType": "ANTIGRAVITY",
		},
	})
	req, _ := http.NewRequest("POST", ProjectURL, bytes.NewBuffer(reqBody))
	transport.SetStandardHeaders(req, a.AccessToken)

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
		a.mu.Lock()
		a.ProjectID = result.CloudAICompanionProject
		a.mu.Unlock()
		log.Printf("[Pool] Resolved project for %s: %s", a.Email, a.ProjectID)
	}
}

type sessionBinding struct {
	email    string
	lastUsed time.Time
}

type AccountPool struct {
	mu              sync.RWMutex
	accounts        []*Account
	currentIndex    int
	storagePath     string
	sessionMu       sync.RWMutex
	sessionBindings map[string]sessionBinding
}

var (
	globalPool *AccountPool
	poolOnce   sync.Once
)

func GetPool() *AccountPool {
	poolOnce.Do(func() {
		homeDir, _ := os.UserHomeDir()
		storageDir := filepath.Join(homeDir, ".antigravity-api")
		os.MkdirAll(storageDir, 0700)
		storagePath := filepath.Join(storageDir, "accounts.json")

		globalPool = &AccountPool{
			accounts:        []*Account{},
			storagePath:     storagePath,
			sessionBindings: make(map[string]sessionBinding),
		}
		globalPool.Load()
		globalPool.startBackgroundRefresher()
	})
	return globalPool
}

func (p *AccountPool) startBackgroundRefresher() {
	go func() {
		ticker := time.NewTicker(3 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			p.refreshExpiringTokens()
		}
	}()
}

func (p *AccountPool) refreshExpiringTokens() {
	p.mu.RLock()
	accs := make([]*Account, len(p.accounts))
	copy(accs, p.accounts)
	p.mu.RUnlock()

	threshold := time.Now().Add(10 * time.Minute)
	for _, acc := range accs {
		acc.mu.RLock()
		needsRefresh := acc.RefreshToken != "" && (acc.AccessToken == "" || threshold.After(acc.ExpiresAt))
		email := acc.Email
		acc.mu.RUnlock()

		if needsRefresh {
			if _, err := acc.Refresh(); err == nil {
				log.Printf("[Pool] Proactively refreshed token for %s", email)
			}
		}
	}
}

func (p *AccountPool) Load() {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 0. Environment variable takes top priority so headless/docker runs need
	// no files at all (README documents this as Method B).
	if envToken := strings.TrimSpace(os.Getenv("ANTIGRAVITY_REFRESH_TOKEN")); envToken != "" {
		p.accounts = append(p.accounts, &Account{
			ID:           "env-account",
			Email:        "env-user",
			RefreshToken: envToken,
			ProjectID:    DefaultProject,
			Quotas:       make(map[string]float64),
		})
		log.Printf("[Pool] Loaded account from ANTIGRAVITY_REFRESH_TOKEN")
		return
	}

	// 1. Try ~/.antigravity-api/accounts.json
	if data, err := os.ReadFile(p.storagePath); err == nil {
		type FileFormat struct {
			Accounts []struct {
				ID           string `json:"id"`
				Email        string `json:"email"`
				RefreshToken string `json:"refreshToken"`
				AccessToken  string `json:"accessToken"`
				ProjectID    string `json:"projectId"`
			} `json:"accounts"`
		}
		var ff FileFormat
		if json.Unmarshal(data, &ff) == nil && len(ff.Accounts) > 0 {
			for _, item := range ff.Accounts {
				p.accounts = append(p.accounts, &Account{
					ID:           item.ID,
					Email:        item.Email,
					RefreshToken: item.RefreshToken,
					AccessToken:  item.AccessToken,
					ProjectID:    item.ProjectID,
					Quotas:       make(map[string]float64),
				})
			}
			log.Printf("[Pool] Loaded %d accounts from %s", len(p.accounts), p.storagePath)
			return
		}
	}

	// 2. Import from ~/.anti-api/accounts.json
	homeDir, _ := os.UserHomeDir()
	antiApiPath := filepath.Join(homeDir, ".anti-api", "accounts.json")
	if data, err := os.ReadFile(antiApiPath); err == nil {
		type FileFormat struct {
			Accounts []struct {
				ID           string `json:"id"`
				Email        string `json:"email"`
				RefreshToken string `json:"refreshToken"`
				AccessToken  string `json:"accessToken"`
				ProjectID    string `json:"projectId"`
			} `json:"accounts"`
		}
		var ff FileFormat
		if json.Unmarshal(data, &ff) == nil && len(ff.Accounts) > 0 {
			for _, item := range ff.Accounts {
				p.accounts = append(p.accounts, &Account{
					ID:           item.ID,
					Email:        item.Email,
					RefreshToken: item.RefreshToken,
					AccessToken:  item.AccessToken,
					ProjectID:    item.ProjectID,
					Quotas:       make(map[string]float64),
				})
			}
			log.Printf("[Pool] Imported %d accounts from %s", len(p.accounts), antiApiPath)
			p.saveLocked()
			return
		}
	}

	// 3. Fallback: discover from IDE state DB (Linux & macOS)
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
				token := string(match)
				p.accounts = append(p.accounts, &Account{
					ID:           "ide-account",
					Email:        "ide-user",
					RefreshToken: token,
					ProjectID:    DefaultProject,
					Quotas:       make(map[string]float64),
				})
				log.Printf("[Pool] Discovered account from IDE storage: %s", dbPath)
				p.saveLocked()
				return
			}
		}
	}
}

func (p *AccountPool) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

func (p *AccountPool) saveLocked() error {
	type SavedAccount struct {
		ID           string `json:"id"`
		Email        string `json:"email"`
		RefreshToken string `json:"refreshToken"`
		AccessToken  string `json:"accessToken"`
		ProjectID    string `json:"projectId"`
	}
	type FileFormat struct {
		Accounts []SavedAccount `json:"accounts"`
	}

	var ff FileFormat
	for _, acc := range p.accounts {
		acc.mu.RLock()
		ff.Accounts = append(ff.Accounts, SavedAccount{
			ID:           acc.ID,
			Email:        acc.Email,
			RefreshToken: acc.RefreshToken,
			AccessToken:  acc.AccessToken,
			ProjectID:    acc.ProjectID,
		})
		acc.mu.RUnlock()
	}

	data, err := json.MarshalIndent(ff, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := p.storagePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write tmp accounts file: %w", err)
	}
	if err := os.Rename(tmpPath, p.storagePath); err != nil {
		return fmt.Errorf("failed to atomically rename accounts file: %w", err)
	}
	return nil
}

func (p *AccountPool) AddAccount(email, refreshToken, projectID string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if exists
	for _, a := range p.accounts {
		if a.Email == email || a.RefreshToken == refreshToken {
			a.mu.Lock()
			a.Email = email
			a.RefreshToken = refreshToken
			if projectID != "" {
				a.ProjectID = projectID
			}
			a.mu.Unlock()
			p.saveLocked()
			return a
		}
	}

	acc := &Account{
		ID:           email,
		Email:        email,
		RefreshToken: refreshToken,
		ProjectID:    projectID,
		Quotas:       make(map[string]float64),
	}
	if acc.ProjectID == "" {
		acc.ProjectID = DefaultProject
	}
	p.accounts = append(p.accounts, acc)
	p.saveLocked()
	log.Printf("[Pool] Added account %s to pool (total: %d)", email, len(p.accounts))
	return acc
}

func (p *AccountPool) RemoveAccount(email string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx := -1
	for i, a := range p.accounts {
		if a.Email == email || a.ID == email {
			idx = i
			break
		}
	}
	if idx >= 0 {
		p.accounts = append(p.accounts[:idx], p.accounts[idx+1:]...)
		p.saveLocked()
		log.Printf("[Pool] Removed account %s (remaining: %d)", email, len(p.accounts))
		return true
	}
	return false
}

// NextAvailableAccount selects an account using round-robin across healthy and available accounts.
// An optional exclusion map can be supplied to skip accounts that have already failed during a retry chain.
func (p *AccountPool) NextAvailableAccount(exclude ...map[string]bool) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.accounts)
	if n == 0 {
		return nil, fmt.Errorf("no accounts in pool")
	}

	var excluded map[string]bool
	if len(exclude) > 0 {
		excluded = exclude[0]
	}

	// Priority 1: Check accounts that are available, have healthy quota, AND have available concurrency slots
	for i := 0; i < n; i++ {
		idx := (p.currentIndex + i) % n
		acc := p.accounts[idx]
		if excluded != nil && excluded[acc.Email] {
			continue
		}
		if acc.IsAvailable() && acc.HasHealthyQuota() && acc.AvailableSlots() > 0 {
			p.currentIndex = (idx + 1) % n
			return acc, nil
		}
	}

	// Priority 2: Check accounts that are available AND have healthy remaining quota (> 5%)
	for i := 0; i < n; i++ {
		idx := (p.currentIndex + i) % n
		acc := p.accounts[idx]
		if excluded != nil && excluded[acc.Email] {
			continue
		}
		if acc.IsAvailable() && acc.HasHealthyQuota() {
			p.currentIndex = (idx + 1) % n
			return acc, nil
		}
	}

	// Priority 2: Fallback to any available account (even if low quota)
	for i := 0; i < n; i++ {
		idx := (p.currentIndex + i) % n
		acc := p.accounts[idx]
		if excluded != nil && excluded[acc.Email] {
			continue
		}
		if acc.IsAvailable() {
			p.currentIndex = (idx + 1) % n
			return acc, nil
		}
	}

	// Priority 3: All accounts in cooldown; find the one with the earliest reset
	var bestAcc *Account
	var earliest time.Time
	for _, acc := range p.accounts {
		acc.mu.RLock()
		until := acc.RateLimitedUntil
		if acc.QuarantinedUntil.After(until) {
			until = acc.QuarantinedUntil
		}
		acc.mu.RUnlock()
		if bestAcc == nil || until.Before(earliest) {
			bestAcc = acc
			earliest = until
		}
	}

	if bestAcc != nil {
		return nil, fmt.Errorf("all %d accounts in pool are in cooldown (earliest unlock: %s at %s)", len(p.accounts), bestAcc.Email, earliest.Format(time.Kitchen))
	}
	return nil, fmt.Errorf("no available accounts in pool")
}

// GetAccountForSession selects an account for a session, keeping multi-turn conversations
// pinned to the same account for prompt KV cache reuse. Falls back to round-robin on failover.
func (p *AccountPool) GetAccountForSession(sessionID string, exclude ...map[string]bool) (*Account, error) {
	var excluded map[string]bool
	if len(exclude) > 0 {
		excluded = exclude[0]
	}

	if sessionID != "" {
		p.sessionMu.RLock()
		binding, exists := p.sessionBindings[sessionID]
		p.sessionMu.RUnlock()

		if exists && (excluded == nil || !excluded[binding.email]) {
			p.mu.RLock()
			var boundAcc *Account
			for _, a := range p.accounts {
				if a.Email == binding.email {
					boundAcc = a
					break
				}
			}
			p.mu.RUnlock()

			if boundAcc != nil && boundAcc.IsAvailable() && boundAcc.HasHealthyQuota() && boundAcc.AvailableSlots() > 0 {
				p.sessionMu.Lock()
				p.sessionBindings[sessionID] = sessionBinding{
					email:    binding.email,
					lastUsed: time.Now(),
				}
				p.sessionMu.Unlock()
				return boundAcc, nil
			}
		}
	}

	acc, err := p.NextAvailableAccount(exclude...)
	if err != nil {
		return nil, err
	}

	if sessionID != "" {
		p.sessionMu.Lock()
		p.sessionBindings[sessionID] = sessionBinding{
			email:    acc.Email,
			lastUsed: time.Now(),
		}
		now := time.Now()
		for k, v := range p.sessionBindings {
			if now.Sub(v.lastUsed) > time.Hour {
				delete(p.sessionBindings, k)
			}
		}
		p.sessionMu.Unlock()
	}

	return acc, nil
}

// ReorderAccounts sets the priority order of accounts based on the provided list of emails
func (p *AccountPool) ReorderAccounts(orderedEmails []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	emailMap := make(map[string]*Account)
	for _, a := range p.accounts {
		emailMap[a.Email] = a
	}

	newAccounts := make([]*Account, 0, len(p.accounts))
	for _, email := range orderedEmails {
		if acc, ok := emailMap[email]; ok {
			newAccounts = append(newAccounts, acc)
			delete(emailMap, email)
		}
	}

	// Append any remaining accounts that were not in the reorder list
	for _, acc := range p.accounts {
		if _, ok := emailMap[acc.Email]; ok {
			newAccounts = append(newAccounts, acc)
		}
	}

	p.accounts = newAccounts
	p.currentIndex = 0
	log.Printf("[Pool] Reordered accounts by priority: %v", orderedEmails)
	return p.saveLocked()
}

func (p *AccountPool) ClearAllRateLimits() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		a.ClearRateLimit()
	}
}

func (p *AccountPool) GetAllAccounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	res := make([]*Account, len(p.accounts))
	copy(res, p.accounts)
	return res
}

func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

type QuotaBucket struct {
	BucketID          string  `json:"bucketId"`
	DisplayName       string  `json:"displayName"`
	Window            string  `json:"window"`
	ResetTime         string  `json:"resetTime"`
	Description       string  `json:"description"`
	RemainingFraction float64 `json:"remainingFraction"`
}

type QuotaGroup struct {
	DisplayName string        `json:"displayName"`
	Description string        `json:"description"`
	Buckets     []QuotaBucket `json:"buckets"`
}

type UserQuotaSummary struct {
	Groups      []QuotaGroup `json:"groups"`
	Description string       `json:"description"`
}

func (a *Account) FetchQuotaSummary() (*UserQuotaSummary, error) {
	a.mu.RLock()
	if a.LastQuotaSummary != nil && time.Since(a.LastQuotaFetchTime) < 60*time.Second {
		summary := a.LastQuotaSummary
		a.mu.RUnlock()
		return summary, nil
	}
	a.mu.RUnlock()

	token, err := a.GetAccessToken()
	if err != nil {
		a.mu.RLock()
		cached := a.LastQuotaSummary
		a.mu.RUnlock()
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}

	project := a.ProjectID
	if project == "" {
		project = DefaultProject
	}

	reqData, _ := json.Marshal(map[string]string{"project": project})
	endpoints := []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
		"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	}

	client := &http.Client{Timeout: 6 * time.Second}
	var lastErr error

	for _, ep := range endpoints {
		req, _ := http.NewRequest("POST", ep, bytes.NewBuffer(reqData))
		transport.SetStandardHeaders(req, token)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var summary UserQuotaSummary
			err := json.NewDecoder(resp.Body).Decode(&summary)
			resp.Body.Close()
			if err == nil {
				a.mu.Lock()
				a.LastQuotaSummary = &summary
				a.LastQuotaFetchTime = time.Now()
				a.mu.Unlock()
				return &summary, nil
			}
		} else {
			resp.Body.Close()
		}
		lastErr = fmt.Errorf("retrieveUserQuotaSummary returned %d", resp.StatusCode)
	}

	// Record fetch time even on error to throttle repeated checks on failing or rate-limited accounts
	a.mu.Lock()
	a.LastQuotaFetchTime = time.Now()
	a.mu.Unlock()

	// Fall back to cached summary if available so temporary upstream rate-limits don't blank out state
	a.mu.RLock()
	cached := a.LastQuotaSummary
	a.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	return nil, lastErr
}
