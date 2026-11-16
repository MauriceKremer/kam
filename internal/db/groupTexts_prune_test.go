package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// openGroupTextTestDB opens a fresh SQLite file with the group-texts schema
// ready.
func openGroupTextTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenKamDB(filepath.Join(t.TempDir(), "group.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureGroupTextsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return db
}

func countGroupTexts(t *testing.T, db *sql.DB, channel string) int {
	t.Helper()
	var n int
	if channel == "" {
		if err := db.QueryRow(`SELECT COUNT(*) FROM group_texts`).Scan(&n); err != nil {
			t.Fatalf("count group texts: %v", err)
		}
		return n
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM group_texts WHERE channel_name = ?`, channel).Scan(&n); err != nil {
		t.Fatalf("count group texts in %q: %v", channel, err)
	}
	return n
}

func TestPruneGroupTextsPerChannel(t *testing.T) {
	db := openGroupTextTestDB(t)
	rows := []GroupTextRecord{
		{ChannelName: "#a", Body: "a1", Timestamp: 1},
		{ChannelName: "#a", Body: "a2", Timestamp: 2},
		{ChannelName: "#a", Body: "a3", Timestamp: 3},
		{ChannelName: "#b", Body: "b1", Timestamp: 4},
		{ChannelName: "#b", Body: "b2", Timestamp: 5},
	}
	if err := InsertGroupTextsBatch(db, rows); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// #a is 40 days old, #b is 10. The override asks a 1-day age for #a and
	// leaves #b on the 30-day default.
	if _, err := db.Exec(`UPDATE group_texts SET received_at = unixepoch() - ?*86400`, 40); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := db.Exec(`UPDATE group_texts SET received_at = unixepoch() - ?*86400 WHERE channel_name = '#b'`, 10); err != nil {
		t.Fatalf("backdate #b: %v", err)
	}

	retention := GroupTextsRetention{
		Default:  GroupTextLimits{MaxAgeDays: 30},
		Channels: map[string]GroupTextLimits{"#a": {MaxAgeDays: 1}},
	}
	deleted, err := PruneGroupTexts(db, retention)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// All three #a rows are older than one day…
	if got, want := countGroupTexts(t, db, "#a"), 0; got != want {
		t.Fatalf("#a rows after age prune = %d, want %d", got, want)
	}
	// …#b's 10-day-old rows survive its 30-day bound.
	if got, want := countGroupTexts(t, db, "#b"), 2; got != want {
		t.Fatalf("#b rows after age prune = %d, want %d", got, want)
	}
	if deleted != 3 {
		t.Fatalf("age prune deleted = %d, want 3", deleted)
	}

	// Count prune with both channels capped at one row.
	deleted, err = PruneGroupTexts(db, GroupTextsRetention{Default: GroupTextLimits{MaxPosts: 1}})
	if err != nil {
		t.Fatalf("count prune: %v", err)
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM group_texts WHERE channel_name = '#b'`).Scan(&body); err != nil {
		t.Fatalf("read #b survivor: %v", err)
	}
	// #b's newest row by received_at (then id) survives; equal timestamps go
	// to the larger id.
	if body != "b2" {
		t.Fatalf("#b survivor = %q, want b2", body)
	}
	if got, want := countGroupTexts(t, db, "#a"), 0; got != want {
		t.Fatalf("#a rows after count prune = %d, want %d", got, want)
	}
	if got, want := countGroupTexts(t, db, ""), 1; got != want {
		t.Fatalf("rows after count prune = %d, want %d", got, want)
	}
	if deleted != 1 {
		t.Fatalf("count prune deleted = %d, want 1", deleted)
	}

	// No bounds at all: prune is a no-op.
	deleted, err = PruneGroupTexts(db, GroupTextsRetention{})
	if err != nil || deleted != 0 {
		t.Fatalf("disabled prune deleted=%d, err=%v", deleted, err)
	}
}

// TestGroupTextQueuePrunesOnStart covers the wiring from retention bounds to
// the writer goroutine: the startup prune deletes stale rows without any
// traffic, and an override reaches a channel that is no longer configured.
func TestGroupTextQueuePrunesOnStart(t *testing.T) {
	db := openGroupTextTestDB(t)
	if err := InsertGroupTextsBatch(db, []GroupTextRecord{{
		ChannelName: "#retired", Body: "old", Timestamp: 1,
	}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(`UPDATE group_texts SET received_at = unixepoch() - ?*86400`, 40); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	q := NewGroupTextQueue(db, GroupTextsRetention{
		Channels: map[string]GroupTextLimits{"#retired": {MaxAgeDays: 30}},
	})
	defer q.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if countGroupTexts(t, db, "") == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup prune did not delete the stale rows (%d remain)", countGroupTexts(t, db, ""))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
