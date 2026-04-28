package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// PBRManager manages per-WAN-interface policy-based routing tables using
// fwmark-based rules. Each (interface, gateway) tuple gets a deterministic
// table + fwmark pair so that probe sockets tagged with SO_MARK route via
// the correct gateway. fwmark is used instead of source-based matching so
// multiple WANs sharing the same source IP (common in single-NIC
// containers) can coexist with different gateways.
type PBRManager struct {
	mu      sync.Mutex
	entries map[string]pbrEntry
	logger  *slog.Logger
}

type pbrEntry struct {
	table int
	mark  uint32
}

func NewPBRManager(logger *slog.Logger) *PBRManager {
	return &PBRManager{entries: make(map[string]pbrEntry), logger: logger}
}

// DeriveMarkAndTable returns a stable (table, mark) pair for a given key.
// Table range 100-65535; mark range 0x10001-0x1ff00 (above common user marks
// to minimize collision with existing iptables rules).
func DeriveMarkAndTable(key string) (table int, mark uint32) {
	var hash uint32
	for _, c := range key {
		hash = hash*31 + uint32(c)
	}
	table = 100 + int(hash%65435)
	mark = 0x10001 + (hash % 0xfeff)
	return
}

// pbrKey canonicalises the lookup key so it is consistent across all callers.
func pbrKey(interfaceName, gateway string) string {
	return fmt.Sprintf("%s|%s", interfaceName, gateway)
}

