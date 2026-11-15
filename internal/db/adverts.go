package db

import (
	"database/sql"
	"fmt"
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
	defaultAdvertPruneInterval = time.Hour
)

// AdvertRetention bounds the adverts table; a zero field disables that bound.
// The caller derives it from the adverts section of kam.yml.
type AdvertRetention struct {
	KeepDays int64
	MaxKept  int64
}

// PruneAdverts deletes adverts not heard for keepDays (0 keeps all), then
// every advert beyond the newest-heard maxKept rows (0 keeps all). What goes
// is the oldest last-heard; rows are refreshed when a node is reheard, so
// active nodes survive and stale ghosts go first.
func PruneAdverts(db *sql.DB, keepDays, maxKept int64) (int64, error) {
	var deleted int64
	if keepDays > 0 {
		result, err := db.Exec(`DELETE FROM adverts WHERE received_at < unixepoch() - ?`, keepDays*86400)
		if err != nil {
			return deleted, fmt.Errorf("prune adverts by age: %w", err)
		}
		n, _ := result.RowsAffected()
		deleted += n
	}
	if maxKept > 0 {
		// Everything after the newest-heard maxKept rows, in that order: the
		// oldest-heard go first.
		result, err := db.Exec(`DELETE FROM adverts WHERE id IN (
  SELECT id FROM adverts ORDER BY received_at DESC, id DESC LIMIT -1 OFFSET ?)`, maxKept)
		if err != nil {
			return deleted, fmt.Errorf("prune adverts by count: %w", err)
		}
		n, _ := result.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// AdvertQueue decouples advert ingestion from SQLite writes.
type AdvertQueue struct {
	db *sql.DB
	// retention bounds the stored advert history; a zero struct never prunes.
	retention AdvertRetention

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
// handle with the given retention bounds; call Stop to flush remaining items.
// The caller remains responsible for closing the database handle.
func NewAdvertQueue(db *sql.DB, retention AdvertRetention) *AdvertQueue {
	q := &AdvertQueue{
		db:            db,
		ch:            make(chan parsers.AdvertPacket, defaultAdvertQueueSize),
		stopCh:        make(chan struct{}),
		batchSize:     defaultAdvertBatchSize,
		flushInterval: defaultAdvertFlushInterval,
		retention:     retention,
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

	if q.retention.KeepDays > 0 || q.retention.MaxKept > 0 {
		q.prune()
	}
	pruneTicker := time.NewTicker(defaultAdvertPruneInterval)
	defer pruneTicker.Stop()

	ticker := time.NewTicker(q.flushInterval)
	defer ticker.Stop()

	batch := make([]AdvertRecord, 0, q.batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := UpsertAdvertsBatch(q.db, batch); err != nil {
			q.failed.Add(int64(len(batch)))
			slog.Error("[AdvertQueue] batch write failed", "error", err, "count", len(batch))
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

		case <-pruneTicker.C:
			q.prune()
		}
	}
}

// prune deletes adverts outside the retention bounds. It runs on the writer
// goroutine, so it never races the batch inserts.
func (q *AdvertQueue) prune() {
	deleted, err := PruneAdverts(q.db, q.retention.KeepDays, q.retention.MaxKept)
	if err != nil {
		slog.Error("[AdvertQueue] prune failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("[AdvertQueue] pruned adverts", "deleted", deleted)
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
		HopCount:         a.HopCount,
		HopStations:      marshalHopStations(a.HopStations),
	}
}
