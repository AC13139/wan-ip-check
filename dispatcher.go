package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Dispatcher manages source check scheduling and action execution.
type Dispatcher struct {
	db           *DB
	registry     *ActionRegistry
	encKey       []byte
	directClient *http.Client
	proxyClient  *http.Client
	logger       *slog.Logger
	cron         *cron.Cron
	entryMap     map[int64]cron.EntryID // source ID → cron entry ID
	wanInterfaceLocks  map[int64]*sync.Mutex  // per-source execution lock
	locksMu      sync.Mutex
}

// NewDispatcher creates a new dispatcher.
func NewDispatcher(app *App) *Dispatcher {
	return &Dispatcher{
		db:           app.db,
		registry:     app.registry,
		encKey:       app.encKey,
		directClient: app.directClient,
		proxyClient:  app.proxyClient,
		logger:       app.logger,
		entryMap:     make(map[int64]cron.EntryID),
		wanInterfaceLocks:  make(map[int64]*sync.Mutex),
	}
}

// Start loads all enabled sources, runs them once, and starts the cron scheduler.
func (d *Dispatcher) Start(ctx context.Context) error {
	// Run all sources once synchronously before starting cron.
	sources, err := d.db.ListWANInterfaces()
	if err != nil {
		return err
	}
	for _, src := range sources {
		if src.Enabled {
			d.checkWANInterface(ctx, src.ID)
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

// Reload stops the current cron, re-reads sources from DB, and reschedules.
// Called by API handlers after source create/update/delete.
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

	sources, err := d.db.ListWANInterfaces()
	if err != nil {
		return err
	}

	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		s := src

		// Apply per-source timezone to cron
		loc := time.Now().Location()
		if s.Timezone != "" && s.Timezone != "Local" {
			if parsed, err := time.LoadLocation(s.Timezone); err == nil {
				loc = parsed
			}
		}

		// Wrap schedule with timezone
		schedule := s.Schedule
		entryID, err := d.cron.AddFunc("CRON_TZ="+loc.String()+" "+schedule, func() {
			d.checkWANInterface(ctx, s.ID)
		})
		if err != nil {
			d.logger.Error("invalid schedule for WAN interface", "source_id", s.ID, "name", s.Name, "schedule", s.Schedule, "error", err)
			continue
		}
		d.entryMap[s.ID] = entryID
		d.logger.Info("scheduled WAN interface", "source_id", s.ID, "name", s.Name, "schedule", s.Schedule, "timezone", loc)
	}

	d.cron.Start()
	d.logger.Info("dispatcher started", "wan_interfaces", len(d.entryMap))
	return nil
}

// checkWANInterface runs the IP detection + action dispatch cycle for a single source.
// Per-source mutex prevents concurrent execution from cron + manual API calls.
func (d *Dispatcher) checkWANInterface(ctx context.Context, sourceID int64) {
	mu := d.getWANInterfaceLock(sourceID)
	mu.Lock()
	defer mu.Unlock()

	source, err := d.db.GetWANInterface(sourceID)
	if err != nil {
		d.logger.Error("failed to load WAN interface", "wan_interface_id", sourceID, "error", err)
		return
	}

	state, err := d.db.GetWANInterfaceState(sourceID)
	if err != nil {
		d.logger.Error("failed to load WAN interface state", "wan_interface_id", sourceID, "error", err)
		return
	}

	logger := d.logger.With("wan_interface_id", sourceID, "wan_interface", source.Name)

	// Detect current IP
	newIP, err := DetectIP(ctx, source.Config, d.directClient)
	now := time.Now()

	// Update check timestamp regardless of outcome
	state.LastCheckAt = now

	if err != nil {
		state.LastStatus = "failed"
		state.ConsecutiveFailures++
		state.LastError = err.Error()
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist source state", "error", err)
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
			logger.Error("failed to persist source state", "error", err)
		}
		logger.Info("first run, IP saved", "ip", newIP)
		return
	}

	// Retry any previously failed actions for this source
	d.retryFailed(ctx, sourceID, state.LastObservedIP, logger)

	if newIP != oldIP {
		// IP changed
		state.LastObservedIP = newIP
		state.LastStatus = "changed"
		state.LastSummaryDate = now.Format("2006-01-02") // suppress same-day summary
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist source state", "error", err)
		}
		logger.Info("IP changed", "old", oldIP, "new", newIP)

		event := IPEvent{
			WANInterfaceID:   sourceID,
			WANInterfaceName: source.Name,
			EventType:  "ip_changed",
			OldIP:      oldIP,
			NewIP:      newIP,
			Timestamp:  now.UTC().Format(time.RFC3339),
		}
		d.executeActions(ctx, event, logger)
	} else {
		// IP unchanged
		state.LastStatus = "ok"
		if err := d.db.UpdateWANInterfaceState(state); err != nil {
			logger.Error("failed to persist source state", "error", err)
		}
		logger.Debug("IP unchanged", "ip", newIP)

		d.checkDailySummary(ctx, source, state, newIP, logger)
	}

	// Prune old action_runs to prevent unbounded growth (keep last 10000)
	if err := d.db.PruneActionRuns(10000); err != nil {
		logger.Warn("failed to prune action runs", "error", err)
	}
}

