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

	"github.com/robfig/cron/v3"
)

// App holds runtime state: config, HTTP clients, and logger.
type App struct {
	cfg          *Config
	directClient *http.Client // Cloudflare API + IP check (no proxy)
	proxyClient  *http.Client // Discord webhook (via proxy)
	logger       *slog.Logger
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := LoadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	// Create state directory
	if err := os.MkdirAll("./app", 0755); err != nil {
		logger.Error("failed to create state directory", "error", err)
		os.Exit(1)
	}

	app := &App{
		cfg:          cfg,
		directClient: newHTTPClient(nil, 30*time.Second),
		proxyClient:  newHTTPClient(cfg.DiscordProxy, 30*time.Second),
		logger:       logger,
	}

	// Log config summary
	logger.Info("starting wan-ip-check",
		"ip_service", cfg.IPCheckService,
		"cron", cfg.CronSchedule,
		"daily_summary_time", cfg.DailySummaryTime,
		"timezone", cfg.Timezone,
		"cf_dns", cfg.CFDNSEnabled(),
		"cf_gateway", cfg.CFGatewayEnabled(),
		"pbr", cfg.PBREnabled(),
	)

	// Setup PBR if configured
	if cfg.PBREnabled() {
		if err := app.setupPBR(); err != nil {
			logger.Warn("PBR setup encountered errors", "error", err)
		}
	}

	// Load timezone for cron scheduler
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		logger.Warn("invalid TIMEZONE, using local", "timezone", cfg.Timezone, "error", err)
		loc = time.Now().Location()
	}

	// Validate cron schedule
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(cfg.CronSchedule)
	if err != nil {
		logger.Error("invalid CRON_SCHEDULE", "schedule", cfg.CronSchedule, "error", err)
		os.Exit(1)
	}
	nextRun := schedule.Next(time.Now().In(loc))
	logger.Info("cron schedule validated", "next_run", nextRun.Format(time.RFC3339))

	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Run first check immediately
	app.checkIP(ctx)

	// Start cron scheduler with timezone and skip-if-still-running
	c := cron.New(
		cron.WithLocation(loc),
		cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)),
	)
	c.AddFunc(cfg.CronSchedule, func() {
		app.checkIP(ctx)
	})
	c.Start()
	logger.Info("cron scheduler started")

	// Block until signal
	<-ctx.Done()
	logger.Info("shutdown signal received, stopping...")
	<-c.Stop().Done()
	logger.Info("shutdown complete")
}

// newHTTPClient creates an HTTP client with explicit transport settings.
func newHTTPClient(proxy *url.URL, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxy),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// setupPBR configures Policy-Based Routing for the notification interface.
func (a *App) setupPBR() error {
	iface := a.cfg.NotificationIface
	a.logger.Info("configuring PBR", "interface", iface)

	// Get container IP from the notification interface
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

	gw := a.cfg.PBRGateway
	dev := a.cfg.PBRDev
	table := a.cfg.PBRTable

	// ip route replace (idempotent)
	a.logger.Info("setting up routing table", "gateway", gw, "dev", dev, "table", table)
	out, err := exec.Command("ip", "route", "replace", "default", "via", gw, "dev", dev, "table", table).CombinedOutput()
	if err != nil {
		a.logger.Warn("ip route replace failed", "output", strings.TrimSpace(string(out)), "error", err)
	}

	// ip rule add (ignore if already exists)
	a.logger.Info("adding policy rule", "from", containerIP, "table", table)
	out, err = exec.Command("ip", "rule", "add", "from", containerIP, "lookup", table).CombinedOutput()
	if err != nil {
		a.logger.Warn("ip rule add failed (may already exist)", "output", strings.TrimSpace(string(out)), "error", err)
	}

	return nil
}
