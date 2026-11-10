package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"antiapi/auth"
	"antiapi/models"
	"antiapi/translator"
	"antiapi/tui"
	"antiapi/tunnel"
	"antiapi/upstream"
	"antiapi/web"
)

func applyAdaptiveJitter(ctx context.Context, payloadLen int) {
	baseJitter := 0
	if envVal := os.Getenv("ANTIAPI_JITTER_MS"); envVal != "" {
		if parsed, err := strconv.Atoi(envVal); err == nil && parsed >= 0 {
			baseJitter = parsed
		}
	}
	if baseJitter <= 0 {
		return
	}

	extraMs := 0
	if payloadLen > 50000 {
		extraMs = 80 + rand.Intn(120)
	} else if payloadLen > 10000 {
		extraMs = 30 + rand.Intn(50)
	}

	minMs := 10
	maxMs := baseJitter + extraMs
	sleepMs := maxMs
	if maxMs > minMs {
		sleepMs = minMs + rand.Intn(maxMs-minMs)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(sleepMs) * time.Millisecond):
	}
}

type Server struct {
	upstreamClient *upstream.Client
	mux            *http.ServeMux
	publicURL      string
	publicURLMu    sync.RWMutex
}

func (s *Server) SetPublicURL(u string) {
	s.publicURLMu.Lock()
	defer s.publicURLMu.Unlock()
	s.publicURL = u
}

func (s *Server) GetPublicURL() string {
	s.publicURLMu.RLock()
	defer s.publicURLMu.RUnlock()
	return s.publicURL
}

func NewServer() *Server {
	s := &Server{
		upstreamClient: upstream.NewClient(),
		mux:            http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/chat/completions", s.cors(s.handleChatCompletions))
	s.mux.HandleFunc("/v1/responses", s.cors(s.handleResponses))
	s.mux.HandleFunc("/v1/messages", s.cors(s.handleMessages))
	s.mux.HandleFunc("/v1/models", s.cors(s.handleModels))
	s.mux.HandleFunc("/v1/models/", s.cors(s.handleModels))
	s.mux.HandleFunc("/api/status", s.cors(s.handleStatus))
	s.mux.HandleFunc("/api/accounts", s.cors(s.handleAccounts))
	s.mux.HandleFunc("/api/accounts/add", s.cors(s.handleAddAccount))
	s.mux.HandleFunc("/api/accounts/remove", s.cors(s.handleRemoveAccount))
	s.mux.HandleFunc("/api/accounts/reorder", s.cors(s.handleReorderAccounts))
	s.mux.HandleFunc("/api/accounts/clear_cooldown", s.cors(s.handleClearCooldown))
	s.mux.HandleFunc("/api/refresh", s.cors(s.handleClearCooldown))
	s.mux.HandleFunc("/api/fast_mode", s.cors(s.handleFastMode))
	s.mux.HandleFunc("/api/tunnel", s.cors(s.handleTunnel))
	s.mux.HandleFunc("/api/logs", s.cors(s.handleLogs))
	s.mux.HandleFunc("/oauth/login", s.cors(s.handleOAuthLogin))
	s.mux.HandleFunc("/quota", s.cors(s.handleStatus))
	s.mux.HandleFunc("/health", s.cors(s.handleHealth))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/Heartbeat", s.cors(s.handleConnectHeartbeat))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetAvailableModels", s.cors(s.handleConnectAvailableModels))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetServerConfiguration", s.cors(s.handleConnectServerConfiguration))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetUserStatus", s.cors(s.handleConnectUserStatus))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetAuthStatus", s.cors(s.handleConnectAuthStatus))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetCascadeState", s.cors(s.handleConnectCascadeState))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetAllConversations", s.cors(s.handleConnectAllConversations))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetSlashCommands", s.cors(s.handleConnectSlashCommands))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetSidecars", s.cors(s.handleConnectSidecars))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetSidecarState", s.cors(s.handleConnectSidecarState))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetLocalUserInfo", s.cors(s.handleConnectLocalUserInfo))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/ListProjects", s.cors(s.handleConnectListProjects))
	s.mux.HandleFunc("/exa.language_server_pb.LanguageServerService/", s.cors(s.handleConnectRPCFallback))
	s.mux.HandleFunc("/", s.cors(s.handleDashboard))
}

