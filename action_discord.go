package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	colorBlue  = 3447003 // 0x3498DB — IP change alert
	colorGreen = 3066993 // 0x2ECC71 — daily summary
	avatarURL  = "https://i.imgur.com/gK9wLgS.png"
)

// DiscordExecutor sends notifications via Discord webhook.
type DiscordExecutor struct {
	client *http.Client // proxy-configured client for Discord API access
}

type discordConfig struct {
	WebhookURL string `json:"webhook_url"`
	ProxyURL   string `json:"proxy_url"` // informational only — proxy is set on the HTTP client
}

func (e *DiscordExecutor) Type() string { return "discord" }

func (e *DiscordExecutor) SecretFields() []string { return []string{"webhook_url"} }

func (e *DiscordExecutor) DefaultConfig() json.RawMessage {
	data, _ := json.Marshal(discordConfig{})
	return data
}

func (e *DiscordExecutor) ConfigSchema() []ConfigField {
	return []ConfigField{
		{Name: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, IsSecret: true, HelpText: "Discord webhook URL (Settings > Integrations > Webhooks)"},
		{Name: "proxy_url", Label: "Proxy URL", Type: "text", Required: false, HelpText: "HTTP proxy for Discord access (set via DISCORD_PROXY env var)"},
	}
}

func (e *DiscordExecutor) ValidateConfig(raw json.RawMessage) error {
	var cfg discordConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return fmt.Errorf("webhook_url is required")
	}
	return nil
}

func (e *DiscordExecutor) Execute(ctx context.Context, event IPEvent, raw json.RawMessage) error {
	var cfg discordConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	var payload discordPayload
	switch event.EventType {
	case "ip_changed":
		payload = buildChangePayload(event)
	case "ip_unchanged_daily":
		payload = buildSummaryPayload(event)
	default:
		return fmt.Errorf("unsupported event type: %s", event.EventType)
	}

	return e.postWebhook(ctx, cfg.WebhookURL, payload)
}

func (e *DiscordExecutor) postWebhook(ctx context.Context, webhookURL string, payload discordPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Discord returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Discord payload types

type discordPayload struct {
	Username  string         `json:"username"`
	AvatarURL string         `json:"avatar_url"`
	Embeds    []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Color       int          `json:"color"`
	Fields      []embedField `json:"fields"`
	Timestamp   string       `json:"timestamp"`
	Footer      embedFooter  `json:"footer"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type embedFooter struct {
	Text string `json:"text"`
}

func buildChangePayload(event IPEvent) discordPayload {
	fields := []embedField{
		{Name: "WANInterface", Value: event.WANInterfaceName, Inline: true},
		{Name: "Old IP", Value: event.OldIP, Inline: true},
		{Name: "New IP", Value: "**" + event.NewIP + "**", Inline: true},
	}
	return discordPayload{
		Username:  "WAN IP Check",
		AvatarURL: avatarURL,
		Embeds: []discordEmbed{{
			Title:       ":globe_with_meridians: Public IP Address Change Detected!",
			Description: "Your public IP address has been updated by the ISP.",
			Color:       colorBlue,
			Fields:      fields,
			Timestamp:   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			Footer:      embedFooter{Text: "WAN IP Check"},
		}},
	}
}

func buildSummaryPayload(event IPEvent) discordPayload {
	return discordPayload{
		Username:  "WAN IP Check",
		AvatarURL: avatarURL,
		Embeds: []discordEmbed{{
			Title:       ":white_check_mark: Daily IP Summary: No Change",
			Description: fmt.Sprintf("WANInterface **%s**: IP address remains **%s**.", event.WANInterfaceName, event.NewIP),
			Color:       colorGreen,
			Fields: []embedField{
				{Name: "Current IP", Value: "**" + event.NewIP + "**", Inline: false},
			},
			Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			Footer:    embedFooter{Text: "WAN IP Check - Daily Summary"},
		}},
	}
}
