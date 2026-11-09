package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPoolRoundRobin(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "accounts.json")

	pool := &AccountPool{
		storagePath: storagePath,
		accounts: []*Account{
			{ID: "acc1", Email: "acc1@test.com", RefreshToken: "tok1", ProjectID: "proj1"},
			{ID: "acc2", Email: "acc2@test.com", RefreshToken: "tok2", ProjectID: "proj2"},
			{ID: "acc3", Email: "acc3@test.com", RefreshToken: "tok3", ProjectID: "proj3"},
		},
	}

	// 1. Verify standard round-robin rotation cycles across accounts
	expectedEmails := []string{"acc1@test.com", "acc2@test.com", "acc3@test.com", "acc1@test.com"}
	for i, expected := range expectedEmails {
		acc, err := pool.NextAvailableAccount()
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if acc.Email != expected {
			t.Errorf("iteration %d: expected %s, got %s", i, expected, acc.Email)
		}
	}
}

func TestPoolExclusionFilter(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "accounts.json")

	pool := &AccountPool{
		storagePath: storagePath,
		accounts: []*Account{
			{ID: "acc1", Email: "acc1@test.com", RefreshToken: "tok1", ProjectID: "proj1"},
			{ID: "acc2", Email: "acc2@test.com", RefreshToken: "tok2", ProjectID: "proj2"},
			{ID: "acc3", Email: "acc3@test.com", RefreshToken: "tok3", ProjectID: "proj3"},
		},
	}

	// Exclude acc1 and acc2
	exclude := map[string]bool{
		"acc1@test.com": true,
		"acc2@test.com": true,
	}

	acc, err := pool.NextAvailableAccount(exclude)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.Email != "acc3@test.com" {
		t.Errorf("expected acc3@test.com, got %s", acc.Email)
	}
}

func TestPoolAtomicSave(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "accounts.json")

	pool := &AccountPool{
		storagePath: storagePath,
	}

	pool.AddAccount("user@test.com", "refresh-token-123", "proj-xyz")

	// Ensure accounts.json exists
	info, err := os.Stat(storagePath)
	if err != nil {
		t.Fatalf("accounts.json not found: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("accounts.json is empty")
	}

	// Ensure no lingering .tmp file
	if _, err := os.Stat(storagePath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file %s.tmp was not removed", storagePath)
	}
}

func TestPoolConcurrentAccess(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "accounts.json")

	pool := &AccountPool{
		storagePath: storagePath,
		accounts: []*Account{
			{ID: "acc1", Email: "acc1@test.com", RefreshToken: "tok1", ProjectID: "proj1"},
			{ID: "acc2", Email: "acc2@test.com", RefreshToken: "tok2", ProjectID: "proj2"},
			{ID: "acc3", Email: "acc3@test.com", RefreshToken: "tok3", ProjectID: "proj3"},
		},
	}

	var wg sync.WaitGroup
	workers := 20
	iterations := 50

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				acc, err := pool.NextAvailableAccount()
				if err != nil {
					t.Errorf("concurrent NextAvailableAccount failed: %v", err)
					return
				}
				if acc == nil {
					t.Errorf("concurrent NextAvailableAccount returned nil")
					return
				}
				// Simulate state checking
				_ = acc.IsRateLimited()
				time.Sleep(time.Microsecond * 50)
			}
		}()
	}

	wg.Wait()
}

func TestPoolSessionStickyAffinity(t *testing.T) {
	tempDir := t.TempDir()
	storagePath := filepath.Join(tempDir, "accounts.json")

	pool := &AccountPool{
		storagePath:     storagePath,
		sessionBindings: make(map[string]sessionBinding),
		accounts: []*Account{
			{ID: "acc1", Email: "acc1@test.com", RefreshToken: "tok1", ProjectID: "proj1"},
			{ID: "acc2", Email: "acc2@test.com", RefreshToken: "tok2", ProjectID: "proj2"},
		},
	}

	sessionKey := "chat-session-uuid-12345"

	// First call binds session to an account
	acc1, err := pool.GetAccountForSession(sessionKey)
	if err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}

	// Repeat calls for 5 turns in this session MUST return the exact same account
	for turn := 2; turn <= 5; turn++ {
		accNext, err := pool.GetAccountForSession(sessionKey)
		if err != nil {
			t.Fatalf("turn %d: unexpected error: %v", turn, err)
		}
		if accNext.Email != acc1.Email {
			t.Errorf("turn %d: expected sticky account %s, got %s", turn, acc1.Email, accNext.Email)
		}
	}

	// When account is excluded (e.g. 429 quota exhaustion), it must failover to acc2
	exclude := map[string]bool{acc1.Email: true}
	accFailover, err := pool.GetAccountForSession(sessionKey, exclude)
	if err != nil {
		t.Fatalf("unexpected error on failover: %v", err)
	}
	if accFailover.Email == acc1.Email {
		t.Errorf("expected failover to different account, got %s", accFailover.Email)
	}
}