var (
	metricTotalRequests atomic.Uint64
	metricStatus2xx     atomic.Uint64
	metricStatus4xx     atomic.Uint64
	metricStatus5xx     atomic.Uint64
	metricInFlight      atomic.Int64
	metricTotalDurUs    atomic.Uint64
)

func (s *Server) GetMetrics() (total, s2xx, s4xx, s5xx uint64, inFlight int64, avgMs float64) {
	total = metricTotalRequests.Load()
	s2xx = metricStatus2xx.Load()
	s4xx = metricStatus4xx.Load()
	s5xx = metricStatus5xx.Load()
	inFlight = metricInFlight.Load()
	durUs := metricTotalDurUs.Load()
	if total > 0 {
		avgMs = float64(durUs) / float64(total) / 1000.0
	}
	return
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isPoll := tui.IsInternalPoll(r.URL.Path)
	if !isPoll {
		metricInFlight.Add(1)
		defer metricInFlight.Add(-1)
	}

	start := time.Now()
	lw := &statusLoggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	if !s.requireAPIKey(lw, r) {
		return
	}
	s.mux.ServeHTTP(lw, r)
	dur := time.Since(start)

	if !isPoll {
		metricTotalRequests.Add(1)
		metricTotalDurUs.Add(uint64(dur.Microseconds()))
		if lw.statusCode >= 200 && lw.statusCode < 300 {
			metricStatus2xx.Add(1)
		} else if lw.statusCode >= 400 && lw.statusCode < 500 {
			metricStatus4xx.Add(1)
		} else if lw.statusCode >= 500 {
			metricStatus5xx.Add(1)
		}

		clientIP := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			clientIP = strings.Split(xff, ",")[0]
		}

		recordRequestLog(RequestLog{
			Timestamp: time.Now().Format("15:04:05"),
			Method:    r.Method,
			Path:      r.URL.Path,
			ClientIP:  clientIP,
			Duration:  fmt.Sprintf("%.1fms", float64(dur.Microseconds())/1000.0),
			Status:    lw.statusCode,
		})

		durStr := fmt.Sprintf("%.2fs", dur.Seconds())
		if dur < time.Second {
			durStr = fmt.Sprintf("%dms", dur.Milliseconds())
		}
		tui.SetActivity(fmt.Sprintf("%s %s [%d] %s", r.Method, r.URL.Path, lw.statusCode, durStr))
		log.Printf("[%s] %s %s [%d] (%s)", r.Method, r.URL.Path, clientIP, lw.statusCode, durStr)
	}
}

func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Keyless mode is localhost-only convenience: without a shared secret,
		// a wildcard CORS origin lets any website the operator visits call the
		// gateway from the browser and read account emails or spend quota.
		// With no key configured, only reflect localhost origins; browsers
		// block every other cross-origin caller. When a key IS configured,
		// cross-origin callers still need the key, so a wildcard is safe.
		origin := r.Header.Get("Origin")
		if APIKeyConfigured() {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" && isLocalOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := dashboardBytes()
	if apiKey != "" {
		data = injectDashboardKey(data, apiKey)
	}
	w.Write(data)
}

var dashboardCache struct {
	mu      sync.RWMutex
	modTime time.Time
	data    []byte
}

// dashboardBytes serves the dashboard from disk when it changed (dev mode),
// otherwise from memory, falling back to the embedded copy.
func dashboardBytes() []byte {
	st, err := os.Stat("web/dashboard.html")
	if err != nil {
		return web.DashboardHTML
	}
	dashboardCache.mu.RLock()
	if dashboardCache.data != nil && st.ModTime().Equal(dashboardCache.modTime) {
		defer dashboardCache.mu.RUnlock()
		return dashboardCache.data
	}
	dashboardCache.mu.RUnlock()

	data, err := os.ReadFile("web/dashboard.html")
	if err != nil {
		return web.DashboardHTML
	}
	dashboardCache.mu.Lock()
	dashboardCache.data = data
	dashboardCache.modTime = st.ModTime()
	dashboardCache.mu.Unlock()
	return data
}

