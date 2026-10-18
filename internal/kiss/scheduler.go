package kiss

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"kam.bytecoder.nl/internal/mesh"
)

// TxPriority orders queued transmissions; lower values go first.
type TxPriority int

const (
	// TxPriorityReply is for ACKs and direct replies a client is waiting on.
	TxPriorityReply TxPriority = iota
	// TxPriorityFloodReply is for flood replies and PATH returns.
	TxPriorityFloodReply
	// TxPriorityGroupText is for group texts from the sender and automation.
	TxPriorityGroupText
	// TxPriorityPush is for room posts pushed to clients. Deferrable.
	TxPriorityPush
	// TxPriorityAdvert is for adverts. Deferrable.
	TxPriorityAdvert
)

// deferrable reports whether a transmission may wait for duty-cycle budget or
// be evicted from a full queue. ACKs, replies and group texts never are.
func (p TxPriority) deferrable() bool { return p >= TxPriorityPush }

// Transmit scheduler errors.
var (
	ErrTxQueueFull = errors.New("kiss: transmit queue full")
	ErrTxStopped   = errors.New("kiss: transmit scheduler stopped")
)

// TxRequest is one packet to transmit.
type TxRequest struct {
	// Raw is the MeshCore packet without the KISS command byte.
	Raw      []byte
	Priority TxPriority
	// Delay holds the packet back this long after submission.
	Delay time.Duration
	// Tag lets the producer cancel the packet while it is still queued.
	Tag    string
	Origin string
	Room   string
}

// TxSink is the part of Manager the scheduler drives.
type TxSink interface {
	TxChan() chan<- []byte
	SubscribeModemEvents(ModemEventHandler) (cancel func())
	TxGateTimeout() time.Duration
	MarkSeen(raw []byte)
}

// TxSchedulerConfig tunes the scheduler. Zero values pick defaults.
type TxSchedulerConfig struct {
	// MaxQueue bounds queued packets (default 128).
	MaxQueue int
	// DutyCyclePercent caps estimated airtime per DutyWindow; 0 disables the
	// cap. Only deferrable packets wait for budget.
	DutyCyclePercent float64
	// DutyWindow is the sliding accounting window (default one hour).
	DutyWindow time.Duration
	// Airtime estimates the time on air for a packet of the given length.
	Airtime func(packetBytes int) time.Duration
	// Now overrides the clock for tests.
	Now func() time.Time
}

// TxSchedulerStats is a snapshot of scheduler counters.
type TxSchedulerStats struct {
	Submitted uint64
	Sent      uint64
	Completed uint64
	Rejected  uint64
	Evicted   uint64
	Cancelled uint64
	// Deferred counts packets that had to wait at least once for duty-cycle
	// budget.
	Deferred uint64
	// GateTimeouts counts frames whose completion was never reported.
	GateTimeouts uint64
	Queued       int
	// AirtimeUsed is the estimated airtime inside the current window.
	AirtimeUsed time.Duration
}

type txItem struct {
	req       TxRequest
	seq       uint64
	notBefore time.Time
	deferred  bool
}

type airtimeEntry struct {
	at       time.Time
	duration time.Duration
}

// TxScheduler owns the radio data path. It hands the modem one frame at a
// time, waits for TxDone (or its absence) before the next, and orders waiting
// frames by priority with per-frame delay, cancellation and a duty-cycle cap.
type TxScheduler struct {
	sink TxSink
	cfg  TxSchedulerConfig

	mu            sync.Mutex
	queue         []*txItem
	ledger        []airtimeEntry
	inFlight      bool
	inFlightSince time.Time
	seq           uint64
	stopped       bool
	started       bool
	stats         TxSchedulerStats
	unsubscribe   func()

	legacy    chan []byte
	wake      chan struct{}
	completed chan struct{}
	stop      chan struct{}
	done      chan struct{}
}

