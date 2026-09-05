package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"antiapi/auth"
	"antiapi/models"
	"antiapi/server"
)

func main() {
	port := flag.Int("port", 8964, "Port to listen on")
	host := flag.String("host", "0.0.0.0", "Host address to bind")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[antiapi] ")

	pool := auth.GetPool()
	accCount := pool.Count()
	if accCount == 0 {
		log.Printf("WARNING: No accounts found. Add one via dashboard or set ANTIGRAVITY_REFRESH_TOKEN")
	} else {
		for _, acc := range pool.GetAllAccounts() {
			log.Printf("Loaded account: %s (Project: %s)", acc.Email, acc.ProjectID)
			go acc.Refresh()
		}
	}

	// Fetch initial models in background
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := models.GetRegistry().FetchLiveModels(); err != nil {
			log.Printf("[Models] Background fetch error: %v", err)
		}
	}()

	// Periodic model & quota refresh
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			models.GetRegistry().FetchLiveModels()
		}
	}()

	srv := server.NewServer()
	addr := fmt.Sprintf("%s:%d", *host, *port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv,
	}

	banner := fmt.Sprintf(`
======================================================================
  Antigravity API Gateway (Go Edition - Multi-Account Pool)
======================================================================
  * Web Dashboard & Quota: http://localhost:%d/
  * OpenAI Base URL:       http://localhost:%d/v1
  * Chat Completions:      http://localhost:%d/v1/chat/completions
  * Anthropic Messages:    http://localhost:%d/v1/messages
  * Models List:           http://localhost:%d/v1/models
  * Accounts In Pool:      %d active
======================================================================
`, *port, *port, *port, *port, *port, accCount)

	fmt.Print(banner)

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
	log.Println("Server stopped")
}
