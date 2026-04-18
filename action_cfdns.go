package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// CFDNSExecutor updates Cloudflare DNS A records.
type CFDNSExecutor struct {
	client *http.Client
}

type cfDNSConfig struct {
	APIToken string        `json:"api_token"`
	ZoneID   string        `json:"zone_id"`
	ZoneName string        `json:"zone_name,omitempty"` // display cache; zone_id authoritative
	Records  []cfDNSRecord `json:"records,omitempty"`

	// Deprecated — kept only so already-saved configs keep working until
	// the user resaves through the new form. Ignored when Records is set.
	RecordIDs []string `json:"record_ids,omitempty"`
}

type cfDNSRecord struct {
	Name    string `json:"name"`    // "@", "*", short ("api"), or FQDN ("api.example.com")
	Proxied bool   `json:"proxied"` // orange cloud
}

func (e *CFDNSExecutor) Type() string { return "cloudflare_dns" }

func (e *CFDNSExecutor) DisplayName() string { return "Cloudflare DNS Record" }

func (e *CFDNSExecutor) Description() string {
	return "Update DNS A records (e.g. home.example.com) to point at your current WAN IP whenever it changes."
}

func (e *CFDNSExecutor) SecretFields() []string { return []string{"api_token"} }

func (e *CFDNSExecutor) DefaultConfig() json.RawMessage {
	cfg := cfDNSConfig{Records: []cfDNSRecord{}}
	data, _ := json.Marshal(cfg)
	return data
}

// CustomFormTemplate returns the partial name that renders the Cloudflare
// DNS form (zone loader + records repeater). Declared on a concrete type;
// handleActionForm uses a type assertion so only this executor opts in.
func (e *CFDNSExecutor) CustomFormTemplate() string { return "cloudflare-dns-fields" }

// ParseForm converts the custom cloudflare-dns form submission back into the
// JSON config shape. Called via type-assertion from the action save path so
// we do not pollute the generic executor interface.
func (e *CFDNSExecutor) ParseForm(form url.Values) (json.RawMessage, error) {
	cfg := cfDNSConfig{
		APIToken: strings.TrimSpace(form.Get("api_token")),
		ZoneID:   strings.TrimSpace(form.Get("zone_id")),
		ZoneName: strings.TrimSpace(form.Get("zone_name")),
		Records:  []cfDNSRecord{},
	}
	if raw := strings.TrimSpace(form.Get("records_json")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Records); err != nil {
			return nil, fmt.Errorf("parsing records: %w", err)
		}
	}
	return json.Marshal(cfg)
}

// PrepareFormView transforms the raw config map (parsed from masked JSON)
// into the shape the cloudflare-dns-fields template expects: records
// serialised back to JSON for the hidden field, and legacy record_ids
// formatted as a comma-joined string for the migration banner.
func (e *CFDNSExecutor) PrepareFormView(cfg map[string]interface{}) map[string]interface{} {
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	// Records → records_json
	hasRecords := false
	if recs, ok := cfg["records"]; ok {
		if arr, ok := recs.([]interface{}); ok && len(arr) > 0 {
			hasRecords = true
		}
		if b, err := json.Marshal(recs); err == nil {
			cfg["records_json"] = string(b)
		}
	} else {
		cfg["records_json"] = "[]"
	}
	// Legacy banner only when the config is genuinely legacy-only: has
	// record_ids but no new-shape records. Avoids reporting "legacy mode"
	// on a freshly migrated action that still carries stale record_ids.
	if !hasRecords {
		if ids, ok := cfg["record_ids"]; ok {
			if arr, ok := ids.([]interface{}); ok && len(arr) > 0 {
				parts := make([]string, 0, len(arr))
				for _, v := range arr {
					if s, ok := v.(string); ok && s != "" {
						parts = append(parts, s)
					}
				}
				if len(parts) > 0 {
					cfg["legacy_record_ids"] = strings.Join(parts, ", ")
				}
			}
		}
	}
	return cfg
}

func (e *CFDNSExecutor) ConfigSchema() []ConfigField {
	// Schema kept for Test button's generic display and for callers that
	// only need field metadata; the actual form is rendered via the custom
	// partial above.
	return []ConfigField{
		{Name: "api_token", Label: "API Token", Type: "password", Required: true, IsSecret: true,
			Placeholder: "cloudflare-api-token",
			HelpText:    "Create at dash.cloudflare.com → Profile → API Tokens. Requires Zone:Read + DNS:Edit permissions."},
		{Name: "zone_id", Label: "Zone ID", Type: "text", Required: true,
			HelpText: "Picked from the Zone dropdown; populated via Load Zones."},
		{Name: "records", Label: "Records", Type: "text", Required: true,
			HelpText: "Name + Proxied toggle per record; added via the Records repeater."},
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
	if len(cfg.Records) == 0 && len(cfg.RecordIDs) == 0 {
		return fmt.Errorf("add at least one DNS record")
	}
	return nil
}

func (e *CFDNSExecutor) Execute(ctx context.Context, event IPEvent, raw json.RawMessage) error {
	var cfg cfDNSConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Legacy path — unchanged behavior until the user resaves.
	if len(cfg.Records) == 0 && len(cfg.RecordIDs) > 0 {
		return e.executeLegacy(ctx, cfg, event.NewIP)
	}

	// New path: list existing A records, PATCH-or-POST each configured record.
	existing, err := e.listARecords(ctx, cfg.APIToken, cfg.ZoneID)
	if err != nil {
		return fmt.Errorf("listing zone records: %w", err)
	}
	byName := make(map[string]cfRecordSummary, len(existing))
	for _, r := range existing {
		byName[r.Name] = r
	}

	var (
		ok     []string
		failed []string
	)
	for _, rec := range cfg.Records {
		full := canonicalizeRecordName(rec.Name, cfg.ZoneName)
		if full == "" {
			failed = append(failed, rec.Name+"=empty name")
			continue
		}
		match, found := byName[full]
		var err error
		if found {
			err = e.updateRecord(ctx, cfg.APIToken, cfg.ZoneID, match.ID, full, event.NewIP, rec.Proxied)
		} else {
			err = e.createRecord(ctx, cfg.APIToken, cfg.ZoneID, full, event.NewIP, rec.Proxied)
		}
		if err != nil {
			failed = append(failed, rec.Name+"="+err.Error())
		} else {
			ok = append(ok, rec.Name)
		}
	}

	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("updated %d/%d records, failed: [%s]",
		len(ok), len(ok)+len(failed), strings.Join(failed, "; "))
}

// executeLegacy runs the pre-migration record_ids path so existing saved
// actions keep working until the user resaves through the new form.
func (e *CFDNSExecutor) executeLegacy(ctx context.Context, cfg cfDNSConfig, newIP string) error {
	total := len(cfg.RecordIDs)
	success := 0
	for _, recordID := range cfg.RecordIDs {
		url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, cfg.ZoneID, recordID)
		body := map[string]string{"content": newIP}
		resp, err := e.cfRequest(ctx, http.MethodPatch, url, cfg.APIToken, body)
		if err != nil {
			return fmt.Errorf("record %s: %w", recordID, err)
		}
		if !resp.Success {
			return fmt.Errorf("record %s: %s", recordID, cfErrMsg(resp))
		}
		success++
	}
	if success < total {
		return fmt.Errorf("updated %d/%d records (legacy)", success, total)
	}
	return nil
}

