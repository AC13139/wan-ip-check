package main

import (
	"context"
	"log/slog"
	"net/http"
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
	}
}

// Start loads all enabled sources and schedules them.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.cron = cron.New(
		cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)),
	)

	sources, err := d.db.ListSources()
	if err != nil {
		return err
	}

	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		s := src // capture for closure
		entryID, err := d.cron.AddFunc(s.Schedule, func() {
			d.checkSource(ctx, s.ID)
		})
		if err != nil {
			d.logger.Error("invalid schedule for source", "source_id", s.ID, "name", s.Name, "schedule", s.Schedule, "error", err)
			continue
		}
		d.entryMap[s.ID] = entryID
		d.logger.Info("scheduled source", "source_id", s.ID, "name", s.Name, "schedule", s.Schedule)
	}

	// Run all sources once synchronously before starting cron.
	// This avoids racing with cron-scheduled runs and ensures first-run
	// state is persisted before the scheduler fires.
	for _, src := range sources {
		if src.Enabled {
			d.checkSource(ctx, src.ID)
		}
	}

	d.cron.Start()
	d.logger.Info("dispatcher started", "sources", len(d.entryMap))
	return nil
}

// Stop gracefully stops the dispatcher.
func (d *Dispatcher) Stop() {
	if d.cron != nil {
		<-d.cron.Stop().Done()
	}
}

// checkSource runs the IP detection + action dispatch cycle for a single source.
func (d *Dispatcher) checkSource(ctx context.Context, sourceID int64) {
	source, err := d.db.GetSource(sourceID)
	if err != nil {
		d.logger.Error("failed to load source", "source_id", sourceID, "error", err)
		return
	}

	state, err := d.db.GetSourceState(sourceID)
	if err != nil {
		d.logger.Error("failed to load source state", "source_id", sourceID, "error", err)
		return
	}

	logger := d.logger.With("source_id", sourceID, "source", source.Name)

	// Detect current IP
	newIP, err := DetectIP(ctx, source.Config, d.directClient)
	now := time.Now()

	// Update check timestamp regardless of outcome
	state.LastCheckAt = now

	if err != nil {
		state.LastStatus = "failed"
		state.ConsecutiveFailures++
		state.LastError = err.Error()
		if err := d.db.UpdateSourceState(state); err != nil {
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
		if err := d.db.UpdateSourceState(state); err != nil {
			logger.Error("failed to persist source state", "error", err)
		}
		logger.Info("first run, IP saved", "ip", newIP)
		return
	}

	if newIP != oldIP {
		// IP changed
		state.LastObservedIP = newIP
		state.LastStatus = "changed"
		state.LastSummaryDate = now.Format("2006-01-02") // suppress same-day summary
		if err := d.db.UpdateSourceState(state); err != nil {
			logger.Error("failed to persist source state", "error", err)
		}
		logger.Info("IP changed", "old", oldIP, "new", newIP)

		event := IPEvent{
			SourceID:   sourceID,
			SourceName: source.Name,
			EventType:  "ip_changed",
			OldIP:      oldIP,
			NewIP:      newIP,
			Timestamp:  now.UTC().Format(time.RFC3339),
		}
		d.executeActions(ctx, event, logger)
	} else {
		// IP unchanged
		state.LastStatus = "ok"
		if err := d.db.UpdateSourceState(state); err != nil {
			logger.Error("failed to persist source state", "error", err)
		}
		logger.Debug("IP unchanged", "ip", newIP)

		d.checkDailySummary(ctx, source, state, newIP, logger)
	}
}

// checkDailySummary sends the daily "no change" notification if conditions are met.
func (d *Dispatcher) checkDailySummary(ctx context.Context, source *Source, state *SourceState, currentIP string, logger *slog.Logger) {
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
		SourceID:   source.ID,
		SourceName: source.Name,
		EventType:  "ip_unchanged_daily",
		OldIP:      currentIP,
		NewIP:      currentIP,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	d.executeActions(ctx, event, logger)

	// Update summary date
	state.LastSummaryDate = today
	if err := d.db.UpdateSourceState(state); err != nil {
		logger.Error("failed to persist summary date", "error", err)
	}
}

// executeActions runs all linked actions for a source event, sequentially.
func (d *Dispatcher) executeActions(ctx context.Context, event IPEvent, logger *slog.Logger) {
	linkedActions, err := d.db.GetLinkedActions(event.SourceID, event.EventType)
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

func (d *Dispatcher) logRun(event IPEvent, sa SourceAction, status, errMsg string) {
	run := &ActionRun{
		SourceID:   event.SourceID,
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
