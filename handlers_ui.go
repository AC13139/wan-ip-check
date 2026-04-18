package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
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

	pages := []string{"wan_interfaces", "wan_interface_form", "actions", "action_form", "history", "settings"}
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
		"history-rows":           "templates/history.html",
		"config-fields":          "templates/partials/config_fields.html",
		"wan-interface-card":     "templates/partials/wan_interface_card.html",
		"egress-fields":          "templates/partials/egress_fields.html",
		"network-diagnostics":    "templates/partials/network_diagnostics.html",
		"test-route-result":      "templates/partials/test_route_result.html",
		"cloudflare-dns-fields":  "templates/partials/cloudflare_dns_fields.html",
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

// --- WAN Interface Display Data ---

type WANInterfaceDisplay struct {
	WANInterface     WANInterface
	IP               string
	StatusClass      string // ok, changed, failed
	StatusText       string
	StatusBadgeClass string
	LastCheck        string
	LastError        string // surfaced on failed cards
	ActionCount      int
}

func (s *Server) buildWANInterfaceDisplay(wi WANInterface) WANInterfaceDisplay {
	wd := WANInterfaceDisplay{WANInterface: wi}
	state, err := s.app.db.GetWANInterfaceState(wi.ID)
	if err == nil && state != nil {
		wd.IP = state.LastObservedIP
		wd.StatusClass = state.LastStatus
		wd.StatusText = state.LastStatus
		if wd.StatusText == "" {
			wd.StatusText = "pending"
		}
		switch state.LastStatus {
		case "ok":
			wd.StatusBadgeClass = "badge badge-success"
		case "changed":
			wd.StatusBadgeClass = "badge badge-info"
		case "failed":
			wd.StatusBadgeClass = "badge badge-error"
			wd.LastError = state.LastError
		default:
			wd.StatusBadgeClass = "badge badge-muted"
		}
		if !state.LastCheckAt.IsZero() {
			wd.LastCheck = relativeTime(state.LastCheckAt)
		}
	}
	count, _ := s.app.db.GetWANInterfaceLinkedActionCount(wi.ID)
	wd.ActionCount = count
	return wd
}

// --- Page Handlers ---

func (s *Server) handleWANInterfacesPage(w http.ResponseWriter, r *http.Request) {
	wanInterfaces, _ := s.app.db.ListWANInterfaces()
	var displays []WANInterfaceDisplay
	for _, wi := range wanInterfaces {
		displays = append(displays, s.buildWANInterfaceDisplay(wi))
	}

	data := struct {
		PageData
		WANInterfaces []WANInterfaceDisplay
	}{
		PageData:      PageData{Title: "WAN Interfaces", Active: "wan-interfaces", CSRFToken: getCSRFToken(r)},
		WANInterfaces: displays,
	}
	s.templates.renderPage(w, "wan_interfaces", data)
}

func (s *Server) handleWANInterfaceForm(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	isEdit := idStr != ""

	data := struct {
		PageData
		WANInterface   *WANInterface
		IsEdit         bool
		AllActions     []Action
		LinkedIDs      map[int64]bool
		LinkedTriggers map[int64]string // action ID -> trigger_on
		EgressFields   template.HTML    // pre-rendered egress fields
		ScheduleValue  int              // for simple-mode dropdown
		ScheduleUnit   string           // minutes/hours/days
		CustomSchedule bool             // true if cron isn't a simple pattern
	}{
		PageData:       PageData{Active: "wan-interfaces", CSRFToken: getCSRFToken(r)},
		IsEdit:         isEdit,
		LinkedIDs:      make(map[int64]bool),
		LinkedTriggers: make(map[int64]string),
	}

	if isEdit {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		wi, err := s.app.db.GetWANInterface(id)
		if err != nil {
			http.Redirect(w, r, "/wan-interfaces", http.StatusSeeOther)
			return
		}
		wi.Config.MigrateEgress()
		data.WANInterface = wi
		data.Title = "Edit WAN Interface: " + wi.Name

		links, _ := s.app.db.GetLinkedActions(id, "")
		for _, l := range links {
			data.LinkedIDs[l.ActionID] = true
			data.LinkedTriggers[l.ActionID] = l.TriggerOn
		}
	} else {
		data.Title = "Add WAN Interface"
		defaultCfg := DefaultWANInterfaceConfig()
		data.WANInterface = &WANInterface{Config: defaultCfg, Schedule: "*/5 * * * *", Timezone: "Local"}
	}

	if v, u, ok := ParseSimpleSchedule(data.WANInterface.Schedule); ok {
		data.ScheduleValue = v
		data.ScheduleUnit = u
	} else {
		data.CustomSchedule = true
	}

	actions, _ := s.app.db.ListActions()
	data.AllActions = actions
	data.EgressFields = s.renderEgressFields(data.WANInterface.Config.Egress)

	s.templates.renderPage(w, "wan_interface_form", data)
}

