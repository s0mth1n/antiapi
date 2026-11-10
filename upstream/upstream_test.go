package upstream

import (
	"testing"
	"time"
)

func TestEndpointCooldownsArePerAccount(t *testing.T) {
	ClearAllEndpointCooldowns()

	markEndpointCooldown("alice@example.com", StandardEndpoint, time.Minute)

	if !isEndpointInCooldown("alice@example.com", StandardEndpoint) {
		t.Fatalf("expected alice to be in cooldown on standard endpoint")
	}
	if isEndpointInCooldown("bob@example.com", StandardEndpoint) {
		t.Fatalf("bob must not share alice's endpoint cooldown")
	}
	if isEndpointInCooldown("alice@example.com", DailyEndpoint) {
		t.Fatalf("cooldown must not leak across endpoints")
	}

	clearEndpointCooldown("alice@example.com", StandardEndpoint)
	if isEndpointInCooldown("alice@example.com", StandardEndpoint) {
		t.Fatalf("expected cooldown cleared")
	}
}

func TestClearAllEndpointCooldowns(t *testing.T) {
	markEndpointCooldown("a@x.com", StandardEndpoint, time.Minute)
	markEndpointCooldown("b@x.com", DailyEndpoint, time.Minute)
	ClearAllEndpointCooldowns()
	if isEndpointInCooldown("a@x.com", StandardEndpoint) || isEndpointInCooldown("b@x.com", DailyEndpoint) {
		t.Fatalf("expected all cooldowns cleared")
	}
}

func TestExtractValidationURLFromMetadata(t *testing.T) {
	body := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","domain":"cloudcode-pa.googleapis.com","metadata":{"validationUrl":"https://accounts.google.com/AccountChooser?continue=verify"}}]}}`)
	got := ExtractValidationURL(body)
	want := "https://accounts.google.com/AccountChooser?continue=verify"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestExtractValidationURLSnakeCaseAndRaw(t *testing.T) {
	body := []byte(`{"error":{"details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://accounts.google.com/verify?x=1"}}]}}`)
	if got := ExtractValidationURL(body); got != "https://accounts.google.com/verify?x=1" {
		t.Fatalf("snake_case: got %q", got)
	}

	raw := []byte(`some non-json 403 text with https://accounts.google.com/EmbeddedSignin?foo=bar inside`)
	if got := ExtractValidationURL(raw); got != "https://accounts.google.com/EmbeddedSignin?foo=bar" {
		t.Fatalf("raw fallback: got %q", got)
	}
}

func TestExtractValidationURLAbsent(t *testing.T) {
	if got := ExtractValidationURL([]byte(`{"error":{"code":403}}`)); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestProdEndpointGating(t *testing.T) {
	if prodEndpointEnabled() {
		t.Error("prod endpoint should be opt-in, not default")
	}
	t.Setenv("ANTIAPI_ENABLE_PROD_ENDPOINT", "1")
	if !prodEndpointEnabled() {
		t.Error("prod endpoint should be enabled with env=1")
	}
	t.Setenv("ANTIAPI_ENABLE_PROD_ENDPOINT", "true")
	if !prodEndpointEnabled() {
		t.Error("prod endpoint should be enabled with env=true")
	}
	t.Setenv("ANTIAPI_ENABLE_PROD_ENDPOINT", "0")
	if prodEndpointEnabled() {
		t.Error("prod endpoint should be disabled with env=0")
	}
}

func TestCompletionEndpointsDefaultDailyFirst(t *testing.T) {
	ClearAllEndpointCooldowns()
	email := "daily-default@example.com"
	got := completionEndpoints(email)
	if len(got) != 2 || got[0] != DailyEndpoint || got[1] != StandardEndpoint {
		t.Fatalf("default order should be daily then prod, got %v", got)
	}
}

func TestCompletionEndpointsProdOptIn(t *testing.T) {
	ClearAllEndpointCooldowns()
	t.Setenv("ANTIAPI_ENABLE_PROD_ENDPOINT", "1")
	got := completionEndpoints("prod-optin@example.com")
	if len(got) != 2 || got[0] != StandardEndpoint || got[1] != DailyEndpoint {
		t.Fatalf("prod opt-in order should be prod then daily, got %v", got)
	}
}

func TestCompletionEndpointsSkipsCoolingDownPrimary(t *testing.T) {
	ClearAllEndpointCooldowns()
	email := "cooling@example.com"
	markEndpointCooldown(email, DailyEndpoint, time.Minute)
	got := completionEndpoints(email)
	if got[0] != StandardEndpoint {
		t.Fatalf("cooling-down daily should yield prod first, got %v", got)
	}
	clearEndpointCooldown(email, DailyEndpoint)
}

func TestCompletionEndpointsRemembersPreference(t *testing.T) {
	ClearAllEndpointCooldowns()
	email := "pref@example.com"
	setPreferredEndpoint(email, StandardEndpoint)
	defer setPreferredEndpoint(email, "")
	got := completionEndpoints(email)
	if got[0] != StandardEndpoint {
		t.Fatalf("remembered prod preference should win, got %v", got)
	}
	// A preference in cooldown must be ignored.
	markEndpointCooldown(email, StandardEndpoint, time.Minute)
	got = completionEndpoints(email)
	if got[0] != DailyEndpoint {
		t.Fatalf("cooling-down preference must not lead, got %v", got)
	}
	clearEndpointCooldown(email, StandardEndpoint)
}