// checkDailySummary sends the daily "no change" notification if conditions are met.
func (d *Dispatcher) checkDailySummary(ctx context.Context, source *WANInterface, state *WANInterfaceState, currentIP string, logger *slog.Logger) {
	// Load timezone
	tz := source.Timezone
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
		WANInterfaceID:   source.ID,
		WANInterfaceName: source.Name,
		EventType:  "ip_unchanged_daily",
		OldIP:      currentIP,
		NewIP:      currentIP,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	d.executeActions(ctx, event, logger)

	// Update summary date
	state.LastSummaryDate = today
	if err := d.db.UpdateWANInterfaceState(state); err != nil {
		logger.Error("failed to persist summary date", "error", err)
	}
}

// executeActions runs all linked actions for a source event, sequentially.
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

	for _, sa := range linkedActions {
		actionLogger := logger.With("action_id", sa.ActionID, "action", sa.ActionName, "type", sa.ActionType)

		executor, err := d.registry.Get(sa.ActionType)
		if err != nil {
			actionLogger.Error("no executor registered", "error", err)
			d.logRun(event, sa, "failed", "no executor for type: "+sa.ActionType)
			continue
		}

		// Decrypt secrets in config before execution
		decryptedConfig, err := DecryptActionSecrets(sa.ActionConfigJSON, executor.SecretFields(), d.encKey)
		if err != nil {
			actionLogger.Error("failed to decrypt config", "error", err)
			d.logRun(event, sa, "failed", "decrypt error: "+err.Error())
			continue
		}

		actionLogger.Info("executing action")
		if err := executor.Execute(ctx, event, decryptedConfig); err != nil {
			actionLogger.Error("action failed", "error", err)
			d.logRun(event, sa, "failed", err.Error())
		} else {
			actionLogger.Info("action succeeded")
			d.logRun(event, sa, "success", "")
		}
	}
}

// retryFailed re-executes actions that failed on the previous check for this source.
func (d *Dispatcher) retryFailed(ctx context.Context, sourceID int64, currentIP string, logger *slog.Logger) {
	failedRuns, err := d.db.GetFailedRuns(sourceID)
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
			WANInterfaceID:   sourceID,
			WANInterfaceName: "retry",
			EventType:  run.EventType,
			OldIP:      run.OldIP,
			NewIP:      run.NewIP,
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		}

		if err := executor.Execute(ctx, event, decryptedConfig); err != nil {
			logger.Warn("retry still failed", "action_id", run.ActionID, "error", err)
		} else {
			logger.Info("retry succeeded", "action_id", run.ActionID)
			d.db.ClearRetryFlag(run.ID)
		}
	}
}

func (d *Dispatcher) getWANInterfaceLock(sourceID int64) *sync.Mutex {
	d.locksMu.Lock()
	defer d.locksMu.Unlock()
	if mu, ok := d.wanInterfaceLocks[sourceID]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	d.wanInterfaceLocks[sourceID] = mu
	return mu
}

func (d *Dispatcher) logRun(event IPEvent, sa WANInterfaceAction, status, errMsg string) {
	run := &ActionRun{
		WANInterfaceID:   event.WANInterfaceID,
		ActionID:   sa.ActionID,
		EventType:  event.EventType,
		OldIP:      event.OldIP,
		NewIP:      event.NewIP,
		Status:     status,
		NeedsRetry: status == "failed",
		ErrorMsg:   errMsg,
	}
	if err := d.db.LogActionRun(run); err != nil {
		d.logger.Error("failed to log action run", "error", err)
	}
}
