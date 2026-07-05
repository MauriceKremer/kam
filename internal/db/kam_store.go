package db

import (
	"database/sql"
	"fmt"

	"kam.bytecoder.nl/internal/models"

	_ "modernc.org/sqlite"
)

type ChannelRecord struct {
	ID          int64
	Name        string
	KeyStrategy string
	KeyHex      string
	Enabled     bool
	Source      string
}

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

func EnsureChannelsSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS channels (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  key_strategy TEXT NOT NULL CHECK (key_strategy IN ('public', 'derived', 'explicit')),
  key_hex TEXT,
  enabled INTEGER NOT NULL DEFAULT 1,
  source TEXT NOT NULL DEFAULT 'runtime',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channels_enabled ON channels(enabled);
`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure channels schema: %w", err)
	}
	return nil
}

func UpsertSeedChannels(db *sql.DB, cfg *models.KamConfig) error {
	return upsertChannels(db, cfg.Channels, "seed")
}

func UpsertRuntimeChannel(db *sql.DB, ch models.ChannelConfig) error {
	return upsertChannels(db, []models.ChannelConfig{ch}, "runtime")
}

func upsertChannels(db *sql.DB, channels []models.ChannelConfig, source string) error {
	const upsertQuery = `
INSERT INTO channels (name, key_strategy, key_hex, enabled, source, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(name) DO UPDATE SET
  key_strategy = excluded.key_strategy,
  key_hex = excluded.key_hex,
  enabled = excluded.enabled,
  source = excluded.source,
  updated_at = CURRENT_TIMESTAMP;
`

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin upsert transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(upsertQuery)
	if err != nil {
		return fmt.Errorf("prepare upsert statement: %w", err)
	}
	defer stmt.Close()

	for _, ch := range channels {
		enabled := 0
		if ch.Enabled {
			enabled = 1
		}

		if _, err := stmt.Exec(ch.Name, ch.KeyStrategy, nullableKeyHex(ch.KeyHex), enabled, source); err != nil {
			return fmt.Errorf("upsert channel %q: %w", ch.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert transaction: %w", err)
	}

	return nil
}

func GetChannelByName(db *sql.DB, name string) (*ChannelRecord, error) {
	const query = `
SELECT id, name, key_strategy, COALESCE(key_hex, ''), enabled, source
FROM channels
WHERE name = ?
LIMIT 1;
`

	var row ChannelRecord
	var enabled int
	if err := db.QueryRow(query, name).Scan(&row.ID, &row.Name, &row.KeyStrategy, &row.KeyHex, &enabled, &row.Source); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("channel %q not found", name)
		}
		return nil, fmt.Errorf("query channel %q: %w", name, err)
	}
	row.Enabled = enabled == 1

	return &row, nil
}

func ListEnabledNonPublicChannels(db *sql.DB) ([]ChannelRecord, error) {
	const query = `
SELECT id, name, key_strategy, COALESCE(key_hex, ''), enabled, source
FROM channels
WHERE enabled = 1 AND name != '#public'
ORDER BY name;
`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query enabled channels: %w", err)
	}
	defer rows.Close()

	out := make([]ChannelRecord, 0)
	for rows.Next() {
		var row ChannelRecord
		var enabled int
		if err := rows.Scan(&row.ID, &row.Name, &row.KeyStrategy, &row.KeyHex, &enabled, &row.Source); err != nil {
			return nil, fmt.Errorf("scan enabled channel row: %w", err)
		}
		row.Enabled = enabled == 1
		out = append(out, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enabled channel rows: %w", err)
	}

	return out, nil
}

func nullableKeyHex(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