// renderEgressFields renders all three egress-mode variants (default,
// gateway, proxy) wrapped in visibility-control divs. A tiny inline script
// in the form template shows only the variant matching the current
// dropdown value. This eliminates the HTMX round-trip on dropdown change
// (previously failed behind reverse proxies with 522 timeouts) and makes
// the mode switch instant.
func (s *Server) renderEgressFields(egress EgressConfig) template.HTML {
	ifaces, _ := listNetworkInterfaces()
	selectedMode := egress.Mode
	if selectedMode == "" {
		selectedMode = "default"
	}
	tmpl, ok := s.templates.partials["egress-fields"]
	if !ok {
		return template.HTML("")
	}
	render := func(mode string) string {
		var buf strings.Builder
		_ = tmpl.Lookup("egress-fields").Execute(&buf, struct {
			Mode          string
			Interfaces    []NetworkInterface
			InterfaceName string
			Gateway       string
			ProxyURL      string
		}{
			Mode: mode, Interfaces: ifaces,
			InterfaceName: egress.InterfaceName,
			Gateway:       egress.Gateway,
			ProxyURL:      egress.ProxyURL,
		})
		return buf.String()
	}
	var buf strings.Builder
	for _, m := range []string{"default", "gateway", "proxy"} {
		style := "display:none"
		if m == selectedMode {
			style = ""
		}
		fmt.Fprintf(&buf, `<div data-egress-mode=%q style=%q>%s</div>`, m, style, render(m))
	}
	return template.HTML(buf.String())
}

func (s *Server) handleActionsPage(w http.ResponseWriter, r *http.Request) {
	actions, _ := s.app.db.ListActions()

	type ActionDisplay struct {
		Action              Action
		TypeDisplayName     string
		LinkedWANInterfaces []string
	}
	var displays []ActionDisplay
	for _, a := range actions {
		a.ConfigJSON = maskSecrets(a.Type, a.ConfigJSON, s.app.registry)
		wanInterfaces, _ := s.app.db.GetActionLinkedWANInterfaces(a.ID)
		displays = append(displays, ActionDisplay{
			Action:              a,
			TypeDisplayName:     s.app.registry.DisplayName(a.Type),
			LinkedWANInterfaces: wanInterfaces,
		})
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
		Action          *Action
		IsEdit          bool
		ActionType      string
		Schema          []ConfigField
		ConfigValues    map[string]interface{}
		AllTypes        []ActionTypeInfo
		TypeDisplayName string
		SchemaPanels    template.HTML // pre-rendered panels for all types (new form only)
		CustomFormHTML  template.HTML // pre-rendered custom form (edit path for CF DNS)
		AllWANs         []WANInterface
		TriggersJSON    template.JS
	}{
		PageData:     PageData{Active: "actions", CSRFToken: getCSRFToken(r)},
		IsEdit:       isEdit,
		AllTypes:     s.app.registry.TypeInfos(),
		ConfigValues: make(map[string]interface{}),
		TriggersJSON: template.JS("[]"),
	}

	// Triggers source-of-truth for the action-side form. The WAN form keeps
	// its Linked Actions section as a secondary bulk-link affordance.
	allWANs, _ := s.app.db.ListWANInterfaces()
	data.AllWANs = allWANs

	if isEdit {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		action, err := s.app.db.GetAction(id)
		if err != nil {
			http.Redirect(w, r, "/actions", http.StatusSeeOther)
			return
		}
		data.Action = action
		data.ActionType = action.Type
		data.TypeDisplayName = s.app.registry.DisplayName(action.Type)
		data.Title = "Edit Action: " + action.Name

		masked := maskSecrets(action.Type, action.ConfigJSON, s.app.registry)
		json.Unmarshal(masked, &data.ConfigValues)

		if executor, err := s.app.registry.Get(action.Type); err == nil {
			data.Schema = executor.ConfigSchema()
			if html := s.renderCustomActionForm(executor, data.ConfigValues); html != "" {
				data.CustomFormHTML = template.HTML(html)
			}
		}

		triggers, _ := s.app.db.GetActionTriggers(id)
		triggersJSON, _ := json.Marshal(triggers)
		data.TriggersJSON = template.JS(string(triggersJSON))
	} else {
		data.Title = "Add Action"
		data.ActionType = actionType
		data.SchemaPanels = s.renderAllActionSchemas(actionType)
	}

	s.templates.renderPage(w, "action_form", data)
}

