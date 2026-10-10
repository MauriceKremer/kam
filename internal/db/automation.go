package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const DefaultAutomationTimeout = 500 * time.Millisecond

type AutomationScript struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Source        string `json:"source"`
	Enabled       bool   `json:"enabled"`
	TimeoutMS     int64  `json:"timeout_ms"`
	RuntimeStatus string `json:"runtime_status"`
	LastError     string `json:"last_error,omitempty"`
	Revision      int64  `json:"revision"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type AutomationRepository struct {
	db *sql.DB
}

var ErrAutomationNotFound = errors.New("automation script not found")
var ErrAutomationConflict = errors.New("automation script revision conflict")
var ErrAutomationNameTaken = errors.New("automation script name already in use")

func OpenAutomationRepository(path string) (*AutomationRepository, error) {
	db, err := OpenKamDB(path)
	if err != nil {
		return nil, err
	}
	// PRAGMA busy_timeout is per connection; a single connection guarantees it always applies.
	db.SetMaxOpenConns(1)
	if err := EnsureAutomationScriptsSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &AutomationRepository{db: db}, nil
}

func EnsureAutomationScriptsSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS automation_scripts (
  script_id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  source TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  timeout_ms INTEGER NOT NULL DEFAULT 500 CHECK (timeout_ms > 0),
  runtime_status TEXT NOT NULL DEFAULT 'READY' CHECK (runtime_status IN ('READY', 'BROKEN', 'FROZEN')),
  last_error TEXT,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL DEFAULT (unixepoch()),
  updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure automation schema: %w", err)
	}
	return nil
}

func (r *AutomationRepository) Close() error { return r.db.Close() }

func (r *AutomationRepository) List(ctx context.Context) ([]AutomationScript, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT script_id, name, source, enabled, timeout_ms, runtime_status, COALESCE(last_error, ''), revision, created_at, updated_at FROM automation_scripts ORDER BY script_id`)
	if err != nil {
		return nil, fmt.Errorf("list automation scripts: %w", err)
	}
	defer rows.Close()
	scripts := []AutomationScript{}
	for rows.Next() {
		var script AutomationScript
		var enabled int
		if err := rows.Scan(&script.ID, &script.Name, &script.Source, &enabled, &script.TimeoutMS, &script.RuntimeStatus, &script.LastError, &script.Revision, &script.CreatedAt, &script.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan automation script: %w", err)
		}
		script.Enabled = enabled != 0
		scripts = append(scripts, script)
	}
	return scripts, rows.Err()
}

func (r *AutomationRepository) Get(ctx context.Context, id string) (AutomationScript, error) {
	var script AutomationScript
	var enabled int
	err := r.db.QueryRowContext(ctx, `SELECT script_id, name, source, enabled, timeout_ms, runtime_status, COALESCE(last_error, ''), revision, created_at, updated_at FROM automation_scripts WHERE script_id = ?`, id).Scan(
		&script.ID, &script.Name, &script.Source, &enabled, &script.TimeoutMS, &script.RuntimeStatus, &script.LastError, &script.Revision, &script.CreatedAt, &script.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationScript{}, ErrAutomationNotFound
	}
	if err != nil {
		return AutomationScript{}, fmt.Errorf("get automation script: %w", err)
	}
	script.Enabled = enabled != 0
	return script, nil
}

