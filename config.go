package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var dailySummaryTimeRe = regexp.MustCompile(`^\d{4}$`)

// BootConfig holds infrastructure-level config loaded from env vars.
// These are NOT stored in the database — they're container/host-level settings.
type BootConfig struct {
	// Data directory for SQLite DB and encryption key
	DataDir string

	// HTTP server port
	Port string

	// Discord proxy (infrastructure — network routing, not per-action)
	DiscordProxy *url.URL

	// PBR routing (container networking)
	NotificationIface string
	PBRGateway        string
	PBRDev            string
	PBRTable          string
}

// LoadBootConfig reads infrastructure config from environment variables.
func LoadBootConfig() (*BootConfig, error) {
	cfg := &BootConfig{
		DataDir:           envOrDefault("DATA_DIR", "./data"),
		Port:              envOrDefault("PORT", "7373"),
		NotificationIface: os.Getenv("NOTIFICATION_INTERFACE"),
		PBRGateway:        envOrDefault("PBR_GATEWAY", "192.168.10.1"),
		PBRDev:            envOrDefault("PBR_DEV", "eth1"),
		PBRTable:          envOrDefault("PBR_TABLE", "10"),
	}

	// Discord proxy is optional at boot — only required if a Discord action exists
	if proxyStr := os.Getenv("DISCORD_PROXY"); proxyStr != "" {
		proxyURL, err := url.Parse(proxyStr)
		if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
			return nil, fmt.Errorf("DISCORD_PROXY must be a valid URL with scheme and host, got %q", proxyStr)
		}
		cfg.DiscordProxy = proxyURL
	}

	return cfg, nil
}

// PBREnabled returns true if PBR routing should be configured.
func (c *BootConfig) PBREnabled() bool {
	return c.NotificationIface != ""
}

// MigrateEnvToDB seeds the database from legacy env vars on first run.
// Only runs if the database has no sources configured.
func MigrateEnvToDB(db *DB, encKey []byte, logger *slog.Logger) error {
	sources, err := db.ListSources()
	if err != nil {
		return fmt.Errorf("listing sources: %w", err)
	}
	if len(sources) > 0 {
		logger.Debug("database already has sources, skipping env migration")
		return nil
	}

	logger.Info("empty database detected, migrating from env vars...")

	// Seed global settings
	seedSettings(db, logger)

	// Create default source from env vars
	sourceID, err := seedDefaultSource(db, logger)
	if err != nil {
		return err
	}
	if sourceID == 0 {
		logger.Info("no source created (no IP_CHECK_SERVICE configured)")
		return nil
	}

	// Create actions from env vars and link to the source
	seedActions(db, sourceID, encKey, logger)

	logger.Info("env migration complete")
	return nil
}

func seedSettings(db *DB, logger *slog.Logger) {
	settings := map[string]string{
		"daily_summary_time": envOrDefault("DAILY_SUMMARY_TIME", "2000"),
		"timezone":           envOrDefault("TIMEZONE", "Local"),
	}
	for k, v := range settings {
		if err := db.SetSetting(k, v); err != nil {
			logger.Warn("failed to seed setting", "key", k, "error", err)
		}
	}
}

func seedDefaultSource(db *DB, logger *slog.Logger) (int64, error) {
	ipService := envOrDefault("IP_CHECK_SERVICE", "https://icanhazip.com")
	schedule := envOrDefault("CRON_SCHEDULE", "*/5 * * * *")
	timezone := envOrDefault("TIMEZONE", "Local")

	cfg := DefaultSourceConfig()
	// If legacy env var specified a custom IP service, prepend it to probe list
	if ipService != "https://icanhazip.com" {
		cfg.ProbeURLs = append([]string{ipService}, cfg.ProbeURLs...)
	}

	source := &Source{
		Name:     "Default",
		Enabled:  true,
		Schedule: schedule,
		Timezone: timezone,
		Config:   cfg,
	}

	id, err := db.CreateSource(source)
	if err != nil {
		return 0, fmt.Errorf("creating default source: %w", err)
	}
	logger.Info("created default source", "id", id, "schedule", schedule)
	return id, nil
}

