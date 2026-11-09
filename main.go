package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"antiapi/auth"
	"antiapi/models"
	"antiapi/server"
	"antiapi/translator"
	"antiapi/transport"
	"antiapi/tui"
	"antiapi/tunnel"
	"antiapi/upstream"
)

func initAndroidDNS() {
	if runtime.GOOS == "android" || os.Getenv("ANDROID_ROOT") != "" {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 3 * time.Second}
				conn, err := d.DialContext(ctx, "udp", "1.1.1.1:53")
				if err != nil {
					return d.DialContext(ctx, "udp", "8.8.8.8:53")
				}
				return conn, nil
			},
		}
	}
}

func main() {
	initAndroidDNS()
	port := flag.Int("port", 8964, "Port to listen on")
	host := flag.String("host", "0.0.0.0", "Host address to bind")
	noTui := flag.Bool("no-tui", false, "Disable interactive TUI and use streaming logs")
	enableTunnel := flag.Bool("tunnel", false, "Start a cloudflared public tunnel and expose the API over HTTPS")
	fastMode := flag.Bool("fast-mode", false, "Enable low-latency fast mode (disables thinking budget for instant TTFT)")
	flag.Parse()

	if *fastMode || os.Getenv("ANTIAPI_FAST_MODE") == "1" || strings.ToLower(os.Getenv("ANTIAPI_FAST_MODE")) == "true" {
		translator.SetGlobalFastMode(true)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[antiapi] ")

	srv := server.NewServer()
	addr := fmt.Sprintf("%s:%d", *host, *port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	useTUI := !*noTui && tui.IsTerminal(os.Stdout.Fd())

	var appTUI *tui.TUI
	tui.SetQuotaRefreshHook(func() {
		auth.GetPool().ClearAllRateLimits()
		upstream.ClearAllEndpointCooldowns()
		for _, acc := range auth.GetPool().GetAllAccounts() {
			_, _ = acc.Refresh()
			_, _ = acc.FetchQuotaSummary()
		}
		_ = models.GetRegistry().FetchLiveModels()
	})

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
		if translator.IsGlobalFastMode() {
			log.Printf("⚡ Fast Mode: ENABLED (Zero-thinking budget for minimum latency)")
		}
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

	// Prewarm HTTP/2 multiplexed connections to Google Cloud Code upstream
	transport.PrewarmConnection(upstream.StandardEndpoint)
	transport.PrewarmConnection(upstream.DailyEndpoint)

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

	// Cloudflare Tunnel Manager initialization
	tunnel.GetManager().Init(*port, func(url string) {
		srv.SetPublicURL(url)
		if useTUI {
			tui.SetTunnelURL(url)
			tui.SetActivity(fmt.Sprintf("⚡ Tunnel active: %s", url))
		} else {
			log.Printf("Public API Tunnel: %s/v1", url)
		}
	}, func() {
		srv.SetPublicURL("")
		if useTUI {
			tui.SetTunnelURL("")
			tui.SetActivity("Cloudflare tunnel stopped")
		} else {
			log.Printf("Public API Tunnel stopped")
		}
	})

	if *enableTunnel || os.Getenv("ANTIAPI_TUNNEL") == "1" || strings.ToLower(os.Getenv("ANTIAPI_TUNNEL")) == "true" {
		go func() {
			if _, err := tunnel.GetManager().Start(); err != nil {
				log.Printf("[Tunnel] Warning: could not start cloudflared tunnel: %v", err)
			}
		}()
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
	tunnel.GetManager().Stop()
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