func (r *AutomationRepository) Save(ctx context.Context, script AutomationScript, expectedRevision int64) (AutomationScript, error) {
	if strings.TrimSpace(script.ID) == "" || strings.TrimSpace(script.Name) == "" {
		return AutomationScript{}, errors.New("script id and name are required")
	}
	if script.TimeoutMS <= 0 {
		return AutomationScript{}, errors.New("timeout_ms must be positive")
	}
	if script.RuntimeStatus == "" {
		script.RuntimeStatus = "READY"
	}
	if script.RuntimeStatus != "READY" && script.RuntimeStatus != "BROKEN" && script.RuntimeStatus != "FROZEN" {
		return AutomationScript{}, errors.New("invalid runtime status")
	}
	if script.RuntimeStatus == "READY" {
		script.LastError = ""
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AutomationScript{}, fmt.Errorf("begin automation update: %w", err)
	}
	defer tx.Rollback()
	// Write first: a read-then-write transaction fails immediately with SQLITE_BUSY_SNAPSHOT
	// when the persistence connection commits in between.
	if expectedRevision == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_scripts (script_id, name, source, enabled, timeout_ms, runtime_status, last_error) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''))`, script.ID, script.Name, script.Source, boolToInt(script.Enabled), script.TimeoutMS, script.RuntimeStatus, script.LastError)
		if isConstraintError(err) {
			if exists, existsErr := automationExists(ctx, tx, script.ID); existsErr == nil && exists {
				return AutomationScript{}, ErrAutomationConflict
			}
			return AutomationScript{}, ErrAutomationNameTaken
		}
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE automation_scripts SET name = ?, source = ?, enabled = ?, timeout_ms = ?, runtime_status = ?, last_error = NULLIF(?, ''), revision = revision + 1, updated_at = unixepoch() WHERE script_id = ? AND revision = ?`, script.Name, script.Source, boolToInt(script.Enabled), script.TimeoutMS, script.RuntimeStatus, script.LastError, script.ID, expectedRevision)
		if isConstraintError(err) {
			return AutomationScript{}, ErrAutomationNameTaken
		}
		if err == nil {
			if count, _ := result.RowsAffected(); count == 0 {
				return AutomationScript{}, missingOrConflict(ctx, tx, script.ID)
			}
		}
	}
	if err != nil {
		return AutomationScript{}, fmt.Errorf("write automation script: %w", err)
	}
	saved, err := scanAutomationScript(tx.QueryRowContext(ctx, `SELECT script_id, name, source, enabled, timeout_ms, runtime_status, COALESCE(last_error, ''), revision, created_at, updated_at FROM automation_scripts WHERE script_id = ?`, script.ID))
	if err != nil {
		return AutomationScript{}, fmt.Errorf("read saved automation script: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AutomationScript{}, fmt.Errorf("commit automation script: %w", err)
	}
	return saved, nil
}

func (r *AutomationRepository) SetStatus(ctx context.Context, id, status, detail string) error {
	if status != "READY" && status != "BROKEN" && status != "FROZEN" {
		return errors.New("invalid runtime status")
	}
	if status == "READY" {
		detail = ""
	}
	result, err := r.db.ExecContext(ctx, `UPDATE automation_scripts SET runtime_status = ?, last_error = NULLIF(?, ''), updated_at = unixepoch() WHERE script_id = ?`, status, detail, id)
	if err != nil {
		return fmt.Errorf("set automation status: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrAutomationNotFound
	}
	return nil
}

func (r *AutomationRepository) Delete(ctx context.Context, id string, expectedRevision int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM automation_scripts WHERE script_id = ? AND revision = ?`, id, expectedRevision)
	if err != nil {
		return fmt.Errorf("delete automation script: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return missingOrConflict(ctx, r.db, id)
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func automationExists(ctx context.Context, q rowQuerier, id string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM automation_scripts WHERE script_id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// missingOrConflict explains why a revision-guarded write matched no row.
func missingOrConflict(ctx context.Context, q rowQuerier, id string) error {
	exists, err := automationExists(ctx, q, id)
	if err != nil {
		return fmt.Errorf("check automation script: %w", err)
	}
	if !exists {
		return ErrAutomationNotFound
	}
	return ErrAutomationConflict
}

func isConstraintError(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}

type automationScanner interface{ Scan(...any) error }

func scanAutomationScript(row automationScanner) (AutomationScript, error) {
	var script AutomationScript
	var enabled int
	err := row.Scan(&script.ID, &script.Name, &script.Source, &enabled, &script.TimeoutMS, &script.RuntimeStatus, &script.LastError, &script.Revision, &script.CreatedAt, &script.UpdatedAt)
	script.Enabled = enabled != 0
	return script, err
}
