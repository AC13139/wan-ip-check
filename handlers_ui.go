package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html templates/partials/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// PageData is the base data passed to every full-page template.
type PageData struct {
	Title     string
	Active    string
	CSRFToken string
	Flash     string
	Error     string
	Content   interface{} // ignored — data is embedded via specific fields below
}

// --- Template Engine ---

// Templates holds parsed template sets (one per page to avoid block collisions).
type Templates struct {
	pages    map[string]*template.Template
	partials map[string]*template.Template
}

func loadTemplates() (*Templates, error) {
	t := &Templates{
		pages:    make(map[string]*template.Template),
		partials: make(map[string]*template.Template),
	}

	// Read layout content once
	layoutBytes, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("reading layout.html: %w", err)
	}
	layoutContent := string(layoutBytes)

	pages := []string{"dashboard", "wan_interfaces", "wan_interface_form", "actions", "action_form", "history", "settings"}
	for _, page := range pages {
		pageBytes, err := fs.ReadFile(templateFS, "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("reading %s.html: %w", page, err)
		}
		// Parse layout first as the root template, then parse the page template into it
		tmpl, err := template.New("layout").Parse(layoutContent)
		if err != nil {
			return nil, fmt.Errorf("parsing layout for %s: %w", page, err)
		}
		if _, err := tmpl.Parse(string(pageBytes)); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", page, err)
		}
		t.pages[page] = tmpl
	}

	// Partials — read and parse individually
	partialFiles := map[string]string{
		"history-rows": "templates/history.html",
		"config-fields": "templates/partials/config_fields.html",
		"wan-interface-card": "templates/partials/wan_interface_card.html",
	}
	for name, path := range partialFiles {
		content, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return nil, fmt.Errorf("reading partial %s: %w", name, err)
		}
		tmpl, err := template.New(name).Parse(string(content))
		if err != nil {
			return nil, fmt.Errorf("parsing partial %s: %w", name, err)
		}
		t.partials[name] = tmpl
	}

	return t, nil
}

