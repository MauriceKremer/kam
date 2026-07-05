package db

import (
	"database/sql"
	"fmt"
)

func EnsureAdvertsSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS adverts (
  id INTEGER PRIMARY KEY,
  node_hash INTEGER NOT NULL,
  public_key BLOB NOT NULL,
  advert_timestamp INTEGER NOT NULL,
  signature BLOB NOT NULL,
  signature_valid INTEGER NOT NULL,
  is_chat_node INTEGER NOT NULL,
  is_repeater INTEGER NOT NULL,
  is_room_server INTEGER NOT NULL,
  is_sensor INTEGER NOT NULL,
  nickname TEXT,
  has_location INTEGER NOT NULL,
  latitude REAL,
  longitude REAL,
  time_drift_seconds INTEGER NOT NULL,
  raw_payload BLOB NOT NULL,
  received_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_adverts_received_at ON adverts(received_at);
CREATE INDEX IF NOT EXISTS idx_adverts_public_key ON adverts(public_key);
CREATE INDEX IF NOT EXISTS idx_adverts_node_hash ON adverts(node_hash);
CREATE INDEX IF NOT EXISTS idx_adverts_node_type ON adverts(is_chat_node, is_repeater, is_room_server, is_sensor);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("ensure adverts schema: %w", err)
	}
	return nil
}

type AdvertRecord struct {
	NodeHash         byte
	PublicKey        []byte
	Timestamp        int64
	Signature        []byte
	SignatureValid   bool
	IsChatNode       bool
	IsRepeater       bool
	IsRoomServer     bool
	IsSensor         bool
	Nickname         string
	HasLocation      bool
	Latitude         float32
	Longitude        float32
	TimeDriftSeconds int64
	RawPayload       []byte
}

func InsertAdvertsBatch(db *sql.DB, records []AdvertRecord) error {
	if len(records) == 0 {
		return nil
	}
	const query = `
INSERT INTO adverts (
  node_hash, public_key, advert_timestamp, signature, signature_valid,
  is_chat_node, is_repeater, is_room_server, is_sensor,
  nickname, has_location, latitude, longitude, time_drift_seconds, raw_payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin advert batch: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("prepare advert insert: %w", err)
	}
	defer stmt.Close()

	for i, r := range records {
		if _, err := stmt.Exec(
			r.NodeHash,
			r.PublicKey,
			r.Timestamp,
			r.Signature,
			boolToInt(r.SignatureValid),
			boolToInt(r.IsChatNode),
			boolToInt(r.IsRepeater),
			boolToInt(r.IsRoomServer),
			boolToInt(r.IsSensor),
			r.Nickname,
			boolToInt(r.HasLocation),
			r.Latitude,
			r.Longitude,
			r.TimeDriftSeconds,
			r.RawPayload,
		); err != nil {
			return fmt.Errorf("insert advert %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit advert batch: %w", err)
	}
	return nil
}