// NewTxScheduler creates a scheduler for sink. Call Start to run it.
func NewTxScheduler(sink TxSink, cfg TxSchedulerConfig) *TxScheduler {
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = 128
	}
	if cfg.DutyWindow <= 0 {
		cfg.DutyWindow = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TxScheduler{
		sink:      sink,
		cfg:       cfg,
		legacy:    make(chan []byte, 16),
		wake:      make(chan struct{}, 1),
		completed: make(chan struct{}, 4),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// TxChan accepts KISS data frames (CMD_DATA | packet) from producers that
// predate the scheduler, such as send.Sender. They queue as group texts.
func (s *TxScheduler) TxChan() chan<- []byte { return s.legacy }

// Start launches the scheduling goroutine and subscribes to modem events.
func (s *TxScheduler) Start() {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	unsubscribe := s.sink.SubscribeModemEvents(func(event ModemEvent) {
		switch event.(type) {
		case TxDone, TxAbandoned:
			select {
			case s.completed <- struct{}{}:
			default:
			}
		}
	})
	s.mu.Lock()
	s.unsubscribe = unsubscribe
	s.mu.Unlock()
	go s.run()
}

// Stop ends scheduling. Queued packets are discarded.
func (s *TxScheduler) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	started := s.started
	unsubscribe := s.unsubscribe
	s.queue = nil
	s.mu.Unlock()
	close(s.stop)
	if started {
		<-s.done
		if unsubscribe != nil {
			unsubscribe()
		}
	}
}

// Submit queues a packet. It never blocks. A full queue evicts the newest
// lowest-priority deferrable packet to admit a non-deferrable one, and rejects
// a deferrable one outright.
func (s *TxScheduler) Submit(req TxRequest) error {
	if _, err := mesh.Parse(req.Raw); err != nil {
		s.mu.Lock()
		s.stats.Rejected++
		s.mu.Unlock()
		return fmt.Errorf("invalid transmit packet: %w", err)
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrTxStopped
	}
	if len(s.queue) >= s.cfg.MaxQueue && !s.makeRoomLocked(req.Priority) {
		s.stats.Rejected++
		s.mu.Unlock()
		return ErrTxQueueFull
	}
	s.seq++
	s.queue = append(s.queue, &txItem{
		req:       TxRequest{Raw: append([]byte(nil), req.Raw...), Priority: req.Priority, Delay: req.Delay, Tag: req.Tag, Origin: req.Origin, Room: req.Room},
		seq:       s.seq,
		notBefore: s.cfg.Now().Add(req.Delay),
	})
	s.stats.Submitted++
	s.mu.Unlock()
	s.signal()
	return nil
}

// makeRoomLocked frees one queue slot for a packet of the given priority.
func (s *TxScheduler) makeRoomLocked(priority TxPriority) bool {
	if priority.deferrable() {
		return false
	}
	victim := -1
	for i, it := range s.queue {
		if !it.req.Priority.deferrable() {
			continue
		}
		if victim < 0 || it.req.Priority > s.queue[victim].req.Priority ||
			(it.req.Priority == s.queue[victim].req.Priority && it.seq > s.queue[victim].seq) {
			victim = i
		}
	}
	if victim < 0 {
		return false
	}
	s.queue = append(s.queue[:victim], s.queue[victim+1:]...)
	s.stats.Evicted++
	return true
}

// Cancel removes queued packets with the tag and returns how many were
// removed. A packet already handed to the modem cannot be recalled.
func (s *TxScheduler) Cancel(tag string) int {
	if tag == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.queue[:0]
	removed := 0
	for _, it := range s.queue {
		if it.req.Tag == tag {
			removed++
			continue
		}
		kept = append(kept, it)
	}
	s.queue = kept
	s.stats.Cancelled += uint64(removed)
	return removed
}

// Stats returns a snapshot of the counters.
func (s *TxScheduler) Stats() TxSchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.Queued = len(s.queue)
	out.AirtimeUsed = s.usedLocked(s.cfg.Now())
	return out
}

func (s *TxScheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *TxScheduler) run() {
	defer close(s.done)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wait := s.pump()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if wait <= 0 {
			wait = time.Hour
		}
		timer.Reset(wait)

		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-timer.C:
		case <-s.completed:
			s.markComplete()
		case frame := <-s.legacy:
			s.submitLegacy(frame)
		}
	}
}

// markComplete records that the modem finished (or abandoned) the frame in
// flight. Completion events that arrive with nothing in flight are ignored.
func (s *TxScheduler) markComplete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		s.inFlight = false
		s.stats.Completed++
	}
}

func (s *TxScheduler) submitLegacy(frame []byte) {
	if len(frame) < 2 || frame[0] != CMD_DATA {
		s.mu.Lock()
		s.stats.Rejected++
		s.mu.Unlock()
		slog.Warn("[TxScheduler] rejecting legacy frame that is not a KISS data frame", "length", len(frame))
		return
	}
	if err := s.Submit(TxRequest{Raw: frame[1:], Priority: TxPriorityGroupText}); err != nil {
		slog.Warn("[TxScheduler] dropping legacy frame", "error", err)
	}
}

