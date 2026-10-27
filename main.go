package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"antiapi/auth"
	"antiapi/models"
	"antiapi/server"
	"antiapi/tui"
	"antiapi/tunnel"
)

func main() {
	port := flag.Int("port", 8964, "Port to listen on")
	host := flag.String("host", "0.0.0.0", "Host address to bind")
	noTui := flag.Bool("no-tui", false, "Disable interactive TUI and use streaming logs")
	enableTunnel := flag.Bool("tunnel", false, "Start a cloudflared public tunnel and expose the API over HTTPS")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[antiapi] ")

	srv := server.NewServer()
	addr := fmt.Sprintf("%s:%d", *host, *port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv,
	}

	useTUI := !*noTui && tui.IsTerminal(os.Stdout.Fd())

	var appTUI *tui.TUI
	if useTUI {
		homeDir, _ := os.UserHomeDir()
		logDir := filepath.Join(homeDir, ".anti-api")
		os.MkdirAll(logDir, 0755)
		if logFile, err := os.OpenFile(filepath.Join(logDir, "antiapi.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(logFile)
		} else {
			log.SetOutput(io.Discard)
		}
		appTUI = tui.Init(*port, srv)
		appTUI.Start()
		defer appTUI.Stop()
	} else {
		log.Printf("Starting AntiAPI Gateway v1.1.0 on http://%s", addr)
		log.Printf("Dashboard: http://localhost:%d/ | OpenAI: http://localhost:%d/v1", *port, *port)
	}

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

	// Cloudflare Tunnel
	var tun *tunnel.Tunnel
	if *enableTunnel || os.Getenv("ANTIAPI_TUNNEL") == "1" || os.Getenv("ANTIAPI_TUNNEL") == "true" {
		t, err := tunnel.Start(context.Background(), *port)
		if err != nil {
			log.Printf("[Tunnel] Warning: could not start cloudflared tunnel: %v", err)
		} else {
			tun = t
			go func() {
				url, err := tun.WaitURL(30 * time.Second)
				if err != nil {
					log.Printf("[Tunnel] Failed to resolve public URL: %v", err)
					return
				}
				srv.SetPublicURL(url)
				log.Printf("Public API Tunnel: %s/v1", url)
				if useTUI {
					tui.SetTunnelURL(url)
				}
			}()
		}
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			if useTUI && appTUI != nil {
				appTUI.Stop()
			}
			fmt.Fprintf(os.Stderr, "[antiapi] Server listen error: %v\n", err)
			os.Exit(1)
		}
	}()

	<-stop
	if tun != nil {
		tun.Stop()
	}
	if useTUI && appTUI != nil {
		appTUI.Stop()
	}
	log.Println("Shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
	log.Println("Server stopped")
}
