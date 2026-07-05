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
  received_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_group_texts_received_at ON group_texts(received_at);
CREATE INDEX IF NOT EXISTS idx_group_texts_channel ON group_texts(channel_name);
CREATE INDEX IF NOT EXISTS idx_group_texts_sender ON group_texts(sender);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure group texts schema: %w", err)
	}
	return nil
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
}

func InsertGroupTextsBatch(db *sql.DB, records []GroupTextRecord) error {
	if len(records) == 0 {
		return nil
	}
	const query = `
INSERT INTO group_texts (
  channel_name, sender, body, txt_type, attempt,
  public_key_prefix, message_timestamp, time_drift_seconds
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);
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
		if _, err := stmt.Exec(
			r.ChannelName,
			r.Sender,
			r.Body,
			r.TxtType,
			r.Attempt,
			r.PublicKeyPrefix,
			r.Timestamp,
			r.TimeDriftSeconds,
		); err != nil {
			return fmt.Errorf("insert group text %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit group text batch: %w", err)
	}
	return nil
}
