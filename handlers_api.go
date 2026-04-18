package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// --- Status ---

func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	wanInterfaces, err := s.app.db.ListWANInterfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list WAN interfaces")
		return
	}

	type wanInterfaceStatus struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Enabled   bool   `json:"enabled"`
		IP        string `json:"ip"`
		Status    string `json:"status"`
		LastCheck string `json:"last_check"`
	}

	var statuses []wanInterfaceStatus
	for _, wi := range wanInterfaces {
		state, err := s.app.db.GetWANInterfaceState(wi.ID)
		ws := wanInterfaceStatus{ID: wi.ID, Name: wi.Name, Enabled: wi.Enabled}
		if err == nil {
			ws.IP = state.LastObservedIP
			ws.Status = state.LastStatus
			if !state.LastCheckAt.IsZero() {
				ws.LastCheck = state.LastCheckAt.Format(time.RFC3339)
			}
		}
		statuses = append(statuses, ws)
	}

	actions, _ := s.app.db.ListActions()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"app":            "wan-ip-check",
		"version":        "2.0.0-alpha",
		"wan_interfaces": statuses,
		"action_count":   len(actions),
	})
}

// --- WAN Interfaces ---

func (s *Server) handleListWANInterfaces(w http.ResponseWriter, r *http.Request) {
	wanInterfaces, err := s.app.db.ListWANInterfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list WAN interfaces")
		return
	}

	type wanInterfaceWithState struct {
		WANInterface
		State *WANInterfaceState `json:"state,omitempty"`
	}

	var result []wanInterfaceWithState
	for _, wi := range wanInterfaces {
		wi.Config.MigrateEgress()
		wiws := wanInterfaceWithState{WANInterface: wi}
		if state, err := s.app.db.GetWANInterfaceState(wi.ID); err == nil {
			wiws.State = state
		}
		result = append(result, wiws)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCreateWANInterface(w http.ResponseWriter, r *http.Request) {
	var wi WANInterface
	if err := decodeJSON(r, &wi); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if wi.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if wi.Schedule == "" {
		wi.Schedule = "*/5 * * * *"
	} else if err := validateCron(wi.Schedule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if wi.Timezone == "" {
		wi.Timezone = "Local"
	} else if err := validateTimezone(wi.Timezone); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(wi.Config.ProbeURLs) == 0 {
		wi.Config = DefaultWANInterfaceConfig()
	}
	wi.Enabled = true

	id, err := s.app.db.CreateWANInterface(&wi)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create WAN interface")
		return
	}
	wi.ID = id
	s.dispatcher.Reload(r.Context())
	writeJSON(w, http.StatusCreated, wi)
}

func (s *Server) handleGetWANInterface(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}

	wi, err := s.app.db.GetWANInterface(id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "WAN interface not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get WAN interface")
		return
	}

	wi.Config.MigrateEgress()
	state, _ := s.app.db.GetWANInterfaceState(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"wan_interface": wi,
		"state":         state,
	})
}