// EnsureRoute sets up fwmark-based PBR for the given (interface, gateway).
// Returns the table number and fwmark that probe sockets must use to route
// through this gateway. Idempotent — safe to call repeatedly.
func (m *PBRManager) EnsureRoute(interfaceName, gateway string) (int, uint32, error) {
	key := pbrKey(interfaceName, gateway)
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[key]; ok {
		return e.table, e.mark, nil
	}

	tbl, mark := DeriveMarkAndTable(key)
	tblStr := fmt.Sprintf("%d", tbl)
	markStr := fmt.Sprintf("0x%x", mark)

	m.logger.Info("PBR: setting up route",
		"interface", interfaceName, "gateway", gateway, "table", tbl, "fwmark", markStr)

	out, err := exec.Command("ip", "route", "replace", "default",
		"via", gateway, "dev", interfaceName, "table", tblStr).CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("ip route replace: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Explicit /mask makes the exact-match semantics clear and avoids
	// ambiguity if a future iproute2 change adjusts defaults.
	out, err = exec.Command("ip", "rule", "add",
		"fwmark", markStr+"/0xffffffff", "lookup", tblStr).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		return 0, 0, fmt.Errorf("ip rule add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	m.entries[key] = pbrEntry{table: tbl, mark: mark}
	return tbl, mark, nil
}

// CleanupRoute removes PBR routing for a given (interface, gateway).
func (m *PBRManager) CleanupRoute(interfaceName, gateway string) {
	key := pbrKey(interfaceName, gateway)
	m.mu.Lock()
	e, ok := m.entries[key]
	if ok {
		delete(m.entries, key)
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	tblStr := fmt.Sprintf("%d", e.table)
	markStr := fmt.Sprintf("0x%x", e.mark)
	exec.Command("ip", "rule", "del", "fwmark", markStr+"/0xffffffff", "lookup", tblStr).Run()
	exec.Command("ip", "route", "del", "default", "table", tblStr).Run()
	m.logger.Info("PBR: cleaned up route",
		"interface", interfaceName, "gateway", gateway, "table", e.table, "fwmark", markStr)
}

// CleanupAll removes all PBR routes managed by this instance.
func (m *PBRManager) CleanupAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		tblStr := fmt.Sprintf("%d", e.table)
		markStr := fmt.Sprintf("0x%x", e.mark)
		exec.Command("ip", "rule", "del", "fwmark", markStr+"/0xffffffff", "lookup", tblStr).Run()
		exec.Command("ip", "route", "del", "default", "table", tblStr).Run()
		m.logger.Debug("PBR: cleaned up", "table", e.table, "fwmark", markStr)
	}
	m.entries = make(map[string]pbrEntry)
}

// Dispatcher manages WAN interface check scheduling and action execution.
type Dispatcher struct {
	db                *DB
	registry          *ActionRegistry
	encKey            []byte
	directClient      *http.Client
	proxyClient       *http.Client
	logger            *slog.Logger
	cron              *cron.Cron
	entryMap          map[int64]cron.EntryID
	wanInterfaceLocks map[int64]*sync.Mutex
	locksMu           sync.Mutex
	cronMu            sync.Mutex // protects cron lifecycle (Start/Stop/Reload)
	pbrManager        *PBRManager

	// parentCtx is the long-lived (signal-scoped) context set by Start().
	// Scheduled cron jobs MUST use this — never the request-scoped context
	// passed into Reload(), which is canceled when the HTTP handler returns
	// and would silently cancel every subsequent scheduled probe.
	parentCtx context.Context
}

// NewDispatcher creates a new dispatcher.
func NewDispatcher(app *App) *Dispatcher {
	return &Dispatcher{
		db:                app.db,
		registry:          app.registry,
		encKey:            app.encKey,
		directClient:      app.directClient,
		proxyClient:       app.proxyClient,
		logger:            app.logger,
		entryMap:          make(map[int64]cron.EntryID),
		wanInterfaceLocks: make(map[int64]*sync.Mutex),
		pbrManager:        NewPBRManager(app.logger),
		// Safety net: a Reload that somehow runs before Start would otherwise
		// pass nil into the cron job closure and panic on the next tick.
		parentCtx: context.Background(),
	}
}

// Start loads all enabled WAN interfaces, runs them once, and starts the cron scheduler.
func (d *Dispatcher) Start(ctx context.Context) error {
	// Acquire cronMu so the write to parentCtx happens-before any concurrent
	// Reload reader. Pairs with the Reload lock at the read site.
	d.cronMu.Lock()
	d.parentCtx = ctx
	d.cronMu.Unlock()

	wanInterfaces, err := d.db.ListWANInterfaces()
	if err != nil {
		return err
	}

	// Remove legacy source-based PBR rules from previous beta versions so
	// they do not shadow the new fwmark rules. Best-effort — errors are
	// logged and ignored.
	d.cleanupLegacySourceRules(wanInterfaces)

	for _, wi := range wanInterfaces {
		if wi.Enabled {
			d.checkWANInterface(d.parentCtx, wi.ID, "scheduled")
		}
	}

	return d.startCron(d.parentCtx)
}

// cleanupLegacySourceRules deletes any `ip rule from SOURCE lookup TBL`
// entries that would have been installed by the previous source-based PBR
// scheme. It reconstructs the old key (interface|source|gateway) for each
// saved gateway-mode WAN and removes the rule if present. Leaves the table's
// default route alone — the new EnsureRoute will replace it on the next tick.
func (d *Dispatcher) cleanupLegacySourceRules(wanInterfaces []WANInterface) {
	for _, wi := range wanInterfaces {
		wi.Config.MigrateEgress()
		if wi.Config.Egress.Mode != "gateway" {
			continue
		}
		iface := wi.Config.Egress.InterfaceName
		gw := wi.Config.Egress.Gateway
		if iface == "" || gw == "" {
			continue
		}
		src := wi.Config.Egress.SourceAddress
		if src == "" {
			if resolved, err := resolveSourceIP(iface); err == nil {
				src = resolved
			}
		}
		if src == "" {
			continue
		}
		oldKey := fmt.Sprintf("%s|%s|%s", iface, src, gw)
		oldTable, _ := DeriveMarkAndTable(oldKey)
		tblStr := fmt.Sprintf("%d", oldTable)
		if out, err := exec.Command("ip", "rule", "del", "from", src, "lookup", tblStr).CombinedOutput(); err == nil {
			d.logger.Info("PBR: removed legacy source-based rule",
				"interface", iface, "source", src, "gateway", gw, "table", oldTable)
		} else if !strings.Contains(string(out), "No such") {
			d.logger.Debug("PBR: legacy rule cleanup attempt",
				"interface", iface, "source", src, "table", oldTable,
				"stderr", strings.TrimSpace(string(out)))
		}
	}
}

// Stop gracefully stops the dispatcher.
func (d *Dispatcher) Stop() {
	if d.cron != nil {
		<-d.cron.Stop().Done()
	}
}

// Reload stops the current cron, re-reads WAN interfaces from DB, and reschedules.
// Called by API handlers after WAN interface create/update/delete.
//
// The caller's context is intentionally ignored: it is request-scoped and gets
// canceled the moment the HTTP handler returns. Scheduled cron jobs must run
// under d.parentCtx (set by Start) so they survive past the handler that
// triggered the reload.
func (d *Dispatcher) Reload(_ context.Context) error {
	d.cronMu.Lock()
	defer d.cronMu.Unlock()

	d.logger.Info("reloading dispatcher")

	// Clean up existing PBR routes before reschedule
	d.pbrManager.CleanupAll()

	if d.cron != nil {
		<-d.cron.Stop().Done()
	}
	d.entryMap = make(map[int64]cron.EntryID)
	return d.startCron(d.parentCtx)
}

func (d *Dispatcher) startCron(ctx context.Context) error {
	d.cron = cron.New(
		cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)),
	)

	wanInterfaces, err := d.db.ListWANInterfaces()
	if err != nil {
		return err
	}

	for _, wi := range wanInterfaces {
		if !wi.Enabled {
			continue
		}
		w := wi

		// Apply per-WAN-interface timezone to cron
		loc := time.Now().Location()
		if w.Timezone != "" && w.Timezone != "Local" {
			if parsed, err := time.LoadLocation(w.Timezone); err == nil {
				loc = parsed
			}
		}

		// Wrap schedule with timezone
		schedule := w.Schedule
		entryID, err := d.cron.AddFunc("CRON_TZ="+loc.String()+" "+schedule, func() {
			d.checkWANInterface(ctx, w.ID, "scheduled")
		})
		if err != nil {
			d.logger.Error("invalid schedule for WAN interface", "wan_interface_id", w.ID, "name", w.Name, "schedule", w.Schedule, "error", err)
			continue
		}
		d.entryMap[w.ID] = entryID
		d.logger.Info("scheduled WAN interface", "wan_interface_id", w.ID, "name", w.Name, "schedule", w.Schedule, "timezone", loc)
	}

	// Hourly history sweep per the retention config (#29).
	if _, err := d.cron.AddFunc("0 * * * *", func() { d.runHistorySweep() }); err != nil {
		d.logger.Warn("failed to register history sweep job", "error", err)
	}

	d.cron.Start()
	d.logger.Info("dispatcher started", "wan_interfaces", len(d.entryMap))
	return nil
}

