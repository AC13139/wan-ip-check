package main

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var dailySummaryTimeRe = regexp.MustCompile(`^\d{4}$`)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	// Required
	DiscordWebhookURL string
	DiscordProxy      *url.URL

	// Optional with defaults
	IPCheckService   string
	CronSchedule     string
	DailySummaryTime string
	Timezone         string

	// Cloudflare (optional — all-or-nothing per group)
	CFAPIToken     string
	CFZoneID       string
	CFDNSRecordIDs []string
	CFAccountID    string
	CFLocationID   string

	// PBR routing (optional — only if NotificationIface set)
	NotificationIface string
	PBRGateway        string
	PBRDev            string
	PBRTable          string

	// State file paths
	IPFile           string
	SummaryTrackFile string
}

// CFDNSEnabled returns true if Cloudflare DNS update is configured.
func (c *Config) CFDNSEnabled() bool {
	return c.CFAPIToken != "" && c.CFZoneID != "" && len(c.CFDNSRecordIDs) > 0
}

// CFGatewayEnabled returns true if Cloudflare Gateway update is configured.
func (c *Config) CFGatewayEnabled() bool {
	return c.CFAPIToken != "" && c.CFAccountID != "" && c.CFLocationID != ""
}

// PBREnabled returns true if Policy-Based Routing is configured.
func (c *Config) PBREnabled() bool {
	return c.NotificationIface != ""
}

// LoadConfig reads configuration from environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		IPCheckService:   envOrDefault("IP_CHECK_SERVICE", "https://icanhazip.com"),
		CronSchedule:     envOrDefault("CRON_SCHEDULE", "*/5 * * * *"),
		DailySummaryTime: envOrDefault("DAILY_SUMMARY_TIME", "2000"),
		Timezone:         envOrDefault("TIMEZONE", "Local"),

		CFAPIToken:        os.Getenv("CF_API_TOKEN"),
		CFZoneID:          os.Getenv("CF_ZONE_ID"),
		CFAccountID:       os.Getenv("CF_ACCOUNT_ID"),
		CFLocationID:      os.Getenv("CF_LOCATION_ID"),
		NotificationIface: os.Getenv("NOTIFICATION_INTERFACE"),
		PBRGateway:        envOrDefault("PBR_GATEWAY", "192.168.10.1"),
		PBRDev:            envOrDefault("PBR_DEV", "eth1"),
		PBRTable:          envOrDefault("PBR_TABLE", "10"),

		IPFile:           "./app/last_public_ip.txt",
		SummaryTrackFile: "./app/last_daily_summary_date.txt",
	}

	// Required: Discord webhook URL
	cfg.DiscordWebhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	if cfg.DiscordWebhookURL == "" {
		return nil, fmt.Errorf("DISCORD_WEBHOOK_URL is required")
	}

	// Required: Discord proxy
	proxyStr := os.Getenv("DISCORD_PROXY")
	if proxyStr == "" {
		return nil, fmt.Errorf("DISCORD_PROXY is required")
	}
	proxyURL, err := url.Parse(proxyStr)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("DISCORD_PROXY must be a valid URL with scheme and host, got %q", proxyStr)
	}
	cfg.DiscordProxy = proxyURL

	// Validate DAILY_SUMMARY_TIME format (HHMM, valid range 0000-2359)
	if !dailySummaryTimeRe.MatchString(cfg.DailySummaryTime) {
		return nil, fmt.Errorf("DAILY_SUMMARY_TIME must be 4 digits (HHMM), got %q", cfg.DailySummaryTime)
	}
	hh := cfg.DailySummaryTime[:2]
	mm := cfg.DailySummaryTime[2:]
	if hh > "23" || mm > "59" {
		return nil, fmt.Errorf("DAILY_SUMMARY_TIME is not a valid time (HHMM), got %q", cfg.DailySummaryTime)
	}

	// Parse CF_DNS_RECORD_IDS (comma-separated, filter empty)
	if raw := os.Getenv("CF_DNS_RECORD_IDS"); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				cfg.CFDNSRecordIDs = append(cfg.CFDNSRecordIDs, id)
			}
		}
	}

	// Group-validate Cloudflare DNS config: if any DNS-related var is set, all must be
	if cfg.CFAPIToken != "" || cfg.CFZoneID != "" || len(cfg.CFDNSRecordIDs) > 0 {
		var missing []string
		if cfg.CFAPIToken == "" {
			missing = append(missing, "CF_API_TOKEN")
		}
		if cfg.CFZoneID == "" {
			missing = append(missing, "CF_ZONE_ID")
		}
		if len(cfg.CFDNSRecordIDs) == 0 {
			missing = append(missing, "CF_DNS_RECORD_IDS")
		}
		if len(missing) > 0 && len(missing) < 3 {
			return nil, fmt.Errorf("partial Cloudflare DNS config: missing %s", strings.Join(missing, ", "))
		}
	}

	// Group-validate Cloudflare Gateway config: if any Gateway-related var is set, all must be
	if cfg.CFAccountID != "" || cfg.CFLocationID != "" {
		var missing []string
		if cfg.CFAPIToken == "" {
			missing = append(missing, "CF_API_TOKEN")
		}
		if cfg.CFAccountID == "" {
			missing = append(missing, "CF_ACCOUNT_ID")
		}
		if cfg.CFLocationID == "" {
			missing = append(missing, "CF_LOCATION_ID")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("partial Cloudflare Gateway config: missing %s", strings.Join(missing, ", "))
		}
	}

	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
