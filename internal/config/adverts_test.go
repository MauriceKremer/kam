package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss"
)

func TestAdvertQueueEnqueueAndFlush(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	q, err := NewAdvertQueue(dbPath)
	if err != nil {
		t.Fatalf("create advert queue: %v", err)
	}
	defer q.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()

	advert := kiss.AdvertPacket{
		NodeHash:         pub[0],
		PublicKey:        pub,
		Timestamp:        now,
		Signature:        []byte("sig"),
		SignatureValid:   true,
		IsChatNode:       true,
		IsRepeater:       false,
		IsRoomServer:     false,
		IsSensor:         false,
		Nickname:         "test-node",
		HasLocation:      true,
		Latitude:         52.0,
		Longitude:        4.0,
		TimeDriftSeconds: 12,
		RawPayload:       []byte("raw-advert-payload"),
	}

	if !q.Enqueue(advert) {
		t.Fatal("enqueue returned false on empty queue")
	}

	// Force flush via interval; default is 1s.
	time.Sleep(1200 * time.Millisecond)

	m := q.Metrics()
	if m.Enqueued != 1 {
		t.Fatalf("expected enqueued=1, got %d", m.Enqueued)
	}
	if m.Flushed != 1 {
		t.Fatalf("expected flushed=1, got %d", m.Flushed)
	}
	if m.Dropped != 0 || m.Failed != 0 {
		t.Fatalf("unexpected drops/failures: %+v", m)
	}

	// Verify row persisted.
	db, err := OpenChannelsDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM adverts").Scan(&count); err != nil {
		t.Fatalf("count adverts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 advert row, got %d", count)
	}
}

func TestAdvertQueueDropOnFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	// Use a tiny queue and a slow/no-op writer by replacing flush interval far
	// in the future so queue fills.
	q, err := NewAdvertQueue(dbPath)
	if err != nil {
		t.Fatalf("create advert queue: %v", err)
	}
	defer q.Close()

	// Fill queue beyond capacity. Default capacity is 256.
	sent := 0
	dropped := 0
	for i := 0; i < defaultAdvertQueueSize*2; i++ {
		advert := kiss.AdvertPacket{
			PublicKey: []byte{byte(i)},
			Signature: []byte("x"),
			Timestamp: time.Unix(int64(i), 0),
		}
		if q.Enqueue(advert) {
			sent++
		} else {
			dropped++
		}
	}

	if sent == 0 {
		t.Fatal("expected some adverts to be enqueued")
	}
	if dropped == 0 {
		t.Fatal("expected some adverts to be dropped when queue full")
	}

	m := q.Metrics()
	if m.Enqueued != int64(sent) {
		t.Fatalf("expected enqueued=%d, got %d", sent, m.Enqueued)
	}
	if m.Dropped != int64(dropped) {
		t.Fatalf("expected dropped=%d, got %d", dropped, m.Dropped)
	}
}

func TestAdvertBatchInsert(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	db, err := OpenChannelsDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	records := []AdvertRecord{
		{
			NodeHash:         0xAB,
			PublicKey:        []byte("key1"),
			Timestamp:        1,
			Signature:        []byte("sig1"),
			SignatureValid:   true,
			IsChatNode:       true,
			IsRepeater:       false,
			IsRoomServer:     false,
			IsSensor:         false,
			Nickname:         "a",
			HasLocation:      false,
			TimeDriftSeconds: 0,
			RawPayload:       []byte("batch1"),
		},
		{
			NodeHash:         0xCD,
			PublicKey:        []byte("key2"),
			Timestamp:        2,
			Signature:        []byte("sig2"),
			SignatureValid:   false,
			IsChatNode:       false,
			IsRepeater:       true,
			IsRoomServer:     false,
			IsSensor:         false,
			Nickname:         "b",
			HasLocation:      true,
			Latitude:         1.0,
			Longitude:        2.0,
			TimeDriftSeconds: 3,
			RawPayload:       []byte("batch2"),
		},
	}

	if err := InsertAdvertsBatch(db, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM adverts").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows, got %d", count)
	}
}

func TestAdvertQueueGracefulShutdownFlush(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	q, err := NewAdvertQueue(dbPath)
	if err != nil {
		t.Fatalf("create advert queue: %v", err)
	}

	advert := kiss.AdvertPacket{
		PublicKey: []byte("shutdown-key"),
		Signature: []byte("sig"),
		Timestamp: time.Now(),
		RawPayload: []byte("shutdown-raw"),
		NodeHash:  0xEF,
	}
	if !q.Enqueue(advert) {
		t.Fatal("enqueue failed")
	}

	// Close before interval flush; should still persist queued advert.
	if err := q.Close(); err != nil {
		t.Fatalf("close queue: %v", err)
	}

	db, err := OpenChannelsDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM adverts").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected shutdown flush to persist 1 advert, got %d", count)
	}
}

func TestAdvertEnqueue(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	q, err := NewAdvertQueue(dbPath)
	if err != nil {
		t.Fatalf("create advert queue: %v", err)
	}
	defer q.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	q.Enqueue(kiss.AdvertPacket{
		NodeHash:         pub[0],
		PublicKey:        pub,
		Timestamp:        time.Now(),
		Signature:        []byte("sig"),
		SignatureValid:   true,
		IsChatNode:       true,
		IsRepeater:       false,
		IsRoomServer:     false,
		IsSensor:         false,
		HasLocation:      false,
		TimeDriftSeconds: 0,
		RawPayload:       []byte("enqueue-raw"),
	})

	time.Sleep(200 * time.Millisecond)
	m := q.Metrics()
	if m.Enqueued != 1 {
		t.Fatalf("expected enqueued=1, got %d", m.Enqueued)
	}
}

func TestAdvertToRecordConversion(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Unix(1234567890, 0)

	a := kiss.AdvertPacket{
		NodeHash:         pub[0],
		PublicKey:        pub,
		Timestamp:        now,
		Signature:        []byte("signature"),
		SignatureValid:   true,
		IsChatNode:       true,
		IsRepeater:       false,
		IsRoomServer:     false,
		IsSensor:         false,
		Nickname:         "node-1",
		HasLocation:      true,
		Latitude:         12.34,
		Longitude:        56.78,
		TimeDriftSeconds: 7,
		RawPayload:       []byte("raw-bytes"),
	}

	r := advertToRecord(a)
	if r.NodeHash != pub[0] {
		t.Fatalf("node hash mismatch: expected %x, got %x", pub[0], r.NodeHash)
	}
	if string(r.PublicKey) != string(pub) {
		t.Fatal("public key mismatch")
	}
	if r.Timestamp != now.Unix() {
		t.Fatalf("timestamp mismatch: expected %d, got %d", now.Unix(), r.Timestamp)
	}
	if string(r.Signature) != "signature" {
		t.Fatal("signature mismatch")
	}
	if !r.SignatureValid || !r.IsChatNode || !r.HasLocation {
		t.Fatal("bool fields not preserved")
	}
	if r.Nickname != "node-1" || r.Latitude != 12.34 || r.Longitude != 56.78 || r.TimeDriftSeconds != 7 {
		t.Fatalf("field mismatch: %+v", r)
	}
	if string(r.RawPayload) != "raw-bytes" {
		t.Fatalf("raw payload mismatch: %s", r.RawPayload)
	}
}