// renderAllActionSchemas pre-renders a panel per registered action type,
// wrapped in <div data-action-type="X" style="display:none"> so the new
// form's type dropdown can toggle them via a local script. Eliminates the
// HTMX round-trip to /ui/action-schema (which 522s behind reverse proxies).
func (s *Server) renderAllActionSchemas(selected string) template.HTML {
	genericTmpl := s.templates.partials["config-fields"]
	var buf strings.Builder
	placeholderStyle := "display:none"
	if selected == "" {
		placeholderStyle = ""
	}
	fmt.Fprintf(&buf, `<div data-action-type="" style=%q><p class="text-muted">Select an action type.</p></div>`,
		placeholderStyle)
	for _, t := range s.app.registry.Types() {
		executor, err := s.app.registry.Get(t)
		if err != nil {
			continue
		}
		style := "display:none"
		if t == selected {
			style = ""
		}
		var sub strings.Builder
		if rendered := s.renderCustomActionForm(executor, nil); rendered != "" {
			sub.WriteString(rendered)
		} else if genericTmpl != nil {
			_ = genericTmpl.Execute(&sub, executor.ConfigSchema())
		}
		fmt.Fprintf(&buf, `<div data-action-type=%q style=%q>%s</div>`,
			t, style, sub.String())
	}
	return template.HTML(buf.String())
}