func (t *Templates) renderPage(w http.ResponseWriter, page string, data interface{}) {
	tmpl, ok := t.pages[page]
	if !ok {
		http.Error(w, "template not found: "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (t *Templates) renderPartial(w http.ResponseWriter, name string, data interface{}) {
	tmpl, ok := t.partials[name]
	if !ok {
		http.Error(w, "partial not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Try executing a named template first (for {{define "X"}} blocks), fall back to root
	if sub := tmpl.Lookup(name); sub != nil {
		sub.Execute(w, data)
	} else {
		tmpl.Execute(w, data)
	}
}

// --- CSRF ---

func generateCSRFToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Generate token for GET requests (stored in cookie)
		if r.Method == http.MethodGet {
			cookie, err := r.Cookie("csrf_token")
			if err != nil || cookie.Value == "" {
				token := generateCSRFToken()
				http.SetCookie(w, &http.Cookie{
					Name:     "csrf_token",
					Value:    token,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
			}
			next.ServeHTTP(w, r)
			return
		}

		// Validate token for state-changing requests
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
			cookie, err := r.Cookie("csrf_token")
			if err != nil {
				http.Error(w, "CSRF token missing", http.StatusForbidden)
				return
			}
			// Check form field or header
			token := r.FormValue("csrf_token")
			if token == "" {
				token = r.Header.Get("X-CSRF-Token")
			}
			if token != cookie.Value {
				http.Error(w, "CSRF token mismatch", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func getCSRFToken(r *http.Request) string {
	cookie, err := r.Cookie("csrf_token")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// --- WANInterface Display Data ---

type WANInterfaceDisplay struct {
	WANInterface          WANInterface
	IP              string
	StatusClass     string // ok, changed, failed
	StatusText      string
	StatusBadgeClass string
	LastCheck       string
	ActionCount     int
}

func (s *Server) buildWANInterfaceDisplay(src WANInterface) WANInterfaceDisplay {
	sd := WANInterfaceDisplay{WANInterface: src}
	state, err := s.app.db.GetWANInterfaceState(src.ID)
	if err == nil && state != nil {
		sd.IP = state.LastObservedIP
		sd.StatusClass = state.LastStatus
		sd.StatusText = state.LastStatus
		if sd.StatusText == "" {
			sd.StatusText = "pending"
		}
		switch state.LastStatus {
		case "ok":
			sd.StatusBadgeClass = "badge badge-success"
		case "changed":
			sd.StatusBadgeClass = "badge badge-info"
		case "failed":
			sd.StatusBadgeClass = "badge badge-error"
		default:
			sd.StatusBadgeClass = "badge badge-muted"
		}
		if !state.LastCheckAt.IsZero() {
			sd.LastCheck = relativeTime(state.LastCheckAt)
		}
	}
	count, _ := s.app.db.GetWANInterfaceLinkedActionCount(src.ID)
	sd.ActionCount = count
	return sd
}

// --- Page Handlers ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	sources, _ := s.app.db.ListWANInterfaces()
	var displays []WANInterfaceDisplay
	for _, src := range sources {
		displays = append(displays, s.buildWANInterfaceDisplay(src))
	}

	runs, _, _ := s.app.db.GetRecentRunsJoined(HistoryFilter{Limit: 10})

	data := struct {
		PageData
		Sources    []WANInterfaceDisplay
		RecentRuns []ActionRunJoined
	}{
		PageData:   PageData{Title: "Dashboard", Active: "dashboard", CSRFToken: getCSRFToken(r)},
		Sources:    displays,
		RecentRuns: runs,
	}
	s.templates.renderPage(w, "dashboard", data)
}

func (s *Server) handleWANInterfacesPage(w http.ResponseWriter, r *http.Request) {
	sources, _ := s.app.db.ListWANInterfaces()
	var displays []WANInterfaceDisplay
	for _, src := range sources {
		displays = append(displays, s.buildWANInterfaceDisplay(src))
	}

	data := struct {
		PageData
		Sources []WANInterfaceDisplay
	}{
		PageData: PageData{Title: "WAN Interfaces", Active: "wan_interfaces", CSRFToken: getCSRFToken(r)},
		Sources:  displays,
	}
	s.templates.renderPage(w, "wan_interfaces", data)
}

func (s *Server) handleWANInterfaceForm(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	isEdit := idStr != ""

	data := struct {
		PageData
		WANInterface     *WANInterface
		IsEdit    bool
		AllActions  []Action
		LinkedIDs  map[int64]bool
		LinkedTriggers map[int64]string // action ID → trigger_on
	}{
		PageData:  PageData{Active: "wan_interfaces", CSRFToken: getCSRFToken(r)},
		IsEdit:         isEdit,
		LinkedIDs:      make(map[int64]bool),
		LinkedTriggers: make(map[int64]string),
	}

	if isEdit {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		src, err := s.app.db.GetWANInterface(id)
		if err != nil {
			http.Redirect(w, r, "/sources", http.StatusSeeOther)
			return
		}
		data.WANInterface = src
		data.Title = "Edit WANInterface: " + src.Name

		links, _ := s.app.db.GetLinkedActions(id, "")
		for _, l := range links {
			data.LinkedIDs[l.ActionID] = true
			data.LinkedTriggers[l.ActionID] = l.TriggerOn
		}
	} else {
		data.Title = "Add WANInterface"
		defaultCfg := DefaultWANInterfaceConfig()
		data.WANInterface = &WANInterface{Config: defaultCfg, Schedule: "*/5 * * * *", Timezone: "Local"}
	}

	actions, _ := s.app.db.ListActions()
	data.AllActions = actions

	s.templates.renderPage(w, "wan_interface_form", data)
}

func (s *Server) handleActionsPage(w http.ResponseWriter, r *http.Request) {
	actions, _ := s.app.db.ListActions()

	type ActionDisplay struct {
		Action        Action
		LinkedSources []string
	}
	var displays []ActionDisplay
	for _, a := range actions {
		a.ConfigJSON = maskSecrets(a.Type, a.ConfigJSON, s.app.registry)
		sources, _ := s.app.db.GetActionLinkedWANInterfaces(a.ID)
		displays = append(displays, ActionDisplay{Action: a, LinkedSources: sources})
	}

	data := struct {
		PageData
		Actions     []ActionDisplay
		ActionTypes []string
	}{
		PageData:    PageData{Title: "Actions", Active: "actions", CSRFToken: getCSRFToken(r)},
		Actions:     displays,
		ActionTypes: s.app.registry.Types(),
	}
	s.templates.renderPage(w, "actions", data)
}

func (s *Server) handleActionForm(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	isEdit := idStr != ""
	actionType := r.URL.Query().Get("type")

	data := struct {
		PageData
		Action       *Action
		IsEdit       bool
		ActionType   string
		Schema       []ConfigField
		ConfigValues map[string]interface{}
		AllTypes     []string
	}{
		PageData:     PageData{Active: "actions", CSRFToken: getCSRFToken(r)},
		IsEdit:       isEdit,
		AllTypes:     s.app.registry.Types(),
		ConfigValues: make(map[string]interface{}),
	}

	if isEdit {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		action, err := s.app.db.GetAction(id)
		if err != nil {
			http.Redirect(w, r, "/actions", http.StatusSeeOther)
			return
		}
		data.Action = action
		data.ActionType = action.Type
		data.Title = "Edit Action: " + action.Name

		// Mask secrets and parse config for form values
		masked := maskSecrets(action.Type, action.ConfigJSON, s.app.registry)
		json.Unmarshal(masked, &data.ConfigValues)

		if executor, err := s.app.registry.Get(action.Type); err == nil {
			data.Schema = executor.ConfigSchema()
		}
	} else {
		data.Title = "Add Action"
		data.ActionType = actionType
		if actionType != "" {
			if executor, err := s.app.registry.Get(actionType); err == nil {
				data.Schema = executor.ConfigSchema()
				json.Unmarshal(executor.DefaultConfig(), &data.ConfigValues)
			}
		}
	}

	s.templates.renderPage(w, "action_form", data)
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	sources, _ := s.app.db.ListWANInterfaces()
	filter := parseHistoryFilter(r)
	runs, total, _ := s.app.db.GetRecentRunsJoined(filter)

	data := struct {
		PageData
		Sources []WANInterface
		Runs    []ActionRunJoined
		Total   int
		Limit   int
	}{
		PageData: PageData{Title: "Event History", Active: "history", CSRFToken: getCSRFToken(r)},
		Sources:  sources,
		Runs:     runs,
		Total:    total,
		Limit:    filter.Limit,
	}
	s.templates.renderPage(w, "history", data)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	summaryTime, _ := s.app.db.GetSetting("daily_summary_time")
	tz, _ := s.app.db.GetSetting("timezone")
	if summaryTime == "" {
		summaryTime = "2000"
	}
	if tz == "" {
		tz = "Local"
	}

	data := struct {
		PageData
		DailySummaryTime string
		Timezone         string
		KeyPath          string
	}{
		PageData:         PageData{Title: "Settings", Active: "settings", CSRFToken: getCSRFToken(r)},
		DailySummaryTime: summaryTime,
		Timezone:         tz,
		KeyPath:          filepath.Join(s.app.boot.DataDir, "encryption.key"),
	}
	s.templates.renderPage(w, "settings", data)
}

// --- HTMX Partial Handlers ---

func (s *Server) handleUIManualCheck(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	s.dispatcher.checkWANInterface(r.Context(), id)

	src, err := s.app.db.GetWANInterface(id)
	if err != nil {
		http.Error(w, "WAN interface not found", http.StatusNotFound)
		return
	}
	display := s.buildWANInterfaceDisplay(*src)
	s.templates.renderPartial(w, "wan-interface-card", display)
}

func (s *Server) handleUIToggleWANInterface(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	src, err := s.app.db.GetWANInterface(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	src.Enabled = !src.Enabled
	if err := s.app.db.UpdateWANInterface(src); err != nil {
		http.Error(w, "failed to update source", http.StatusInternalServerError)
		return
	}
	s.dispatcher.Reload(r.Context())
	w.Header().Set("HX-Redirect", "/sources")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUIToggleAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	action, err := s.app.db.GetAction(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	action.Enabled = !action.Enabled
	if err := s.app.db.UpdateAction(action); err != nil {
		http.Error(w, "failed to update action", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/actions")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUITestAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	action, err := s.app.db.GetAction(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	executor, err := s.app.registry.Get(action.Type)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">unknown type</span>`)
		return
	}

	// CF actions: validate only. Discord/webhook: execute test.
	if action.Type == "cloudflare_dns" || action.Type == "cloudflare_gateway" {
		if err := executor.ValidateConfig(action.ConfigJSON); err != nil {
			fmt.Fprintf(w, `<span class="badge badge-error">%s</span>`, template.HTMLEscapeString(err.Error()))
		} else {
			fmt.Fprintf(w, `<span class="badge badge-success">config valid</span>`)
		}
		return
	}

	decrypted, _ := DecryptActionSecrets(action.ConfigJSON, executor.SecretFields(), s.app.encKey)
	event := IPEvent{
		WANInterfaceName: "Test",
		EventType:  "ip_changed",
		OldIP:      "203.0.113.0",
		NewIP:      "203.0.113.1",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := executor.Execute(r.Context(), event, decrypted); err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">%s</span>`, template.HTMLEscapeString(err.Error()))
	} else {
		fmt.Fprintf(w, `<span class="badge badge-success">sent</span>`)
	}
}

func (s *Server) handleUIDeleteWANInterface(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.app.db.DeleteWANInterface(id); err != nil {
		http.Error(w, "failed to delete source", http.StatusInternalServerError)
		return
	}
	s.dispatcher.Reload(r.Context())
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUIDeleteAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.app.db.DeleteAction(id); err != nil {
		http.Error(w, "failed to delete action", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUIActionSchema(w http.ResponseWriter, r *http.Request) {
	actionType := r.URL.Query().Get("type")
	if actionType == "" {
		fmt.Fprint(w, `<p class="text-muted">Select an action type.</p>`)
		return
	}
	executor, err := s.app.registry.Get(actionType)
	if err != nil {
		fmt.Fprintf(w, `<p class="text-error">Unknown type: %s</p>`, template.HTMLEscapeString(actionType))
		return
	}
	s.templates.renderPartial(w, "config-fields", executor.ConfigSchema())
}

func (s *Server) handleUIHistoryTable(w http.ResponseWriter, r *http.Request) {
	filter := parseHistoryFilter(r)
	runs, _, _ := s.app.db.GetRecentRunsJoined(filter)

	data := struct {
		Runs []ActionRunJoined
	}{Runs: runs}
	s.templates.renderPartial(w, "history-rows", data)
}

func (s *Server) handleUICreateWANInterface(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	src := &WANInterface{
		Name:     r.FormValue("name"),
		Enabled:  true,
		Schedule: r.FormValue("schedule"),
		Timezone: r.FormValue("timezone"),
		Config: WANInterfaceConfig{
			ProbeURLs:     parseLines(r.FormValue("probe_urls")),
			Strategy:      r.FormValue("strategy"),
			BindInterface: r.FormValue("bind_interface"),
			BindAddress:   r.FormValue("bind_address"),
			TimeoutSecs:   parseInt(r.FormValue("timeout"), 10),
		},
	}
	if src.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if src.Schedule == "" {
		src.Schedule = "*/5 * * * *"
	}
	if err := validateCron(src.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if _, err := s.app.db.CreateWANInterface(src); err != nil {
		http.Error(w, "failed to create source", http.StatusInternalServerError)
		return
	}
	s.dispatcher.Reload(r.Context())
	w.Header().Set("HX-Redirect", "/sources")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUIUpdateWANInterface(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	r.ParseForm()

	existing, err := s.app.db.GetWANInterface(id)
	if err != nil {
		http.Error(w, "WAN interface not found", http.StatusNotFound)
		return
	}

	existing.Name = r.FormValue("name")
	existing.Enabled = r.FormValue("enabled") == "true"
	existing.Schedule = r.FormValue("schedule")
	existing.Timezone = r.FormValue("timezone")
	existing.Config = WANInterfaceConfig{
		ProbeURLs:     parseLines(r.FormValue("probe_urls")),
		Strategy:      r.FormValue("strategy"),
		BindInterface: r.FormValue("bind_interface"),
		BindAddress:   r.FormValue("bind_address"),
		TimeoutSecs:   parseInt(r.FormValue("timeout"), 10),
	}

	if err := validateCron(existing.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.app.db.UpdateWANInterface(existing); err != nil {
		http.Error(w, "failed to update source", http.StatusInternalServerError)
		return
	}

	// Update action links
	existingLinks, _ := s.app.db.GetLinkedActions(id, "")
	for _, l := range existingLinks {
		s.app.db.UnlinkAction(id, l.ActionID)
	}
	for _, aidStr := range r.Form["action_ids"] {
		aid, _ := strconv.ParseInt(aidStr, 10, 64)
		triggerOn := r.FormValue(fmt.Sprintf("trigger_%d", aid))
		if triggerOn == "" {
			triggerOn = "ip_changed"
		}
		s.app.db.LinkAction(&WANInterfaceAction{
			WANInterfaceID:  id,
			ActionID:  aid,
			TriggerOn: triggerOn,
			Enabled:   true,
		})
	}

	s.dispatcher.Reload(r.Context())
	w.Header().Set("HX-Redirect", "/sources")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUICreateAction(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	actionType := r.FormValue("type")
	name := r.FormValue("name")

	if actionType == "" || name == "" {
		http.Error(w, "type and name required", http.StatusBadRequest)
		return
	}

	executor, err := s.app.registry.Get(actionType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	configJSON := buildConfigFromForm(r, executor.ConfigSchema())

	if err := executor.ValidateConfig(configJSON); err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}

	encrypted, err := EncryptActionSecrets(configJSON, executor.SecretFields(), s.app.encKey)
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	action := &Action{Type: actionType, Name: name, Enabled: true, ConfigJSON: encrypted}
	if _, err := s.app.db.CreateAction(action); err != nil {
		http.Error(w, "failed to create action", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/actions")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUIUpdateAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	r.ParseForm()

	existing, err := s.app.db.GetAction(id)
	if err != nil {
		http.Error(w, "action not found", http.StatusNotFound)
		return
	}

	existing.Name = r.FormValue("name")
	existing.Enabled = r.FormValue("enabled") == "true"

	executor, err := s.app.registry.Get(existing.Type)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	configJSON := buildConfigFromForm(r, executor.ConfigSchema())
	// Merge masked secrets with existing encrypted values
	merged := mergeSecretFields(configJSON, existing.ConfigJSON, executor.SecretFields())

	if err := executor.ValidateConfig(merged); err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}

	encrypted, err := EncryptActionSecrets(merged, executor.SecretFields(), s.app.encKey)
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	existing.ConfigJSON = encrypted
	if err := s.app.db.UpdateAction(existing); err != nil {
		http.Error(w, "failed to update action", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/actions")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUIUpdateSettings(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	summaryTime := r.FormValue("daily_summary_time")
	tz := r.FormValue("timezone")

	if summaryTime != "" {
		if !dailySummaryTimeRe.MatchString(summaryTime) || len(summaryTime) != 4 || summaryTime[:2] > "23" || summaryTime[2:] > "59" {
			http.Error(w, "invalid HHMM format", http.StatusBadRequest)
			return
		}
		if err := s.app.db.SetSetting("daily_summary_time", summaryTime); err != nil {
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if tz != "" {
		if err := validateTimezone(tz); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.app.db.SetSetting("timezone", tz); err != nil {
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}

	// HX-Redirect to reload the full settings page cleanly
	w.Header().Set("HX-Redirect", "/settings")
	w.WriteHeader(http.StatusOK)
}

// --- Helpers ---

func buildConfigFromForm(r *http.Request, schema []ConfigField) json.RawMessage {
	config := make(map[string]interface{})
	for _, field := range schema {
		val := r.FormValue("config_" + field.Name)
		if field.Type == "tags" && val != "" {
			// Split comma-separated into array
			var tags []string
			for _, t := range strings.Split(val, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tags = append(tags, t)
				}
			}
			config[field.Name] = tags
		} else {
			config[field.Name] = val
		}
	}
	data, _ := json.Marshal(config)
	return data
}

func parseHistoryFilter(r *http.Request) HistoryFilter {
	f := HistoryFilter{Limit: 50}
	if v := r.URL.Query().Get("waninterface"); v != "" {
		f.WANInterfaceID, _ = strconv.ParseInt(v, 10, 64)
	}
	f.EventType = r.URL.Query().Get("event")
	f.Status = r.URL.Query().Get("status")
	if v := r.URL.Query().Get("offset"); v != "" {
		f.Offset, _ = strconv.Atoi(v)
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
		if f.Limit <= 0 || f.Limit > 500 {
			f.Limit = 50
		}
	}
	return f
}

func parseLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseInt(s string, defaultVal int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return n
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}

// --- Static File Server ---

func staticFileServer() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	return http.FileServer(http.FS(sub))
}
