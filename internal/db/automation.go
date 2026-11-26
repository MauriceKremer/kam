package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
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

// AutomationScriptVersion is one superseded script revision kept in the
// automation_script_journal. The live row lives in automation_scripts; the
// journal holds every earlier revision, plus the final revision after a
// delete.
type AutomationScriptVersion struct {
	AutomationScript
	// Fingerprint identifies the source content; revisions with identical
	// source share it.
	Fingerprint string `json:"fingerprint"`
	ArchivedAt  int64  `json:"archived_at"`
}

// sourceFingerprint is the version's public id: the first 8 bytes of the
// source's SHA-256, hex encoded.
func sourceFingerprint(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:8])
}

// init exposes sourceFingerprint to SQLite so the archive write can compute
// the column in one statement. It runs before any connection is opened, which
// is when the driver makes a registered function available.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("kam_source_fingerprint", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var source string
		switch value := args[0].(type) {
		case string:
			source = value
		case []byte:
			source = string(value)
		default:
			return nil, fmt.Errorf("kam_source_fingerprint: source is %T, not text", args[0])
		}
		return sourceFingerprint(source), nil
	})
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
	);
	CREATE TABLE IF NOT EXISTS automation_script_journal (
	  journal_id INTEGER PRIMARY KEY AUTOINCREMENT,
	  script_id TEXT NOT NULL,
	  name TEXT NOT NULL,
	  source TEXT NOT NULL,
	  fingerprint TEXT NOT NULL,
	  enabled INTEGER NOT NULL,
	  timeout_ms INTEGER NOT NULL,
	  runtime_status TEXT NOT NULL,
	  last_error TEXT,
	  revision INTEGER NOT NULL,
	  created_at INTEGER NOT NULL,
	  updated_at INTEGER NOT NULL,
	  archived_at INTEGER NOT NULL DEFAULT (unixepoch()),
	  UNIQUE (script_id, revision)
	);`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure automation schema: %w", err)
	}
	return nil
}

// archiveAutomationVersionSQL copies the addressed revision into the journal.
// It is an INSERT ... SELECT so the caller's transaction still starts with a
// write, which avoids SQLITE_BUSY_SNAPSHOT when the persistence connection
// commits between a read and a write. The fingerprint is computed in the same
// statement from the stored source.
const archiveAutomationVersionSQL = `
INSERT OR IGNORE INTO automation_script_journal
  (script_id, name, source, fingerprint, enabled, timeout_ms, runtime_status, last_error, revision, created_at, updated_at)
SELECT script_id, name, source, kam_source_fingerprint(source), enabled, timeout_ms, runtime_status, last_error, revision, created_at, updated_at
FROM automation_scripts
WHERE script_id = ? AND revision = ?`

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

// Versions returns the script's superseded revisions, newest first. A script
// that has never been updated or deleted has no journal entries.
func (r *AutomationRepository) Versions(ctx context.Context, id string) ([]AutomationScriptVersion, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT script_id, name, source, fingerprint, enabled, timeout_ms, runtime_status, COALESCE(last_error, ''), revision, created_at, updated_at, archived_at FROM automation_script_journal WHERE script_id = ? ORDER BY revision DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("list automation script versions: %w", err)
	}
	defer rows.Close()
	versions := []AutomationScriptVersion{}
	for rows.Next() {
		var version AutomationScriptVersion
		var enabled int
		if err := rows.Scan(&version.ID, &version.Name, &version.Source, &version.Fingerprint, &enabled, &version.TimeoutMS, &version.RuntimeStatus, &version.LastError, &version.Revision, &version.CreatedAt, &version.UpdatedAt, &version.ArchivedAt); err != nil {
			return nil, fmt.Errorf("scan automation script version: %w", err)
		}
		version.Enabled = enabled != 0
		versions = append(versions, version)
	}
	return versions, rows.Err()
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
		// Journal the version being replaced before overwriting it.
		if _, err = tx.ExecContext(ctx, archiveAutomationVersionSQL, script.ID, expectedRevision); err != nil {
			return AutomationScript{}, fmt.Errorf("journal automation script: %w", err)
		}
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
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin automation delete: %w", err)
	}
	defer tx.Rollback()
	// Journal the final revision before removing it.
	if _, err := tx.ExecContext(ctx, archiveAutomationVersionSQL, id, expectedRevision); err != nil {
		return fmt.Errorf("journal automation script: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM automation_scripts WHERE script_id = ? AND revision = ?`, id, expectedRevision)
	if err != nil {
		return fmt.Errorf("delete automation script: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return missingOrConflict(ctx, tx, id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit automation delete: %w", err)
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