// renderCustomActionForm renders the executor's custom partial (if any)
// against the provided values. Returns "" when the executor does not
// declare a custom template. Executors may optionally implement
// PrepareFormView to transform raw config into template-ready values.
func (s *Server) renderCustomActionForm(executor ActionExecutor, values map[string]interface{}) string {
	type customTmpl interface{ CustomFormTemplate() string }
	c, ok := executor.(customTmpl)
	if !ok {
		return ""
	}
	name := c.CustomFormTemplate()
	if name == "" {
		return ""
	}
	tmpl := s.templates.partials[name]
	if tmpl == nil {
		return ""
	}
	if values == nil {
		values = map[string]interface{}{}
	}
	if prep, ok := executor.(interface {
		PrepareFormView(map[string]interface{}) map[string]interface{}
	}); ok {
		values = prep.PrepareFormView(values)
	}
	var sub strings.Builder
	_ = tmpl.Execute(&sub, values)
	return sub.String()
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	wanInterfaces, _ := s.app.db.ListWANInterfaces()
	filter := parseHistoryFilter(r)
	entries, total, _ := s.app.db.GetMergedHistory(filter)

	data := struct {
		PageData
		WANInterfaces []WANInterface
		Entries       []HistoryEntry
		Total         int
		Limit         int
	}{
		PageData:      PageData{Title: "Event History", Active: "history", CSRFToken: getCSRFToken(r)},
		WANInterfaces: wanInterfaces,
		Entries:       entries,
		Total:         total,
		Limit:         filter.Limit,
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

	s.dispatcher.checkWANInterface(r.Context(), id, "manual")

	wi, err := s.app.db.GetWANInterface(id)
	if err != nil {
		http.Error(w, "WAN interface not found", http.StatusNotFound)
		return
	}
	display := s.buildWANInterfaceDisplay(*wi)
	s.templates.renderPartial(w, "wan-interface-card", display)
}

func (s *Server) handleUIToggleWANInterface(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	wi, err := s.app.db.GetWANInterface(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	wi.Enabled = !wi.Enabled
	if err := s.app.db.UpdateWANInterface(wi); err != nil {
		http.Error(w, "failed to update WAN interface", http.StatusInternalServerError)
		return
	}
	s.dispatcher.Reload(r.Context())
	display := s.buildWANInterfaceDisplay(*wi)
	s.templates.renderPartial(w, "wan-interface-card", display)
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

// handleUIExecuteAction runs the saved action once per linked WAN interface,
// using each WAN's current last_observed_ip. Each run is logged to
// action_runs. Manual executions never set NeedsRetry — retries are a
// property of automated scheduled runs only. Returns an HTMX partial listing
// per-WAN outcomes.
func (s *Server) handleUIExecuteAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	action, err := s.app.db.GetAction(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !action.Enabled {
		fmt.Fprintf(w, `<span class="badge badge-error">action disabled</span>`)
		return
	}
	executor, err := s.app.registry.Get(action.Type)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">unknown type</span>`)
		return
	}
	links, err := s.app.db.GetActionLinkedWANInterfacesWithState(id)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">load error</span>`)
		return
	}
	if len(links) == 0 {
		fmt.Fprintf(w, `<span class="badge badge-muted">no linked WAN</span>`)
		return
	}
	decrypted, err := DecryptActionSecrets(action.ConfigJSON, executor.SecretFields(), s.app.encKey)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">decrypt error</span>`)
		return
	}

	type result struct {
		Name   string
		OK     bool
		Detail string
	}
	results := make([]result, 0, len(links))
	for _, link := range links {
		if link.LastObservedIP == "" {
			results = append(results, result{Name: link.Name, OK: false, Detail: "no IP yet"})
			s.app.db.LogActionRun(&ActionRun{
				WANInterfaceID: link.WANInterfaceID, ActionID: id,
				EventType: "ip_changed", Status: "failed",
				NeedsRetry: false, ErrorMsg: "manual execute skipped: no observed IP",
			})
			continue
		}
		execErr := s.safeExecute(r.Context(), executor, IPEvent{
			WANInterfaceID:   link.WANInterfaceID,
			WANInterfaceName: link.Name,
			EventType:        "ip_changed",
			OldIP:            link.LastObservedIP,
			NewIP:            link.LastObservedIP,
			Timestamp:        time.Now().UTC().Format(time.RFC3339),
		}, decrypted, 15*time.Second)
		status := "success"
		errMsg := ""
		if execErr != nil {
			status = "failed"
			errMsg = execErr.Error()
		}
		s.app.db.LogActionRun(&ActionRun{
			WANInterfaceID: link.WANInterfaceID, ActionID: id,
			EventType: "ip_changed", OldIP: link.LastObservedIP, NewIP: link.LastObservedIP,
			// NeedsRetry is always false for manual execute — the retry
			// queue is for automated scheduled runs only.
			Status: status, NeedsRetry: false, ErrorMsg: errMsg,
		})
		results = append(results, result{
			Name: link.Name, OK: execErr == nil,
			Detail: link.LastObservedIP,
		})
	}

	// Render inline per-WAN result badges.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for i, rr := range results {
		if i > 0 {
			fmt.Fprint(w, " ")
		}
		class := "badge-success"
		icon := "✓"
		if !rr.OK {
			class = "badge-error"
			icon = "✗"
		}
		fmt.Fprintf(w, `<span class="badge %s">%s %s (%s)</span>`,
			class, icon,
			template.HTMLEscapeString(rr.Name),
			template.HTMLEscapeString(rr.Detail))
	}
}

