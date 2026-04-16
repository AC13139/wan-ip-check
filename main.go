package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// App holds runtime state: config, database, HTTP clients, registries, and logger.
type App struct {
	boot         *BootConfig
	db           *DB
	encKey       []byte
	registry     *ActionRegistry
	directClient *http.Client // Cloudflare API + IP check (no proxy)
	proxyClient  *http.Client // Discord webhook (via proxy)
	logger       *slog.Logger
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Load infrastructure config from env vars
	boot, err := LoadBootConfig()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	// Create data directory
	if err := os.MkdirAll(boot.DataDir, 0700); err != nil {
		logger.Error("failed to create data directory", "error", err)
		os.Exit(1)
	}

	// Also create legacy state dir (for backward compat during transition)
	os.MkdirAll("./app", 0755)

	// Load or generate encryption key
	encKey, err := LoadOrGenerateKey(boot.DataDir, logger)
	if err != nil {
		logger.Error("encryption key error", "error", err)
		os.Exit(1)
	}

	// Open database
	db, err := OpenDB(boot.DataDir, logger)
	if err != nil {
		logger.Error("database error", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Migrate legacy env vars to database on first run
	if err := MigrateEnvToDB(db, encKey, logger); err != nil {
		logger.Error("env migration error", "error", err)
		os.Exit(1)
	}

	// Build HTTP clients
	directClient := newHTTPClient(nil, 30*time.Second)
	var proxyClient *http.Client
	if boot.DiscordProxy != nil {
		proxyClient = newHTTPClient(boot.DiscordProxy, 30*time.Second)
	} else {
		proxyClient = directClient // fallback to direct if no proxy configured
	}

	// Build action registry and register all built-in executors
	registry := NewActionRegistry()
	registry.Register(&CFDNSExecutor{client: directClient})
	registry.Register(&CFGatewayExecutor{client: directClient})
	registry.Register(&DiscordExecutor{client: proxyClient})
	registry.Register(&WebhookExecutor{client: directClient})

	app := &App{
		boot:         boot,
		db:           db,
		encKey:       encKey,
		registry:     registry,
		directClient: directClient,
		proxyClient:  proxyClient,
		logger:       logger,
	}

	// Log startup info
	sources, _ := db.ListSources()
	actions, _ := db.ListActions()
	logger.Info("starting wan-ip-check v2",
		"port", boot.Port,
		"data_dir", boot.DataDir,
		"sources", len(sources),
		"actions", len(actions),
		"pbr", boot.PBREnabled(),
	)

	// Setup PBR if configured
	if boot.PBREnabled() {
		if err := app.setupPBR(); err != nil {
			logger.Warn("PBR setup encountered errors", "error", err)
		}
	}

	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Start dispatcher (source check scheduling + action execution)
	dispatcher := NewDispatcher(app)
	if err := dispatcher.Start(ctx); err != nil {
		logger.Error("failed to start dispatcher", "error", err)
		os.Exit(1)
	}

	// Start HTTP server (API + Web UI)
	srv, err := NewServer(app, dispatcher)
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	logger.Info("wan-ip-check running", "port", boot.Port)

	// Block until signal
	<-ctx.Done()
	logger.Info("shutdown signal received, stopping...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
	dispatcher.Stop()
	logger.Info("shutdown complete")
}

// newHTTPClient creates an HTTP client with explicit transport settings.
func newHTTPClient(proxy *url.URL, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// setupPBR configures Policy-Based Routing for the notification interface.
func (a *App) setupPBR() error {
	iface := a.boot.NotificationIface
	a.logger.Info("configuring PBR", "interface", iface)

	netIface, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", iface, err)
	}
	addrs, err := netIface.Addrs()
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("no addresses on interface %s", iface)
	}

	var containerIP string
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
			containerIP = ipNet.IP.String()
			break
		}
	}
	if containerIP == "" {
		return fmt.Errorf("no IPv4 address found on interface %s", iface)
	}

	gw := a.boot.PBRGateway
	dev := a.boot.PBRDev
	table := a.boot.PBRTable

	a.logger.Info("setting up routing table", "gateway", gw, "dev", dev, "table", table)
	out, err := exec.Command("ip", "route", "replace", "default", "via", gw, "dev", dev, "table", table).CombinedOutput()
	if err != nil {
		a.logger.Warn("ip route replace failed", "output", strings.TrimSpace(string(out)), "error", err)
	}

	a.logger.Info("adding policy rule", "from", containerIP, "table", table)
	out, err = exec.Command("ip", "rule", "add", "from", containerIP, "lookup", table).CombinedOutput()
	if err != nil {
		a.logger.Warn("ip rule add failed (may already exist)", "output", strings.TrimSpace(string(out)), "error", err)
	}

	return nil
}