// injectDashboardKey embeds the configured API key into the dashboard HTML so
// the web UI can call the protected /api and /v1 endpoints. Only reachable
// after the request already passed requireAPIKey.
func injectDashboardKey(html []byte, key string) []byte {
	escKey, _ := json.Marshal(key)
	snippet := []byte(`<script>window.ANTIAPI_API_KEY=` + string(escKey) + `;</script></head>`)
	return bytes.Replace(html, []byte("</head>"), snippet, 1)
}

// isLocalOrigin reports whether an Origin header points at this machine.
func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// writeJSONError emits a spec-shaped error body with safe JSON escaping.
func writeJSONError(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(payload)
	w.Write(body)
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeJSONError(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "server_error",
		},
	})
}

func writeClaudeError(w http.ResponseWriter, status int, msg string) {
	writeJSONError(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	pool := auth.GetPool()
	accs := pool.GetAllAccounts()

	accountEmails := []string{}
	for _, a := range accs {
		accountEmails = append(accountEmails, a.Email)
	}

	selectedEmail := r.URL.Query().Get("account")
	if selectedEmail == "" && len(accountEmails) > 0 {
		selectedEmail = accountEmails[0]
	}

	// Serve whatever quota summaries are cached instantly; kick async refreshes
	// for stale entries. Never block the status endpoint on upstream calls
	// (a failing account can take ~12s to time out).
	quotaSummaries := make(map[string]*auth.UserQuotaSummary)
	for _, a := range accs {
		if cached, fetchedAt := a.GetCachedQuotaSummary(); cached != nil {
			quotaSummaries[a.Email] = cached
			if time.Since(fetchedAt) > 60*time.Second {
				go a.FetchQuotaSummary()
			}
		} else {
			go a.FetchQuotaSummary()
		}
	}

	// Backwards compatibility percentages
	claudePct := 100.0
	geminiPct := 100.0
	if selSum, ok := quotaSummaries[selectedEmail]; ok && selSum != nil {
		for _, g := range selSum.Groups {
			if strings.Contains(strings.ToLower(g.DisplayName), "gemini") {
				for _, b := range g.Buckets {
					if b.Window == "5h" {
						geminiPct = b.RemainingFraction * 100.0
					}
				}
			}
			if strings.Contains(strings.ToLower(g.DisplayName), "claude") {
				for _, b := range g.Buckets {
					if b.Window == "5h" {
						claudePct = b.RemainingFraction * 100.0
					}
				}
			}
		}
	}

	res := map[string]any{
		"status":           "online",
		"accounts":         accountEmails,
		"total":            len(accs),
		"selected_account": selectedEmail,
		"fast_mode":        translator.IsGlobalFastMode(),
		"quota_summaries":  quotaSummaries,
		"public_url":       s.GetPublicURL(),
		"tunnel_active":    tunnel.GetManager().IsActive(),
		"tunnel_starting":  tunnel.GetManager().IsStarting(),
		"quotas": map[string]float64{
			"claude_gpt":   claudePct,
			"gemini_pro":   geminiPct,
			"gemini_flash": geminiPct,
		},
	}
	json.NewEncoder(w).Encode(res)
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	accs := auth.GetPool().GetAllAccounts()

	type AccountInfo struct {
		ID               string `json:"id"`
		Email            string `json:"email"`
		ProjectID        string `json:"projectId"`
		IsRateLimited    bool   `json:"isRateLimited"`
		RateLimitedUntil string `json:"rateLimitedUntil,omitempty"`
		NeedsValidation  bool   `json:"needsValidation,omitempty"`
		ValidationURL    string `json:"validationUrl,omitempty"`
	}

	res := []AccountInfo{}
	for _, a := range accs {
		rl := a.IsRateLimited()
		until := ""
		if rl {
			until = a.RateLimitedUntil.Format(time.RFC3339)
		}
		res = append(res, AccountInfo{
			ID:               a.ID,
			Email:            a.Email,
			ProjectID:        a.ProjectID,
			IsRateLimited:    rl,
			RateLimitedUntil: until,
			NeedsValidation:  a.GetValidationURL() != "",
			ValidationURL:    a.GetValidationURL(),
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"accounts": res})
}

func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Email        string `json:"email"`
		RefreshToken string `json:"refreshToken"`
		ProjectID    string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
		http.Error(w, "Missing refreshToken", http.StatusBadRequest)
		return
	}
	email := body.Email
	if email == "" {
		email = "user-" + fmt.Sprintf("%d", time.Now().Unix())
	}
	acc := auth.GetPool().AddAccount(email, body.RefreshToken, body.ProjectID)
	go acc.Refresh()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "added", "email": acc.Email})
}

