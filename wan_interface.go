package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// soMark is Linux SO_MARK socket option value. Defined locally to avoid a
// dependency on golang.org/x/sys. Setting this on a socket tags its egress
// packets with an fwmark matched by `ip rule add fwmark ... lookup TBL`.
const soMark = 36

// WANInterface represents an IP detection WAN interface (input plugin).
type WANInterface struct {
	ID        int64              `json:"id"`
	Name      string             `json:"name"`
	Enabled   bool               `json:"enabled"`
	Schedule  string             `json:"schedule"`
	Timezone  string             `json:"timezone"`
	Config    WANInterfaceConfig `json:"config"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// EgressConfig defines how IP detection traffic is routed.
type EgressConfig struct {
	Mode          string `json:"mode"`           // "default", "gateway", "proxy"
	InterfaceName string `json:"interface_name"`  // for default/gateway modes
	SourceAddress string `json:"source_address"`  // auto-detected or manual override
	Gateway       string `json:"gateway"`          // for gateway mode (PBR routing)
	ProxyURL      string `json:"proxy_url"`        // for proxy mode
}

// WANInterfaceConfig holds the detection configuration for a WAN interface.
type WANInterfaceConfig struct {
	ProbeURLs   []string     `json:"probe_urls"`
	Strategy    string       `json:"strategy"`        // "fallback" or "single"
	TimeoutSecs int          `json:"timeout_seconds"`
	Egress      EgressConfig `json:"egress"`
	// Deprecated: kept for backward-compat JSON loading
	BindInterface string `json:"bind_interface,omitempty"`
	BindAddress   string `json:"bind_address,omitempty"`
}

// MigrateEgress converts old BindInterface/BindAddress to EgressConfig.
func (c *WANInterfaceConfig) MigrateEgress() {
	if c.Egress.Mode != "" {
		return // already migrated
	}
	if c.BindInterface != "" || c.BindAddress != "" {
		c.Egress = EgressConfig{
			Mode:          "default",
			InterfaceName: c.BindInterface,
			SourceAddress: c.BindAddress,
		}
	} else {
		c.Egress = EgressConfig{Mode: "default"}
	}
}

// WANInterfaceState holds the runtime state for a WAN interface.
type WANInterfaceState struct {
	WANInterfaceID      int64     `json:"wan_interface_id"`
	LastObservedIP      string    `json:"last_observed_ip"`
	LastCheckAt         time.Time `json:"last_check_at"`
	LastSuccessAt       time.Time `json:"last_success_at"`
	LastStatus          string    `json:"last_status"` // ok, changed, failed
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NextRunAt           time.Time `json:"next_run_at"`
	LastSummaryDate     string    `json:"last_summary_date"` // YYYY-MM-DD
	LastError           string    `json:"last_error"`
}

// DefaultWANInterfaceConfig returns sensible defaults for a new WAN interface.
func DefaultWANInterfaceConfig() WANInterfaceConfig {
	return WANInterfaceConfig{
		ProbeURLs: []string{
			"https://api.ipify.org",
			"https://checkip.amazonaws.com",
			"https://ifconfig.me/ip",
		},
		Strategy:    "fallback",
		TimeoutSecs: 10,
		Egress:      EgressConfig{Mode: "default"},
	}
}

// Client caches to avoid transport leaks.
var (
	clientsMu    sync.Mutex
	clientsCache = make(map[string]*http.Client)
)

// DetectIP probes the configured URLs to determine the current public IP.
// Dispatches to the correct egress mode: default, gateway, or proxy.
func DetectIP(ctx context.Context, cfg WANInterfaceConfig, baseClient *http.Client) (string, error) {
	if len(cfg.ProbeURLs) == 0 {
		return "", fmt.Errorf("no probe URLs configured")
	}

	cfg.MigrateEgress()

	timeout := time.Duration(cfg.TimeoutSecs) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	client, err := buildEgressClient(cfg.Egress, baseClient, timeout)
	if err != nil {
		return "", fmt.Errorf("building egress client: %w", err)
	}

	urls := cfg.ProbeURLs
	if cfg.Strategy == "single" && len(urls) > 0 {
		urls = urls[:1] // only use the first URL
	}

	var lastErr error
	for _, probeURL := range urls {
		ip, err := probeIP(ctx, client, probeURL, timeout)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", probeURL, err)
			continue
		}
		return ip, nil
	}

	return "", fmt.Errorf("all probes failed, last error: %w", lastErr)
}

// buildEgressClient returns an HTTP client configured for the egress mode.
func buildEgressClient(egress EgressConfig, baseClient *http.Client, timeout time.Duration) (*http.Client, error) {
	switch egress.Mode {
	case "proxy":
		if egress.ProxyURL == "" {
			return nil, fmt.Errorf("proxy mode requires proxy_url")
		}
		return getProxyClient(egress.ProxyURL, timeout)

	case "gateway":
		// Gateway mode: tag probe sockets with an fwmark matching the PBR rule
		// installed by the dispatcher. The same deterministic derivation is
		// used by both EnsureRoute and here so we don't need to plumb the
		// mark through config.
		if egress.InterfaceName == "" || egress.Gateway == "" {
			return baseClient, nil
		}
		_, mark := DeriveMarkAndTable(egress.InterfaceName + "|" + egress.Gateway)
		return getCachedClient(fmt.Sprintf("mark|%d|%s", mark, timeout), func() *http.Client {
			return buildClientWithMark(mark, timeout)
		}), nil

	default: // "default" or ""
		if egress.InterfaceName != "" || egress.SourceAddress != "" {
			addr := egress.SourceAddress
			if addr == "" && egress.InterfaceName != "" {
				resolved, err := resolveSourceIP(egress.InterfaceName)
				if err != nil {
					return nil, fmt.Errorf("default mode: interface %q not found or has no IPv4: %w", egress.InterfaceName, err)
				}
				addr = resolved
			}
			if addr != "" {
				return getCachedClient("bind|"+addr+"|"+timeout.String(), func() *http.Client {
					return buildClientWithLocalAddr(addr, timeout)
				}), nil
			}
		}
		return baseClient, nil
	}
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

// resolveSourceIP gets the first IPv4 address from a named network interface.
func resolveSourceIP(interfaceName string) (string, error) {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return "", fmt.Errorf("interface %s not found: %w", interfaceName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("listing addresses for %s: %w", interfaceName, err)
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address on interface %s", interfaceName)
}

// getCachedClient returns a cached client or creates one using the factory.
func getCachedClient(key string, factory func() *http.Client) *http.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clientsCache[key]; ok {
		return c
	}
	c := factory()
	clientsCache[key] = c
	return c
}

// getProxyClient returns a cached HTTP client configured with the given proxy.
func getProxyClient(proxyURLStr string, timeout time.Duration) (*http.Client, error) {
	proxyURL, err := url.Parse(proxyURLStr)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL: %q", proxyURLStr)
	}
	key := "proxy|" + proxyURLStr + "|" + timeout.String()
	return getCachedClient(key, func() *http.Client {
		transport := &http.Transport{
			Proxy:                 http.ProxyURL(proxyURL),
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:  10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		return &http.Client{Transport: transport, Timeout: timeout}
	}), nil
}

// buildClientWithMark creates an HTTP client that tags each outbound
// connection with the given fwmark (via SO_MARK). Requires CAP_NET_ADMIN.
// Traffic is then routed by the PBR rule `ip rule fwmark MARK lookup TBL`.
func buildClientWithMark(mark uint32, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			ctrlErr := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soMark, int(mark))
			})
			if ctrlErr != nil {
				return ctrlErr
			}
			return sockErr
		},
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// buildClientWithLocalAddr creates an HTTP client bound to a specific source IP.
func buildClientWithLocalAddr(addr string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   timeout,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(addr)},
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