// runHistorySweep prunes expired wan_interface_checks and action_runs rows
// per the configured retention. Logs per-bucket delete counts when any rows
// were removed.
func (d *Dispatcher) runHistorySweep() {
	r := d.db.LoadHistoryRetention()
	stats, err := d.db.PruneHistoryByTTL(r)
	if err != nil {
		d.logger.Warn("history sweep failed", "error", err)
		return
	}
	total := stats.CheckOK + stats.CheckChanged + stats.CheckFailed +
		stats.ActionSuccess + stats.ActionFailed
	if total == 0 {
		return
	}
	d.logger.Info("history sweep",
		"check_ok", stats.CheckOK, "check_changed", stats.CheckChanged,
		"check_failed", stats.CheckFailed,
		"action_success", stats.ActionSuccess, "action_failed", stats.ActionFailed)
}

// checkWANInterface runs the IP detection + action dispatch cycle for a
// single WAN interface. trigger is "scheduled" or "manual". Every check
// outcome is logged to wan_interface_checks. Per-WAN-interface mutex
// prevents concurrent execution from cron + manual API calls.
func (d *Dispatcher) checkWANInterface(ctx context.Context, wanInterfaceID int64, trigger string) {
	mu := d.getWANInterfaceLock(wanInterfaceID)
	mu.Lock()
	defer mu.Unlock()

	if trigger == "" {
		trigger = "scheduled"
	}

	wi, err := d.db.GetWANInterface(wanInterfaceID)
	if err != nil {
		d.logger.Error("failed to load WAN interface", "wan_interface_id", wanInterfaceID, "error", err)
		return
	}

	state, err := d.db.GetWANInterfaceState(wanInterfaceID)
	if err != nil {
		d.logger.Error("failed to load WAN interface state", "wan_interface_id", wanInterfaceID, "error", err)
		return
	}

	logger := d.logger.With("wan_interface_id", wanInterfaceID, "wan_interface", wi.Name, "trigger", trigger)
	previousIP := state.LastObservedIP

	logCheck := func(status, observedIP, errMsg string) {
		if err := d.db.LogWANInterfaceCheck(&WANInterfaceCheck{
			WANInterfaceID: wanInterfaceID,
			Trigger:        trigger,
			Status:         status,
			ObservedIP:     observedIP,
			PreviousIP:     previousIP,
			ErrorMsg:       errMsg,
		}); err != nil {
			logger.Warn("failed to log check", "error", err)
		}
	}

	// Migrate old config and set up gateway PBR if needed
	wi.Config.MigrateEgress()
	if wi.Config.Egress.Mode == "gateway" {
		if err := d.setupGatewayPBR(&wi.Config.Egress, logger); err != nil {
			logger.Error("gateway PBR setup failed — skipping check to avoid wrong-path detection", "error", err)
			state.LastStatus = "failed"
			state.ConsecutiveFailures++
			state.LastError = "PBR setup failed: " + err.Error()
			state.LastCheckAt = time.Now()
			d.db.UpdateWANInterfaceState(state)
			logCheck("failed", "", state.LastError)
			return
		}
	}

	// Detect current IP
	newIP, err := DetectIP(ctx, wi.Config, d.directClient)
	now := time.Now()

	// Update check timestamp regardless of outcome
	state.LastCheckAt = now

	if err != nil {
		state.LastStatus = "failed"
		state.ConsecutiveFailures++
		state.LastError = err.Error()
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist WAN interface state", "error", err)
		}
		logger.Error("IP detection failed", "error", err, "consecutive_failures", state.ConsecutiveFailures)
		logCheck("failed", "", state.LastError)
		return
	}

	// Detection succeeded
	state.LastSuccessAt = now
	state.ConsecutiveFailures = 0
	state.LastError = ""

	oldIP := state.LastObservedIP

	if oldIP == "" {
		// First run: save IP, no actions
		state.LastObservedIP = newIP
		state.LastStatus = "ok"
		state.LastSummaryDate = now.Format("2006-01-02")
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist WAN interface state", "error", err)
		}
		logger.Info("first run, IP saved", "ip", newIP)
		logCheck("ok", newIP, "")
		return
	}

	// Retry any previously failed actions for this WAN interface
	d.retryFailed(ctx, wanInterfaceID, state.LastObservedIP, logger)

	if newIP != oldIP {
		// IP changed
		state.LastObservedIP = newIP
		state.LastStatus = "changed"
		state.LastSummaryDate = now.Format("2006-01-02") // suppress same-day summary
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist WAN interface state", "error", err)
		}
		logger.Info("IP changed", "old", oldIP, "new", newIP)
		logCheck("changed", newIP, "")

		event := IPEvent{
			WANInterfaceID:   wanInterfaceID,
			WANInterfaceName: wi.Name,
			EventType:        "ip_changed",
			OldIP:            oldIP,
			NewIP:            newIP,
			Timestamp:        now.UTC().Format(time.RFC3339),
		}
		d.executeActions(ctx, event, logger)
	} else {
		// IP unchanged
		state.LastStatus = "ok"
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist WAN interface state", "error", err)
		}
		logger.Debug("IP unchanged", "ip", newIP)
		logCheck("ok", newIP, "")

		d.checkDailySummary(ctx, wi, state, newIP, logger)
	}

	// History retention is handled by the hourly sweep (#29). The old
	// blanket 10k-row cap on action_runs is gone.
}

