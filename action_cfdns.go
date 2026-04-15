package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// CFDNSExecutor updates Cloudflare DNS A records.
type CFDNSExecutor struct {
	client *http.Client
}

type cfDNSConfig struct {
	APIToken  string   `json:"api_token"`
	ZoneID    string   `json:"zone_id"`
	RecordIDs []string `json:"record_ids"`
}

func (e *CFDNSExecutor) Type() string { return "cloudflare_dns" }

func (e *CFDNSExecutor) SecretFields() []string { return []string{"api_token"} }

func (e *CFDNSExecutor) DefaultConfig() json.RawMessage {
	cfg := cfDNSConfig{RecordIDs: []string{}}
	data, _ := json.Marshal(cfg)
	return data
}

func (e *CFDNSExecutor) ConfigSchema() []ConfigField {
	return []ConfigField{
		{Name: "api_token", Label: "API Token", Type: "password", Required: true, IsSecret: true, HelpText: "Cloudflare API token with DNS edit permission"},
		{Name: "zone_id", Label: "Zone ID", Type: "text", Required: true, HelpText: "Found in Cloudflare dashboard > domain > Overview"},
		{Name: "record_ids", Label: "DNS Record IDs", Type: "tags", Required: true, HelpText: "Comma-separated A record IDs to update"},
	}
}

func (e *CFDNSExecutor) ValidateConfig(raw json.RawMessage) error {
	var cfg cfDNSConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.APIToken == "" {
		return fmt.Errorf("api_token is required")
	}
	if cfg.ZoneID == "" {
		return fmt.Errorf("zone_id is required")
	}
	if len(cfg.RecordIDs) == 0 {
		return fmt.Errorf("at least one record_id is required")
	}
	return nil
}

func (e *CFDNSExecutor) Execute(ctx context.Context, event IPEvent, raw json.RawMessage) error {
	var cfg cfDNSConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	total := len(cfg.RecordIDs)
	success := 0

	for _, recordID := range cfg.RecordIDs {
		if err := e.patchRecord(ctx, cfg.APIToken, cfg.ZoneID, recordID, event.NewIP); err != nil {
			return fmt.Errorf("record %s: %w", recordID, err)
		}
		success++
	}

	if success < total {
		return fmt.Errorf("updated %d/%d records", success, total)
	}
	return nil
}

func (e *CFDNSExecutor) patchRecord(ctx context.Context, token, zoneID, recordID, newIP string) error {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)

	// GET to validate record exists
	getResp, err := e.cfRequest(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return fmt.Errorf("GET failed: %w", err)
	}
	if !getResp.Success {
		return fmt.Errorf("GET failed: %s", cfErrMsg(getResp))
	}

	// PATCH content only
	body := map[string]string{"content": newIP}
	patchResp, err := e.cfRequest(ctx, http.MethodPatch, url, token, body)
	if err != nil {
		return fmt.Errorf("PATCH failed: %w", err)
	}
	if !patchResp.Success {
		return fmt.Errorf("PATCH failed: %s", cfErrMsg(patchResp))
	}

	return nil
}

// Shared Cloudflare API types and helpers

type cfResponse struct {
	Success bool     `json:"success"`
	Errors  []cfErr  `json:"errors"`
}

type cfErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *CFDNSExecutor) cfRequest(ctx context.Context, method, url, token string, body interface{}) (*cfResponse, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var cfResp cfResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &cfResp, nil
}

func cfErrMsg(resp *cfResponse) string {
	if len(resp.Errors) > 0 {
		return resp.Errors[0].Message
	}
	return "unknown error"
}
