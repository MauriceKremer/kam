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
  hop_count INTEGER NOT NULL DEFAULT 0,
  hop_stations TEXT NOT NULL DEFAULT '[]',
  received_at INTEGER NOT NULL DEFAULT (unixepoch()),
  first_received_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_adverts_received_at ON adverts(received_at);
CREATE INDEX IF NOT EXISTS idx_adverts_public_key ON adverts(public_key);
CREATE INDEX IF NOT EXISTS idx_adverts_node_hash ON adverts(node_hash);
CREATE INDEX IF NOT EXISTS idx_adverts_node_type ON adverts(is_chat_node, is_repeater, is_room_server, is_sensor);
CREATE INDEX IF NOT EXISTS idx_adverts_hop_count ON adverts(hop_count);
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
	HopCount         int
	HopStations      string // JSON array of hex-encoded path hashes, e.g. `["a1","b2c3"]`
}

// UpsertAdvertsBatch stores adverts in one transaction. An advert from a
// public key that is already stored updates the existing row (latest wins),
// so a reheard advert moves the last-heard time instead of adding a row;
// first_received_at keeps the original insert time.
func UpsertAdvertsBatch(db *sql.DB, records []AdvertRecord) error {
	if len(records) == 0 {
		return nil
	}
	// received_at is bumped on update so the row tracks when the node was
	// last heard; first_received_at is only ever set on insert.
	const updateQuery = `
UPDATE adverts SET
  node_hash = ?, advert_timestamp = ?, signature = ?, signature_valid = ?,
  is_chat_node = ?, is_repeater = ?, is_room_server = ?, is_sensor = ?,
  nickname = ?, has_location = ?, latitude = ?, longitude = ?,
  time_drift_seconds = ?, raw_payload = ?, hop_count = ?, hop_stations = ?,
  received_at = unixepoch()
WHERE public_key = ?;
`
	const insertQuery = `
INSERT INTO adverts (
  node_hash, public_key, advert_timestamp, signature, signature_valid,
  is_chat_node, is_repeater, is_room_server, is_sensor,
  nickname, has_location, latitude, longitude, time_drift_seconds, raw_payload,
  hop_count, hop_stations, first_received_at, received_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch(), unixepoch());
`
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin advert batch: %w", err)
	}
	defer tx.Rollback()

	upd, err := tx.Prepare(updateQuery)
	if err != nil {
		return fmt.Errorf("prepare advert update: %w", err)
	}
	defer upd.Close()
	ins, err := tx.Prepare(insertQuery)
	if err != nil {
		return fmt.Errorf("prepare advert insert: %w", err)
	}
	defer ins.Close()

	for i, r := range records {
		hopStations := r.HopStations
		if hopStations == "" {
			hopStations = "[]"
		}
		// Payload values shared by both statements: everything except the
		// node_hash (first column in both) and the public_key (last argument
		// of the UPDATE's WHERE, second column of the INSERT).
		payload := []any{
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
			r.HopCount,
			hopStations,
		}
		updateArgs := append([]any{r.NodeHash}, payload...)
		result, err := upd.Exec(append(updateArgs, r.PublicKey)...)
		if err != nil {
			return fmt.Errorf("update advert %d: %w", i, err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			insertArgs := append([]any{r.NodeHash, r.PublicKey}, payload...)
			if _, err := ins.Exec(insertArgs...); err != nil {
				return fmt.Errorf("insert advert %d: %w", i, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit advert batch: %w", err)
	}
	return nil
}
