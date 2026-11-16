package db

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

const (
	defaultGroupTextQueueSize     = 256
	defaultGroupTextBatchSize     = 50
	defaultGroupTextFlushInterval = time.Second
	defaultGroupTextPruneInterval = time.Hour
)

// GroupTextQueue decouples group text ingestion from SQLite writes.
type GroupTextQueue struct {
	db *sql.DB

	ch       chan parsers.GroupTextMessage
	stopCh   chan struct{}
	stopOnce sync.Once
	done     sync.WaitGroup

	batchSize     int
	flushInterval time.Duration
	retention     GroupTextsRetention

	enqueued atomic.Int64
	dropped  atomic.Int64
	flushed  atomic.Int64
	failed   atomic.Int64
}

// NewGroupTextQueue starts a batched group-text writer against an already-open
// SQLite handle, applying retention bounds to each channel; a zero retention
// keeps every row. Call Stop to flush remaining items; the caller remains
// responsible for closing the database handle.
func NewGroupTextQueue(db *sql.DB, retention GroupTextsRetention) *GroupTextQueue {
	q := &GroupTextQueue{
		db:            db,
		ch:            make(chan parsers.GroupTextMessage, defaultGroupTextQueueSize),
		stopCh:        make(chan struct{}),
		batchSize:     defaultGroupTextBatchSize,
		flushInterval: defaultGroupTextFlushInterval,
		retention:     retention,
	}

	q.done.Add(1)
	go q.writerLoop()

	slog.Info("[GroupTextQueue] started", "capacity", defaultGroupTextQueueSize)
	return q
}

// Enqueue attempts to store a decrypted group text for batched persistence.
// Returns false without blocking when the queue is full.
func (q *GroupTextQueue) Enqueue(msg parsers.GroupTextMessage) bool {
	select {
	case q.ch <- msg:
		q.enqueued.Add(1)
		return true
	default:
		q.dropped.Add(1)
		slog.Warn("[GroupTextQueue] dropped message: queue full", "channel", msg.ChannelName)
		return false
	}
}

// Stop signals the writer to stop and flushes any buffered group texts. It is
// safe to call multiple times and does not close the database handle.
func (q *GroupTextQueue) Stop() {
	q.stopOnce.Do(func() {
		close(q.stopCh)
	})
	q.done.Wait()
}

// Metrics returns a snapshot of queue counters.
func (q *GroupTextQueue) Metrics() GroupTextMetrics {
	return GroupTextMetrics{
		Enqueued: q.enqueued.Load(),
		Dropped:  q.dropped.Load(),
		Flushed:  q.flushed.Load(),
		Failed:   q.failed.Load(),
		Depth:    len(q.ch),
	}
}

type GroupTextMetrics struct {
	Enqueued int64
	Dropped  int64
	Flushed  int64
	Failed   int64
	Depth    int
}

func (q *GroupTextQueue) writerLoop() {
	defer q.done.Done()

	ticker := time.NewTicker(q.flushInterval)
	defer ticker.Stop()

	var pruneTicker *time.Ticker
	var pruneC <-chan time.Time
	if q.retention.anyBound() {
		pruneTicker = time.NewTicker(defaultGroupTextPruneInterval)
		defer pruneTicker.Stop()
		pruneC = pruneTicker.C
		q.prune() // bounds apply to history already in the table
	}

	batch := make([]GroupTextRecord, 0, q.batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := InsertGroupTextsBatch(q.db, batch); err != nil {
			q.failed.Add(int64(len(batch)))
			slog.Error("[GroupTextQueue] batch insert failed", "error", err, "count", len(batch))
		} else {
			q.flushed.Add(int64(len(batch)))
			slog.Info("[GroupTextQueue] batch flushed", "count", len(batch))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-q.stopCh:
			drain := true
			for drain {
				select {
				case msg := <-q.ch:
					batch = append(batch, groupTextToRecord(msg))
				default:
					drain = false
				}
			}
			flush()
			return

		case msg := <-q.ch:
			batch = append(batch, groupTextToRecord(msg))
			if len(batch) >= q.batchSize {
				flush()
			}

		case <-ticker.C:
			flush()

		case <-pruneC:
			q.prune()
		}
	}
}

// prune deletes group texts outside each channel's retention bounds. Like
// AdvertQueue's prune it runs on the writer goroutine, so it never overlaps a
// batch insert.
func (q *GroupTextQueue) prune() {
	deleted, err := PruneGroupTexts(q.db, q.retention)
	if err != nil {
		slog.Error("[GroupTextQueue] prune failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("[GroupTextQueue] pruned group texts", "deleted", deleted)
	}
}

func groupTextToRecord(m parsers.GroupTextMessage) GroupTextRecord {
	return GroupTextRecord{
		ChannelName:      m.ChannelName,
		Sender:           m.Sender,
		Body:             m.Body,
		TxtType:          m.TxtType,
		Attempt:          m.Attempt,
		PublicKeyPrefix:  m.PublicKeyPrefix,
		Timestamp:        m.Timestamp.Unix(),
		TimeDriftSeconds: m.TimeDriftSeconds,
		HopCount:         m.HopCount,
		HopStations:      marshalHopStations(m.HopStations),
		SNRQuarterDB:     optionalInt(m.HasRxMeta, m.SNR),
		RSSIDBM:          optionalInt(m.HasRxMeta, m.RSSI),
	}
}

func optionalInt(ok bool, value int) *int {
	if !ok {
		return nil
	}
	return &value
}

// marshalHopStations renders per-hop path hashes as a JSON array of hex strings
// for storage in the hop_stations column. Returns "[]" when no hops were
// recorded. Malformed bytes are still hex-encoded; they never break JSON.
func marshalHopStations(stations [][]byte) string {
	hexes := make([]string, len(stations))
	for i, s := range stations {
		hexes[i] = hex.EncodeToString(s)
	}
	b, err := json.Marshal(hexes)
	if err != nil {
		// Cannot fail for []string, but guard against regressions.
		slog.Warn("[GroupTextQueue] marshal hop stations failed", "error", err)
		return "[]"
	}
	return string(b)
}
