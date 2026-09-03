package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"antiapi/auth"
	"antiapi/models"
	"antiapi/translator"
	"antiapi/upstream"
	"antiapi/web"
)

func applyAdaptiveJitter(payloadLen int) {
	baseJitter := 35
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
	if maxMs <= minMs {
		time.Sleep(time.Duration(maxMs) * time.Millisecond)
		return
	}
	sleepMs := minMs + rand.Intn(maxMs-minMs)
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)
}

type Server struct {
	upstreamClient *upstream.Client
	mux            *http.ServeMux
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
	s.mux.HandleFunc("/v1/messages", s.cors(s.handleMessages))
	s.mux.HandleFunc("/v1/models", s.cors(s.handleModels))
	s.mux.HandleFunc("/api/status", s.cors(s.handleStatus))
	s.mux.HandleFunc("/api/accounts", s.cors(s.handleAccounts))
	s.mux.HandleFunc("/api/accounts/add", s.cors(s.handleAddAccount))
	s.mux.HandleFunc("/api/accounts/remove", s.cors(s.handleRemoveAccount))
	s.mux.HandleFunc("/api/accounts/reorder", s.cors(s.handleReorderAccounts))
	s.mux.HandleFunc("/api/logs", s.cors(s.handleLogs))
	s.mux.HandleFunc("/oauth/login", s.cors(s.handleOAuthLogin))
	s.mux.HandleFunc("/quota", s.cors(s.handleStatus))
	s.mux.HandleFunc("/health", s.cors(s.handleHealth))
	s.mux.HandleFunc("/", s.cors(s.handleDashboard))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &statusLoggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	s.mux.ServeHTTP(lw, r)
	dur := time.Since(start)
	
	// Record non-poll requests in logs
	if !strings.HasPrefix(r.URL.Path, "/api/logs") && !strings.HasPrefix(r.URL.Path, "/health") {
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
	}
	log.Printf("[%s] %s %s (%v)", r.Method, r.URL.Path, r.RemoteAddr, dur)
}

func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
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
	w.Write(web.DashboardHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}


func formatResetDuration(resetStr string, fallback string) string {
	if resetStr == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, resetStr)
	if err != nil {
		return fallback
	}
	d := time.Until(t)
	if d <= 0 {
		return "less than 1 minute"
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours >= 24 {
		days := hours / 24
		remHours := hours % 24
		return fmt.Sprintf("%d days, %d hours", days, remHours)
	}
	return fmt.Sprintf("%d hours, %d minutes", hours, mins)
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
		// Prefer sriniv4sreddy if present (IDE active account), else first
		selectedEmail = accountEmails[0]
		for _, e := range accountEmails {
			if strings.Contains(e, "sriniv4s") {
				selectedEmail = e
				break
			}
		}
	}

	type AccQuota struct {
		Email   string                 `json:"email"`
		Summary *auth.UserQuotaSummary `json:"summary"`
		Error   string                 `json:"error,omitempty"`
	}

	quotaSummaries := make(map[string]*auth.UserQuotaSummary)
	for _, a := range accs {
		summary, err := a.FetchQuotaSummary()
		if err == nil && summary != nil {
			quotaSummaries[a.Email] = summary
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
		"quota_summaries":  quotaSummaries,
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

func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	auth.StartOAuthCallbackListener()
	authURL, _ := auth.GenerateAuthURL()
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reg := models.GetRegistry()

	type ModelItem struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	items := []ModelItem{}
	for _, m := range reg.GetAllModels() {
		items = append(items, ModelItem{
			ID:      m.ID,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "antigravity",
		})
	}

	res := map[string]any{
		"object": "list",
		"data":   items,
	}
	json.NewEncoder(w).Encode(res)
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

	applyAdaptiveJitter(len(bodyBytes))

	var req translator.OpenAIChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	gReq, resolvedModel := translator.ConvertOpenAIToGoogle(&req)
	log.Printf("[Chat] Routing request: client_model='%s' -> antigravity_model='%s'", req.Model, resolvedModel)

	eventCh, err := s.upstreamClient.StreamGenerate(r.Context(), gReq)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()), http.StatusBadGateway)
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

		for event := range eventCh {
			if event.Err != nil {
				log.Printf("[Chat] Stream error: %v", event.Err)
				break
			}

			parsed, err := translator.ParseGoogleSSEChunk(event.Data)
			if err != nil || (parsed.Text == "" && parsed.FinishReason == "" && len(parsed.ToolCalls) == 0) {
				continue
			}

			chunk := translator.FormatOpenAIChunk(completionID, resolvedModel, parsed.Text, parsed.FinishReason, parsed.ToolCalls)
			w.Write([]byte(chunk))
			flusher.Flush()
		}

		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
		return
	}

	var fullText strings.Builder
	var allToolCalls []translator.OpenAIToolCall
	for event := range eventCh {
		if event.Err != nil {
			break
		}
		parsed, _ := translator.ParseGoogleSSEChunk(event.Data)
		fullText.WriteString(parsed.Text)
		if len(parsed.ToolCalls) > 0 {
			allToolCalls = append(allToolCalls, parsed.ToolCalls...)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(translator.FormatOpenAINonStream(completionID, resolvedModel, fullText.String(), allToolCalls))
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

	applyAdaptiveJitter(len(bodyBytes))

	var req translator.ClaudeRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	gReq, resolvedModel := translator.ConvertClaudeToGoogle(&req)
	log.Printf("[Claude] Routing request: client_model='%s' -> antigravity_model='%s'", req.Model, resolvedModel)

	eventCh, err := s.upstreamClient.StreamGenerate(r.Context(), gReq)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()), http.StatusBadGateway)
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

		initPayload := map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":    msgID,
				"type":  "message",
				"role":  "assistant",
				"model": resolvedModel,
				"usage": map[string]int{"input_tokens": 10, "output_tokens": 0},
			},
		}
		initBytes, _ := json.Marshal(initPayload)
		w.Write([]byte(fmt.Sprintf("event: message_start\ndata: %s\n\n", string(initBytes))))

		blockStart := map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]string{"type": "text", "text": ""},
		}
		bsBytes, _ := json.Marshal(blockStart)
		w.Write([]byte(fmt.Sprintf("event: content_block_start\ndata: %s\n\n", string(bsBytes))))
		flusher.Flush()

		for event := range eventCh {
			if event.Err != nil {
				break
			}
			parsed, err := translator.ParseGoogleSSEChunk(event.Data)
			if err != nil || parsed.Text == "" {
				continue
			}

			ed := map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]string{"type": "text_delta", "text": parsed.Text},
			}
			dBytes, _ := json.Marshal(ed)
			w.Write([]byte(fmt.Sprintf("event: content_block_delta\ndata: %s\n\n", string(dBytes))))
			flusher.Flush()
		}

		w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
		return
	}

	var fullText strings.Builder
	for event := range eventCh {
		if event.Err != nil {
			break
		}
		parsed, _ := translator.ParseGoogleSSEChunk(event.Data)
		fullText.WriteString(parsed.Text)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(translator.FormatClaudeNonStream(msgID, resolvedModel, fullText.String()))
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
