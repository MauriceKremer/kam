package db

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

func TestAdvertQueueEnqueueAndFlush(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	q := NewAdvertQueue(db)
	defer q.Stop()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()

	advert := parsers.AdvertPacket{
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
	db2, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM adverts").Scan(&count); err != nil {
		t.Fatalf("count adverts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 advert row, got %d", count)
	}
}

func TestAdvertQueueDropOnFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	q := NewAdvertQueue(db)
	defer q.Stop()

	// Fill queue beyond capacity. Default capacity is 256.
	sent := 0
	dropped := 0
	for i := 0; i < defaultAdvertQueueSize*2; i++ {
		advert := parsers.AdvertPacket{
			PublicKey:  []byte{byte(i)},
			Signature:  []byte("x"),
			Timestamp:  time.Unix(int64(i), 0),
			RawPayload: []byte{byte(i)},
			NodeHash:   byte(i),
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

	db, err := OpenKamDB(dbPath)
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

func TestAdvertBatchInsertPersistsHopColumns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	records := []AdvertRecord{{
		NodeHash:    0x01,
		PublicKey:   []byte("k"),
		Timestamp:   1,
		Signature:   []byte("s"),
		RawPayload:  []byte("r"),
		HopCount:    2,
		HopStations: `["aa","bb"]`,
	}}
	if err := InsertAdvertsBatch(db, records); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	var hopCount int
	var hopStations string
	if err := db.QueryRow("SELECT hop_count, hop_stations FROM adverts WHERE node_hash = 1").Scan(&hopCount, &hopStations); err != nil {
		t.Fatalf("query: %v", err)
	}
	if hopCount != 2 {
		t.Fatalf("hop_count mismatch: expected 2, got %d", hopCount)
	}
	if hopStations != `["aa","bb"]` {
		t.Fatalf("hop_stations mismatch: got %s", hopStations)
	}
}

func TestAdvertQueueGracefulShutdownFlush(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	q := NewAdvertQueue(db)

	advert := parsers.AdvertPacket{
		PublicKey:  []byte("shutdown-key"),
		Signature:  []byte("sig"),
		Timestamp:  time.Now(),
		RawPayload: []byte("shutdown-raw"),
		NodeHash:   0xEF,
	}
	if !q.Enqueue(advert) {
		t.Fatal("enqueue failed")
	}

	// Stop before interval flush; should still persist queued advert.
	q.Stop()

	db2, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM adverts").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected shutdown flush to persist 1 advert, got %d", count)
	}
}

func TestAdvertEnqueue(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adverts.db")

	db, err := OpenKamDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	q := NewAdvertQueue(db)
	defer q.Stop()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	q.Enqueue(parsers.AdvertPacket{
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

	a := parsers.AdvertPacket{
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
		HopCount:         3,
		HopStations:      [][]byte{{0xaa}, {0xbb}, {0xcc, 0xdd}},
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
	if r.HopCount != 3 {
		t.Fatalf("hop count mismatch: expected 3, got %d", r.HopCount)
	}
	if r.HopStations != `["aa","bb","ccdd"]` {
		t.Fatalf("hop stations mismatch: got %s", r.HopStations)
	}
}
