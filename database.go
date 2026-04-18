package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database with typed query methods.
type DB struct {
	conn   *sql.DB
	logger *slog.Logger
}

// OpenDB opens (or creates) the SQLite database and runs migrations.
func OpenDB(dataDir string, logger *slog.Logger) (*DB, error) {
	dbPath := filepath.Join(dataDir, "wan-ip-check.db")
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite is single-writer

	// Enable foreign key enforcement
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}

	db := &DB{conn: conn, logger: logger}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	logger.Info("database ready", "path", dbPath)
	return db, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) migrate() error {
	if err := db.renameAlphaSchema(); err != nil {
		return fmt.Errorf("renaming alpha schema: %w", err)
	}
	if _, err := db.conn.Exec(schema); err != nil {
		return err
	}
	return nil
}

// renameAlphaSchema migrates v2.0.0-alpha databases (tables: sources,
// source_state, source_actions; columns: source_id) to the current beta
// schema (wan_interfaces, wan_interface_state, wan_interface_actions;
// wan_interface_id). Idempotent — safe to run on already-migrated DBs.
func (db *DB) renameAlphaSchema() error {
	tableExists := func(name string) (bool, error) {
		var n string
		err := db.conn.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name = ?",
			name,
		).Scan(&n)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}
	columnExists := func(table, column string) (bool, error) {
		var n string
		err := db.conn.QueryRow(
			"SELECT name FROM pragma_table_info(?) WHERE name = ?",
			table, column,
		).Scan(&n)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}

	hasSources, err := tableExists("sources")
	if err != nil {
		return err
	}
	hasSourceState, err := tableExists("source_state")
	if err != nil {
		return err
	}
	hasSourceActions, err := tableExists("source_actions")
	if err != nil {
		return err
	}
	actionRunsHasSourceID, err := columnExists("action_runs", "source_id")
	if err != nil {
		return err
	}

	if !hasSources && !hasSourceState && !hasSourceActions && !actionRunsHasSourceID {
		return nil
	}

	db.logger.Info("migrating alpha schema to wan_interface tables",
		"sources", hasSources, "source_state", hasSourceState,
		"source_actions", hasSourceActions, "action_runs.source_id", actionRunsHasSourceID)

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var stmts []string
	if hasSources {
		stmts = append(stmts, "ALTER TABLE sources RENAME TO wan_interfaces")
	}
	if hasSourceState {
		stmts = append(stmts,
			"ALTER TABLE source_state RENAME TO wan_interface_state",
			"ALTER TABLE wan_interface_state RENAME COLUMN source_id TO wan_interface_id",
		)
	}
	if hasSourceActions {
		stmts = append(stmts,
			"ALTER TABLE source_actions RENAME TO wan_interface_actions",
			"ALTER TABLE wan_interface_actions RENAME COLUMN source_id TO wan_interface_id",
		)
	}
	if actionRunsHasSourceID {
		stmts = append(stmts,
			"ALTER TABLE action_runs RENAME COLUMN source_id TO wan_interface_id",
			"DROP INDEX IF EXISTS idx_action_runs_source",
		)
	}

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s, err)
		}
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS wan_interfaces (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    schedule   TEXT NOT NULL DEFAULT '*/5 * * * *',
    timezone   TEXT NOT NULL DEFAULT 'Local',
    config     TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS wan_interface_state (
    wan_interface_id     INTEGER PRIMARY KEY REFERENCES wan_interfaces(id) ON DELETE CASCADE,
    last_observed_ip     TEXT,
    last_check_at        DATETIME,
    last_success_at      DATETIME,
    last_status          TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_run_at          DATETIME,
    last_summary_date    TEXT,
    last_error           TEXT
);

CREATE TABLE IF NOT EXISTS actions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    type       TEXT NOT NULL,
    name       TEXT NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    config     TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS wan_interface_actions (
    wan_interface_id  INTEGER NOT NULL REFERENCES wan_interfaces(id) ON DELETE CASCADE,
    action_id         INTEGER NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    trigger_on TEXT NOT NULL DEFAULT 'ip_changed',
    run_order  INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (wan_interface_id, action_id)
);

CREATE TABLE IF NOT EXISTS action_runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    wan_interface_id  INTEGER NOT NULL REFERENCES wan_interfaces(id),
    action_id        INTEGER NOT NULL REFERENCES actions(id),
    event_type       TEXT NOT NULL,
    old_ip           TEXT,
    new_ip           TEXT,
    status           TEXT NOT NULL,
    needs_retry      INTEGER NOT NULL DEFAULT 0,
    error_msg        TEXT,
    executed_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_action_runs_wan_interface ON action_runs(wan_interface_id, executed_at DESC);
CREATE INDEX IF NOT EXISTS idx_action_runs_needs_retry ON action_runs(needs_retry) WHERE needs_retry = 1;

CREATE TABLE IF NOT EXISTS wan_interface_checks (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    wan_interface_id  INTEGER NOT NULL REFERENCES wan_interfaces(id) ON DELETE CASCADE,
    trigger           TEXT NOT NULL,              -- 'scheduled' or 'manual'
    status            TEXT NOT NULL,              -- 'ok', 'changed', 'failed'
    observed_ip       TEXT,
    previous_ip       TEXT,
    error_msg         TEXT,
    executed_at       DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_wan_interface_checks_wi ON wan_interface_checks(wan_interface_id, executed_at DESC);
`

// --- WAN Interface Checks (audit log of every IP probe) ---

// WANInterfaceCheck records a single IP-probe outcome. Unlike ActionRun
// (which logs action executions triggered by an event), this records every
// check — scheduled or manual — including ones where the IP did not change.
type WANInterfaceCheck struct {
	ID             int64
	WANInterfaceID int64
	Trigger        string // "scheduled" | "manual"
	Status         string // "ok" | "changed" | "failed"
	ObservedIP     string
	PreviousIP     string
	ErrorMsg       string
	ExecutedAt     time.Time
}

func (db *DB) LogWANInterfaceCheck(c *WANInterfaceCheck) error {
	_, err := db.conn.Exec(
		`INSERT INTO wan_interface_checks
		 (wan_interface_id, trigger, status, observed_ip, previous_ip, error_msg)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.WANInterfaceID, c.Trigger, c.Status,
		nullStr(c.ObservedIP), nullStr(c.PreviousIP), nullStr(c.ErrorMsg),
	)
	return err
}

