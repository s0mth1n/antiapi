package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"antiapi/auth"
	"antiapi/models"
	"antiapi/server"
	"antiapi/translator"
	"antiapi/transport"
	"antiapi/tunnel"
	"antiapi/upstream"
)

func initAndroidDNS() {
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

func main() {
	initAndroidDNS()
	port := flag.Int("port", 8964, "Port to listen on")
	host := flag.String("host", "0.0.0.0", "Host address to bind")
	_ = flag.Bool("no-tui", true, "Compatibility flag for Android background service")
	enableTunnel := flag.Bool("tunnel", false, "Start a cloudflared public tunnel")
	fastMode := flag.Bool("fast-mode", false, "Enable low-latency fast mode (disables thinking budget)")
	flag.Parse()

	if *fastMode || os.Getenv("ANTIAPI_FAST_MODE") == "1" || strings.ToLower(os.Getenv("ANTIAPI_FAST_MODE")) == "true" {
		translator.SetGlobalFastMode(true)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[antiapi-android] ")

	srv := server.NewServer()
	addr := fmt.Sprintf("%s:%d", *host, *port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("Starting AntiAPI Android Service on http://%s", addr)
	if translator.IsGlobalFastMode() {
		log.Printf("⚡ Fast Mode: ENABLED (Zero-thinking budget)")
	}

	pool := auth.GetPool()
	for _, acc := range pool.GetAllAccounts() {
		log.Printf("Loaded account: %s", acc.Email)
		go acc.Refresh()
	}

	transport.PrewarmConnection(upstream.StandardEndpoint)
	transport.PrewarmConnection(upstream.DailyEndpoint)

	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := models.GetRegistry().FetchLiveModels(); err != nil {
			log.Printf("[Models] Background fetch error: %v", err)
		}
	}()

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
		log.Printf("Public API Tunnel: %s/v1", url)
	}, func() {
		srv.SetPublicURL("")
		log.Printf("Public API Tunnel stopped")
	})

	if *enableTunnel || os.Getenv("ANTIAPI_TUNNEL") == "1" || strings.ToLower(os.Getenv("ANTIAPI_TUNNEL")) == "true" {
		go func() {
			if _, err := tunnel.GetManager().Start(); err != nil {
				log.Printf("[Tunnel] Warning: could not start cloudflared tunnel: %v", err)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[antiapi-android] Server listen error: %v\n", err)
			os.Exit(1)
		}
	}()

	<-stop
	tunnel.GetManager().Stop()
	log.Println("Shutting down gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	log.Println("Server stopped")
}