func seedActions(db *DB, sourceID int64, encKey []byte, logger *slog.Logger) {
	// Discord action
	if webhookURL := os.Getenv("DISCORD_WEBHOOK_URL"); webhookURL != "" {
		proxyURL := os.Getenv("DISCORD_PROXY")
		configMap := map[string]string{
			"webhook_url": webhookURL,
			"proxy_url":   proxyURL,
		}
		configJSON, _ := json.Marshal(configMap)

		// Encrypt secrets — fail if encryption fails (don't store unencrypted)
		configJSON, err := EncryptActionSecrets(configJSON, []string{"webhook_url"}, encKey)
		if err != nil {
			logger.Error("failed to encrypt Discord config, skipping action", "error", err)
			return
		}

		action := &Action{Type: "discord", Name: "Discord", Enabled: true, ConfigJSON: configJSON}
		actionID, err := db.CreateAction(action)
		if err != nil {
			logger.Warn("failed to create Discord action", "error", err)
		} else {
			db.LinkAction(&SourceAction{SourceID: sourceID, ActionID: actionID, TriggerOn: "both", Enabled: true})
			logger.Info("created Discord action", "id", actionID)
		}
	}

	// Cloudflare DNS action
	cfToken := os.Getenv("CF_API_TOKEN")
	cfZoneID := os.Getenv("CF_ZONE_ID")
	cfRecordIDs := os.Getenv("CF_DNS_RECORD_IDS")
	if cfToken != "" && cfZoneID != "" && cfRecordIDs != "" {
		// Parse and filter record IDs
		var ids []string
		for _, id := range strings.Split(cfRecordIDs, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}

		configMap := map[string]interface{}{
			"api_token":  cfToken,
			"zone_id":    cfZoneID,
			"record_ids": ids,
		}
		configJSON, _ := json.Marshal(configMap)
		var encErr error
		configJSON, encErr = EncryptActionSecrets(configJSON, []string{"api_token"}, encKey)
		if encErr != nil {
			logger.Error("failed to encrypt CF DNS config, skipping", "error", encErr)
			return
		}

		action := &Action{Type: "cloudflare_dns", Name: "Cloudflare DNS", Enabled: true, ConfigJSON: configJSON}
		actionID, err := db.CreateAction(action)
		if err != nil {
			logger.Warn("failed to create CF DNS action", "error", err)
		} else {
			db.LinkAction(&SourceAction{SourceID: sourceID, ActionID: actionID, TriggerOn: "ip_changed", Enabled: true})
			logger.Info("created Cloudflare DNS action", "id", actionID)
		}
	}

	// Cloudflare Gateway action
	cfAccountID := os.Getenv("CF_ACCOUNT_ID")
	cfLocationID := os.Getenv("CF_LOCATION_ID")
	if cfToken != "" && cfAccountID != "" && cfLocationID != "" {
		configMap := map[string]string{
			"api_token":   cfToken,
			"account_id":  cfAccountID,
			"location_id": cfLocationID,
		}
		configJSON, _ := json.Marshal(configMap)
		var encErr error
		configJSON, encErr = EncryptActionSecrets(configJSON, []string{"api_token"}, encKey)
		if encErr != nil {
			logger.Error("failed to encrypt CF Gateway config, skipping", "error", encErr)
			return
		}

		action := &Action{Type: "cloudflare_gateway", Name: "Cloudflare Gateway", Enabled: true, ConfigJSON: configJSON}
		actionID, err := db.CreateAction(action)
		if err != nil {
			logger.Warn("failed to create CF Gateway action", "error", err)
		} else {
			db.LinkAction(&SourceAction{SourceID: sourceID, ActionID: actionID, TriggerOn: "ip_changed", Enabled: true})
			logger.Info("created Cloudflare Gateway action", "id", actionID)
		}
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
