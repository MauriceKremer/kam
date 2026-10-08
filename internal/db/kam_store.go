package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// OpenKamDB opens the SQLite database at path and applies WAL tuning.
func OpenKamDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite db: %w", err)
	}
	if err := ApplySQLiteTuning(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply sqlite tuning: %w", err)
	}
	return db, nil
}

// ApplySQLiteTuning sets WAL journal mode, relaxed synchronous mode, and a
// busy timeout so reads stay responsive while writers batch and the node
// survives power loss.
func ApplySQLiteTuning(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		return fmt.Errorf("set wal mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous = NORMAL;"); err != nil {
		return fmt.Errorf("set synchronous: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	return nil
}

// boolToInt converts a bool to the integer encoding used by SQLite tables.
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
