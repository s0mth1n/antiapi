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
)

type Account struct {
	ID                string             `json:"id"`
	Email             string             `json:"email"`
	RefreshToken      string             `json:"refreshToken"`
	AccessToken       string             `json:"accessToken"`
	ExpiresAt         time.Time          `json:"expiresAt"`
	ProjectID         string             `json:"projectId"`
	RateLimitedUntil  time.Time          `json:"rateLimitedUntil,omitempty"`
	QuarantinedUntil  time.Time          `json:"quarantinedUntil,omitempty"`
	ConsecutiveErrors int                `json:"consecutiveErrors,omitempty"`
	Quotas            map[string]float64 `json:"quotas,omitempty"` // Tier -> percentage
	LastQuotaSummary  *UserQuotaSummary  `json:"-"`
	sem               chan struct{}      `json:"-"`
	semOnce           sync.Once          `json:"-"`
	mu                sync.RWMutex
}

func (a *Account) getSem() chan struct{} {
	a.semOnce.Do(func() {
		maxConc := 2
		if envVal := os.Getenv("ANTIAPI_MAX_CONCURRENCY"); envVal != "" {
			if parsed, err := strconv.Atoi(envVal); err == nil && parsed > 0 {
				maxConc = parsed
			}
		}
		a.sem = make(chan struct{}, maxConc)
	})
	return a.sem
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

func (a *Account) ClearRateLimit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.RateLimitedUntil = time.Time{}
}

func (a *Account) RecordSuccess() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ConsecutiveErrors = 0
	a.RateLimitedUntil = time.Time{}
	a.QuarantinedUntil = time.Time{}
}

func (a *Account) RecordError(statusCode int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if statusCode == http.StatusTooManyRequests { // 429
		a.RateLimitedUntil = time.Now().Add(5 * time.Minute)
		log.Printf("[Pool] Account %s rate-limited for 5m on 429 (until %s)", a.Email, a.RateLimitedUntil.Format(time.Kitchen))
		return
	}

	if statusCode == http.StatusBadRequest || statusCode == http.StatusForbidden { // 400 or 403
		a.ConsecutiveErrors++
		log.Printf("[Pool] Account %s recorded error #%d (status %d)", a.Email, a.ConsecutiveErrors, statusCode)
		if a.ConsecutiveErrors >= 3 {
			a.QuarantinedUntil = time.Now().Add(10 * time.Minute)
			log.Printf("[Pool] Circuit breaker tripped for %s: quarantined for 10m to prevent account flagging (until %s)", a.Email, a.QuarantinedUntil.Format(time.Kitchen))
		}
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
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity/hub/2.9.1 linux/x64")

	client := &http.Client{Timeout: 10 * time.Second}
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

type AccountPool struct {
	mu           sync.RWMutex
	accounts     []*Account
	currentIndex int
	storagePath  string
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
			accounts:    []*Account{},
			storagePath: storagePath,
		}
		globalPool.Load()
	})
	return globalPool
}

func (p *AccountPool) Load() {
	p.mu.Lock()
	defer p.mu.Unlock()

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

	// 3. Fallback: discover from IDE state DB
	dbCandidates := []string{
		filepath.Join(homeDir, ".config", "Antigravity", "User", "globalStorage", "state.vscdb"),
		filepath.Join(homeDir, ".antigravity-ide", "User", "globalStorage", "state.vscdb"),
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
	return os.WriteFile(p.storagePath, data, 0600)
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

// NextAvailableAccount selects an account that is not rate-limited using round-robin
// NextAvailableAccount selects the highest-priority account (top of list) that is not rate-limited
func (p *AccountPool) NextAvailableAccount() (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.accounts)
	if n == 0 {
		return nil, fmt.Errorf("no accounts in pool")
	}

	// Priority-based order: check accounts sequentially from index 0 (top) downwards
	for _, acc := range p.accounts {
		if acc.IsAvailable() {
			return acc, nil
		}
	}

	// All accounts are rate-limited or quarantined; find the one with the earliest reset
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

	log.Printf("[Pool] All accounts in cooldown; falling back to %s (unlocks %s)", bestAcc.Email, earliest.Format(time.Kitchen))
	return bestAcc, nil
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
	token, err := a.GetAccessToken()
	if err != nil {
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

	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error

	for _, ep := range endpoints {
		req, _ := http.NewRequest("POST", ep, bytes.NewBuffer(reqData))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "antigravity/hub/2.9.1 darwin/arm64")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var summary UserQuotaSummary
			if err := json.NewDecoder(resp.Body).Decode(&summary); err == nil {
			a.mu.Lock()
			a.LastQuotaSummary = &summary
			a.mu.Unlock()
				return &summary, nil
			}
		}
		lastErr = fmt.Errorf("retrieveUserQuotaSummary returned %d", resp.StatusCode)
	}
	return nil, lastErr
}