func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Email == "" {
		body.Email = r.URL.Query().Get("email")
	}

	success := auth.GetPool().RemoveAccount(body.Email)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": success})
}

func (s *Server) handleClearCooldown(w http.ResponseWriter, r *http.Request) {
	auth.GetPool().ClearAllRateLimits()
	for _, acc := range auth.GetPool().GetAllAccounts() {
		go acc.Refresh()
	}
	tui.SetActivity("Refreshed quotas & cooldowns via API")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": "cooldowns cleared and quotas refreshed"})
}

func (s *Server) handleFastMode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		var body struct {
			FastMode *bool `json:"fast_mode"`
			Enabled  *bool `json:"enabled"`
			Toggle   bool  `json:"toggle"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if body.Toggle {
				current := translator.IsGlobalFastMode()
				translator.SetGlobalFastMode(!current)
			} else if body.FastMode != nil {
				translator.SetGlobalFastMode(*body.FastMode)
			} else if body.Enabled != nil {
				translator.SetGlobalFastMode(*body.Enabled)
			}
		} else {
			current := translator.IsGlobalFastMode()
			translator.SetGlobalFastMode(!current)
		}
		modeStr := "DISABLED"
		if translator.IsGlobalFastMode() {
			modeStr = "ENABLED"
		}
		tui.SetActivity(fmt.Sprintf("Fast Mode %s via API", modeStr))
	}
	json.NewEncoder(w).Encode(map[string]any{
		"fast_mode": translator.IsGlobalFastMode(),
	})
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mgr := tunnel.GetManager()
	if r.Method == http.MethodPost {
		var body struct {
			Action string `json:"action"` // "start", "stop", "toggle"
			Toggle bool   `json:"toggle"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Action == "stop" {
			mgr.Stop()
		} else if body.Action == "start" {
			go func() {
				_, _ = mgr.Start()
			}()
		} else {
			if mgr.IsActive() {
				mgr.Stop()
			} else {
				go func() {
					_, _ = mgr.Start()
				}()
			}
		}
	}
	resp := map[string]any{
		"active":   mgr.IsActive(),
		"starting": mgr.IsStarting(),
		"url":      mgr.URL(),
	}
	if mgr.IsActive() && !APIKeyConfigured() {
		resp["warning"] = "Public tunnel is active but ANTIAPI_API_KEY is not set: anyone with the URL can use this gateway."
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !auth.OAuthConfigured() {
		writeJSONError(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"message": auth.ErrOAuthNotConfigured.Error(),
				"type":    "configuration_error",
			},
		})
		return
	}
	auth.StartOAuthCallbackListener()
	authURL, _ := auth.GenerateAuthURL()
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reg := models.GetRegistry()

	type ModelItem struct {
		ID              string               `json:"id"`
		Name            string               `json:"name,omitempty"`
		DisplayName     string               `json:"display_name,omitempty"`
		Object          string               `json:"object"`
		Created         int64                `json:"created"`
		OwnedBy         string               `json:"owned_by"`
		ContextWindow   int                  `json:"context_window,omitempty"`
		MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
		ContextLength   int                  `json:"context_length,omitempty"`
		MaxTokens       int                  `json:"max_tokens,omitempty"`
		Pricing         *models.ModelPricing `json:"pricing,omitempty"`
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/models")
	trimmed = strings.TrimPrefix(trimmed, "/")
	trimmed = strings.TrimSpace(trimmed)

	if trimmed != "" {
		m, ok := reg.GetModel(trimmed)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "Model not found",
					"type":    "invalid_request_error",
					"code":    "model_not_found",
				},
			})
			return
		}
		item := ModelItem{
			ID:              m.ID,
			Name:            m.Name,
			DisplayName:     m.Name,
			Object:          "model",
			Created:         time.Now().Unix(),
			OwnedBy:         "antigravity",
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
			ContextLength:   m.ContextWindow,
			MaxTokens:       m.MaxOutputTokens,
			Pricing:         m.Pricing,
		}
		json.NewEncoder(w).Encode(item)
		return
	}

	items := []ModelItem{}
	for _, m := range reg.GetAllModels() {
		items = append(items, ModelItem{
			ID:              m.ID,
			Name:            m.Name,
			DisplayName:     m.Name,
			Object:          "model",
			Created:         time.Now().Unix(),
			OwnedBy:         "antigravity",
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
			ContextLength:   m.ContextWindow,
			MaxTokens:       m.MaxOutputTokens,
			Pricing:         m.Pricing,
		})
	}

	res := map[string]any{
		"object": "list",
		"data":   items,
	}
	json.NewEncoder(w).Encode(res)
}

