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
	colorGreen = 3066993 // 0x2ECC71 — daily summary "all good"
	avatarURL  = "https://i.imgur.com/gK9wLgS.png"
)

// Discord webhook payload types.

type DiscordPayload struct {
	Username  string         `json:"username"`
	AvatarURL string         `json:"avatar_url"`
	Embeds    []DiscordEmbed `json:"embeds"`
}

type DiscordEmbed struct {
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Color       int          `json:"color"`
	Fields      []EmbedField `json:"fields"`
	Timestamp   string       `json:"timestamp"`
	Footer      EmbedFooter  `json:"footer"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

// sendDiscordAlert sends an IP change notification to Discord.
func (a *App) sendDiscordAlert(ctx context.Context, oldIP, newIP, dnsStatus, gwStatus string) error {
	fields := []EmbedField{
		{Name: "Old IP Address", Value: oldIP, Inline: true},
		{Name: "New IP Address", Value: "**" + newIP + "**", Inline: true},
	}
	if dnsStatus != "N/A" {
		fields = append(fields, EmbedField{Name: "DNS Update", Value: dnsStatus, Inline: true})
	}
	if gwStatus != "N/A" {
		fields = append(fields, EmbedField{Name: "Gateway Update", Value: gwStatus, Inline: true})
	}

	payload := DiscordPayload{
		Username:  "Public IP Alert",
		AvatarURL: avatarURL,
		Embeds: []DiscordEmbed{
			{
				Title:       ":globe_with_meridians: Public IP Address Change Detected!",
				Description: "Your public IP address has been updated by the ISP.",
				Color:       colorBlue,
				Fields:      fields,
				Timestamp:   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				Footer:      EmbedFooter{Text: "WAN IP Check - Critical Alert"},
			},
		},
	}

	return a.postDiscord(ctx, payload)
}

// sendNoChangeAlert sends a daily "no change" summary to Discord.
func (a *App) sendNoChangeAlert(ctx context.Context, currentIP string) error {
	now := a.nowInTimezone()

	payload := DiscordPayload{
		Username:  "Public IP Monitor",
		AvatarURL: avatarURL,
		Embeds: []DiscordEmbed{
			{
				Title:       ":white_check_mark: Daily IP Summary: No Change",
				Description: fmt.Sprintf("As of %s, the public IP address remains unchanged today.", now.Format("15:04")),
				Color:       colorGreen,
				Fields: []EmbedField{
					{Name: "Current IP Address", Value: "**" + currentIP + "**", Inline: false},
				},
				Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				Footer:    EmbedFooter{Text: "WAN IP Check - Daily Summary"},
			},
		},
	}

	return a.postDiscord(ctx, payload)
}

// postDiscord marshals the payload and POSTs it to the Discord webhook via proxy.
func (a *App) postDiscord(ctx context.Context, payload DiscordPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling Discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.DiscordWebhookURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating Discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.proxyClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending Discord request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Discord returned HTTP %d", resp.StatusCode)
	}

	a.logger.Info("Discord notification sent", "status", resp.StatusCode)
	return nil
}
