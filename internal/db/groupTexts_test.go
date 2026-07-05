package db

import (
	"path/filepath"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

func TestGroupTextQueueEnqueueAndFlush(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "group.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureGroupTextsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	q := NewGroupTextQueue(db)
	defer q.Stop()

	msg := parsers.GroupTextMessage{
		ChannelName:      "#mesh-dev",
		Timestamp:        time.Now(),
		Message:          "alice: hello world",
		Sender:           "alice",
		Body:             "hello world",
		TxtType:          0,
		Attempt:          0,
		TimeDriftSeconds: 5,
	}

	if !q.Enqueue(msg) {
		t.Fatal("enqueue returned false on empty queue")
	}

	time.Sleep(1200 * time.Millisecond)

	m := q.Metrics()
	if m.Enqueued != 1 || m.Flushed != 1 || m.Dropped != 0 || m.Failed != 0 {
		t.Fatalf("unexpected metrics: %+v", m)
	}

	db2, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM group_texts").Scan(&count); err != nil {
		t.Fatalf("count group texts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 group text row, got %d", count)
	}
}

func TestGroupTextQueueSignedMessage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "group.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureGroupTextsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	q := NewGroupTextQueue(db)
	defer q.Stop()

	msg := parsers.GroupTextMessage{
		ChannelName:      "#public",
		Timestamp:        time.Unix(1234567890, 0),
		Message:          "signed body",
		Sender:           "",
		Body:             "signed body",
		TxtType:          2,
		Attempt:          1,
		PublicKeyPrefix:  []byte{0xAA, 0xBB, 0xCC, 0xDD},
		TimeDriftSeconds: 0,
	}

	if !q.Enqueue(msg) {
		t.Fatal("enqueue failed")
	}

	time.Sleep(200 * time.Millisecond)
	q.Stop()

	db2, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()

	var sender string
	var body string
	var txtType int
	var attempt int
	var prefix []byte
	if err := db2.QueryRow("SELECT sender, body, txt_type, attempt, public_key_prefix FROM group_texts LIMIT 1").Scan(
		&sender, &body, &txtType, &attempt, &prefix); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body != "signed body" || txtType != 2 || attempt != 1 || string(prefix) != string([]byte{0xAA, 0xBB, 0xCC, 0xDD}) {
		t.Fatalf("row mismatch: sender=%q body=%q txt_type=%d attempt=%d prefix=%x", sender, body, txtType, attempt, prefix)
	}
}

func TestGroupTextRecordConversion(t *testing.T) {
	msg := parsers.GroupTextMessage{
		ChannelName:      "#test",
		Timestamp:        time.Unix(1234567890, 0),
		Message:          "bob: hi",
		Sender:           "bob",
		Body:             "hi",
		TxtType:          0,
		Attempt:          0,
		PublicKeyPrefix:  nil,
		TimeDriftSeconds: 3,
	}

	r := groupTextToRecord(msg)
	if r.ChannelName != "#test" || r.Sender != "bob" || r.Body != "hi" || r.TxtType != 0 || r.Attempt != 0 || r.TimeDriftSeconds != 3 {
		t.Fatalf("record mismatch: %+v", r)
	}
}

func TestGroupTextBatchInsert(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "group.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureGroupTextsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	records := []GroupTextRecord{
		{
			ChannelName:      "#a",
			Sender:           "u1",
			Body:             "m1",
			TxtType:          0,
			Attempt:          0,
			Timestamp:        1,
			TimeDriftSeconds: 0,
		},
		{
			ChannelName:      "#b",
			Sender:           "u2",
			Body:             "m2",
			TxtType:          1,
			Attempt:          2,
			PublicKeyPrefix:  []byte{1, 2, 3, 4},
			Timestamp:        2,
			TimeDriftSeconds: 1,
		},
	}

	if err := InsertGroupTextsBatch(db, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM group_texts").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows, got %d", count)
	}
}
