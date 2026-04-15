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
	_, err := db.conn.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sources (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    schedule   TEXT NOT NULL DEFAULT '*/5 * * * *',
    timezone   TEXT NOT NULL DEFAULT 'Local',
    config     TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS source_state (
    source_id            INTEGER PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
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

CREATE TABLE IF NOT EXISTS source_actions (
    source_id  INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    action_id  INTEGER NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    trigger_on TEXT NOT NULL DEFAULT 'ip_changed',
    run_order  INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (source_id, action_id)
);

CREATE TABLE IF NOT EXISTS action_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id   INTEGER NOT NULL REFERENCES sources(id),
    action_id   INTEGER NOT NULL REFERENCES actions(id),
    event_type  TEXT NOT NULL,
    old_ip      TEXT,
    new_ip      TEXT,
    status      TEXT NOT NULL,
    needs_retry INTEGER NOT NULL DEFAULT 0,
    error_msg   TEXT,
    executed_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_action_runs_source ON action_runs(source_id, executed_at DESC);
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

// --- Sources ---

func (db *DB) CreateSource(s *Source) (int64, error) {
	configJSON, err := json.Marshal(s.Config)
	if err != nil {
		return 0, fmt.Errorf("marshaling source config: %w", err)
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		"INSERT INTO sources (name, enabled, schedule, timezone, config) VALUES (?, ?, ?, ?, ?)",
		s.Name, s.Enabled, s.Schedule, s.Timezone, string(configJSON),
	)
	if err != nil {
		return 0, err
	}
	id, _ := result.LastInsertId()

	if _, err := tx.Exec("INSERT INTO source_state (source_id) VALUES (?)", id); err != nil {
		return 0, fmt.Errorf("creating source state: %w", err)
	}

	return id, tx.Commit()
}

func (db *DB) GetSource(id int64) (*Source, error) {
	s := &Source{}
	var configJSON string
	err := db.conn.QueryRow(
		"SELECT id, name, enabled, schedule, timezone, config, created_at, updated_at FROM sources WHERE id = ?", id,
	).Scan(&s.ID, &s.Name, &s.Enabled, &s.Schedule, &s.Timezone, &configJSON, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(configJSON), &s.Config); err != nil {
		return nil, fmt.Errorf("parsing source config: %w", err)
	}
	return s, nil
}

func (db *DB) ListSources() ([]Source, error) {
	rows, err := db.conn.Query(
		"SELECT id, name, enabled, schedule, timezone, config, created_at, updated_at FROM sources ORDER BY id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var s Source
		var configJSON string
		if err := rows.Scan(&s.ID, &s.Name, &s.Enabled, &s.Schedule, &s.Timezone, &configJSON, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(configJSON), &s.Config); err != nil {
			db.logger.Warn("invalid source config JSON", "source_id", s.ID, "error", err)
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}

func (db *DB) UpdateSource(s *Source) error {
	configJSON, err := json.Marshal(s.Config)
	if err != nil {
		return fmt.Errorf("marshaling source config: %w", err)
	}
	_, err = db.conn.Exec(
		"UPDATE sources SET name = ?, enabled = ?, schedule = ?, timezone = ?, config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		s.Name, s.Enabled, s.Schedule, s.Timezone, string(configJSON), s.ID,
	)
	return err
}

func (db *DB) DeleteSource(id int64) error {
	_, err := db.conn.Exec("DELETE FROM sources WHERE id = ?", id)
	return err
}

// --- Source State ---

func (db *DB) GetSourceState(sourceID int64) (*SourceState, error) {
	ss := &SourceState{SourceID: sourceID}
	var lastIP, lastCheck, lastSuccess, lastStatus, nextRun, summaryDate, lastErr sql.NullString
	err := db.conn.QueryRow(
		`SELECT last_observed_ip, last_check_at, last_success_at, last_status,
		        consecutive_failures, next_run_at, last_summary_date, last_error
		 FROM source_state WHERE source_id = ?`, sourceID,
	).Scan(&lastIP, &lastCheck, &lastSuccess, &lastStatus,
		&ss.ConsecutiveFailures, &nextRun, &summaryDate, &lastErr)
	if err != nil {
		return nil, err
	}
	ss.LastObservedIP = lastIP.String
	ss.LastStatus = lastStatus.String
	ss.LastSummaryDate = summaryDate.String
	ss.LastError = lastErr.String
	if lastCheck.Valid {
		ss.LastCheckAt, _ = time.Parse(time.DateTime, lastCheck.String)
	}
	if lastSuccess.Valid {
		ss.LastSuccessAt, _ = time.Parse(time.DateTime, lastSuccess.String)
	}
	if nextRun.Valid {
		ss.NextRunAt, _ = time.Parse(time.DateTime, nextRun.String)
	}
	return ss, nil
}

func (db *DB) UpdateSourceState(ss *SourceState) error {
	_, err := db.conn.Exec(
		`UPDATE source_state SET
		    last_observed_ip = ?, last_check_at = ?, last_success_at = ?,
		    last_status = ?, consecutive_failures = ?, next_run_at = ?,
		    last_summary_date = ?, last_error = ?
		 WHERE source_id = ?`,
		nullStr(ss.LastObservedIP),
		nullTime(ss.LastCheckAt),
		nullTime(ss.LastSuccessAt),
		nullStr(ss.LastStatus),
		ss.ConsecutiveFailures,
		nullTime(ss.NextRunAt),
		nullStr(ss.LastSummaryDate),
		nullStr(ss.LastError),
		ss.SourceID,
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

// --- Source Actions (links) ---

func (db *DB) LinkAction(sa *SourceAction) error {
	_, err := db.conn.Exec(
		`INSERT INTO source_actions (source_id, action_id, trigger_on, run_order, enabled)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(source_id, action_id) DO UPDATE SET trigger_on = ?, run_order = ?, enabled = ?`,
		sa.SourceID, sa.ActionID, sa.TriggerOn, sa.RunOrder, sa.Enabled,
		sa.TriggerOn, sa.RunOrder, sa.Enabled,
	)
	return err
}

func (db *DB) UnlinkAction(sourceID, actionID int64) error {
	_, err := db.conn.Exec(
		"DELETE FROM source_actions WHERE source_id = ? AND action_id = ?",
		sourceID, actionID,
	)
	return err
}

func (db *DB) GetLinkedActions(sourceID int64, triggerOn string) ([]SourceAction, error) {
	query := `SELECT sa.source_id, sa.action_id, sa.trigger_on, sa.run_order, sa.enabled,
	                 a.type, a.name, a.config
	          FROM source_actions sa
	          JOIN actions a ON a.id = sa.action_id
	          WHERE sa.source_id = ? AND sa.enabled = 1 AND a.enabled = 1`
	args := []interface{}{sourceID}

	if triggerOn != "" {
		query += " AND (sa.trigger_on = ? OR sa.trigger_on = 'both')"
		args = append(args, triggerOn)
	}
	query += " ORDER BY sa.run_order, sa.action_id"

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []SourceAction
	for rows.Next() {
		var sa SourceAction
		var configJSON string
		if err := rows.Scan(&sa.SourceID, &sa.ActionID, &sa.TriggerOn, &sa.RunOrder, &sa.Enabled,
			&sa.ActionType, &sa.ActionName, &configJSON); err != nil {
			return nil, err
		}
		sa.ActionConfigJSON = json.RawMessage(configJSON)
		links = append(links, sa)
	}
	return links, rows.Err()
}

// --- Action Runs (log) ---

func (db *DB) LogActionRun(run *ActionRun) error {
	_, err := db.conn.Exec(
		`INSERT INTO action_runs (source_id, action_id, event_type, old_ip, new_ip, status, needs_retry, error_msg)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.SourceID, run.ActionID, run.EventType,
		nullStr(run.OldIP), nullStr(run.NewIP),
		run.Status, run.NeedsRetry, nullStr(run.ErrorMsg),
	)
	return err
}

func (db *DB) GetRecentRuns(sourceID int64, limit int) ([]ActionRun, error) {
	query := `SELECT id, source_id, action_id, event_type, old_ip, new_ip, status, needs_retry, error_msg, executed_at
	          FROM action_runs`
	args := []interface{}{}

	if sourceID > 0 {
		query += " WHERE source_id = ?"
		args = append(args, sourceID)
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
		if err := rows.Scan(&r.ID, &r.SourceID, &r.ActionID, &r.EventType,
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

// GetFailedRuns returns action runs that need retry for a source.
func (db *DB) GetFailedRuns(sourceID int64) ([]ActionRunWithType, error) {
	rows, err := db.conn.Query(
		`SELECT ar.id, ar.source_id, ar.action_id, ar.event_type, ar.old_ip, ar.new_ip, a.type
		 FROM action_runs ar JOIN actions a ON a.id = ar.action_id
		 WHERE ar.source_id = ? AND ar.needs_retry = 1
		 ORDER BY ar.executed_at ASC LIMIT 10`,
		sourceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []ActionRunWithType
	for rows.Next() {
		var r ActionRunWithType
		var oldIP, newIP sql.NullString
		if err := rows.Scan(&r.ID, &r.SourceID, &r.ActionID, &r.EventType, &oldIP, &newIP, &r.ActionType); err != nil {
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
