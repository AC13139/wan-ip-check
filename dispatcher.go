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

// PBRManager manages per-WAN-interface policy-based routing tables.
// Uses deterministic table mapping so tables survive process restarts.
type PBRManager struct {
	mu     sync.Mutex
	tables map[string]int
	logger *slog.Logger
}

func NewPBRManager(logger *slog.Logger) *PBRManager {
	return &PBRManager{tables: make(map[string]int), logger: logger}
}

// deterministicTable returns a stable table number (100-65535) for a given key.
// Large range minimizes collision risk across different WAN interface configs.
func deterministicTable(key string) int {
	var hash uint32
	for _, c := range key {
		hash = hash*31 + uint32(c)
	}
	return 100 + int(hash%65435) // range 100-65535
}

// EnsureRoute sets up PBR routing for the given interface/source/gateway combination.
func (m *PBRManager) EnsureRoute(interfaceName, sourceAddr, gateway string) (int, error) {
	key := fmt.Sprintf("%s|%s|%s", interfaceName, sourceAddr, gateway)
	m.mu.Lock()
	defer m.mu.Unlock()

	if tbl, ok := m.tables[key]; ok {
		return tbl, nil
	}

	tbl := deterministicTable(key)
	tblStr := fmt.Sprintf("%d", tbl)

	m.logger.Info("PBR: setting up route", "interface", interfaceName, "source", sourceAddr, "gateway", gateway, "table", tbl)

	out, err := exec.Command("ip", "route", "replace", "default", "via", gateway, "dev", interfaceName, "table", tblStr).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("ip route replace: %s: %w", strings.TrimSpace(string(out)), err)
	}

	out, err = exec.Command("ip", "rule", "add", "from", sourceAddr, "lookup", tblStr).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		return 0, fmt.Errorf("ip rule add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	m.tables[key] = tbl
	return tbl, nil
}

// CleanupRoute removes PBR routing for a given tuple.
func (m *PBRManager) CleanupRoute(interfaceName, sourceAddr, gateway string) {
	key := fmt.Sprintf("%s|%s|%s", interfaceName, sourceAddr, gateway)
	m.mu.Lock()
	tbl, ok := m.tables[key]
	if ok {
		delete(m.tables, key)
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	tblStr := fmt.Sprintf("%d", tbl)
	exec.Command("ip", "rule", "del", "from", sourceAddr, "lookup", tblStr).Run()
	exec.Command("ip", "route", "del", "default", "table", tblStr).Run()
	m.logger.Info("PBR: cleaned up route", "source", sourceAddr, "gateway", gateway, "table", tbl)
}

// Dispatcher manages WAN interface check scheduling and action execution.
type Dispatcher struct {
	db               *DB
	registry         *ActionRegistry
	encKey           []byte
	directClient     *http.Client
	proxyClient      *http.Client
	logger           *slog.Logger
	cron             *cron.Cron
	entryMap         map[int64]cron.EntryID
	wanInterfaceLocks map[int64]*sync.Mutex
	locksMu          sync.Mutex
	pbrManager       *PBRManager
}

// NewDispatcher creates a new dispatcher.
func NewDispatcher(app *App) *Dispatcher {
	return &Dispatcher{
		db:               app.db,
		registry:         app.registry,
		encKey:           app.encKey,
		directClient:     app.directClient,
		proxyClient:      app.proxyClient,
		logger:           app.logger,
		entryMap:         make(map[int64]cron.EntryID),
		wanInterfaceLocks: make(map[int64]*sync.Mutex),
		pbrManager:       NewPBRManager(app.logger),
	}
}

// Start loads all enabled WAN interfaces, runs them once, and starts the cron scheduler.
func (d *Dispatcher) Start(ctx context.Context) error {
	// Run all WAN interfaces once synchronously before starting cron.
	wanInterfaces, err := d.db.ListWANInterfaces()
	if err != nil {
		return err
	}
	for _, wi := range wanInterfaces {
		if wi.Enabled {
			d.checkWANInterface(ctx, wi.ID)
		}
	}

	return d.startCron(ctx)
}

// Stop gracefully stops the dispatcher.
func (d *Dispatcher) Stop() {
	if d.cron != nil {
		<-d.cron.Stop().Done()
	}
}

// Reload stops the current cron, re-reads WAN interfaces from DB, and reschedules.
// Called by API handlers after WAN interface create/update/delete.
func (d *Dispatcher) Reload(ctx context.Context) error {
	d.logger.Info("reloading dispatcher")
	if d.cron != nil {
		<-d.cron.Stop().Done()
	}
	d.entryMap = make(map[int64]cron.EntryID)
	return d.startCron(ctx)
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
			d.checkWANInterface(ctx, w.ID)
		})
		if err != nil {
			d.logger.Error("invalid schedule for WAN interface", "wan_interface_id", w.ID, "name", w.Name, "schedule", w.Schedule, "error", err)
			continue
		}
		d.entryMap[w.ID] = entryID
		d.logger.Info("scheduled WAN interface", "wan_interface_id", w.ID, "name", w.Name, "schedule", w.Schedule, "timezone", loc)
	}

	d.cron.Start()
	d.logger.Info("dispatcher started", "wan_interfaces", len(d.entryMap))
	return nil
}

// checkWANInterface runs the IP detection + action dispatch cycle for a single WAN interface.
// Per-WAN-interface mutex prevents concurrent execution from cron + manual API calls.
func (d *Dispatcher) checkWANInterface(ctx context.Context, wanInterfaceID int64) {
	mu := d.getWANInterfaceLock(wanInterfaceID)
	mu.Lock()
	defer mu.Unlock()

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

	logger := d.logger.With("wan_interface_id", wanInterfaceID, "wan_interface", wi.Name)

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

		d.checkDailySummary(ctx, wi, state, newIP, logger)
	}

	// Prune old action_runs to prevent unbounded growth (keep last 10000)
	if err := d.db.PruneActionRuns(10000); err != nil {
		logger.Warn("failed to prune action runs", "error", err)
	}
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

// setupGatewayPBR ensures PBR routing is configured for gateway mode.
func (d *Dispatcher) setupGatewayPBR(egress *EgressConfig, logger *slog.Logger) error {
	sourceAddr := egress.SourceAddress
	if sourceAddr == "" && egress.InterfaceName != "" {
		resolved, err := resolveSourceIP(egress.InterfaceName)
		if err != nil {
			return fmt.Errorf("resolving source IP: %w", err)
		}
		sourceAddr = resolved
		egress.SourceAddress = sourceAddr // cache for DetectIP
	}
	if sourceAddr == "" || egress.Gateway == "" {
		return fmt.Errorf("gateway mode requires interface + gateway, got source=%q gateway=%q", sourceAddr, egress.Gateway)
	}
	_, err := d.pbrManager.EnsureRoute(egress.InterfaceName, sourceAddr, egress.Gateway)
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
