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

// CFResponse represents a Cloudflare API response envelope.
type CFResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []CFError       `json:"errors"`
}

// CFError represents an error from the Cloudflare API.
type CFError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cfDNSRecord holds the fields we read from a DNS record GET.
type cfDNSRecord struct {
	Name string `json:"name"`
}

// cfGatewayLocation holds the fields we read from a Gateway location GET.
type cfGatewayLocation struct {
	Name string `json:"name"`
}

// cfRequest performs an authenticated Cloudflare API request.
func (a *App) cfRequest(ctx context.Context, method, url string, body interface{}) (*CFResponse, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.CFAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.directClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	var cfResp CFResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &cfResp, nil
}

// updateCloudflareDNS patches all configured DNS A records with the new IP.
// Returns a summary string like "2/2 records" for the Discord embed.
func (a *App) updateCloudflareDNS(ctx context.Context, newIP string) string {
	total := len(a.cfg.CFDNSRecordIDs)
	success := 0

	for _, recordID := range a.cfg.CFDNSRecordIDs {
		recordName := a.patchDNSRecord(ctx, recordID, newIP)
		if recordName != "" {
			success++
			a.logger.Info("DNS record updated", "record", recordName, "ip", newIP)
		}
	}

	return fmt.Sprintf("%d/%d records", success, total)
}

// patchDNSRecord GETs a record for its name, then PATCHes its content.
// Returns the record name on success, empty string on failure.
func (a *App) patchDNSRecord(ctx context.Context, recordID, newIP string) string {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, a.cfg.CFZoneID, recordID)

	// GET to retrieve record name for logging
	getResp, err := a.cfRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.logger.Error("failed to fetch DNS record", "record_id", recordID, "error", err)
		return ""
	}
	if !getResp.Success {
		a.logger.Error("Cloudflare GET failed for DNS record", "record_id", recordID, "error", cfErrorMsg(getResp))
		return ""
	}

	var record cfDNSRecord
	if err := json.Unmarshal(getResp.Result, &record); err != nil {
		a.logger.Warn("failed to parse DNS record response", "record_id", recordID, "error", err)
	}

	// PATCH with new IP only
	patchBody := map[string]string{"content": newIP}
	patchResp, err := a.cfRequest(ctx, http.MethodPatch, url, patchBody)
	if err != nil {
		a.logger.Error("failed to patch DNS record", "record", record.Name, "error", err)
		return ""
	}
	if !patchResp.Success {
		a.logger.Error("Cloudflare PATCH failed for DNS record", "record", record.Name, "error", cfErrorMsg(patchResp))
		return ""
	}

	return record.Name
}

// updateCloudflareGateway patches the Gateway DNS location with the new source IP.
// Returns "Success" or "FAILED" for the Discord embed.
func (a *App) updateCloudflareGateway(ctx context.Context, newIP string) string {
	url := fmt.Sprintf("%s/accounts/%s/gateway/locations/%s", cfAPIBase, a.cfg.CFAccountID, a.cfg.CFLocationID)

	// GET to retrieve location name for logging
	getResp, err := a.cfRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.logger.Error("failed to fetch Gateway location", "location_id", a.cfg.CFLocationID, "error", err)
		return "FAILED"
	}
	if !getResp.Success {
		a.logger.Error("Cloudflare GET failed for Gateway location", "location_id", a.cfg.CFLocationID, "error", cfErrorMsg(getResp))
		return "FAILED"
	}

	var location cfGatewayLocation
	if err := json.Unmarshal(getResp.Result, &location); err != nil {
		a.logger.Warn("failed to parse Gateway location response", "location_id", a.cfg.CFLocationID, "error", err)
	}

	// PATCH with updated networks
	patchBody := map[string]interface{}{
		"networks": []map[string]string{
			{"network": newIP + "/32"},
		},
	}
	patchResp, err := a.cfRequest(ctx, http.MethodPatch, url, patchBody)
	if err != nil {
		a.logger.Error("failed to patch Gateway location", "location", location.Name, "error", err)
		return "FAILED"
	}
	if !patchResp.Success {
		a.logger.Error("Cloudflare PATCH failed for Gateway location", "location", location.Name, "error", cfErrorMsg(patchResp))
		return "FAILED"
	}

	a.logger.Info("Gateway location updated", "location", location.Name, "ip", newIP+"/32")
	return "Success"
}

// cfErrorMsg extracts the first error message from a Cloudflare response.
func cfErrorMsg(resp *CFResponse) string {
	if len(resp.Errors) > 0 {
		return resp.Errors[0].Message
	}
	return "unknown error"
}
