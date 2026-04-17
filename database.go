package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
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
`

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
		ws.LastCheckAt, _ = time.Parse(time.DateTime, lastCheck.String)
	}
	if lastSuccess.Valid {
		ws.LastSuccessAt, _ = time.Parse(time.DateTime, lastSuccess.String)
	}
	if nextRun.Valid {
		ws.NextRunAt, _ = time.Parse(time.DateTime, nextRun.String)
	}
	return ws, nil
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
func (db *DB) PruneActionRuns(maxRows int) error {
	_, err := db.conn.Exec(
		`DELETE FROM action_runs WHERE id NOT IN (
			SELECT id FROM action_runs ORDER BY executed_at DESC LIMIT ?
		)`, maxRows,
	)
	return err
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