// --- Settings ---

func (db *DB) GetSetting(key string) (string, error) {
	var value string
	err := db.conn.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (db *DB) SetSetting(key, value string) error {
	_, err := db.conn.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?",
		key, value, value,
	)
	return err
}

// --- WAN Interfaces ---

func (db *DB) CreateWANInterface(wi *WANInterface) (int64, error) {
	configJSON, err := json.Marshal(wi.Config)
	if err != nil {
		return 0, fmt.Errorf("marshaling WAN interface config: %w", err)
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		"INSERT INTO wan_interfaces (name, enabled, schedule, timezone, config) VALUES (?, ?, ?, ?, ?)",
		wi.Name, wi.Enabled, wi.Schedule, wi.Timezone, string(configJSON),
	)
	if err != nil {
		return 0, err
	}
	id, _ := result.LastInsertId()

	if _, err := tx.Exec("INSERT INTO wan_interface_state (wan_interface_id) VALUES (?)", id); err != nil {
		return 0, fmt.Errorf("creating WAN interface state: %w", err)
	}

	return id, tx.Commit()
}

func (db *DB) GetWANInterface(id int64) (*WANInterface, error) {
	wi := &WANInterface{}
	var configJSON string
	err := db.conn.QueryRow(
		"SELECT id, name, enabled, schedule, timezone, config, created_at, updated_at FROM wan_interfaces WHERE id = ?", id,
	).Scan(&wi.ID, &wi.Name, &wi.Enabled, &wi.Schedule, &wi.Timezone, &configJSON, &wi.CreatedAt, &wi.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(configJSON), &wi.Config); err != nil {
		return nil, fmt.Errorf("parsing WAN interface config: %w", err)
	}
	return wi, nil
}

