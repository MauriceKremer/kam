package db

import (
	"database/sql"
	"fmt"
)

func EnsureGroupTextsSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS group_texts (
  id INTEGER PRIMARY KEY,
  channel_name TEXT NOT NULL,
  sender TEXT,
  body TEXT,
  txt_type INTEGER NOT NULL,
  attempt INTEGER NOT NULL,
  public_key_prefix BLOB,
  message_timestamp INTEGER NOT NULL,
  time_drift_seconds INTEGER NOT NULL,
  hop_count INTEGER NOT NULL DEFAULT 0,
  hop_stations TEXT NOT NULL DEFAULT '[]',
	snr_quarter_db INTEGER,
	rssi_dbm INTEGER,
  received_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_group_texts_received_at ON group_texts(received_at);
CREATE INDEX IF NOT EXISTS idx_group_texts_channel ON group_texts(channel_name);
CREATE INDEX IF NOT EXISTS idx_group_texts_sender ON group_texts(sender);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure group texts schema: %w", err)
	}
	if err := migrateGroupTextsColumns(db); err != nil {
		return fmt.Errorf("migrate group texts columns: %w", err)
	}
	return nil
}

// migrateGroupTextsColumns adds columns introduced after the initial schema to
// databases created by older kam versions. SQLite has no ADD COLUMN IF NOT
// EXISTS, so we introspect pragma_table_info and issue idempotent ALTERs.
func migrateGroupTextsColumns(db *sql.DB) error {
	cols, err := tableColumns(db, "group_texts")
	if err != nil {
		return err
	}

	add := func(name, def string) error {
		if cols[name] {
			return nil
		}
		_, err := db.Exec("ALTER TABLE group_texts ADD COLUMN " + name + " " + def)
		if err != nil {
			return fmt.Errorf("add column %s: %w", name, err)
		}
		return nil
	}

	if err := add("hop_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := add("hop_stations", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := add("snr_quarter_db", "INTEGER"); err != nil {
		return err
	}
	if err := add("rssi_dbm", "INTEGER"); err != nil {
		return err
	}
	return nil
}

// tableColumns returns the set of column names for a table.
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("pragma_table_info %s: %w", table, err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan column name: %w", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns: %w", err)
	}
	return out, nil
}

type GroupTextRecord struct {
	ChannelName      string
	Sender           string
	Body             string
	TxtType          uint8
	Attempt          uint8
	PublicKeyPrefix  []byte
	Timestamp        int64
	TimeDriftSeconds int64
	HopCount         int
	HopStations      string // JSON array of hex-encoded path hashes, e.g. `["a1","b2c3"]`
	SNRQuarterDB     *int
	RSSIDBM          *int
}

func InsertGroupTextsBatch(db *sql.DB, records []GroupTextRecord) error {
	if len(records) == 0 {
		return nil
	}
	const query = `
INSERT INTO group_texts (
  channel_name, sender, body, txt_type, attempt,
  public_key_prefix, message_timestamp, time_drift_seconds,
	hop_count, hop_stations, snr_quarter_db, rssi_dbm
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin group text batch: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("prepare group text insert: %w", err)
	}
	defer stmt.Close()

	for i, r := range records {
		hopStations := r.HopStations
		if hopStations == "" {
			hopStations = "[]"
		}
		if _, err := stmt.Exec(
			r.ChannelName,
			r.Sender,
			r.Body,
			r.TxtType,
			r.Attempt,
			r.PublicKeyPrefix,
			r.Timestamp,
			r.TimeDriftSeconds,
			r.HopCount,
			hopStations,
			r.SNRQuarterDB,
			r.RSSIDBM,
		); err != nil {
			return fmt.Errorf("insert group text %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit group text batch: %w", err)
	}
	return nil
}

// (hex helpers live in groupTexts.go where conversion happens)
