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

// GroupTextLimits is the resolved retention pair for one channel.
type GroupTextLimits struct {
	MaxPosts   int64
	MaxAgeDays int64
}

// GroupTextsRetention bounds the group_texts table per channel. Channels
// absent from Channels — and any row under a name that is not a configured
// channel any more — use Default. A zero Default with empty/zero-valued
// entries keeps everything.
type GroupTextsRetention struct {
	Default  GroupTextLimits
	Channels map[string]GroupTextLimits
}

func (r GroupTextsRetention) anyBound() bool {
	if r.Default.MaxPosts > 0 || r.Default.MaxAgeDays > 0 {
		return true
	}
	for _, limits := range r.Channels {
		if limits.MaxPosts > 0 || limits.MaxAgeDays > 0 {
			return true
		}
	}
	return false
}

func (r GroupTextsRetention) limits(channel string) GroupTextLimits {
	if limits, ok := r.Channels[channel]; ok {
		return limits
	}
	return r.Default
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

// PruneGroupTexts applies each channel's retention pair to the group_texts
// table: delete rows older than its max_age_days, then everything beyond its
// newest max_posts rows, oldest first. Both bounds come from the channel's
// override when it has one, from Default otherwise; a pair of zeros prunes
// that channel's rows not at all. Returns the total rows deleted.
func PruneGroupTexts(db *sql.DB, retention GroupTextsRetention) (int64, error) {
	if !retention.anyBound() {
		return 0, nil
	}
	var channels []string
	rows, err := db.Query(`SELECT DISTINCT channel_name FROM group_texts`)
	if err != nil {
		return 0, fmt.Errorf("list group text channels: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan group text channel: %w", err)
		}
		channels = append(channels, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("list group text channels: %w", err)
	}
	rows.Close()

	var deleted int64
	for _, channel := range channels {
		limits := retention.limits(channel)
		if limits.MaxAgeDays > 0 {
			result, err := db.Exec(`DELETE FROM group_texts WHERE channel_name = ? AND received_at < unixepoch() - ?`, channel, limits.MaxAgeDays*86400)
			if err != nil {
				return deleted, fmt.Errorf("prune group texts by age for %s: %w", channel, err)
			}
			n, _ := result.RowsAffected()
			deleted += n
		}
		if limits.MaxPosts > 0 {
			// Everything after the newest maxPosts rows; same shape as
			// PrunePosts against the channel index.
			result, err := db.Exec(`DELETE FROM group_texts WHERE channel_name = ?1 AND id IN (
      SELECT id FROM group_texts WHERE channel_name = ?1 ORDER BY received_at DESC, id DESC LIMIT -1 OFFSET ?2)`, channel, limits.MaxPosts)
			if err != nil {
				return deleted, fmt.Errorf("prune group texts by count for %s: %w", channel, err)
			}
			n, _ := result.RowsAffected()
			deleted += n
		}
	}
	return deleted, nil
}

// (hex helpers live in groupTexts.go where conversion happens)
