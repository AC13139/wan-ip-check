package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// checkIP is the main orchestrator called on each cron tick.
// Overlap prevention is handled by cron.SkipIfStillRunning.
func (a *App) checkIP(ctx context.Context) {
	// Step 1: Fetch current public IP
	newIP, err := a.fetchPublicIP(ctx)
	if err != nil {
		a.logger.Error("failed to fetch public IP", "error", err)
		return
	}
	a.logger.Debug("fetched public IP", "ip", newIP)

	// Step 2: Read last known IP
	lastIP := readFile(a.cfg.IPFile)

	// Step 3: Compare and act
	if newIP != lastIP {
		a.logger.Info("IP change detected", "old", lastIP, "new", newIP)

		if lastIP == "" {
			// First run: save IP, no alert
			if err := writeFileAtomic(a.cfg.IPFile, newIP); err != nil {
				a.logger.Error("failed to save IP", "error", err)
			}
			a.logger.Info("first run, IP saved, no alert sent")
		} else {
			// Actual change: update Cloudflare → notify → save
			dnsResult := "N/A"
			gwResult := "N/A"
			allOK := true

			if a.cfg.CFDNSEnabled() {
				dnsResult = a.updateCloudflareDNS(ctx, newIP)
				if !strings.HasPrefix(dnsResult, fmt.Sprintf("%d/%d", len(a.cfg.CFDNSRecordIDs), len(a.cfg.CFDNSRecordIDs))) {
					allOK = false
				}
			}
			if a.cfg.CFGatewayEnabled() {
				gwResult = a.updateCloudflareGateway(ctx, newIP)
				if gwResult != "Success" {
					allOK = false
				}
			}

			if err := a.sendDiscordAlert(ctx, lastIP, newIP, dnsResult, gwResult); err != nil {
				a.logger.Error("failed to send Discord alert", "error", err)
				allOK = false
			}

			// Only persist new IP if all updates succeeded.
			// If any failed, keep old IP so the next run retries.
			if allOK {
				if err := writeFileAtomic(a.cfg.IPFile, newIP); err != nil {
					a.logger.Error("failed to save IP", "error", err)
				} else {
					a.logger.Info("IP file updated", "ip", newIP)
				}
			} else {
				a.logger.Warn("IP not saved due to update failures — will retry next run", "ip", newIP)
			}
		}

		// Suppress same-day "no change" summary
		today := a.nowInTimezone().Format("2006-01-02")
		if err := writeFileAtomic(a.cfg.SummaryTrackFile, today); err != nil {
			a.logger.Error("failed to write summary tracker", "error", err)
		}
	} else {
		a.logger.Debug("IP unchanged", "ip", newIP)
		a.checkDailySummary(ctx, newIP)
	}
}

// checkDailySummary sends a daily "no change" notification if conditions are met.
func (a *App) checkDailySummary(ctx context.Context, currentIP string) {
	now := a.nowInTimezone()
	currentHHMM := now.Format("1504")

	if currentHHMM < a.cfg.DailySummaryTime {
		return
	}

	today := now.Format("2006-01-02")
	lastSentDate := readFile(a.cfg.SummaryTrackFile)

	if today == lastSentDate {
		return
	}

	a.logger.Info("sending daily summary", "time", currentHHMM, "threshold", a.cfg.DailySummaryTime)
	if err := a.sendNoChangeAlert(ctx, currentIP); err != nil {
		a.logger.Error("failed to send daily summary", "error", err)
		return
	}

	if err := writeFileAtomic(a.cfg.SummaryTrackFile, today); err != nil {
		a.logger.Error("failed to write summary tracker", "error", err)
	}
}

// fetchPublicIP retrieves the current public IP from the configured service.
func (a *App) fetchPublicIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.IPCheckService, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	resp, err := a.directClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching IP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IP service returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	ip := strings.TrimSpace(string(body))

	// Validate IPv4
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("invalid IPv4 address: %q", ip)
	}

	return ip, nil
}

// nowInTimezone returns current time in the configured timezone.
func (a *App) nowInTimezone() time.Time {
	loc, err := time.LoadLocation(a.cfg.Timezone)
	if err != nil {
		a.logger.Warn("invalid timezone, using local", "timezone", a.cfg.Timezone, "error", err)
		return time.Now()
	}
	return time.Now().In(loc)
}

// readFile reads a file and returns its trimmed content, or empty string on error.
func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeFileAtomic writes content to a file via a temp file + rename for safety.
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(content + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}