// canonicalizeRecordName expands user-facing record names (@, short name,
// or already-qualified) to the fully qualified name Cloudflare returns.
// Returns "" when the inputs cannot produce a valid FQDN.
func canonicalizeRecordName(name, zone string) string {
	name = strings.TrimSpace(name)
	zone = strings.TrimSpace(zone)
	if name == "" {
		return ""
	}
	switch name {
	case "@":
		return zone
	case "*":
		if zone == "" {
			return ""
		}
		return "*." + zone
	}
	// already qualified (ends with zone or contains multiple labels)
	if zone != "" && (name == zone || strings.HasSuffix(name, "."+zone)) {
		return name
	}
	// short name — append zone
	if zone != "" {
		return name + "." + zone
	}
	return name
}

// cfRecordSummary is the subset of the Cloudflare DNS-records response used
// internally for matching.
type cfRecordSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListCloudflareZones returns every zone the token has access to. Exported
// so the UI zone-loader handler can call it directly.
func ListCloudflareZones(ctx context.Context, client *http.Client, token string) ([]cfZone, error) {
	var all []cfZone
	page := 1
	for {
		url := fmt.Sprintf("%s/zones?per_page=50&page=%d", cfAPIBase, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Success    bool     `json:"success"`
			Errors     []cfErr  `json:"errors"`
			Result     []cfZone `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding zones: %w", decodeErr)
		}
		if !envelope.Success {
			if len(envelope.Errors) > 0 {
				return nil, fmt.Errorf("cloudflare: %s", envelope.Errors[0].Message)
			}
			return nil, fmt.Errorf("cloudflare: unknown error")
		}
		all = append(all, envelope.Result...)
		if envelope.ResultInfo.Page >= envelope.ResultInfo.TotalPages || envelope.ResultInfo.TotalPages == 0 {
			break
		}
		page++
		if page > 20 {
			break // safety cap
		}
	}
	return all, nil
}

// listARecords fetches all type-A DNS records in a zone, paginating.
func (e *CFDNSExecutor) listARecords(ctx context.Context, token, zoneID string) ([]cfRecordSummary, error) {
	var all []cfRecordSummary
	page := 1
	for {
		url := fmt.Sprintf("%s/zones/%s/dns_records?type=A&per_page=100&page=%d", cfAPIBase, zoneID, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e.client.Do(req)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Success    bool              `json:"success"`
			Errors     []cfErr           `json:"errors"`
			Result     []cfRecordSummary `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding records: %w", decodeErr)
		}
		if !envelope.Success {
			if len(envelope.Errors) > 0 {
				return nil, fmt.Errorf("cloudflare: %s", envelope.Errors[0].Message)
			}
			return nil, fmt.Errorf("cloudflare: unknown error")
		}
		all = append(all, envelope.Result...)
		if envelope.ResultInfo.Page >= envelope.ResultInfo.TotalPages || envelope.ResultInfo.TotalPages == 0 {
			break
		}
		page++
		if page > 20 {
			break
		}
	}
	return all, nil
}

func (e *CFDNSExecutor) updateRecord(ctx context.Context, token, zoneID, recordID, name, newIP string, proxied bool) error {
	// PATCH only the fields we own; leave comment, tags, settings, etc. alone.
	// name is unused here because Cloudflare keys updates by record ID — we
	// accept it for symmetry with createRecord but do not send it.
	_ = name
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	body := map[string]interface{}{
		"content": newIP,
		"proxied": proxied,
	}
	resp, err := e.cfRequest(ctx, http.MethodPatch, url, token, body)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", cfErrMsg(resp))
	}
	return nil
}

func (e *CFDNSExecutor) createRecord(ctx context.Context, token, zoneID, name, newIP string, proxied bool) error {
	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	body := map[string]interface{}{
		"type":    "A",
		"name":    name,
		"content": newIP,
		"proxied": proxied,
		"ttl":     1,
	}
	resp, err := e.cfRequest(ctx, http.MethodPost, url, token, body)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", cfErrMsg(resp))
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
