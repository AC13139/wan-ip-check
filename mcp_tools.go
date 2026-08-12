package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// All tool handlers below use the existing DB + registry, and run
// maskSecrets on every action-config response. If you add a tool that
// surfaces action config, run it through maskSecrets before returning
// (#36 acceptance criterion).

// handleListWANInterfaces — list_wan_interfaces
func (m *MCPServer) handleListWANInterfaces(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	interfaces, err := m.app.db.ListWANInterfaces()
	if err != nil {
		return mcpError("list failed", err)
	}

	type row struct {
		ID              int64  `json:"id"`
		Name            string `json:"name"`
		Enabled         bool   `json:"enabled"`
		Schedule        string `json:"schedule"`
		Timezone        string `json:"timezone"`
		LastObservedIP  string `json:"last_observed_ip,omitempty"`
		LastStatus      string `json:"last_status,omitempty"`
		LastError       string `json:"last_error,omitempty"`
		LastCheckAt     string `json:"last_check_at,omitempty"`
		LastSuccessAt   string `json:"last_success_at,omitempty"`
		ConsecutiveFails int   `json:"consecutive_failures"`
	}
	out := make([]row, 0, len(interfaces))
	for _, wi := range interfaces {
		r := row{
			ID:       wi.ID,
			Name:     wi.Name,
			Enabled:  wi.Enabled,
			Schedule: wi.Schedule,
			Timezone: wi.Timezone,
		}
		state, err := m.app.db.GetWANInterfaceState(wi.ID)
		if err == nil && state != nil {
			r.LastObservedIP = state.LastObservedIP
			r.LastStatus = state.LastStatus
			r.LastError = state.LastError
			r.ConsecutiveFails = state.ConsecutiveFailures
			if !state.LastCheckAt.IsZero() {
				r.LastCheckAt = state.LastCheckAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			if !state.LastSuccessAt.IsZero() {
				r.LastSuccessAt = state.LastSuccessAt.UTC().Format("2006-01-02T15:04:05Z")
			}
		}
		out = append(out, r)
	}
	return mcpJSON("wan_interfaces", out)
}

// handleGetWANStatus — get_wan_status
func (m *MCPServer) handleGetWANStatus(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireInt("wan_id")
	if err != nil {
		return mcpError("invalid wan_id", err)
	}
	if id <= 0 {
		// Spec § Security: "Validate all tool inputs".
		return mcpError("wan_id must be a positive integer", nil)
	}
	wid := int64(id)
	wi, err := m.app.db.GetWANInterface(wid)
	if err != nil {
		return mcpError("wan interface not found", err)
	}
	state, err := m.app.db.GetWANInterfaceState(wid)
	if err != nil {
		return mcpError("state load failed", err)
	}

	type resp struct {
		WAN  *WANInterface     `json:"wan"`
		State *WANInterfaceState `json:"state"`
	}
	out := resp{WAN: wi, State: state}
	return mcpJSON("wan_status", out)
}

// handleListActions — list_actions (secrets masked)
func (m *MCPServer) handleListActions(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actions, err := m.app.db.ListActions()
	if err != nil {
		return mcpError("list failed", err)
	}

	type row struct {
		ID        int64           `json:"id"`
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Enabled   bool            `json:"enabled"`
		Config    json.RawMessage `json:"config"` // masked
		UpdatedAt string          `json:"updated_at,omitempty"`
	}
	out := make([]row, 0, len(actions))
	for _, a := range actions {
		masked := maskSecrets(a.Type, a.ConfigJSON, m.app.registry)
		r := row{
			ID:      a.ID,
			Type:    a.Type,
			Name:    a.Name,
			Enabled: a.Enabled,
			Config:  masked,
		}
		if !a.UpdatedAt.IsZero() {
			r.UpdatedAt = a.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, r)
	}
	return mcpJSON("actions", out)
}

// handleGetRecentHistory — get_recent_history
func (m *MCPServer) handleGetRecentHistory(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireInt("wan_id")
	if err != nil {
		return mcpError("invalid wan_id", err)
	}
	if id <= 0 {
		return mcpError("wan_id must be a positive integer", nil)
	}
	limit := req.GetInt("limit", 50)
	if limit < 1 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	entries, _, err := m.app.db.GetMergedHistory(HistoryFilter{
		WANInterfaceID: int64(id),
		Limit:          limit,
		Offset:         0,
	})
	if err != nil {
		return mcpError("history load failed", err)
	}
	return mcpJSON("history", entries)
}

// handleGetFailedActions — get_failed_actions
func (m *MCPServer) handleGetFailedActions(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Scan every WAN's failed + abandoned queue. Cheap — bounded by
	// total failed runs, not WAN count. Phase 2 will add a SQL-level
	// helper if this becomes a hot path.
	wans, err := m.app.db.ListWANInterfaces()
	if err != nil {
		return mcpError("wan list failed", err)
	}

	type failedRow struct {
		WANInterfaceID  int64  `json:"wan_interface_id"`
		WANInterfaceName string `json:"wan_interface_name,omitempty"`
		ActionID        int64  `json:"action_id"`
		EventType       string `json:"event_type"`
		OldIP           string `json:"old_ip,omitempty"`
		NewIP           string `json:"new_ip,omitempty"`
		Status          string `json:"status"`
		RetryCount      int    `json:"retry_count"`
		ErrorMsg        string `json:"error_msg,omitempty"`
		ExecutedAt      string `json:"executed_at"`
	}

	out := []failedRow{}
	for _, wi := range wans {
		rows, err := m.app.db.GetFailedRuns(wi.ID)
		if err != nil {
			continue
		}
		for _, r := range rows {
			// GetFailedRuns returns ActionRunWithType (no Status); fetch the
			// matching ActionRun to fill Status + ExecutedAt.
			recent, _ := m.app.db.GetRecentRuns(wi.ID, 500)
			var status, ts string
			for _, x := range recent {
				if x.ID == r.ID {
					status = x.Status
					if !x.ExecutedAt.IsZero() {
						ts = x.ExecutedAt.UTC().Format("2006-01-02T15:04:05Z")
					}
					break
				}
			}
			out = append(out, failedRow{
				WANInterfaceID:  r.WANInterfaceID,
				WANInterfaceName: wi.Name,
				ActionID:        r.ActionID,
				EventType:       r.EventType,
				OldIP:           r.OldIP,
				NewIP:           r.NewIP,
				Status:          status,
				RetryCount:      r.RetryCount,
				ErrorMsg:        "", // do not surface here — pulled by get_recent_history if needed
				ExecutedAt:      ts,
			})
		}
	}
	return mcpJSON("failed_actions", out)
}

// handleGetSettings — get_settings
func (m *MCPServer) handleGetSettings(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	r := m.app.db.LoadHistoryRetention()
	out := map[string]any{
		"history_retention": map[string]int{
			"check_ok_seconds":       r.CheckOKSeconds,
			"check_changed_seconds":  r.CheckChangedSeconds,
			"check_failed_seconds":   r.CheckFailedSeconds,
			"action_success_seconds": r.ActionSuccessSeconds,
			"action_failed_seconds":  r.ActionFailedSeconds,
		},
	}
	if v, err := m.app.db.GetSetting("daily_summary_time"); err == nil {
		out["daily_summary_time"] = v
	}
	if v, err := m.app.db.GetSetting("timezone"); err == nil {
		out["timezone"] = v
	}
	if v, err := m.app.db.GetSetting("action.max_retries"); err == nil {
		out["action_max_retries"] = v
	}
	return mcpJSON("settings", out)
}

// mcpJSON wraps a payload as a single MCP result with both
// structuredContent (modern clients) and a text fallback (legacy clients),
// per the 2026-07-28 spec's "Structured Content" section.
func mcpJSON(key string, v any) (*mcp.CallToolResult, error) {
	payload := map[string]any{key: v}
	// Marshal once for the text fallback; structuredContent is sent as the
	// native Go value so clients can introspect types directly.
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", key, err)
	}
	return mcp.NewToolResultStructured(payload, string(data)), nil
}

// mcpError returns a structured error result with isError: true per the
// 2026-07-28 spec's "Tool Execution Errors" section. Tools use this so the
// agent gets a parseable, actionable response — distinct from protocol
// errors (returned as JSON-RPC errors by the transport).
func mcpError(msg string, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("%s: %v", msg, err)), nil
	}
	return mcp.NewToolResultError(msg), nil
}