func (s *Server) handleUpdateWANInterface(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}

	existing, err := s.app.db.GetWANInterface(id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "WAN interface not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get WAN interface")
		return
	}

	var update WANInterface
	if err := decodeJSON(r, &update); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Merge provided fields (PUT semantics: enabled must be explicit)
	if update.Name != "" {
		existing.Name = update.Name
	}
	if update.Schedule != "" {
		if err := validateCron(update.Schedule); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		existing.Schedule = update.Schedule
	}
	if update.Timezone != "" {
		if err := validateTimezone(update.Timezone); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		existing.Timezone = update.Timezone
	}
	// Update config if any fields are provided (not just probe_urls)
	if len(update.Config.ProbeURLs) > 0 || update.Config.Egress.Mode != "" || update.Config.Strategy != "" || update.Config.TimeoutSecs > 0 {
		// Merge: keep existing probe_urls if not provided
		if len(update.Config.ProbeURLs) == 0 {
			update.Config.ProbeURLs = existing.Config.ProbeURLs
		}
		existing.Config = update.Config
	}
	existing.Enabled = update.Enabled

	if err := s.app.db.UpdateWANInterface(existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update WAN interface")
		return
	}
	s.dispatcher.Reload(r.Context())
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteWANInterface(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}
	if err := s.app.db.DeleteWANInterface(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete WAN interface")
		return
	}
	s.dispatcher.Reload(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleManualCheck(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}

	// Verify WAN interface exists
	if _, err := s.app.db.GetWANInterface(id); err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "WAN interface not found")
		return
	}

	// Run check synchronously
	s.dispatcher.checkWANInterface(r.Context(), id, "manual")

	// Return updated state
	state, _ := s.app.db.GetWANInterfaceState(id)
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleWANInterfaceHistory(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}
	limit := queryInt(r, "limit", 50)
	runs, err := s.app.db.GetRecentRuns(id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get history")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// --- Actions ---

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	actions, err := s.app.db.ListActions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list actions")
		return
	}
	// Mask secrets before returning
	for i := range actions {
		actions[i].ConfigJSON = maskSecrets(actions[i].Type, actions[i].ConfigJSON, s.app.registry)
	}
	writeJSON(w, http.StatusOK, actions)
}

func (s *Server) handleCreateAction(w http.ResponseWriter, r *http.Request) {
	var action Action
	if err := decodeJSON(r, &action); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if action.Type == "" || action.Name == "" {
		writeError(w, http.StatusBadRequest, "type and name are required")
		return
	}

	// Validate config against executor
	executor, err := s.app.registry.Get(action.Type)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := executor.ValidateConfig(action.ConfigJSON); err != nil {
		writeError(w, http.StatusBadRequest, "invalid config: "+err.Error())
		return
	}

	// Encrypt secrets before storing
	action.ConfigJSON, err = EncryptActionSecrets(action.ConfigJSON, executor.SecretFields(), s.app.encKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt secrets")
		return
	}
	action.Enabled = true

	id, err := s.app.db.CreateAction(&action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create action")
		return
	}
	action.ID = id
	action.ConfigJSON = maskSecrets(action.Type, action.ConfigJSON, s.app.registry)
	writeJSON(w, http.StatusCreated, action)
}

func (s *Server) handleGetAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid action ID")
		return
	}
	action, err := s.app.db.GetAction(id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get action")
		return
	}
	action.ConfigJSON = maskSecrets(action.Type, action.ConfigJSON, s.app.registry)
	writeJSON(w, http.StatusOK, action)
}

func (s *Server) handleUpdateAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid action ID")
		return
	}

	existing, err := s.app.db.GetAction(id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get action")
		return
	}

	var update Action
	if err := decodeJSON(r, &update); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if update.Name != "" {
		existing.Name = update.Name
	}
	existing.Enabled = update.Enabled

	// If config is provided, merge masked fields with existing encrypted values, then validate + encrypt
	if len(update.ConfigJSON) > 0 {
		executor, err := s.app.registry.Get(existing.Type)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Merge: if a secret field is still masked, keep the existing encrypted value
		merged := mergeSecretFields(update.ConfigJSON, existing.ConfigJSON, executor.SecretFields())

		if err := executor.ValidateConfig(merged); err != nil {
			writeError(w, http.StatusBadRequest, "invalid config: "+err.Error())
			return
		}
		existing.ConfigJSON, err = EncryptActionSecrets(merged, executor.SecretFields(), s.app.encKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encrypt secrets")
			return
		}
	}

	if err := s.app.db.UpdateAction(existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update action")
		return
	}
	existing.ConfigJSON = maskSecrets(existing.Type, existing.ConfigJSON, s.app.registry)
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid action ID")
		return
	}
	if err := s.app.db.DeleteAction(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete action")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid action ID")
		return
	}

	action, err := s.app.db.GetAction(id)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get action")
		return
	}

	executor, err := s.app.registry.Get(action.Type)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Decrypt config for execution
	decryptedConfig, err := DecryptActionSecrets(action.ConfigJSON, executor.SecretFields(), s.app.encKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to decrypt config")
		return
	}

	// Build test event using a safe IP (current detected IP or placeholder for Discord/webhook).
	// For CF DNS/Gateway: use validate-only (GET) not mutate (PUT/PATCH).
	// For Discord/Webhook: send a test notification.
	testIP := "203.0.113.1" // RFC 5737 TEST-NET-3, safe for test messages
	event := IPEvent{
		WANInterfaceName: "Test",
		EventType:        "ip_changed",
		OldIP:            "203.0.113.0",
		NewIP:            testIP,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// For Cloudflare actions: only validate config (GET), don't mutate
	if action.Type == "cloudflare_dns" || action.Type == "cloudflare_gateway" {
		if err := executor.ValidateConfig(decryptedConfig); err != nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"success": false,
				"error":   "config validation failed: " + err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"note":    "config validated (Cloudflare actions are not executed in test mode to avoid mutating production state)",
		})
		return
	}

	// For Discord/Webhook: actually send (they're notification-only, no mutation risk)
	if err := executor.Execute(ctx, event, decryptedConfig); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}