func (db *DB) ListWANInterfaces() ([]WANInterface, error) {
	rows, err := db.conn.Query(
		"SELECT id, name, enabled, schedule, timezone, config, created_at, updated_at FROM wan_interfaces ORDER BY id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var wanInterfaces []WANInterface
	for rows.Next() {
		var wi WANInterface
		var configJSON string
		if err := rows.Scan(&wi.ID, &wi.Name, &wi.Enabled, &wi.Schedule, &wi.Timezone, &configJSON, &wi.CreatedAt, &wi.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(configJSON), &wi.Config); err != nil {
			db.logger.Warn("invalid WAN interface config JSON", "wan_interface_id", wi.ID, "error", err)
		}
		wanInterfaces = append(wanInterfaces, wi)
	}
	return wanInterfaces, rows.Err()
}

func (db *DB) UpdateWANInterface(wi *WANInterface) error {
	configJSON, err := json.Marshal(wi.Config)
	if err != nil {
		return fmt.Errorf("marshaling WAN interface config: %w", err)
	}
	_, err = db.conn.Exec(
		"UPDATE wan_interfaces SET name = ?, enabled = ?, schedule = ?, timezone = ?, config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		wi.Name, wi.Enabled, wi.Schedule, wi.Timezone, string(configJSON), wi.ID,
	)
	return err
}

func (db *DB) DeleteWANInterface(id int64) error {
	_, err := db.conn.Exec("DELETE FROM wan_interfaces WHERE id = ?", id)
	return err
}

// --- WAN Interface State ---

func (db *DB) GetWANInterfaceState(wanInterfaceID int64) (*WANInterfaceState, error) {
	ws := &WANInterfaceState{WANInterfaceID: wanInterfaceID}
	var lastIP, lastCheck, lastSuccess, lastStatus, nextRun, summaryDate, lastErr sql.NullString
	err := db.conn.QueryRow(
		`SELECT last_observed_ip, last_check_at, last_success_at, last_status,
		        consecutive_failures, next_run_at, last_summary_date, last_error
		 FROM wan_interface_state WHERE wan_interface_id = ?`, wanInterfaceID,
	).Scan(&lastIP, &lastCheck, &lastSuccess, &lastStatus,
		&ws.ConsecutiveFailures, &nextRun, &summaryDate, &lastErr)
	if err != nil {
		return nil, err
	}
	ws.LastObservedIP = lastIP.String
	ws.LastStatus = lastStatus.String
	ws.LastSummaryDate = summaryDate.String
	ws.LastError = lastErr.String
	if lastCheck.Valid {
		ws.LastCheckAt = parseSQLiteTime(lastCheck.String)
	}
	if lastSuccess.Valid {
		ws.LastSuccessAt = parseSQLiteTime(lastSuccess.String)
	}
	if nextRun.Valid {
		ws.NextRunAt = parseSQLiteTime(nextRun.String)
	}
	return ws, nil
}

// parseSQLiteTime accepts both ISO-8601 (RFC3339, "2026-04-18T15:37:21Z")
// and SQLite-CLI-style DateTime ("2026-04-18 15:37:21") strings. The
// modernc driver normalises DATETIME columns to RFC3339 when Scan'd into
// a string, but older rows and hand-crafted INSERTs use the space form.
// Returns zero time on any failure.
func parseSQLiteTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.DateTime} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (db *DB) UpdateWANInterfaceState(ws *WANInterfaceState) error {
	_, err := db.conn.Exec(
		`UPDATE wan_interface_state SET
		    last_observed_ip = ?, last_check_at = ?, last_success_at = ?,
		    last_status = ?, consecutive_failures = ?, next_run_at = ?,
		    last_summary_date = ?, last_error = ?
		 WHERE wan_interface_id = ?`,
		nullStr(ws.LastObservedIP),
		nullTime(ws.LastCheckAt),
		nullTime(ws.LastSuccessAt),
		nullStr(ws.LastStatus),
		ws.ConsecutiveFailures,
		nullTime(ws.NextRunAt),
		nullStr(ws.LastSummaryDate),
		nullStr(ws.LastError),
		ws.WANInterfaceID,
	)
	return err
}

// --- Actions ---

