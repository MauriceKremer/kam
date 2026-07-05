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
	defaultGroupTextQueueSize     = 256
	defaultGroupTextBatchSize     = 50
	defaultGroupTextFlushInterval = time.Second
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

	enqueued atomic.Int64
	dropped  atomic.Int64
	flushed  atomic.Int64
	failed   atomic.Int64
}

// NewGroupTextQueue starts a batched group-text writer against an already-open
// SQLite handle. Call Stop to flush remaining items; the caller remains
// responsible for closing the database handle.
func NewGroupTextQueue(db *sql.DB) *GroupTextQueue {
	q := &GroupTextQueue{
		db:            db,
		ch:            make(chan parsers.GroupTextMessage, defaultGroupTextQueueSize),
		stopCh:        make(chan struct{}),
		batchSize:     defaultGroupTextBatchSize,
		flushInterval: defaultGroupTextFlushInterval,
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
		}
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
	}
}
