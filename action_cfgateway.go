package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CFGatewayExecutor updates Cloudflare Zero Trust Gateway DNS location source IP.
type CFGatewayExecutor struct {
	client *http.Client
}

type cfGatewayConfig struct {
	APIToken   string `json:"api_token"`
	AccountID  string `json:"account_id"`
	LocationID string `json:"location_id"`
}

func (e *CFGatewayExecutor) Type() string { return "cloudflare_gateway" }

func (e *CFGatewayExecutor) DisplayName() string { return "Cloudflare Gateway Locations" }

func (e *CFGatewayExecutor) Description() string {
	return "Update the External IP on a Zero Trust Gateway location so DNS-filtering policies keep matching traffic from your WAN."
}

func (e *CFGatewayExecutor) SecretFields() []string { return []string{"api_token"} }

func (e *CFGatewayExecutor) DefaultConfig() json.RawMessage {
	data, _ := json.Marshal(cfGatewayConfig{})
	return data
}

func (e *CFGatewayExecutor) ConfigSchema() []ConfigField {
	return []ConfigField{
		{Name: "api_token", Label: "API Token", Type: "password", Required: true, IsSecret: true,
			Placeholder: "cloudflare-api-token",
			HelpText:    "Cloudflare API token with Account:Zero Trust:Edit permission. Create at dash.cloudflare.com → Profile → API Tokens."},
		{Name: "account_id", Label: "Account ID", Type: "text", Required: true,
			Placeholder: "32-character hex string",
			HelpText:    "Found in Cloudflare dashboard → home → right sidebar under Account Details."},
		{Name: "location_id", Label: "Location ID", Type: "text", Required: true,
			Placeholder: "location UUID",
			HelpText:    "Zero Trust → Gateway → DNS Locations → pick your location → copy the ID from the URL."},
	}
}

func (e *CFGatewayExecutor) ValidateConfig(raw json.RawMessage) error {
	var cfg cfGatewayConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.APIToken == "" {
		return fmt.Errorf("api_token is required")
	}
	if cfg.AccountID == "" {
		return fmt.Errorf("account_id is required")
	}
	if cfg.LocationID == "" {
		return fmt.Errorf("location_id is required")
	}
	return nil
}

func (e *CFGatewayExecutor) Execute(ctx context.Context, event IPEvent, raw json.RawMessage) error {
	var cfg cfGatewayConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	url := fmt.Sprintf("%s/accounts/%s/gateway/locations/%s", cfAPIBase, cfg.AccountID, cfg.LocationID)

	// GET to validate location exists
	getResp, err := e.cfGWRequest(ctx, http.MethodGet, url, cfg.APIToken, nil)
	if err != nil {
		return fmt.Errorf("GET location failed: %w", err)
	}
	if !getResp.Success {
		return fmt.Errorf("GET location failed: %s", cfGWErrMsg(getResp))
	}

	// Extract the location's writable fields from the GET response. PUT is a full
	// update, so fields we do not echo back are reset to their defaults.
	var loc cfGWLocation
	if err := json.Unmarshal(getResp.Result, &loc); err != nil {
		return fmt.Errorf("parsing location response: %w", err)
	}

	// Preserve client_default: omitting it is interpreted as false, which
	// Cloudflare rejects for the account's default location with
	// "Cannot update default location to be non-default".
	body := map[string]interface{}{
		"name":           loc.Name,
		"client_default": loc.ClientDefault,
		"dns64_enabled":  loc.DNS64Enabled,
		"ecs_support":    loc.ECSSupport,
		"networks": []map[string]string{
			{"network": event.NewIP + "/32"},
		},
	}
	if loc.DNSDestinationIPsID != "" {
		body["dns_destination_ips_id"] = loc.DNSDestinationIPsID
	}
	if hasJSONValue(loc.Endpoints) {
		body["endpoints"] = loc.Endpoints
	}
	if hasJSONValue(loc.MaxTTL) {
		body["max_ttl"] = loc.MaxTTL
	}

	putResp, err := e.cfGWRequest(ctx, http.MethodPut, url, cfg.APIToken, body)
	if err != nil {
		return fmt.Errorf("PUT location failed: %w", err)
	}
	if !putResp.Success {
		return fmt.Errorf("PUT location failed: %s", cfGWErrHint(cfGWErrMsg(putResp)))
	}

	return nil
}

type cfGWResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []cfGWErr       `json:"errors"`
}

type cfGWLocation struct {
	Name                string          `json:"name"`
	ClientDefault       bool            `json:"client_default"`
	DNSDestinationIPsID string          `json:"dns_destination_ips_id"`
	DNS64Enabled        bool            `json:"dns64_enabled"`
	ECSSupport          bool            `json:"ecs_support"`
	Endpoints           json.RawMessage `json:"endpoints"`
	MaxTTL              json.RawMessage `json:"max_ttl"`
}

type cfGWErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *CFGatewayExecutor) cfGWRequest(ctx context.Context, method, url, token string, body interface{}) (*cfGWResponse, error) {
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

	var cfResp cfGWResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &cfResp, nil
}

func cfGWErrMsg(resp *cfGWResponse) string {
	if len(resp.Errors) > 0 {
		return resp.Errors[0].Message
	}
	return "unknown error"
}

// cfGWErrHint adds an actionable hint for errors that have a known remedy.
func cfGWErrHint(msg string) string {
	if strings.Contains(msg, "default location to be non-default") {
		return msg + " — the configured location is the account's default location. " +
			"wan-ip-check preserves client_default on update; if this still fails, " +
			"create a dedicated non-default location for this WAN."
	}
	return msg
}

// hasJSONValue reports whether a raw JSON field carries a usable value.
func hasJSONValue(raw json.RawMessage) bool {
	val := strings.TrimSpace(string(raw))
	return val != "" && val != "null"
}
