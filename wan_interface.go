package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WANInterface represents an IP detection source (input plugin).
type WANInterface struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	Enabled   bool         `json:"enabled"`
	Schedule  string       `json:"schedule"`
	Timezone  string       `json:"timezone"`
	Config    WANInterfaceConfig `json:"config"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// WANInterfaceConfig holds the detection configuration for a source.
type WANInterfaceConfig struct {
	ProbeURLs    []string `json:"probe_urls"`
	Strategy     string   `json:"strategy"`      // "fallback" or "single"
	BindInterface string  `json:"bind_interface"` // optional: network interface for egress
	BindAddress  string   `json:"bind_address"`   // optional: source IP for egress
	TimeoutSecs  int      `json:"timeout_seconds"`
}

// WANInterfaceState holds the runtime state for a source.
type WANInterfaceState struct {
	WANInterfaceID            int64     `json:"source_id"`
	LastObservedIP      string    `json:"last_observed_ip"`
	LastCheckAt         time.Time `json:"last_check_at"`
	LastSuccessAt       time.Time `json:"last_success_at"`
	LastStatus          string    `json:"last_status"` // ok, changed, failed
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NextRunAt           time.Time `json:"next_run_at"`
	LastSummaryDate     string    `json:"last_summary_date"` // YYYY-MM-DD
	LastError           string    `json:"last_error"`
}

// DefaultWANInterfaceConfig returns sensible defaults for a new source.
func DefaultWANInterfaceConfig() WANInterfaceConfig {
	return WANInterfaceConfig{
		ProbeURLs: []string{
			"https://api.ipify.org",
			"https://checkip.amazonaws.com",
			"https://ifconfig.me/ip",
		},
		Strategy:    "fallback",
		TimeoutSecs: 10,
	}
}

// boundClients caches HTTP clients per bind config to avoid transport leaks.
var (
	boundClientsMu   sync.Mutex
	boundClientsCache = make(map[string]*http.Client)
)

// DetectIP probes the configured URLs to determine the current public IP.
// Uses the fallback strategy: try each URL in order, return first valid result.
func DetectIP(ctx context.Context, cfg WANInterfaceConfig, baseClient *http.Client) (string, error) {
	if len(cfg.ProbeURLs) == 0 {
		return "", fmt.Errorf("no probe URLs configured")
	}

	timeout := time.Duration(cfg.TimeoutSecs) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	// Use cached bound client to avoid transport/connection leaks
	client := baseClient
	if cfg.BindInterface != "" || cfg.BindAddress != "" {
		client = getBoundClient(cfg, timeout)
	}

	var lastErr error
	for _, probeURL := range cfg.ProbeURLs {
		ip, err := probeIP(ctx, client, probeURL, timeout)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", probeURL, err)
			continue
		}
		return ip, nil
	}

	return "", fmt.Errorf("all probes failed, last error: %w", lastErr)
}

// probeIP fetches the public IP from a single URL.
func probeIP(ctx context.Context, client *http.Client, url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", fmt.Errorf("reading body: %w", err)
	}

	ip := strings.TrimSpace(string(body))
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("invalid IPv4: %q", ip)
	}

	return ip, nil
}

// getBoundClient returns a cached HTTP client bound to a specific interface/address.
func getBoundClient(cfg WANInterfaceConfig, timeout time.Duration) *http.Client {
	key := fmt.Sprintf("%s|%s|%s", cfg.BindInterface, cfg.BindAddress, timeout)
	boundClientsMu.Lock()
	defer boundClientsMu.Unlock()

	if c, ok := boundClientsCache[key]; ok {
		return c
	}
	c := buildBoundClient(cfg, timeout)
	boundClientsCache[key] = c
	return c
}

// buildBoundClient creates an HTTP client that binds to a specific interface or address.
func buildBoundClient(cfg WANInterfaceConfig, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}

	if cfg.BindAddress != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(cfg.BindAddress)}
	} else if cfg.BindInterface != "" {
		// Resolve interface to IP
		iface, err := net.InterfaceByName(cfg.BindInterface)
		if err == nil {
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					dialer.LocalAddr = &net.TCPAddr{IP: ipNet.IP}
					break
				}
			}
		}
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{Transport: transport, Timeout: timeout}
}