func (s *Server) handleListActionTypes(w http.ResponseWriter, r *http.Request) {
	type actionTypeInfo struct {
		Type          string          `json:"type"`
		DefaultConfig json.RawMessage `json:"default_config"`
		Schema        []ConfigField   `json:"schema"`
	}

	var types []actionTypeInfo
	for _, t := range s.app.registry.Types() {
		executor, _ := s.app.registry.Get(t)
		types = append(types, actionTypeInfo{
			Type:          t,
			DefaultConfig: executor.DefaultConfig(),
			Schema:        executor.ConfigSchema(),
		})
	}
	writeJSON(w, http.StatusOK, types)
}

// --- WAN Interface-Action linking ---

func (s *Server) handleGetWANInterfaceActions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}
	links, err := s.app.db.GetLinkedActions(id, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get linked actions")
		return
	}
	// Mask secrets in linked action configs
	for i := range links {
		links[i].ActionConfigJSON = maskSecrets(links[i].ActionType, links[i].ActionConfigJSON, s.app.registry)
	}
	writeJSON(w, http.StatusOK, links)
}

func (s *Server) handleSetWANInterfaceActions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid WAN interface ID")
		return
	}

	var links []WANInterfaceAction
	if err := decodeJSON(r, &links); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Replace all links for this WAN interface
	// First remove existing
	existingLinks, _ := s.app.db.GetLinkedActions(id, "")
	for _, l := range existingLinks {
		s.app.db.UnlinkAction(id, l.ActionID)
	}

	// Add new links
	for i := range links {
		links[i].WANInterfaceID = id
		if links[i].TriggerOn == "" {
			links[i].TriggerOn = "ip_changed"
		}
		links[i].Enabled = true
		links[i].RunOrder = i
		if err := s.app.db.LinkAction(&links[i]); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to link action")
			return
		}
	}

	// Return updated links with masked secrets
	updated, _ := s.app.db.GetLinkedActions(id, "")
	for i := range updated {
		updated[i].ActionConfigJSON = maskSecrets(updated[i].ActionType, updated[i].ActionConfigJSON, s.app.registry)
	}
	writeJSON(w, http.StatusOK, updated)
}

// --- Global ---