func (db *DB) CreateAction(a *Action) (int64, error) {
	result, err := db.conn.Exec(
		"INSERT INTO actions (type, name, enabled, config) VALUES (?, ?, ?, ?)",
		a.Type, a.Name, a.Enabled, string(a.ConfigJSON),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (db *DB) GetAction(id int64) (*Action, error) {
	a := &Action{}
	var configJSON string
	err := db.conn.QueryRow(
		"SELECT id, type, name, enabled, config, created_at, updated_at FROM actions WHERE id = ?", id,
	).Scan(&a.ID, &a.Type, &a.Name, &a.Enabled, &configJSON, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.ConfigJSON = json.RawMessage(configJSON)
	return a, nil
}

func (db *DB) ListActions() ([]Action, error) {
	rows, err := db.conn.Query(
		"SELECT id, type, name, enabled, config, created_at, updated_at FROM actions ORDER BY id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var actions []Action
	for rows.Next() {
		var a Action
		var configJSON string
		if err := rows.Scan(&a.ID, &a.Type, &a.Name, &a.Enabled, &configJSON, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.ConfigJSON = json.RawMessage(configJSON)
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

func (db *DB) UpdateAction(a *Action) error {
	_, err := db.conn.Exec(
		"UPDATE actions SET type = ?, name = ?, enabled = ?, config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		a.Type, a.Name, a.Enabled, string(a.ConfigJSON), a.ID,
	)
	return err
}

func (db *DB) DeleteAction(id int64) error {
	_, err := db.conn.Exec("DELETE FROM actions WHERE id = ?", id)
	return err
}

// --- WAN Interface Actions (links) ---

func (db *DB) LinkAction(wia *WANInterfaceAction) error {
	_, err := db.conn.Exec(
		`INSERT INTO wan_interface_actions (wan_interface_id, action_id, trigger_on, run_order, enabled)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(wan_interface_id, action_id) DO UPDATE SET trigger_on = ?, run_order = ?, enabled = ?`,
		wia.WANInterfaceID, wia.ActionID, wia.TriggerOn, wia.RunOrder, wia.Enabled,
		wia.TriggerOn, wia.RunOrder, wia.Enabled,
	)
	return err
}

func (db *DB) UnlinkAction(wanInterfaceID, actionID int64) error {
	_, err := db.conn.Exec(
		"DELETE FROM wan_interface_actions WHERE wan_interface_id = ? AND action_id = ?",
		wanInterfaceID, actionID,
	)
	return err
}

// ActionTrigger is one row in the Action-form Triggers repeater: a WAN
// interface that fires this action on a given event. Joined with the WAN
// name for form rendering.
type ActionTrigger struct {
	ActionID         int64
	WANInterfaceID   int64
	WANInterfaceName string
	TriggerOn        string // ip_changed | ip_unchanged_daily | both
	RunOrder         int
}

// GetActionTriggers lists the triggers declared for a single action.
func (db *DB) GetActionTriggers(actionID int64) ([]ActionTrigger, error) {
	rows, err := db.conn.Query(
		`SELECT wia.wan_interface_id, COALESCE(wi.name, '?') AS wan_name,
		        wia.trigger_on, wia.run_order
		 FROM wan_interface_actions wia
		 LEFT JOIN wan_interfaces wi ON wi.id = wia.wan_interface_id
		 WHERE wia.action_id = ?
		 ORDER BY wia.run_order, wia.wan_interface_id`,
		actionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionTrigger
	for rows.Next() {
		var t ActionTrigger
		if err := rows.Scan(&t.WANInterfaceID, &t.WANInterfaceName, &t.TriggerOn, &t.RunOrder); err != nil {
			return nil, err
		}
		t.ActionID = actionID
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetWANInterfaceLinks replaces the complete set of links owned by a single
// WAN interface in one transaction. Symmetric with SetActionTriggers: each
// side of the N:M relation can be fully replaced without touching rows
// scoped to the other side. Fixes #3 (non-transactional relink).
func (db *DB) SetWANInterfaceLinks(wanInterfaceID int64, links []WANInterfaceAction) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM wan_interface_actions WHERE wan_interface_id = ?", wanInterfaceID); err != nil {
		return fmt.Errorf("clearing WAN links: %w", err)
	}
	for _, l := range links {
		if l.ActionID == 0 {
			continue
		}
		trigger := l.TriggerOn
		switch trigger {
		case "ip_changed", "ip_unchanged_daily", "both":
		default:
			trigger = "ip_changed"
		}
		if _, err := tx.Exec(
			`INSERT INTO wan_interface_actions (wan_interface_id, action_id, trigger_on, run_order, enabled)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(wan_interface_id, action_id) DO UPDATE SET trigger_on = excluded.trigger_on, run_order = excluded.run_order, enabled = excluded.enabled`,
			wanInterfaceID, l.ActionID, trigger, l.RunOrder, l.Enabled,
		); err != nil {
			return fmt.Errorf("linking action %d: %w", l.ActionID, err)
		}
	}
	return tx.Commit()
}

// SetActionTriggers replaces the complete set of triggers for an action
// in a single transaction. Triggers not present in the passed slice are
// removed; matching rows are upserted with the new trigger_on / run_order.
// WAN-side Linked Actions for other actions are untouched.
func (db *DB) SetActionTriggers(actionID int64, triggers []ActionTrigger) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM wan_interface_actions WHERE action_id = ?", actionID); err != nil {
		return fmt.Errorf("clearing existing triggers: %w", err)
	}
	for _, t := range triggers {
		if t.WANInterfaceID == 0 {
			continue
		}
		trigger := t.TriggerOn
		switch trigger {
		case "ip_changed", "ip_unchanged_daily", "both":
		default:
			trigger = "ip_changed"
		}
		if _, err := tx.Exec(
			`INSERT INTO wan_interface_actions (wan_interface_id, action_id, trigger_on, run_order, enabled)
			 VALUES (?, ?, ?, ?, 1)
			 ON CONFLICT(wan_interface_id, action_id) DO UPDATE SET trigger_on = excluded.trigger_on, run_order = excluded.run_order, enabled = 1`,
			t.WANInterfaceID, actionID, trigger, t.RunOrder,
		); err != nil {
			return fmt.Errorf("inserting trigger for wan %d: %w", t.WANInterfaceID, err)
		}
	}
	return tx.Commit()
}

func (db *DB) GetLinkedActions(wanInterfaceID int64, triggerOn string) ([]WANInterfaceAction, error) {
	query := `SELECT wia.wan_interface_id, wia.action_id, wia.trigger_on, wia.run_order, wia.enabled,
	                 a.type, a.name, a.config
	          FROM wan_interface_actions wia
	          JOIN actions a ON a.id = wia.action_id
	          WHERE wia.wan_interface_id = ? AND wia.enabled = 1 AND a.enabled = 1`
	args := []interface{}{wanInterfaceID}

	if triggerOn != "" {
		query += " AND (wia.trigger_on = ? OR wia.trigger_on = 'both')"
		args = append(args, triggerOn)
	}
	query += " ORDER BY wia.run_order, wia.action_id"

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []WANInterfaceAction
	for rows.Next() {
		var wia WANInterfaceAction
		var configJSON string
		if err := rows.Scan(&wia.WANInterfaceID, &wia.ActionID, &wia.TriggerOn, &wia.RunOrder, &wia.Enabled,
			&wia.ActionType, &wia.ActionName, &configJSON); err != nil {
			return nil, err
		}
		wia.ActionConfigJSON = json.RawMessage(configJSON)
		links = append(links, wia)
	}
	return links, rows.Err()
}

// --- Action Runs (log) ---

func (db *DB) LogActionRun(run *ActionRun) error {
	_, err := db.conn.Exec(
		`INSERT INTO action_runs (wan_interface_id, action_id, event_type, old_ip, new_ip, status, needs_retry, error_msg)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.WANInterfaceID, run.ActionID, run.EventType,
		nullStr(run.OldIP), nullStr(run.NewIP),
		run.Status, run.NeedsRetry, nullStr(run.ErrorMsg),
	)
	return err
}

func (db *DB) GetRecentRuns(wanInterfaceID int64, limit int) ([]ActionRun, error) {
	query := `SELECT id, wan_interface_id, action_id, event_type, old_ip, new_ip, status, needs_retry, error_msg, executed_at
	          FROM action_runs`
	args := []interface{}{}

	if wanInterfaceID > 0 {
		query += " WHERE wan_interface_id = ?"
		args = append(args, wanInterfaceID)
	}
	query += " ORDER BY executed_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []ActionRun
	for rows.Next() {
		var r ActionRun
		var oldIP, newIP, errMsg sql.NullString
		if err := rows.Scan(&r.ID, &r.WANInterfaceID, &r.ActionID, &r.EventType,
			&oldIP, &newIP, &r.Status, &r.NeedsRetry, &errMsg, &r.ExecutedAt); err != nil {
			return nil, err
		}
		r.OldIP = oldIP.String
		r.NewIP = newIP.String
		r.ErrorMsg = errMsg.String
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetFailedRuns returns action runs that need retry for a WAN interface.
func (db *DB) GetFailedRuns(wanInterfaceID int64) ([]ActionRunWithType, error) {
	rows, err := db.conn.Query(
		`SELECT ar.id, ar.wan_interface_id, ar.action_id, ar.event_type, ar.old_ip, ar.new_ip, a.type
		 FROM action_runs ar JOIN actions a ON a.id = ar.action_id
		 WHERE ar.wan_interface_id = ? AND ar.needs_retry = 1
		 ORDER BY ar.executed_at ASC LIMIT 10`,
		wanInterfaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []ActionRunWithType
	for rows.Next() {
		var r ActionRunWithType
		var oldIP, newIP sql.NullString
		if err := rows.Scan(&r.ID, &r.WANInterfaceID, &r.ActionID, &r.EventType, &oldIP, &newIP, &r.ActionType); err != nil {
			return nil, err
		}
		r.OldIP = oldIP.String
		r.NewIP = newIP.String
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// ClearRetryFlag marks an action run as no longer needing retry.
func (db *DB) ClearRetryFlag(runID int64) error {
	_, err := db.conn.Exec("UPDATE action_runs SET needs_retry = 0, status = 'success' WHERE id = ?", runID)
	return err
}

// PruneActionRuns deletes old action run entries beyond the retention limit.
// Kept for backward compatibility; new code should use PruneHistoryByTTL.
func (db *DB) PruneActionRuns(maxRows int) error {
	_, err := db.conn.Exec(
		`DELETE FROM action_runs WHERE id NOT IN (
			SELECT id FROM action_runs ORDER BY executed_at DESC LIMIT ?
		)`, maxRows,
	)
	return err
}

// HistoryRetention is the per-outcome TTL configuration for the History
// auto-recycler (#29). Zero / negative means "keep forever". Each value is
// in seconds.
type HistoryRetention struct {
	CheckOKSeconds       int // unchanged checks — noisy, short TTL
	CheckChangedSeconds  int // IP changed — diagnostic, usually lifetime
	CheckFailedSeconds   int // probe/PBR failure — medium retention
	ActionSuccessSeconds int // successful action run
	ActionFailedSeconds  int // failed action run — usually lifetime
}

// DefaultHistoryRetention returns the conservative defaults: short TTL for
// the noisy unchanged-check bucket, lifetime for the diagnostic buckets.
func DefaultHistoryRetention() HistoryRetention {
	return HistoryRetention{
		CheckOKSeconds:       3600,     // 1 hour — unchanged-check noise
		CheckChangedSeconds:  0,        // lifetime — IP-change timeline is the audit trail
		CheckFailedSeconds:   30 * 86400, // 30 days
		ActionSuccessSeconds: 30 * 86400, // 30 days
		ActionFailedSeconds:  0,        // lifetime — failures must be investigable
	}
}

var historyRetentionKeys = []struct {
	Key string
	Get func(*HistoryRetention) *int
}{
	{"history.check.ok_ttl_seconds", func(r *HistoryRetention) *int { return &r.CheckOKSeconds }},
	{"history.check.changed_ttl_seconds", func(r *HistoryRetention) *int { return &r.CheckChangedSeconds }},
	{"history.check.failed_ttl_seconds", func(r *HistoryRetention) *int { return &r.CheckFailedSeconds }},
	{"history.action.success_ttl_seconds", func(r *HistoryRetention) *int { return &r.ActionSuccessSeconds }},
	{"history.action.failed_ttl_seconds", func(r *HistoryRetention) *int { return &r.ActionFailedSeconds }},
}

// LoadHistoryRetention reads the five retention settings, falling back to
// the defaults for any key that is missing / blank / unparseable.
func (db *DB) LoadHistoryRetention() HistoryRetention {
	r := DefaultHistoryRetention()
	for _, entry := range historyRetentionKeys {
		v, err := db.GetSetting(entry.Key)
		if err != nil || v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		*entry.Get(&r) = n
	}
	return r
}

// SaveHistoryRetention writes the five retention settings. Values ≤ 0 are
// stored as "0" (lifetime).
func (db *DB) SaveHistoryRetention(r HistoryRetention) error {
	for _, entry := range historyRetentionKeys {
		v := *entry.Get(&r)
		if v < 0 {
			v = 0
		}
		if err := db.SetSetting(entry.Key, fmt.Sprintf("%d", v)); err != nil {
			return err
		}
	}
	return nil
}

// HistorySweepStats records how many rows each bucket pruned in a single
// sweep. Used for the log line so operators can see retention at work.
type HistorySweepStats struct {
	CheckOK       int64
	CheckChanged  int64
	CheckFailed   int64
	ActionSuccess int64
	ActionFailed  int64
}

// PruneHistoryByTTL deletes rows older than the per-outcome TTL. Each
// bucket with TTL <= 0 is skipped (kept forever). Returns per-bucket
// delete counts.
func (db *DB) PruneHistoryByTTL(r HistoryRetention) (HistorySweepStats, error) {
	var stats HistorySweepStats
	prune := func(query string, secs int) (int64, error) {
		if secs <= 0 {
			return 0, nil
		}
		cutoff := time.Now().Add(-time.Duration(secs) * time.Second).UTC()
		res, err := db.conn.Exec(query, cutoff)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}

	n, err := prune(`DELETE FROM wan_interface_checks WHERE status = 'ok' AND executed_at < ?`, r.CheckOKSeconds)
	if err != nil {
		return stats, fmt.Errorf("prune check.ok: %w", err)
	}
	stats.CheckOK = n

	n, err = prune(`DELETE FROM wan_interface_checks WHERE status = 'changed' AND executed_at < ?`, r.CheckChangedSeconds)
	if err != nil {
		return stats, fmt.Errorf("prune check.changed: %w", err)
	}
	stats.CheckChanged = n

	n, err = prune(`DELETE FROM wan_interface_checks WHERE status = 'failed' AND executed_at < ?`, r.CheckFailedSeconds)
	if err != nil {
		return stats, fmt.Errorf("prune check.failed: %w", err)
	}
	stats.CheckFailed = n

	n, err = prune(`DELETE FROM action_runs WHERE status = 'success' AND executed_at < ?`, r.ActionSuccessSeconds)
	if err != nil {
		return stats, fmt.Errorf("prune action.success: %w", err)
	}
	stats.ActionSuccess = n

	n, err = prune(`DELETE FROM action_runs WHERE status = 'failed' AND executed_at < ?`, r.ActionFailedSeconds)
	if err != nil {
		return stats, fmt.Errorf("prune action.failed: %w", err)
	}
	stats.ActionFailed = n

	return stats, nil
}

// --- UI Queries (joined, filtered, paginated) ---

// HistoryFilter defines filter/pagination parameters for history queries.
type HistoryFilter struct {
	WANInterfaceID int64
	EventType      string
	Status         string
	Limit          int
	Offset         int
}

// ActionRunJoined extends ActionRun with WAN interface and action names.
type ActionRunJoined struct {
	ActionRun
	WANInterfaceName string
	ActionName       string
}

// GetRecentRunsJoined returns filtered, paginated action runs with joined names.
func (db *DB) GetRecentRunsJoined(filter HistoryFilter) ([]ActionRunJoined, int, error) {
	where := "1=1"
	args := []interface{}{}

	if filter.WANInterfaceID > 0 {
		where += " AND ar.wan_interface_id = ?"
		args = append(args, filter.WANInterfaceID)
	}
	if filter.EventType != "" {
		where += " AND ar.event_type = ?"
		args = append(args, filter.EventType)
	}
	if filter.Status != "" {
		where += " AND ar.status = ?"
		args = append(args, filter.Status)
	}

	// Count total matching rows
	var total int
	countQuery := "SELECT COUNT(*) FROM action_runs ar WHERE " + where
	if err := db.conn.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting runs: %w", err)
	}

	// Fetch page
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	query := fmt.Sprintf(`
		SELECT ar.id, ar.wan_interface_id, ar.action_id, ar.event_type, ar.old_ip, ar.new_ip,
		       ar.status, ar.needs_retry, ar.error_msg, ar.executed_at,
		       COALESCE(wi.name, '?') AS wan_interface_name,
		       COALESCE(a.name, '?') AS action_name
		FROM action_runs ar
		LEFT JOIN wan_interfaces wi ON wi.id = ar.wan_interface_id
		LEFT JOIN actions a ON a.id = ar.action_id
		WHERE %s
		ORDER BY ar.executed_at DESC
		LIMIT ? OFFSET ?`, where)

	pageArgs := append(args, limit, offset)
	rows, err := db.conn.Query(query, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying runs: %w", err)
	}
	defer rows.Close()

	var runs []ActionRunJoined
	for rows.Next() {
		var r ActionRunJoined
		var oldIP, newIP, errMsg sql.NullString
		if err := rows.Scan(&r.ID, &r.WANInterfaceID, &r.ActionID, &r.EventType,
			&oldIP, &newIP, &r.Status, &r.NeedsRetry, &errMsg, &r.ExecutedAt,
			&r.WANInterfaceName, &r.ActionName); err != nil {
			return nil, 0, fmt.Errorf("scanning run: %w", err)
		}
		r.OldIP = oldIP.String
		r.NewIP = newIP.String
		r.ErrorMsg = errMsg.String
		runs = append(runs, r)
	}
	return runs, total, rows.Err()
}

// HistoryEntry is the merged view of a check or an action run, used by the
// History page to present a single chronological timeline.
type HistoryEntry struct {
	Kind             string // "check" or "action"
	ID               int64
	WANInterfaceID   int64
	WANInterfaceName string
	Event            string // check trigger ("manual"/"scheduled") or action event_type
	ActionName       string // empty for checks
	OldIP            string
	NewIP            string
	Status           string
	ErrorMsg         string
	ExecutedAt       time.Time
}

// GetMergedHistory returns the chronologically merged union of check rows
// and action-run rows matching the filter. Filters on WAN interface and
// status apply to both sources; event filter applies to action_runs only
// (checks have no event type, they are always the full probe outcome).
func (db *DB) GetMergedHistory(filter HistoryFilter) ([]HistoryEntry, int, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	// Build WHERE clauses independently for each source so we can UNION.
	checksWhere := "1=1"
	actionsWhere := "1=1"
	checksArgs := []interface{}{}
	actionsArgs := []interface{}{}

	if filter.WANInterfaceID > 0 {
		checksWhere += " AND wc.wan_interface_id = ?"
		actionsWhere += " AND ar.wan_interface_id = ?"
		checksArgs = append(checksArgs, filter.WANInterfaceID)
		actionsArgs = append(actionsArgs, filter.WANInterfaceID)
	}
	if filter.Status != "" {
		// Map UI status "success" to concrete values per source.
		if filter.Status == "success" {
			checksWhere += " AND wc.status IN ('ok','changed')"
			actionsWhere += " AND ar.status = 'success'"
		} else {
			checksWhere += " AND wc.status = ?"
			actionsWhere += " AND ar.status = ?"
			checksArgs = append(checksArgs, filter.Status)
			actionsArgs = append(actionsArgs, filter.Status)
		}
	}
	if filter.EventType != "" {
		// Event filter only applies to action_runs — checks never match.
		actionsWhere += " AND ar.event_type = ?"
		actionsArgs = append(actionsArgs, filter.EventType)
		// Exclude checks entirely when filtering by event_type.
		checksWhere += " AND 0 = 1"
	}

	// Count total (union).
	countQuery := fmt.Sprintf(`
		SELECT
		  (SELECT COUNT(*) FROM wan_interface_checks wc WHERE %s)
		+ (SELECT COUNT(*) FROM action_runs ar WHERE %s)`,
		checksWhere, actionsWhere)
	var total int
	countArgs := append(append([]interface{}{}, checksArgs...), actionsArgs...)
	if err := db.conn.QueryRow(countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting history: %w", err)
	}

	query := fmt.Sprintf(`
		SELECT kind, id, wan_interface_id, wan_interface_name, event, action_name,
		       old_ip, new_ip, status, error_msg, executed_at
		FROM (
		  SELECT 'check' AS kind, wc.id AS id, wc.wan_interface_id,
		         COALESCE(wi.name, '?') AS wan_interface_name,
		         wc.trigger AS event,
		         '' AS action_name,
		         wc.previous_ip AS old_ip,
		         wc.observed_ip AS new_ip,
		         wc.status AS status,
		         wc.error_msg AS error_msg,
		         wc.executed_at AS executed_at
		  FROM wan_interface_checks wc
		  LEFT JOIN wan_interfaces wi ON wi.id = wc.wan_interface_id
		  WHERE %s
		  UNION ALL
		  SELECT 'action' AS kind, ar.id AS id, ar.wan_interface_id,
		         COALESCE(wi.name, '?') AS wan_interface_name,
		         ar.event_type AS event,
		         COALESCE(a.name, '?') AS action_name,
		         ar.old_ip, ar.new_ip,
		         ar.status,
		         ar.error_msg,
		         ar.executed_at
		  FROM action_runs ar
		  LEFT JOIN wan_interfaces wi ON wi.id = ar.wan_interface_id
		  LEFT JOIN actions a ON a.id = ar.action_id
		  WHERE %s
		)
		ORDER BY executed_at DESC
		LIMIT ? OFFSET ?`, checksWhere, actionsWhere)
	pageArgs := append(append([]interface{}{}, checksArgs...), actionsArgs...)
	pageArgs = append(pageArgs, limit, offset)
	rows, err := db.conn.Query(query, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying history: %w", err)
	}
	defer rows.Close()

	var entries []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var oldIP, newIP, errMsg sql.NullString
		if err := rows.Scan(&e.Kind, &e.ID, &e.WANInterfaceID, &e.WANInterfaceName,
			&e.Event, &e.ActionName, &oldIP, &newIP, &e.Status, &errMsg, &e.ExecutedAt); err != nil {
			return nil, 0, fmt.Errorf("scanning history: %w", err)
		}
		e.OldIP = oldIP.String
		e.NewIP = newIP.String
		e.ErrorMsg = errMsg.String
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// GetWANInterfaceLinkedActionCount returns the count of enabled actions linked to a WAN interface.
func (db *DB) GetWANInterfaceLinkedActionCount(wanInterfaceID int64) (int, error) {
	var count int
	err := db.conn.QueryRow(
		"SELECT COUNT(*) FROM wan_interface_actions WHERE wan_interface_id = ? AND enabled = 1",
		wanInterfaceID,
	).Scan(&count)
	return count, err
}

// GetActionLinkedWANInterfaceCount returns the count of WAN interfaces linked to an action.
func (db *DB) GetActionLinkedWANInterfaceCount(actionID int64) (int, error) {
	var count int
	err := db.conn.QueryRow(
		"SELECT COUNT(*) FROM wan_interface_actions WHERE action_id = ? AND enabled = 1",
		actionID,
	).Scan(&count)
	return count, err
}

// LinkedWANInterface captures the subset of WAN interface state Execute
// needs to fire an action against each link.
type LinkedWANInterface struct {
	WANInterfaceID int64
	Name           string
	LastObservedIP string
}

// GetActionLinkedWANInterfacesWithState returns the enabled WAN interfaces
// linked to an action, including each WAN's last observed IP. Used by the
// Actions-list Execute button to fan out against current live state.
func (db *DB) GetActionLinkedWANInterfacesWithState(actionID int64) ([]LinkedWANInterface, error) {
	rows, err := db.conn.Query(
		`SELECT wi.id, wi.name, COALESCE(s.last_observed_ip, '')
		 FROM wan_interface_actions wia
		 JOIN wan_interfaces wi ON wi.id = wia.wan_interface_id
		 LEFT JOIN wan_interface_state s ON s.wan_interface_id = wi.id
		 WHERE wia.action_id = ? AND wia.enabled = 1 AND wi.enabled = 1
		 ORDER BY wi.name`,
		actionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LinkedWANInterface
	for rows.Next() {
		var w LinkedWANInterface
		if err := rows.Scan(&w.WANInterfaceID, &w.Name, &w.LastObservedIP); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetActionLinkedWANInterfaces returns the names of WAN interfaces linked to an action.
func (db *DB) GetActionLinkedWANInterfaces(actionID int64) ([]string, error) {
	rows, err := db.conn.Query(
		`SELECT wi.name FROM wan_interface_actions wia
		 JOIN wan_interfaces wi ON wi.id = wia.wan_interface_id
		 WHERE wia.action_id = ? AND wia.enabled = 1
		 ORDER BY wi.name`,
		actionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// --- Helpers ---

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.DateTime), Valid: true}
}
