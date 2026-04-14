package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (e *CFGatewayExecutor) SecretFields() []string { return []string{"api_token"} }

func (e *CFGatewayExecutor) DefaultConfig() json.RawMessage {
	data, _ := json.Marshal(cfGatewayConfig{})
	return data
}

func (e *CFGatewayExecutor) ConfigSchema() []ConfigField {
	return []ConfigField{
		{Name: "api_token", Label: "API Token", Type: "password", Required: true, IsSecret: true, HelpText: "Cloudflare API token with Zero Trust edit permission"},
		{Name: "account_id", Label: "Account ID", Type: "text", Required: true, HelpText: "Found in Cloudflare dashboard > account home"},
		{Name: "location_id", Label: "Location ID", Type: "text", Required: true, HelpText: "Gateway DNS location ID"},
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

	// Extract location name from GET response (required for PUT)
	var loc cfGWLocation
	if err := json.Unmarshal(getResp.Result, &loc); err != nil {
		return fmt.Errorf("parsing location response: %w", err)
	}

	// PUT with name (required) + updated networks
	body := map[string]interface{}{
		"name": loc.Name,
		"networks": []map[string]string{
			{"network": event.NewIP + "/32"},
		},
	}
	putResp, err := e.cfGWRequest(ctx, http.MethodPut, url, cfg.APIToken, body)
	if err != nil {
		return fmt.Errorf("PUT location failed: %w", err)
	}
	if !putResp.Success {
		return fmt.Errorf("PUT location failed: %s", cfGWErrMsg(putResp))
	}

	return nil
}

type cfGWResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []cfGWErr       `json:"errors"`
}

type cfGWLocation struct {
	Name string `json:"name"`
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