func (s *Server) handleGlobalHistory(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	runs, err := s.app.db.GetRecentRuns(0, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get history")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	keys := []string{"daily_summary_time", "timezone"}
	settings := make(map[string]string)
	for _, k := range keys {
		v, _ := s.app.db.GetSetting(k)
		settings[k] = v
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var settings map[string]string
	if err := decodeJSON(r, &settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	allowed := map[string]bool{"daily_summary_time": true, "timezone": true}

	// Validate ALL settings before writing ANY (atomic semantics)
	for k, v := range settings {
		if !allowed[k] {
			writeError(w, http.StatusBadRequest, "unknown setting: "+k)
			return
		}
		if k == "daily_summary_time" {
			if !dailySummaryTimeRe.MatchString(v) || len(v) != 4 || v[:2] > "23" || v[2:] > "59" {
				writeError(w, http.StatusBadRequest, "daily_summary_time must be valid HHMM (0000-2359)")
				return
			}
		}
		if k == "timezone" {
			if err := validateTimezone(v); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	// All valid — write
	for k, v := range settings {
		if err := s.app.db.SetSetting(k, v); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save setting: "+k)
			return
		}
	}

	writeJSON(w, http.StatusOK, settings)
}

// --- Network ---

type NetworkInterface struct {
	Name      string   `json:"name"`
	Up        bool     `json:"up"`
	Addresses []string `json:"addresses"`
}

func (s *Server) handleNetworkInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces, err := listNetworkInterfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interfaces")
		return
	}
	writeJSON(w, http.StatusOK, ifaces)
}

func (s *Server) handleNetworkCapabilities(w http.ResponseWriter, r *http.Request) {
	ifaces, _ := listNetworkInterfaces()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"net_admin":  checkNetAdmin(),
		"interfaces": ifaces,
	})
}

func (s *Server) handleNetworkDiagnostics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectDiagnostics(r.Context()))
}

func listNetworkInterfaces() ([]NetworkInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []NetworkInterface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		var ipv4s []string
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
				ipv4s = append(ipv4s, ipNet.IP.String())
			}
		}
		if len(ipv4s) == 0 {
			continue
		}
		result = append(result, NetworkInterface{
			Name:      iface.Name,
			Up:        true,
			Addresses: ipv4s,
		})
	}
	return result, nil
}

// netAdminCapability is checked once at startup and cached.
var (
	netAdminOnce   sync.Once
	netAdminCached bool
)

func checkNetAdmin() bool {
	netAdminOnce.Do(func() {
		addErr := exec.Command("ip", "rule", "add", "from", "127.255.255.254", "lookup", "199").Run()
		if addErr != nil {
			netAdminCached = false
			return
		}
		exec.Command("ip", "rule", "del", "from", "127.255.255.254", "lookup", "199").Run()
		netAdminCached = true
	})
	return netAdminCached
}

// --- Helpers ---

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func queryInt(r *http.Request, key string, defaultVal int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 1 {
				return 1
			}
			if n > 500 {
				return 500
			}
			return n
		}
	}
	return defaultVal
}

func validateCron(expr string) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(expr); err != nil {
		return fmt.Errorf("invalid cron schedule %q: %w", expr, err)
	}
	return nil
}

func validateTimezone(tz string) error {
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	return nil
}

// mergeSecretFields preserves existing encrypted values when the update payload contains the mask placeholder.
func mergeSecretFields(updateJSON, existingJSON json.RawMessage, secretFields []string) json.RawMessage {
	var updateMap, existingMap map[string]interface{}
	if json.Unmarshal(updateJSON, &updateMap) != nil || json.Unmarshal(existingJSON, &existingMap) != nil {
		return updateJSON
	}
	for _, field := range secretFields {
		if val, ok := updateMap[field]; ok {
			if s, ok := val.(string); ok && s == secretMask {
				// Keep existing encrypted value
				if existing, ok := existingMap[field]; ok {
					updateMap[field] = existing
				}
			}
		}
	}
	merged, _ := json.Marshal(updateMap)
	return merged
}

// maskSecrets replaces encrypted secret field values with "•••••" for API responses.
func maskSecrets(actionType string, configJSON json.RawMessage, registry *ActionRegistry) json.RawMessage {
	executor, err := registry.Get(actionType)
	if err != nil {
		return configJSON
	}
	secretFields := executor.SecretFields()
	if len(secretFields) == 0 {
		return configJSON
	}

	var config map[string]interface{}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return configJSON
	}

	for _, field := range secretFields {
		if _, ok := config[field]; ok {
			config[field] = secretMask
		}
	}

	masked, _ := json.Marshal(config)
	return masked
}
