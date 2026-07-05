package db

import (
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

const (
	defaultAdvertQueueSize     = 256
	defaultAdvertBatchSize     = 50
	defaultAdvertFlushInterval = time.Second
)

// AdvertQueue decouples advert ingestion from SQLite writes.
type AdvertQueue struct {
	db *sql.DB

	ch       chan parsers.AdvertPacket
	stopCh   chan struct{}
	stopOnce sync.Once
	done     sync.WaitGroup

	batchSize     int
	flushInterval time.Duration

	enqueued atomic.Int64
	dropped  atomic.Int64
	flushed  atomic.Int64
	failed   atomic.Int64
}

// NewAdvertQueue starts a batched advert writer against an already-open SQLite
// handle. Call Stop to flush remaining items; the caller remains responsible for
// closing the database handle.
func NewAdvertQueue(db *sql.DB) *AdvertQueue {
	q := &AdvertQueue{
		db:            db,
		ch:            make(chan parsers.AdvertPacket, defaultAdvertQueueSize),
		stopCh:        make(chan struct{}),
		batchSize:     defaultAdvertBatchSize,
		flushInterval: defaultAdvertFlushInterval,
	}

	q.done.Add(1)
	go q.writerLoop()

	slog.Info("[AdvertQueue] started", "capacity", defaultAdvertQueueSize)
	return q
}

// Enqueue attempts to store an advert for batched persistence. Returns false
// without blocking when the queue is full. Metrics counter is incremented.
func (q *AdvertQueue) Enqueue(advert parsers.AdvertPacket) bool {
	select {
	case q.ch <- advert:
		q.enqueued.Add(1)
		return true
	default:
		q.dropped.Add(1)
		slog.Warn("[AdvertQueue] dropped advert: queue full", "nickname", advert.Nickname)
		return false
	}
}

// Stop signals the writer to stop and flushes any buffered adverts. It is
// safe to call multiple times and does not close the database handle.
func (q *AdvertQueue) Stop() {
	q.stopOnce.Do(func() {
		close(q.stopCh)
	})
	q.done.Wait()
}

// Metrics returns a snapshot of queue counters.
func (q *AdvertQueue) Metrics() AdvertMetrics {
	return AdvertMetrics{
		Enqueued: q.enqueued.Load(),
		Dropped:  q.dropped.Load(),
		Flushed:  q.flushed.Load(),
		Failed:   q.failed.Load(),
		Depth:    len(q.ch),
	}
}

type AdvertMetrics struct {
	Enqueued int64
	Dropped  int64
	Flushed  int64
	Failed   int64
	Depth    int
}

func (q *AdvertQueue) writerLoop() {
	defer q.done.Done()

	ticker := time.NewTicker(q.flushInterval)
	defer ticker.Stop()

	batch := make([]AdvertRecord, 0, q.batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := InsertAdvertsBatch(q.db, batch); err != nil {
			q.failed.Add(int64(len(batch)))
			slog.Error("[AdvertQueue] batch insert failed", "error", err, "count", len(batch))
		} else {
			q.flushed.Add(int64(len(batch)))
			slog.Info("[AdvertQueue] batch flushed", "count", len(batch))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-q.stopCh:
			// Drain remaining queued items before final flush.
			drain := true
			for drain {
				select {
				case advert := <-q.ch:
					batch = append(batch, advertToRecord(advert))
				default:
					drain = false
				}
			}
			flush()
			return

		case advert := <-q.ch:
			batch = append(batch, advertToRecord(advert))
			if len(batch) >= q.batchSize {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}

func advertToRecord(a parsers.AdvertPacket) AdvertRecord {
	return AdvertRecord{
		NodeHash:         a.NodeHash,
		PublicKey:        a.PublicKey,
		Timestamp:        a.Timestamp.Unix(),
		Signature:        a.Signature,
		SignatureValid:   a.SignatureValid,
		IsChatNode:       a.IsChatNode,
		IsRepeater:       a.IsRepeater,
		IsRoomServer:     a.IsRoomServer,
		IsSensor:         a.IsSensor,
		Nickname:         a.Nickname,
		HasLocation:      a.HasLocation,
		Latitude:         a.Latitude,
		Longitude:        a.Longitude,
		TimeDriftSeconds: a.TimeDriftSeconds,
		RawPayload:       a.RawPayload,
	}
}
