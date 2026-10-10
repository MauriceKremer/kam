package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RoomPost is one stored room post.
type RoomPost struct {
	// Timestamp is the server-assigned time in unix seconds; it is unique and
	// increasing per room and doubles as the sync cursor value.
	Timestamp uint32
	// Author is the 32-byte public key of the poster (the room's own key for
	// system posts).
	Author []byte
	Text   string
}

// RoomClient is the persisted part of a client session. Shared secrets are
// recomputed from the key, never stored.
type RoomClient struct {
	PublicKey     []byte
	Role          uint8
	SyncSince     uint32
	LastTimestamp uint32
	// OutPathLen is the encoded path_len of the stored route, or 0xFF when no
	// route is known.
	OutPathLen uint8
	OutPath    []byte
}

// ErrRoomPostTimestampTaken reports that the room already has a post with that
// timestamp.
var ErrRoomPostTimestampTaken = errors.New("room post timestamp already used")

// RoomRepository stores rooms, posts and client sessions on its own SQLite
// connection with synchronous=FULL, so a post is durable before its ACK is sent.
type RoomRepository struct {
	db *sql.DB
}

// roomMigrations are applied in order; the index plus one is the schema
// version recorded in room_schema_version.
var roomMigrations = []string{
	`CREATE TABLE IF NOT EXISTS rooms (
  room_id TEXT PRIMARY KEY,
  public_key BLOB NOT NULL,
  created_at INTEGER NOT NULL DEFAULT (unixepoch()),
  updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE TABLE IF NOT EXISTS room_posts (
  room_id TEXT NOT NULL,
  post_ts INTEGER NOT NULL,
  author BLOB NOT NULL,
  body TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT (unixepoch()),
  PRIMARY KEY (room_id, post_ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS room_clients (
  room_id TEXT NOT NULL,
  public_key BLOB NOT NULL,
  role INTEGER NOT NULL,
  sync_since INTEGER NOT NULL,
  last_timestamp INTEGER NOT NULL,
  out_path_len INTEGER NOT NULL,
  out_path BLOB NOT NULL,
  updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
  PRIMARY KEY (room_id, public_key)
) WITHOUT ROWID;`,
}

// OpenRoomRepository opens the repository at the database path and applies
// pending migrations.
func OpenRoomRepository(path string) (*RoomRepository, error) {
	db, err := OpenKamDB(path)
	if err != nil {
		return nil, err
	}
	// PRAGMA settings are per connection; one connection keeps them in force.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA synchronous = FULL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set room synchronous mode: %w", err)
	}
	if err := EnsureRoomSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &RoomRepository{db: db}, nil
}

// EnsureRoomSchema creates or upgrades the room tables. It is idempotent.
func EnsureRoomSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS room_schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("ensure room schema version table: %w", err)
	}
	var current int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM room_schema_version`).Scan(&current)
	if err != nil {
		return fmt.Errorf("read room schema version: %w", err)
	}
	if current > len(roomMigrations) {
		return fmt.Errorf("room schema version %d is newer than this build supports (%d)", current, len(roomMigrations))
	}
	for version := current + 1; version <= len(roomMigrations); version++ {
		// The first statement of the transaction is a write; a read first would
		// fail with SQLITE_BUSY_SNAPSHOT if another connection commits meanwhile.
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin room migration %d: %w", version, err)
		}
		if _, err := tx.Exec(roomMigrations[version-1]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply room migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO room_schema_version (version) VALUES (?)`, version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record room migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit room migration %d: %w", version, err)
		}
	}
	return nil
}

// Close closes the connection.
func (r *RoomRepository) Close() error { return r.db.Close() }