func isFastModeRequest(r *http.Request) bool {
	h := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Antiapi-Fast-Mode")))
	if h == "1" || h == "true" || h == "yes" || h == "on" {
		return true
	}
	h2 := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Fast-Mode")))
	if h2 == "1" || h2 == "true" || h2 == "yes" || h2 == "on" {
		return true
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("fast")))
	if q == "1" || q == "true" || q == "yes" || q == "on" {
		return true
	}
	q2 := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("fast_mode")))
	if q2 == "1" || q2 == "true" || q2 == "yes" || q2 == "on" {
		return true
	}
	return false
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	applyAdaptiveJitter(r.Context(), len(bodyBytes))

	var req translator.OpenAIChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if isFastModeRequest(r) && req.FastMode == nil {
		fast := true
		req.FastMode = &fast
	}

	gReq, resolvedModel := translator.ConvertOpenAIToGoogle(&req)
	log.Printf("[Chat] Routing request: client_model='%s' -> antigravity_model='%s'", req.Model, resolvedModel)

	sessID := r.Header.Get("X-Session-ID")
	if sessID == "" {
		sessID = r.Header.Get("Session-ID")
	}
	if sessID == "" {
		sessID = r.Header.Get("X-Conversation-ID")
	}
	if sessID == "" && req.User != "" {
		sessID = req.User
	}

	eventCh, err := s.upstreamClient.StreamGenerate(r.Context(), gReq, sessID)
	if err != nil {
		statusCode := http.StatusBadGateway
		if strings.Contains(err.Error(), "(400)") {
			statusCode = http.StatusBadRequest
		}
		writeOpenAIError(w, statusCode, err.Error())
		return
	}

	completionID := translator.GenerateID("chatcmpl")

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		var lastUsage *translator.GoogleUsageMetadata

		for event := range eventCh {
			if event.Err != nil {
				log.Printf("[Chat] Stream error: %v", event.Err)
				break
			}

			parsed, err := translator.ParseGoogleSSEChunk(event.Data)
			if err != nil {
				continue
			}

			if parsed.Usage != nil {
				lastUsage = parsed.Usage
			}

			if parsed.Text == "" && parsed.ReasoningText == "" && parsed.FinishReason == "" && len(parsed.ToolCalls) == 0 {
				continue
			}

			chunk := translator.FormatOpenAIChunk(completionID, resolvedModel, parsed.Text, parsed.ReasoningText, parsed.FinishReason, parsed.ToolCalls)
			w.Write([]byte(chunk))
			flusher.Flush()
		}

		// Spec: emit the trailing usage chunk only when the client opts in via
		// stream_options.include_usage. Default to sending it when stream_options
		// is absent entirely - several gateway clients (e.g. Hermes) depend on it.
		if lastUsage != nil && (req.StreamOptions == nil || req.StreamOptions.IncludeUsage) {
			w.Write([]byte(translator.FormatOpenAIUsageChunk(completionID, resolvedModel, lastUsage)))
			flusher.Flush()
		}

		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
		return
	}

	var fullText strings.Builder
	var fullReasoning strings.Builder
	var allToolCalls []translator.OpenAIToolCall
	var lastUsage *translator.GoogleUsageMetadata
	var streamErr error
	finishReason := ""
	for event := range eventCh {
		if event.Err != nil {
			streamErr = event.Err
			break
		}
		parsed, _ := translator.ParseGoogleSSEChunk(event.Data)
		if parsed.Usage != nil {
			lastUsage = parsed.Usage
		}
		if parsed.FinishReason != "" {
			finishReason = parsed.FinishReason
		}
		fullText.WriteString(parsed.Text)
		fullReasoning.WriteString(parsed.ReasoningText)
		if len(parsed.ToolCalls) > 0 {
			allToolCalls = append(allToolCalls, parsed.ToolCalls...)
		}
	}

	if streamErr != nil && fullText.Len() == 0 && fullReasoning.Len() == 0 && len(allToolCalls) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "upstream stream failed: "+streamErr.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(translator.FormatOpenAINonStream(completionID, resolvedModel, fullText.String(), fullReasoning.String(), allToolCalls, lastUsage, finishReason))
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	applyAdaptiveJitter(r.Context(), len(bodyBytes))

	var req translator.ClaudeRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if isFastModeRequest(r) && req.FastMode == nil {
		fast := true
		req.FastMode = &fast
	}

	gReq, resolvedModel := translator.ConvertClaudeToGoogle(&req)
	log.Printf("[Claude] Routing request: client_model='%s' -> antigravity_model='%s'", req.Model, resolvedModel)

	sessID := r.Header.Get("X-Session-ID")
	if sessID == "" {
		sessID = r.Header.Get("Session-ID")
	}
	if sessID == "" {
		sessID = r.Header.Get("X-Conversation-ID")
	}
	if sessID == "" && req.Metadata != nil {
		if uid, ok := req.Metadata["user_id"].(string); ok {
			sessID = uid
		}
	}

	eventCh, err := s.upstreamClient.StreamGenerate(r.Context(), gReq, sessID)
	if err != nil {
		statusCode := http.StatusBadGateway
		if strings.Contains(err.Error(), "(400)") {
			statusCode = http.StatusBadRequest
		}
		writeClaudeError(w, statusCode, err.Error())
		return
	}

	msgID := translator.GenerateID("msg")

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		msgStart := map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         resolvedModel,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
			},
		}
		msBytes, _ := json.Marshal(msgStart)
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", msBytes)
		flusher.Flush()

		thinkingBlockStarted := false
		textBlockStarted := false
		blockIndex := 0
		stopReason := "end_turn"
		var lastUsage *translator.GoogleUsageMetadata

		for event := range eventCh {
			if event.Err != nil {
				break
			}
			parsed, err := translator.ParseGoogleSSEChunk(event.Data)
			if err != nil {
				continue
			}

			if parsed.Usage != nil {
				lastUsage = parsed.Usage
			}

			if parsed.FinishReason != "" {
				switch strings.ToUpper(parsed.FinishReason) {
				case "MAX_TOKENS":
					stopReason = "max_tokens"
				case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
					stopReason = "refusal"
				}
			}

			if parsed.ReasoningText != "" {
				if !thinkingBlockStarted {
					fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n", blockIndex)
					thinkingBlockStarted = true
				}
				escThinking, _ := json.Marshal(parsed.ReasoningText)
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":%s}}\n\n", blockIndex, escThinking)
				flusher.Flush()
			}

			if parsed.Text != "" {
				if thinkingBlockStarted {
					fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", blockIndex)
					blockIndex++
					thinkingBlockStarted = false
				}
				if !textBlockStarted {
					fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", blockIndex)
					textBlockStarted = true
				}

				escText, _ := json.Marshal(parsed.Text)
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", blockIndex, escText)
				flusher.Flush()
			}

			if len(parsed.ToolCalls) > 0 {
				stopReason = "tool_use"
				if thinkingBlockStarted {
					fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", blockIndex)
					blockIndex++
					thinkingBlockStarted = false
				}
				if textBlockStarted {
					fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", blockIndex)
					blockIndex++
					textBlockStarted = false
				}

				for _, tc := range parsed.ToolCalls {
					cbStart := map[string]any{
						"type":  "content_block_start",
						"index": blockIndex,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  tc.Function.Name,
							"input": map[string]any{},
						},
					}
					csbBytes, _ := json.Marshal(cbStart)
					fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", csbBytes)

					cbDelta := map[string]any{
						"type":  "content_block_delta",
						"index": blockIndex,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					}
					cdBytes, _ := json.Marshal(cbDelta)
					fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", cdBytes)

					fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", blockIndex)
					blockIndex++
				}
				flusher.Flush()
			}
		}

		if thinkingBlockStarted {
			fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", blockIndex)
		}
		if textBlockStarted {
			fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", blockIndex)
		}

		inputTokens := 0
		outputTokens := 0
		cacheReadTokens := 0
		if lastUsage != nil {
			inputTokens = lastUsage.PromptTokenCount
			outputTokens = lastUsage.CandidatesTokenCount + lastUsage.ThoughtsTokenCount
			if outputTokens == 0 && lastUsage.TotalTokenCount > lastUsage.PromptTokenCount {
				outputTokens = lastUsage.TotalTokenCount - lastUsage.PromptTokenCount
			}
			cacheReadTokens = lastUsage.CachedContentTokenCount
		}

		msgDelta := map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]int{
				"input_tokens":                inputTokens,
				"output_tokens":               outputTokens,
				"cache_read_input_tokens":     cacheReadTokens,
				"cache_creation_input_tokens": 0,
			},
		}
		mdBytes, _ := json.Marshal(msgDelta)
		fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", mdBytes)

		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
		return
	}

	var fullText strings.Builder
	var fullReasoning strings.Builder
	var allToolCalls []translator.OpenAIToolCall
	var lastUsage *translator.GoogleUsageMetadata
	var streamErr error
	finishReason := ""
	for event := range eventCh {
		if event.Err != nil {
			streamErr = event.Err
			break
		}
		parsed, _ := translator.ParseGoogleSSEChunk(event.Data)
		if parsed.Usage != nil {
			lastUsage = parsed.Usage
		}
		if parsed.FinishReason != "" {
			finishReason = parsed.FinishReason
		}
		fullText.WriteString(parsed.Text)
		fullReasoning.WriteString(parsed.ReasoningText)
		if len(parsed.ToolCalls) > 0 {
			allToolCalls = append(allToolCalls, parsed.ToolCalls...)
		}
	}

	if streamErr != nil && fullText.Len() == 0 && fullReasoning.Len() == 0 && len(allToolCalls) == 0 {
		writeClaudeError(w, http.StatusBadGateway, "upstream stream failed: "+streamErr.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(translator.FormatClaudeNonStream(msgID, resolvedModel, fullText.String(), fullReasoning.String(), allToolCalls, lastUsage, finishReason))
}

func (s *Server) handleReorderAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Order) == 0 {
		http.Error(w, "Invalid order payload", http.StatusBadRequest)
		return
	}

	if err := auth.GetPool().ReorderAccounts(body.Order); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "order": body.Order})
}