// checkDailySummary sends the daily "no change" notification if conditions are met.
func (d *Dispatcher) checkDailySummary(ctx context.Context, wi *WANInterface, state *WANInterfaceState, currentIP string, logger *slog.Logger) {
	// Load timezone
	tz := wi.Timezone
	if tz == "" {
		tz = "Local"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Now().Location()
	}
	now := time.Now().In(loc)

	// Get daily summary time from settings
	summaryTime, err := d.db.GetSetting("daily_summary_time")
	if err != nil {
		logger.Warn("failed to read daily_summary_time setting", "error", err)
	}
	if summaryTime == "" {
		summaryTime = "2000"
	}

	currentHHMM := now.Format("1504")
	if currentHHMM < summaryTime {
		return
	}

	today := now.Format("2006-01-02")
	if today == state.LastSummaryDate {
		return
	}

	logger.Info("sending daily summary", "time", currentHHMM, "threshold", summaryTime)

	event := IPEvent{
		WANInterfaceID:   wi.ID,
		WANInterfaceName: wi.Name,
		EventType:        "ip_unchanged_daily",
		OldIP:            currentIP,
		NewIP:            currentIP,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}
	d.executeActions(ctx, event, logger)

	// Update summary date
	state.LastSummaryDate = today
	if err := d.db.UpdateWANInterfaceState(state); err != nil {
		logger.Error("failed to persist summary date", "error", err)
	}
}