// handleUITestAction runs a dry-run with synthetic IPs against an unsaved
// (in-progress) action config submitted via the edit form. Does NOT write
// to action_runs. Returns a single inline badge.
func (s *Server) handleUITestAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">invalid form</span>`)
		return
	}
	actionType := r.FormValue("type")
	executor, err := s.app.registry.Get(actionType)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">unknown type</span>`)
		return
	}

	// Build config JSON from form fields (mask-merge secrets against existing
	// record if an action_id is provided so users editing existing actions
	// can test without re-entering secrets).
	configJSON, err := buildActionConfig(r, executor)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">invalid form: %s</span>`, template.HTMLEscapeString(err.Error()))
		return
	}
	if existingIDStr := r.FormValue("action_id"); existingIDStr != "" {
		if existingID, err := strconv.ParseInt(existingIDStr, 10, 64); err == nil {
			if existing, err := s.app.db.GetAction(existingID); err == nil {
				configJSON = mergeSecretFields(configJSON, existing.ConfigJSON, executor.SecretFields())
			}
		}
	}

	if err := executor.ValidateConfig(configJSON); err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">invalid config: %s</span>`,
			template.HTMLEscapeString(err.Error()))
		return
	}

	encrypted, err := EncryptActionSecrets(configJSON, executor.SecretFields(), s.app.encKey)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">encrypt error</span>`)
		return
	}
	decrypted, err := DecryptActionSecrets(encrypted, executor.SecretFields(), s.app.encKey)
	if err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">decrypt error</span>`)
		return
	}

	// Prefer a read-only TestConfig probe when the executor provides one
	// (currently: Cloudflare DNS). Otherwise fall back to a synthetic-IP
	// Execute (Discord/Webhook — sends a real test notification).
	if tester, ok := executor.(interface {
		TestConfig(context.Context, json.RawMessage) (string, error)
	}); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		summary, err := tester.TestConfig(ctx, decrypted)
		if err != nil {
			fmt.Fprintf(w, `<span class="badge badge-error">✗ %s</span>`,
				template.HTMLEscapeString(err.Error()))
			return
		}
		msg := summary
		if msg == "" {
			msg = "config OK"
		}
		fmt.Fprintf(w, `<span class="badge badge-success">✓ %s</span>`,
			template.HTMLEscapeString(msg))
		return
	}

	event := IPEvent{
		WANInterfaceName: "Test",
		EventType:        "ip_changed",
		OldIP:            "198.51.100.1",
		NewIP:            "203.0.113.42",
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.safeExecute(r.Context(), executor, event, decrypted, 15*time.Second); err != nil {
		fmt.Fprintf(w, `<span class="badge badge-error">✗ %s</span>`,
			template.HTMLEscapeString(err.Error()))
		return
	}
	fmt.Fprintf(w, `<span class="badge badge-success">✓ test sent with synthetic IP 203.0.113.42</span>`)
}

// safeExecute wraps executor.Execute with a timeout context and panic
// containment so a buggy executor cannot take down the request goroutine.
func (s *Server) safeExecute(ctx context.Context, executor ActionExecutor,
	event IPEvent, config json.RawMessage, timeout time.Duration) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("executor panic: %v", rec)
			s.logger.Error("executor panic",
				"action_type", event.EventType, "panic", rec)
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return executor.Execute(runCtx, event, config)
}