type RequestLog struct {
	Timestamp string `json:"timestamp"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	ClientIP  string `json:"client_ip"`
	Duration  string `json:"duration"`
	Status    int    `json:"status"`
}

var (
	recentLogsMu sync.RWMutex
	recentLogs   []RequestLog
)

func recordRequestLog(entry RequestLog) {
	recentLogsMu.Lock()
	defer recentLogsMu.Unlock()
	recentLogs = append(recentLogs, entry)
	if len(recentLogs) > 100 {
		recentLogs = recentLogs[len(recentLogs)-100:]
	}
}

type statusLoggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusLoggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusLoggingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	recentLogsMu.RLock()
	defer recentLogsMu.RUnlock()

	// Return reversed (newest first)
	n := len(recentLogs)
	rev := make([]RequestLog, n)
	for i := 0; i < n; i++ {
		rev[i] = recentLogs[n-1-i]
	}

	json.NewEncoder(w).Encode(map[string]any{"logs": rev})
}

func (s *Server) handleConnectHeartbeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"healthy":   true,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
	})
}

func (s *Server) handleConnectServerConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"version":            "2.6.0",
			"serverCapabilities": []string{"LANGUAGE_SERVER", "AGENT_HUB", "BROWSER_AUTOMATION"},
			"defaultModel":       "gemini-3.8-flash-high",
		},
	})
}

func (s *Server) handleConnectAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"authResult": map[string]any{
			"hasValidAuth":  true,
			"grantedScopes": []string{"email", "profile", "https://www.googleapis.com/auth/aicode"},
		},
	})
}

func (s *Server) handleConnectUserStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	pool := auth.GetPool()
	accs := pool.GetAllAccounts()
	email := "user@antigravity.local"
	if len(accs) > 0 {
		email = accs[0].Email
	}
	json.NewEncoder(w).Encode(map[string]any{
		"userStatus": map[string]any{
			"name":  "Antigravity User",
			"email": email,
			"planStatus": map[string]any{
				"planInfo": map[string]any{
					"teamsTier": "TEAMS_TIER_PRO",
					"planName":  "Pro",
				},
				"availablePromptCredits": 50000,
				"availableFlowCredits":   150000,
			},
		},
	})
}

func (s *Server) handleConnectAvailableModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reg := models.GetRegistry()
	all := reg.GetAllModels()
	catalog := map[string]any{
		"defaultAgentModelId": "gemini-3.8-flash-high",
		"models":              all,
	}
	json.NewEncoder(w).Encode(map[string]any{
		"response": catalog,
	})
}

func (s *Server) handleConnectCascadeState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"cascadeId":    "cascade-active-01",
			"status":       "CASCADE_STATUS_IDLE",
			"currentModel": "gemini-3.8-flash-high",
		},
	})
}

func (s *Server) handleConnectAllConversations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"conversations": []any{},
		},
	})
}

func (s *Server) handleConnectSlashCommands(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"slashCommands": []map[string]string{
				{"name": "goal", "description": "Long-running autonomous execution."},
				{"name": "schedule", "description": "Cron or one-shot timers."},
				{"name": "compact", "description": "Compact conversation context."},
				{"name": "plan", "description": "Interactive design interview and planning."},
			},
		},
	})
}

func (s *Server) handleConnectSidecars(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"sidecars": []any{},
		},
	})
}

func (s *Server) handleConnectSidecarState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{
			"active": false,
		},
	})
}

func (s *Server) handleConnectLocalUserInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	username := os.Getenv("USER")
	if username == "" {
		username = "user"
	}
	home, _ := os.UserHomeDir()
	json.NewEncoder(w).Encode(map[string]any{
		"username":   username,
		"homeDirUri": "file://" + home,
	})
}

func (s *Server) handleConnectListProjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"projectIds": []string{"default-cli-project", "outside-of-project"},
	})
}

func (s *Server) handleConnectRPCFallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response": map[string]any{},
	})
}