// executeActions runs all linked actions for a WAN interface event, sequentially.
func (d *Dispatcher) executeActions(ctx context.Context, event IPEvent, logger *slog.Logger) {
	linkedActions, err := d.db.GetLinkedActions(event.WANInterfaceID, event.EventType)
	if err != nil {
		logger.Error("failed to get linked actions", "error", err)
		return
	}

	if len(linkedActions) == 0 {
		logger.Debug("no linked actions for event", "event", event.EventType)
		return
	}

	for _, wia := range linkedActions {
		actionLogger := logger.With("action_id", wia.ActionID, "action", wia.ActionName, "type", wia.ActionType)

		executor, err := d.registry.Get(wia.ActionType)
		if err != nil {
			actionLogger.Error("no executor registered", "error", err)
			d.logRun(event, wia, "failed", "no executor for type: "+wia.ActionType)
			continue
		}

		// Decrypt secrets in config before execution
		decryptedConfig, err := DecryptActionSecrets(wia.ActionConfigJSON, executor.SecretFields(), d.encKey)
		if err != nil {
			actionLogger.Error("failed to decrypt config", "error", err)
			d.logRun(event, wia, "failed", "decrypt error: "+err.Error())
			continue
		}

		actionLogger.Info("executing action")
		if err := executor.Execute(ctx, event, decryptedConfig); err != nil {
			actionLogger.Error("action failed", "error", err)
			d.logRun(event, wia, "failed", err.Error())
		} else {
			actionLogger.Info("action succeeded")
			d.logRun(event, wia, "success", "")
		}
	}
}

// retryFailed re-executes actions that failed on the previous check for this WAN interface.
func (d *Dispatcher) retryFailed(ctx context.Context, wanInterfaceID int64, currentIP string, logger *slog.Logger) {
	failedRuns, err := d.db.GetFailedRuns(wanInterfaceID)
	if err != nil || len(failedRuns) == 0 {
		return
	}

	logger.Info("retrying failed actions", "count", len(failedRuns))
	for _, run := range failedRuns {
		executor, err := d.registry.Get(run.ActionType)
		if err != nil {
			continue
		}

		action, err := d.db.GetAction(run.ActionID)
		if err != nil {
			continue
		}

		decryptedConfig, err := DecryptActionSecrets(action.ConfigJSON, executor.SecretFields(), d.encKey)
		if err != nil {
			continue
		}

		event := IPEvent{
			WANInterfaceID:   wanInterfaceID,
			WANInterfaceName: "retry",
			EventType:        run.EventType,
			OldIP:            run.OldIP,
			NewIP:            run.NewIP,
			Timestamp:        time.Now().UTC().Format(time.RFC3339),
		}

		if err := executor.Execute(ctx, event, decryptedConfig); err != nil {
			logger.Warn("retry still failed", "action_id", run.ActionID, "error", err)
		} else {
			logger.Info("retry succeeded", "action_id", run.ActionID)
			d.db.ClearRetryFlag(run.ID)
		}
	}
}

// setupGatewayPBR ensures fwmark-based PBR is configured for gateway mode.
// The source IP is resolved for display/state purposes only — PBR selection
// is by fwmark, not source address.
func (d *Dispatcher) setupGatewayPBR(egress *EgressConfig, logger *slog.Logger) error {
	if egress.InterfaceName == "" || egress.Gateway == "" {
		return fmt.Errorf("gateway mode requires interface + gateway, got interface=%q gateway=%q",
			egress.InterfaceName, egress.Gateway)
	}
	if egress.SourceAddress == "" {
		if resolved, err := resolveSourceIP(egress.InterfaceName); err == nil {
			egress.SourceAddress = resolved
		}
	}
	_, _, err := d.pbrManager.EnsureRoute(egress.InterfaceName, egress.Gateway)
	return err
}

func (d *Dispatcher) getWANInterfaceLock(wanInterfaceID int64) *sync.Mutex {
	d.locksMu.Lock()
	defer d.locksMu.Unlock()
	if mu, ok := d.wanInterfaceLocks[wanInterfaceID]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	d.wanInterfaceLocks[wanInterfaceID] = mu
	return mu
}

func (d *Dispatcher) logRun(event IPEvent, wia WANInterfaceAction, status, errMsg string) {
	run := &ActionRun{
		WANInterfaceID: event.WANInterfaceID,
		ActionID:       wia.ActionID,
		EventType:      event.EventType,
		OldIP:          event.OldIP,
		NewIP:          event.NewIP,
		Status:         status,
		NeedsRetry:     status == "failed",
		ErrorMsg:       errMsg,
	}
	if err := d.db.LogActionRun(run); err != nil {
		d.logger.Error("failed to log action run", "error", err)
	}
}