// pump hands the next eligible packet to the modem and returns how long to
// sleep before looking again (0 when only an event can change anything).
func (s *TxScheduler) pump() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return 0
	}
	now := s.cfg.Now()
	gateTimeout := s.sink.TxGateTimeout() + 500*time.Millisecond
	if s.inFlight {
		remaining := gateTimeout - now.Sub(s.inFlightSince)
		if remaining > 0 {
			return remaining
		}
		s.inFlight = false
		s.stats.GateTimeouts++
		slog.Warn("[TxScheduler] no TxDone for the last frame; continuing")
	}

	item, wait := s.nextLocked(now)
	if item == nil {
		return wait
	}
	frame := make([]byte, 0, 1+len(item.req.Raw))
	frame = append(frame, CMD_DATA)
	frame = append(frame, item.req.Raw...)
	if observed, ok := s.sink.(interface {
		TryTransmit([]byte, string, string) bool
	}); ok {
		if !observed.TryTransmit(frame, item.req.Origin, item.req.Room) {
			return 50 * time.Millisecond
		}
	} else {
		select {
		case s.sink.TxChan() <- frame:
		default:
			return 50 * time.Millisecond
		}
	}
	s.removeLocked(item)
	if item.req.Priority != TxPriorityGroupText {
		// An echo of a room packet from a repeater must not be processed as new
		// traffic. Group-text echoes keep reaching the parsers, as before.
		s.sink.MarkSeen(item.req.Raw)
	}
	s.inFlight = true
	s.inFlightSince = now
	s.stats.Sent++
	if s.cfg.Airtime != nil {
		s.ledger = append(s.ledger, airtimeEntry{at: now, duration: s.cfg.Airtime(len(item.req.Raw))})
	}
	return gateTimeout
}

// nextLocked picks the best eligible packet. When none is eligible it returns
// the time until one might become so.
func (s *TxScheduler) nextLocked(now time.Time) (*txItem, time.Duration) {
	var best *txItem
	var wake time.Duration
	note := func(d time.Duration) {
		if d <= 0 {
			d = time.Millisecond
		}
		if wake == 0 || d < wake {
			wake = d
		}
	}
	for _, it := range s.queue {
		if it.notBefore.After(now) {
			note(it.notBefore.Sub(now))
			continue
		}
		if it.req.Priority.deferrable() {
			if allowed, retry := s.budgetAllowsLocked(now, len(it.req.Raw)); !allowed {
				if !it.deferred {
					it.deferred = true
					s.stats.Deferred++
				}
				note(retry)
				continue
			}
		}
		if best == nil || it.req.Priority < best.req.Priority || (it.req.Priority == best.req.Priority && it.seq < best.seq) {
			best = it
		}
	}
	if best != nil {
		return best, 0
	}
	return nil, wake
}

func (s *TxScheduler) removeLocked(item *txItem) {
	for i, it := range s.queue {
		if it == item {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

// usedLocked prunes expired ledger entries and returns the airtime in window.
func (s *TxScheduler) usedLocked(now time.Time) time.Duration {
	cut := 0
	for cut < len(s.ledger) && now.Sub(s.ledger[cut].at) >= s.cfg.DutyWindow {
		cut++
	}
	s.ledger = s.ledger[cut:]
	var used time.Duration
	for _, e := range s.ledger {
		used += e.duration
	}
	return used
}

// budgetAllowsLocked reports whether a deferrable packet fits the duty-cycle
// budget, and when to look again if not.
func (s *TxScheduler) budgetAllowsLocked(now time.Time, packetBytes int) (bool, time.Duration) {
	if s.cfg.DutyCyclePercent <= 0 || s.cfg.Airtime == nil {
		return true, 0
	}
	used := s.usedLocked(now)
	if used == 0 {
		return true, 0
	}
	budget := time.Duration(float64(s.cfg.DutyWindow) * s.cfg.DutyCyclePercent / 100)
	if used+s.cfg.Airtime(packetBytes) <= budget {
		return true, 0
	}
	// The oldest entry leaves the window first; look again then.
	return false, s.cfg.DutyWindow - now.Sub(s.ledger[0].at)
}
