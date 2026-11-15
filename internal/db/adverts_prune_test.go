package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// openAdvertTestDB opens a fresh SQLite file with the adverts schema ready.
func openAdvertTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenKamDB(filepath.Join(t.TempDir(), "adverts.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return db
}

func TestAdvertUpsertUpdatesPreviousRecord(t *testing.T) {
	db := openAdvertTestDB(t)
	key := []byte("node-key")

	first := AdvertRecord{
		PublicKey: key, NodeHash: key[0], Timestamp: 1,
		Signature: []byte("s1"), SignatureValid: true, IsChatNode: true,
		Nickname: "first-name", RawPayload: []byte("raw1"),
	}
	if err := UpsertAdvertsBatch(db, []AdvertRecord{first}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var firstReceived, origFirst int64
	if err := db.QueryRow(`SELECT received_at, first_received_at FROM adverts WHERE public_key = ?`, key).
		Scan(&firstReceived, &origFirst); err != nil {
		t.Fatalf("read first row: %v", err)
	}
	if origFirst <= 0 {
		t.Fatalf("first_received_at not set on insert: %d", origFirst)
	}

	// The same node is reheard with a changed role and name.
	second := AdvertRecord{
		PublicKey: key, NodeHash: key[0], Timestamp: 2,
		Signature: []byte("s2"), IsRepeater: true, IsRoomServer: true,
		Nickname: "second-name", RawPayload: []byte("raw2"),
	}
	if err := UpsertAdvertsBatch(db, []AdvertRecord{second}); err != nil {
		t.Fatalf("reheard write: %v", err)
	}

	var signatureValid, isChatNode, isRepeater bool
	var nickname string
	var receivedAt, firstSeen int64
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM adverts WHERE public_key = ?`, key).Scan(&count); err != nil {
		t.Fatalf("count after rehear: %v", err)
	}
	if err := db.QueryRow(`
SELECT signature_valid, is_chat_node, is_repeater, nickname, received_at, first_received_at
FROM adverts WHERE public_key = ?`, key).
		Scan(&signatureValid, &isChatNode, &isRepeater, &nickname, &receivedAt, &firstSeen); err != nil {
		t.Fatalf("read after rehear: %v", err)
	}
	if count != 1 {
		t.Fatalf("reheard advert added a row: %d rows", count)
	}
	if nickname != "second-name" {
		t.Fatalf("nickname = %q, want second-name", nickname)
	}
	if signatureValid || !isRepeater || isChatNode {
		t.Fatalf("fields not updated: valid=%v repeater=%v chat=%v", signatureValid, isRepeater, isChatNode)
	}
	if firstSeen != origFirst {
		t.Fatalf("first_received_at changed: %d -> %d", origFirst, firstSeen)
	}
	if receivedAt < firstReceived {
		t.Fatalf("received_at not bumped: %d < %d", receivedAt, firstReceived)
	}

	// A different public key is still its own row.
	other := second
	other.PublicKey = []byte("other-key")
	other.Nickname = "other"
	if err := UpsertAdvertsBatch(db, []AdvertRecord{other}); err != nil {
		t.Fatalf("other key write: %v", err)
	}
	if n := countAdverts(t, db); n != 2 {
		t.Fatalf("rows after other-key write = %d, want 2", n)
	}
}

func TestPruneAdvertsDeletesOldestHeardFirst(t *testing.T) {
	db := openAdvertTestDB(t)
	record := func(key string) AdvertRecord {
		return AdvertRecord{PublicKey: []byte(key), Signature: []byte{0}, RawPayload: []byte{0}}
	}
	if err := UpsertAdvertsBatch(db, []AdvertRecord{record("a"), record("b"), record("c")}); err != nil {
		t.Fatalf("write adverts: %v", err)
	}
	// Backdate a to heard 40 days ago and b to heard 10 days ago; c stays now.
	backdated := []struct {
		key  string
		days int
	}{{"a", 40}, {"b", 10}}
	for _, item := range backdated {
		if _, err := db.Exec(`UPDATE adverts SET received_at = unixepoch() - ?*86400 WHERE public_key = ?`, item.days, []byte(item.key)); err != nil {
			t.Fatalf("backdate %s: %v", item.key, err)
		}
	}

	// Age prune: 30 days removes only the 40-day-old a.
	deleted, err := PruneAdverts(db, 30, 0)
	if err != nil || deleted != 1 {
		t.Fatalf("age prune deleted=%d, err=%v", deleted, err)
	}
	if n := countAdverts(t, db); n != 2 {
		t.Fatalf("rows after age prune = %d, want 2", n)
	}
	if !advertExists(t, db, "b") || !advertExists(t, db, "c") || advertExists(t, db, "a") {
		t.Fatal("age prune removed the wrong advert")
	}

	// Count prune: keeping 1 removes everything but the newest-heard (c).
	deleted, err = PruneAdverts(db, 0, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("count prune deleted=%d, err=%v", deleted, err)
	}
	if !advertExists(t, db, "c") || advertExists(t, db, "b") {
		t.Fatal("count prune kept the wrong advert")
	}

	// Zero bounds disable pruning.
	deleted, err = PruneAdverts(db, 0, 0)
	if err != nil || deleted != 0 {
		t.Fatalf("disabled prune deleted=%d, err=%v", deleted, err)
	}
}

// TestAdvertQueuePrunesOnStart covers the wiring from retention bounds to the
// writer goroutine: the startup prune deletes stale rows without any traffic.
func TestAdvertQueuePrunesOnStart(t *testing.T) {
	db := openAdvertTestDB(t)
	old := AdvertRecord{PublicKey: []byte("old"), Signature: []byte{0}, RawPayload: []byte{0}}
	if err := UpsertAdvertsBatch(db, []AdvertRecord{old}); err != nil {
		t.Fatalf("write old advert: %v", err)
	}
	if _, err := db.Exec(`UPDATE adverts SET received_at = unixepoch() - 40*86400 WHERE public_key = ?`, []byte("old")); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	q := NewAdvertQueue(db, AdvertRetention{KeepDays: 30})
	defer q.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if n := countAdverts(t, db); n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup prune did not delete the stale advert (%d rows remain)", countAdverts(t, db))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countAdverts(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM adverts`).Scan(&n); err != nil {
		t.Fatalf("count adverts: %v", err)
	}
	return n
}

func advertExists(t *testing.T, db *sql.DB, key string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM adverts WHERE public_key = ?`, []byte(key)).Scan(&n); err != nil {
		t.Fatalf("check advert %q: %v", key, err)
	}
	return n > 0
}