// RegisterRoom records the room's public key. changedFrom is the previously
// stored key when it differs from publicKey, which means the identity file was
// replaced and the room now has a new address.
func (r *RoomRepository) RegisterRoom(ctx context.Context, roomID string, publicKey []byte) (changedFrom []byte, err error) {
	var stored []byte
	err = r.db.QueryRowContext(ctx, `SELECT public_key FROM rooms WHERE room_id = ?`, roomID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = r.db.ExecContext(ctx, `INSERT INTO rooms (room_id, public_key) VALUES (?, ?)`, roomID, publicKey)
		if err != nil {
			return nil, fmt.Errorf("register room: %w", err)
		}
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read room: %w", err)
	case bytes.Equal(stored, publicKey):
		return nil, nil
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE rooms SET public_key = ?, updated_at = unixepoch() WHERE room_id = ?`, publicKey, roomID); err != nil {
		return nil, fmt.Errorf("update room key: %w", err)
	}
	return stored, nil
}

// AppendPost stores a post and, in the same transaction, the author's last
// client timestamp, so a retry after a crash is recognised. It returns
// ErrRoomPostTimestampTaken when the timestamp is already used in the room.
func (r *RoomRepository) AppendPost(ctx context.Context, roomID string, post RoomPost, clientKey []byte, clientLastTimestamp uint32) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin room post: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_posts (room_id, post_ts, author, body) VALUES (?, ?, ?, ?)`, roomID, post.Timestamp, post.Author, post.Text); err != nil {
		if isConstraintError(err) {
			return ErrRoomPostTimestampTaken
		}
		return fmt.Errorf("insert room post: %w", err)
	}
	if len(clientKey) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE room_clients SET last_timestamp = ?, updated_at = unixepoch() WHERE room_id = ? AND public_key = ?`, clientLastTimestamp, roomID, clientKey); err != nil {
			return fmt.Errorf("update client timestamp: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit room post: %w", err)
	}
	return nil
}

// LatestPosts returns the newest limit posts of the room, oldest first.
func (r *RoomRepository) LatestPosts(ctx context.Context, roomID string, limit int) ([]RoomPost, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT post_ts, author, body FROM room_posts WHERE room_id = ? ORDER BY post_ts DESC LIMIT ?`, roomID, limit)
	if err != nil {
		return nil, fmt.Errorf("list room posts: %w", err)
	}
	defer rows.Close()
	var posts []RoomPost
	for rows.Next() {
		var post RoomPost
		if err := rows.Scan(&post.Timestamp, &post.Author, &post.Text); err != nil {
			return nil, fmt.Errorf("scan room post: %w", err)
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(posts)-1; i < j; i, j = i+1, j-1 {
		posts[i], posts[j] = posts[j], posts[i]
	}
	return posts, nil
}

// CountPosts returns how many posts the room has stored.
func (r *RoomRepository) CountPosts(ctx context.Context, roomID string) (int64, error) {
	var count int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM room_posts WHERE room_id = ?`, roomID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count room posts: %w", err)
	}
	return count, nil
}

// PrunePosts deletes posts older than olderThan (a timestamp; 0 keeps all) and
// then everything but the newest keepNewest posts (0 keeps all). It returns
// the number of posts deleted.
func (r *RoomRepository) PrunePosts(ctx context.Context, roomID string, keepNewest int, olderThan uint32) (int64, error) {
	var deleted int64
	if olderThan > 0 {
		result, err := r.db.ExecContext(ctx, `DELETE FROM room_posts WHERE room_id = ? AND post_ts < ?`, roomID, olderThan)
		if err != nil {
			return deleted, fmt.Errorf("prune room posts by age: %w", err)
		}
		n, _ := result.RowsAffected()
		deleted += n
	}
	if keepNewest > 0 {
		result, err := r.db.ExecContext(ctx, `DELETE FROM room_posts WHERE room_id = ?1 AND post_ts <= COALESCE((SELECT post_ts FROM room_posts WHERE room_id = ?1 ORDER BY post_ts DESC LIMIT 1 OFFSET ?2), -1)`, roomID, keepNewest)
		if err != nil {
			return deleted, fmt.Errorf("prune room posts by count: %w", err)
		}
		n, _ := result.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// SaveClients upserts client sessions in one transaction.
func (r *RoomRepository) SaveClients(ctx context.Context, roomID string, clients []RoomClient) error {
	if len(clients) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin client save: %w", err)
	}
	defer tx.Rollback()
	for _, c := range clients {
		path := c.OutPath
		if path == nil {
			path = []byte{}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO room_clients (room_id, public_key, role, sync_since, last_timestamp, out_path_len, out_path)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (room_id, public_key) DO UPDATE SET role = excluded.role, sync_since = excluded.sync_since,
  last_timestamp = excluded.last_timestamp, out_path_len = excluded.out_path_len, out_path = excluded.out_path, updated_at = unixepoch()`,
			roomID, c.PublicKey, c.Role, c.SyncSince, c.LastTimestamp, c.OutPathLen, path); err != nil {
			return fmt.Errorf("save room client: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit client save: %w", err)
	}
	return nil
}

// DeleteClient removes a client session.
func (r *RoomRepository) DeleteClient(ctx context.Context, roomID string, publicKey []byte) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM room_clients WHERE room_id = ? AND public_key = ?`, roomID, publicKey); err != nil {
		return fmt.Errorf("delete room client: %w", err)
	}
	return nil
}

// LoadClients returns the stored client sessions, most recently updated first.
func (r *RoomRepository) LoadClients(ctx context.Context, roomID string) ([]RoomClient, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT public_key, role, sync_since, last_timestamp, out_path_len, out_path FROM room_clients WHERE room_id = ? ORDER BY updated_at DESC, public_key`, roomID)
	if err != nil {
		return nil, fmt.Errorf("list room clients: %w", err)
	}
	defer rows.Close()
	var clients []RoomClient
	for rows.Next() {
		var c RoomClient
		if err := rows.Scan(&c.PublicKey, &c.Role, &c.SyncSince, &c.LastTimestamp, &c.OutPathLen, &c.OutPath); err != nil {
			return nil, fmt.Errorf("scan room client: %w", err)
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}