func (s *Server) handleUIDeleteWANInterface(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.app.db.DeleteWANInterface(id); err != nil {
		http.Error(w, "failed to delete WAN interface", http.StatusInternalServerError)
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

func (s *Server) handleUINetworkDiagnostics(w http.ResponseWriter, r *http.Request) {
	d := collectDiagnostics(r.Context())
	s.templates.renderPartial(w, "network-diagnostics", d)
}

func (s *Server) handleUITestRoute(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.templates.renderPartial(w, "test-route-result", testRouteResult{OK: false, Message: "Invalid form data"})
		return
	}
	egress := parseEgressForm(r)

	probeURL := firstProbeURL(r.FormValue("probe_urls"))
	if probeURL == "" {
		s.templates.renderPartial(w, "test-route-result", testRouteResult{
			OK: false, Message: "No probe URL configured",
		})
		return
	}

	cfg := WANInterfaceConfig{
		ProbeURLs:   []string{probeURL},
		Strategy:    "single",
		TimeoutSecs: 10,
		Egress:      egress,
	}

	// Gateway mode requires fwmark-based PBR setup. To avoid racing with
	// scheduled checks that manage PBR for the same tuple, we check whether
	// the tuple is owned by an enabled saved WAN. If so, reuse its rule.
	// If not, install ephemerally and remove after probing.
	if cfg.Egress.Mode == "gateway" {
		if !checkNetAdmin() {
			s.templates.renderPartial(w, "test-route-result", testRouteResult{
				OK: false, Message: "Gateway mode requires NET_ADMIN capability.",
			})
			return
		}
		if cfg.Egress.InterfaceName == "" || cfg.Egress.Gateway == "" {
			s.templates.renderPartial(w, "test-route-result", testRouteResult{
				OK: false, Message: "Gateway mode requires both interface and gateway IP.",
			})
			return
		}
		// Source IP resolved for display/state only; not used for PBR routing.
		if src, err := resolveSourceIP(cfg.Egress.InterfaceName); err == nil {
			cfg.Egress.SourceAddress = src
		}

		owned := s.tupleOwnedBySavedWAN(cfg.Egress.InterfaceName, cfg.Egress.Gateway)
		if _, _, err := s.dispatcher.pbrManager.EnsureRoute(cfg.Egress.InterfaceName, cfg.Egress.Gateway); err != nil {
			s.templates.renderPartial(w, "test-route-result", testRouteResult{
				OK: false, Message: "Failed to install PBR rule: " + err.Error(),
			})
			return
		}
		if !owned {
			defer s.dispatcher.pbrManager.CleanupRoute(cfg.Egress.InterfaceName, cfg.Egress.Gateway)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	ip, err := DetectIP(ctx, cfg, s.dispatcher.directClient)
	if err != nil {
		s.templates.renderPartial(w, "test-route-result", testRouteResult{
			OK: false, Message: "Probe failed: " + err.Error(),
		})
		return
	}
	s.templates.renderPartial(w, "test-route-result", testRouteResult{
		OK: true, Message: "Detected " + ip + " via " + probeURL,
	})
}

type testRouteResult struct {
	OK      bool
	Message string
}

func (s *Server) handleUIEgressFields(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("egress_mode")
	if mode == "" {
		mode = "default"
	}
	ifaces, _ := listNetworkInterfaces()
	data := struct {
		Mode          string
		Interfaces    []NetworkInterface
		InterfaceName string
		Gateway       string
		ProxyURL      string
	}{
		Mode:          mode,
		Interfaces:    ifaces,
		InterfaceName: r.URL.Query().Get("interface_name"),
		Gateway:       r.URL.Query().Get("gateway"),
		ProxyURL:      r.URL.Query().Get("proxy_url"),
	}
	s.templates.renderPartial(w, "egress-fields", data)
}

// handleUICloudflareZones fetches the zones reachable by a given API token
// and returns an HTML fragment of <option> tags ready to swap into the
// Cloudflare DNS form's zone select. Accepts either a fresh api_token or,
// for the edit path, an action_id whose stored token is decrypted for reuse
// (so users editing an existing action need not re-enter the secret).
func (s *Server) handleUICloudflareZones(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.FormValue("api_token"))
	if token == "" || token == secretMask {
		if idStr := r.FormValue("action_id"); idStr != "" {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				if action, err := s.app.db.GetAction(id); err == nil {
					if dec, err := DecryptActionSecrets(action.ConfigJSON, []string{"api_token"}, s.app.encKey); err == nil {
						var cfg struct {
							APIToken string `json:"api_token"`
						}
						json.Unmarshal(dec, &cfg)
						token = cfg.APIToken
					}
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if token == "" {
		fmt.Fprint(w, `<option value="">Enter API Token first</option>`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	zones, err := ListCloudflareZones(ctx, s.dispatcher.directClient, token)
	if err != nil {
		fmt.Fprintf(w, `<option value="">Error: %s</option>`, template.HTMLEscapeString(err.Error()))
		return
	}
	if len(zones) == 0 {
		fmt.Fprint(w, `<option value="">No zones found for this token</option>`)
		return
	}
	fmt.Fprint(w, `<option value="">Select a zone…</option>`)
	selectedZone := r.FormValue("zone_id")
	for _, z := range zones {
		sel := ""
		if z.ID == selectedZone {
			sel = " selected"
		}
		fmt.Fprintf(w, `<option value=%q data-zone-name=%q%s>%s</option>`,
			z.ID, z.Name, sel, template.HTMLEscapeString(z.Name))
	}
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
	entries, _, _ := s.app.db.GetMergedHistory(filter)

	data := struct {
		Entries []HistoryEntry
	}{Entries: entries}
	s.templates.renderPartial(w, "history-rows", data)
}

func (s *Server) handleUICreateWANInterface(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	schedule, err := scheduleFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wi := &WANInterface{
		Name:     r.FormValue("name"),
		Enabled:  true,
		Schedule: schedule,
		Timezone: "Local",
		Config: WANInterfaceConfig{
			ProbeURLs:   parseLines(r.FormValue("probe_urls")),
			Strategy:    r.FormValue("strategy"),
			TimeoutSecs: parseInt(r.FormValue("timeout"), 10),
			Egress:      parseEgressForm(r),
		},
	}
	if wi.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := validateCron(wi.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if _, err := s.app.db.CreateWANInterface(wi); err != nil {
		http.Error(w, "failed to create WAN interface", http.StatusInternalServerError)
		return
	}
	s.dispatcher.Reload(r.Context())
	w.Header().Set("HX-Redirect", "/wan-interfaces")
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

	schedule, err := scheduleFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing.Name = r.FormValue("name")
	existing.Enabled = r.FormValue("enabled") == "true"
	existing.Schedule = schedule
	existing.Timezone = "Local"
	existing.Config = WANInterfaceConfig{
		ProbeURLs:   parseLines(r.FormValue("probe_urls")),
		Strategy:    r.FormValue("strategy"),
		TimeoutSecs: parseInt(r.FormValue("timeout"), 10),
		Egress:      parseEgressForm(r),
	}

	if err := validateCron(existing.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.app.db.UpdateWANInterface(existing); err != nil {
		http.Error(w, "failed to update WAN interface", http.StatusInternalServerError)
		return
	}

	// Update action links in a single transaction (#3).
	var links []WANInterfaceAction
	for _, aidStr := range r.Form["action_ids"] {
		aid, err := strconv.ParseInt(aidStr, 10, 64)
		if err != nil || aid == 0 {
			continue
		}
		triggerOn := r.FormValue(fmt.Sprintf("trigger_%d", aid))
		if triggerOn == "" {
			triggerOn = "ip_changed"
		}
		links = append(links, WANInterfaceAction{
			WANInterfaceID: id,
			ActionID:       aid,
			TriggerOn:      triggerOn,
			Enabled:        true,
		})
	}
	if err := s.app.db.SetWANInterfaceLinks(id, links); err != nil {
		http.Error(w, "failed to update action links: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.dispatcher.Reload(r.Context())
	w.Header().Set("HX-Redirect", "/wan-interfaces")
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

	configJSON, err := buildActionConfig(r, executor)
	if err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := executor.ValidateConfig(configJSON); err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}

	encrypted, err := EncryptActionSecrets(configJSON, executor.SecretFields(), s.app.encKey)
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	// Parse triggers BEFORE creating the action so invalid triggers_json
	// does not leave an orphan action row behind.
	triggers, err := parseTriggersForm(r, 0)
	if err != nil {
		http.Error(w, "invalid triggers: "+err.Error(), http.StatusBadRequest)
		return
	}

	action := &Action{Type: actionType, Name: name, Enabled: true, ConfigJSON: encrypted}
	newID, err := s.app.db.CreateAction(action)
	if err != nil {
		http.Error(w, "failed to create action", http.StatusInternalServerError)
		return
	}

	if len(triggers) > 0 {
		for i := range triggers {
			triggers[i].ActionID = newID
		}
		if err := s.app.db.SetActionTriggers(newID, triggers); err != nil {
			// Action row exists but triggers failed — keep the action so
			// user can re-save triggers via Edit instead of losing their
			// config input. Surface the partial result as an error.
			http.Error(w, "action created but saving triggers failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
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

	configJSON, err := buildActionConfig(r, executor)
	if err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
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

	if triggers, err := parseTriggersForm(r, id); err != nil {
		http.Error(w, "invalid triggers: "+err.Error(), http.StatusBadRequest)
		return
	} else if err := s.app.db.SetActionTriggers(id, triggers); err != nil {
		http.Error(w, "failed to save triggers: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// No dispatcher.Reload(): scheduled checks call GetLinkedActions live
	// on every event, so trigger changes take effect on the next check
	// cycle without a reload.

	w.Header().Set("HX-Redirect", "/actions")
	w.WriteHeader(http.StatusOK)
}

// parseTriggersForm reads the triggers_json hidden field into []ActionTrigger.
// Empty / missing is valid (no triggers = action is dormant).
func parseTriggersForm(r *http.Request, actionID int64) ([]ActionTrigger, error) {
	raw := strings.TrimSpace(r.FormValue("triggers_json"))
	if raw == "" {
		return nil, nil
	}
	var rows []struct {
		WANInterfaceID int64  `json:"wan_interface_id"`
		TriggerOn      string `json:"trigger_on"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("parsing triggers: %w", err)
	}
	out := make([]ActionTrigger, 0, len(rows))
	for i, t := range rows {
		if t.WANInterfaceID == 0 {
			continue
		}
		out = append(out, ActionTrigger{
			ActionID:       actionID,
			WANInterfaceID: t.WANInterfaceID,
			TriggerOn:      t.TriggerOn,
			RunOrder:       i,
		})
	}
	return out, nil
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

// buildActionConfig picks the right form-to-JSON strategy for an executor.
// Executors that implement the optional formParser interface (e.g.
// Cloudflare DNS with its repeater) get their custom parsing; everyone
// else falls back to the generic schema-driven reader.
func buildActionConfig(r *http.Request, executor ActionExecutor) (json.RawMessage, error) {
	if cp, ok := executor.(interface {
		ParseForm(url.Values) (json.RawMessage, error)
	}); ok {
		return cp.ParseForm(r.PostForm)
	}
	return buildConfigFromForm(r, executor.ConfigSchema()), nil
}

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
	if v := r.URL.Query().Get("wan_interface"); v != "" {
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

// tupleOwnedBySavedWAN reports whether any saved WAN interface owns the given
// gateway-mode tuple (interface name + gateway). Used by Test Route to avoid
// racing with scheduled checks that manage the same PBR rule.
func (s *Server) tupleOwnedBySavedWAN(ifaceName, gateway string) bool {
	wanInterfaces, err := s.app.db.ListWANInterfaces()
	if err != nil {
		return false
	}
	for _, wi := range wanInterfaces {
		if !wi.Enabled {
			continue
		}
		wi.Config.MigrateEgress()
		if wi.Config.Egress.Mode == "gateway" &&
			wi.Config.Egress.InterfaceName == ifaceName &&
			wi.Config.Egress.Gateway == gateway {
			return true
		}
	}
	return false
}

// firstProbeURL returns the first non-empty line from a probe_urls textarea value.
func firstProbeURL(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

// scheduleFromForm builds a cron string from the form. The form submits
// either schedule_mode=simple with schedule_value + schedule_unit, or
// schedule_mode=cron with schedule_cron (used only when the existing cron
// could not be parsed into the simple dropdown).
func scheduleFromForm(r *http.Request) (string, error) {
	mode := r.FormValue("schedule_mode")
	if mode == "cron" {
		raw := strings.TrimSpace(r.FormValue("schedule_cron"))
		if raw == "" {
			return "", fmt.Errorf("cron expression is required")
		}
		return raw, nil
	}
	valueStr := r.FormValue("schedule_value")
	unit := r.FormValue("schedule_unit")
	if valueStr == "" || unit == "" {
		return "*/5 * * * *", nil
	}
	v, err := strconv.Atoi(valueStr)
	if err != nil {
		return "", fmt.Errorf("schedule_value must be a number")
	}
	return ScheduleFromSimple(v, unit)
}

func parseEgressForm(r *http.Request) EgressConfig {
	mode := r.FormValue("egress_mode")
	if mode == "" {
		mode = "default"
	}
	return EgressConfig{
		Mode:          mode,
		InterfaceName: r.FormValue("egress_interface"),
		Gateway:       r.FormValue("egress_gateway"),
		ProxyURL:      r.FormValue("egress_proxy_url"),
	}
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
