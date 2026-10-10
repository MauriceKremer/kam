package kiss

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/mesh"
)

type fakeTxSink struct {
	tx   chan []byte
	gate time.Duration

	mu       sync.Mutex
	handlers []ModemEventHandler
	marked   [][]byte
}

func newFakeTxSink() *fakeTxSink {
	return &fakeTxSink{tx: make(chan []byte, 16), gate: time.Second}
}

func (f *fakeTxSink) TxChan() chan<- []byte        { return f.tx }
func (f *fakeTxSink) TxGateTimeout() time.Duration { return f.gate }
func (f *fakeTxSink) SubscribeModemEvents(h ModemEventHandler) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
	return func() {}
}
func (f *fakeTxSink) MarkSeen(raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, append([]byte(nil), raw...))
}
func (f *fakeTxSink) emit(e ModemEvent) {
	f.mu.Lock()
	handlers := append([]ModemEventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range handlers {
		h(e)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testPacket(tag byte) []byte {
	raw, err := mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadTxtMsg), Payload: []byte{tag, 0x55}}.Encode()
	if err != nil {
		panic(err)
	}
	return raw
}

func frameOf(raw []byte) []byte { return append([]byte{CMD_DATA}, raw...) }

// drain sends everything the scheduler is willing to send right now, one frame
// per completion, and returns the packets in order.
func drain(t *testing.T, s *TxScheduler, sink *fakeTxSink) [][]byte {
	t.Helper()
	var out [][]byte
	for i := 0; i < 64; i++ {
		s.pump()
		select {
		case frame := <-sink.tx:
			out = append(out, frame[1:])
			s.markComplete()
		default:
			return out
		}
	}
	t.Fatal("scheduler kept sending")
	return nil
}

func newSyncScheduler(sink *fakeTxSink, cfg TxSchedulerConfig, clock *fakeClock) *TxScheduler {
	cfg.Now = clock.Now
	return NewTxScheduler(sink, cfg)
}

func TestSchedulerSendsOneFrameAtATimeUntilCompletion(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	for tag := byte(1); tag <= 3; tag++ {
		if err := s.Submit(TxRequest{Raw: testPacket(tag), Priority: TxPriorityReply}); err != nil {
			t.Fatal(err)
		}
	}
	s.pump()
	if len(sink.tx) != 1 {
		t.Fatalf("modem has %d frames queued, want exactly 1 in flight", len(sink.tx))
	}
	s.pump()
	if len(sink.tx) != 1 {
		t.Fatal("scheduler sent a second frame before the first completed")
	}
	if got := <-sink.tx; !reflect.DeepEqual(got, frameOf(testPacket(1))) {
		t.Fatalf("first frame: %x", got)
	}
	s.markComplete()
	s.pump()
	if got := <-sink.tx; !reflect.DeepEqual(got, frameOf(testPacket(2))) {
		t.Fatalf("second frame: %x", got)
	}
	if st := s.Stats(); st.Sent != 2 || st.Completed != 1 || st.Queued != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if len(sink.marked) != 2 {
		t.Fatalf("MarkSeen called %d times, want 2", len(sink.marked))
	}
}

func TestSchedulerMarksRoomPacketsSeenButNotGroupTexts(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityGroupText})
	_ = s.Submit(TxRequest{Raw: testPacket(2), Priority: TxPriorityPush})
	drain(t, s, sink)
	if len(sink.marked) != 1 || !reflect.DeepEqual(sink.marked[0], testPacket(2)) {
		t.Fatalf("marked %x; only the room packet may be marked so group-text echoes keep reaching the parsers", sink.marked)
	}
}

func TestSchedulerOrdersByPriorityThenArrival(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	submit := func(tag byte, p TxPriority) {
		if err := s.Submit(TxRequest{Raw: testPacket(tag), Priority: p}); err != nil {
			t.Fatal(err)
		}
	}
	submit(1, TxPriorityAdvert)
	submit(2, TxPriorityPush)
	submit(3, TxPriorityGroupText)
	submit(4, TxPriorityFloodReply)
	submit(5, TxPriorityReply)
	submit(6, TxPriorityReply)
	submit(7, TxPriorityPush)

	var order []byte
	for _, raw := range drain(t, s, sink) {
		order = append(order, raw[len(raw)-2])
	}
	if want := []byte{5, 6, 4, 3, 2, 7, 1}; !reflect.DeepEqual(order, want) {
		t.Fatalf("send order %v, want %v", order, want)
	}
}

func TestSchedulerHonoursDelayAndLateHighPriority(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityReply, Delay: 200 * time.Millisecond})
	_ = s.Submit(TxRequest{Raw: testPacket(2), Priority: TxPriorityPush})

	// The delayed reply is not eligible yet, so the lower-priority push goes first.
	if got := drain(t, s, sink); len(got) != 1 || got[0][len(got[0])-2] != 2 {
		t.Fatalf("before the delay: %x", got)
	}
	if wait := s.pump(); wait < 150*time.Millisecond || wait > 200*time.Millisecond {
		t.Fatalf("scheduler should sleep until the delayed packet is due, got %v", wait)
	}
	clock.Advance(200 * time.Millisecond)
	if got := drain(t, s, sink); len(got) != 1 || got[0][len(got[0])-2] != 1 {
		t.Fatalf("after the delay: %x", got)
	}
}

func TestSchedulerCancelRemovesQueuedByTag(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityPush, Tag: "push:a"})
	_ = s.Submit(TxRequest{Raw: testPacket(2), Priority: TxPriorityPush, Tag: "push:b"})
	_ = s.Submit(TxRequest{Raw: testPacket(3), Priority: TxPriorityPush, Tag: "push:a"})
	if n := s.Cancel("push:a"); n != 2 {
		t.Fatalf("cancelled %d, want 2", n)
	}
	if n := s.Cancel(""); n != 0 {
		t.Fatal("empty tag must cancel nothing")
	}
	got := drain(t, s, sink)
	if len(got) != 1 || got[0][len(got[0])-2] != 2 {
		t.Fatalf("remaining: %x", got)
	}
	if st := s.Stats(); st.Cancelled != 2 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestSchedulerDutyCycleDefersOnlyDeferrablePackets(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{
		DutyCyclePercent: 10, DutyWindow: time.Hour,
		Airtime: func(int) time.Duration { return 100 * time.Second },
	}, clock)

	// The budget is 360 s. The reply uses 100 s, two adverts bring it to 300 s,
	// and the next advert would exceed the budget.
	for tag := byte(1); tag <= 4; tag++ {
		_ = s.Submit(TxRequest{Raw: testPacket(tag), Priority: TxPriorityAdvert})
	}
	_ = s.Submit(TxRequest{Raw: testPacket(9), Priority: TxPriorityReply})

	got := drain(t, s, sink)
	if len(got) != 3 {
		t.Fatalf("sent %d packets, want 3 (reply and two adverts)", len(got))
	}
	if got[0][len(got[0])-2] != 9 {
		t.Fatalf("reply must go first: %x", got[0])
	}
	if st := s.Stats(); st.Queued != 2 || st.Deferred != 2 {
		t.Fatalf("after budget: %+v", st)
	}

	// Replies and ACKs ignore the budget even when it is spent.
	_ = s.Submit(TxRequest{Raw: testPacket(10), Priority: TxPriorityReply})
	if got := drain(t, s, sink); len(got) != 1 {
		t.Fatalf("reply was held back by the duty cycle: %x", got)
	}

	// The deferred adverts are released, in order, once the window has moved on.
	if wait := s.pump(); wait <= 0 || wait > time.Hour {
		t.Fatalf("deferred packet needs a retry time, got %v", wait)
	}
	clock.Advance(time.Hour)
	got = drain(t, s, sink)
	if len(got) != 2 || got[0][len(got[0])-2] != 3 || got[1][len(got[1])-2] != 4 {
		t.Fatalf("deferred adverts not released in order: %x", got)
	}
}

func TestSchedulerDutyCycleDisabledAndOversizedFrame(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{Airtime: func(int) time.Duration { return time.Hour }}, clock)
	for tag := byte(1); tag <= 3; tag++ {
		_ = s.Submit(TxRequest{Raw: testPacket(tag), Priority: TxPriorityAdvert})
	}
	if got := drain(t, s, sink); len(got) != 3 {
		t.Fatalf("with the cap disabled sent %d, want 3", len(got))
	}

	// A single frame longer than the whole budget must still be sendable.
	s = newSyncScheduler(sink, TxSchedulerConfig{DutyCyclePercent: 1, Airtime: func(int) time.Duration { return time.Hour }}, clock)
	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityAdvert})
	if got := drain(t, s, sink); len(got) != 1 {
		t.Fatal("frame larger than the budget was deferred forever")
	}
}

func TestSchedulerBoundedQueue(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{MaxQueue: 2}, clock)

	if err := s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityAdvert}); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(TxRequest{Raw: testPacket(2), Priority: TxPriorityPush}); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(TxRequest{Raw: testPacket(3), Priority: TxPriorityPush}); !errors.Is(err, ErrTxQueueFull) {
		t.Fatalf("deferrable packet on a full queue: got %v, want ErrTxQueueFull", err)
	}
	// An ACK evicts the lowest-priority deferrable packet (the advert).
	if err := s.Submit(TxRequest{Raw: testPacket(4), Priority: TxPriorityReply}); err != nil {
		t.Fatalf("ACK rejected while deferrable packets are queued: %v", err)
	}
	if err := s.Submit(TxRequest{Raw: testPacket(5), Priority: TxPriorityReply}); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(TxRequest{Raw: testPacket(6), Priority: TxPriorityReply}); !errors.Is(err, ErrTxQueueFull) {
		t.Fatalf("queue of only non-deferrable packets: got %v, want ErrTxQueueFull", err)
	}
	var order []byte
	for _, raw := range drain(t, s, sink) {
		order = append(order, raw[len(raw)-2])
	}
	if want := []byte{4, 5}; !reflect.DeepEqual(order, want) {
		t.Fatalf("surviving packets %v, want %v", order, want)
	}
	if st := s.Stats(); st.Evicted != 2 || st.Rejected != 2 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestSchedulerRejectsInvalidPacketsAndStoppedState(t *testing.T) {
	sink := newFakeTxSink()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	for name, raw := range map[string][]byte{
		"empty":         nil,
		"header only":   {0x11},
		"no payload":    {0x11, 0x00},
		"bad path size": {0x11, 0xC0, 1, 2},
		"oversized":     append([]byte{0x11, 0x00}, make([]byte, mesh.MaxPacketPayload+1)...),
	} {
		if err := s.Submit(TxRequest{Raw: raw, Priority: TxPriorityReply}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if st := s.Stats(); st.Rejected != 5 || st.Queued != 0 {
		t.Fatalf("stats: %+v", st)
	}
	s.Stop()
	if err := s.Submit(TxRequest{Raw: testPacket(1)}); !errors.Is(err, ErrTxStopped) {
		t.Fatalf("submit after stop: %v", err)
	}
	s.Stop() // idempotent
}

func TestSchedulerContinuesAfterMissingTxDone(t *testing.T) {
	sink := newFakeTxSink()
	sink.gate = 2 * time.Second
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s := newSyncScheduler(sink, TxSchedulerConfig{}, clock)
	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityReply})
	_ = s.Submit(TxRequest{Raw: testPacket(2), Priority: TxPriorityReply})

	s.pump()
	<-sink.tx
	if wait := s.pump(); wait != 2500*time.Millisecond {
		t.Fatalf("scheduler should wait gate timeout plus margin, got %v", wait)
	}
	clock.Advance(2400 * time.Millisecond)
	s.pump()
	if len(sink.tx) != 0 {
		t.Fatal("sent before the gate timeout")
	}
	clock.Advance(200 * time.Millisecond)
	s.pump()
	if got := <-sink.tx; !reflect.DeepEqual(got, frameOf(testPacket(2))) {
		t.Fatalf("second frame after timeout: %x", got)
	}
	if st := s.Stats(); st.GateTimeouts != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestSchedulerRunsAgainstModemEvents(t *testing.T) {
	sink := newFakeTxSink()
	s := NewTxScheduler(sink, TxSchedulerConfig{})
	s.Start()
	defer s.Stop()

	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityReply})
	_ = s.Submit(TxRequest{Raw: testPacket(2), Priority: TxPriorityReply})
	if got := awaitFrame(t, sink.tx); !reflect.DeepEqual(got, frameOf(testPacket(1))) {
		t.Fatalf("first frame: %x", got)
	}
	select {
	case got := <-sink.tx:
		t.Fatalf("second frame sent before TxDone: %x", got)
	case <-time.After(50 * time.Millisecond):
	}
	sink.emit(TxBusy{}) // informational only
	sink.emit(TxDone{OK: true})
	if got := awaitFrame(t, sink.tx); !reflect.DeepEqual(got, frameOf(testPacket(2))) {
		t.Fatalf("second frame: %x", got)
	}
	sink.emit(TxAbandoned{})
	_ = s.Submit(TxRequest{Raw: testPacket(3), Priority: TxPriorityReply})
	if got := awaitFrame(t, sink.tx); !reflect.DeepEqual(got, frameOf(testPacket(3))) {
		t.Fatalf("frame after abandoned: %x", got)
	}
}

func TestSchedulerLegacyChannelQueuesGroupTexts(t *testing.T) {
	sink := newFakeTxSink()
	s := NewTxScheduler(sink, TxSchedulerConfig{})
	s.Start()
	defer s.Stop()

	s.TxChan() <- []byte{CMD_SETHARDWARE, SUB_PING} // not a data frame
	s.TxChan() <- []byte{CMD_DATA}                  // no packet
	s.TxChan() <- frameOf(testPacket(7))
	if got := awaitFrame(t, sink.tx); !reflect.DeepEqual(got, frameOf(testPacket(7))) {
		t.Fatalf("legacy frame: %x", got)
	}
	if st := s.Stats(); st.Rejected != 2 || st.Sent != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestSchedulerSleepsUntilDelayedPacketIsDue(t *testing.T) {
	sink := newFakeTxSink()
	s := NewTxScheduler(sink, TxSchedulerConfig{})
	s.Start()
	defer s.Stop()
	start := time.Now()
	_ = s.Submit(TxRequest{Raw: testPacket(1), Priority: TxPriorityReply, Delay: 80 * time.Millisecond})
	awaitFrame(t, sink.tx)
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("sent after %v, delay was 80ms", elapsed)
	}
}

func TestWriterPublishesTxAbandonedWhenGateTimesOut(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	m.csma = CSMAConfig{}
	m.setAirtime(1) // gate timeout of 3 ms
	m.writeHook = func([]byte) error { return nil }
	events := make(chan ModemEvent, 4)
	m.SubscribeModemEvents(func(e ModemEvent) { events <- e })
	go m.writeLoop()
	go m.modemEventLoop()
	defer close(m.stopChan)

	m.TxChan() <- []byte{CMD_DATA, 0x01}
	select {
	case e := <-events:
		if _, ok := e.(TxAbandoned); !ok {
			t.Fatalf("event %T, want TxAbandoned", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no TxAbandoned event after the gate timed out")
	}
}

func awaitFrame(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case frame := <-ch:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a transmitted frame")
		return nil
	}
}

func TestLoRaAirtime(t *testing.T) {
	// 166 bytes at SF7 / 62.5 kHz / CR 4/5 is about 0.53 s (docs/FeatureRoomServer.md R3).
	got := LoRaAirtime(7, 62_500, 5, 166)
	if got < 520*time.Millisecond || got > 545*time.Millisecond {
		t.Fatalf("SF7/62.5k/166B airtime = %v, want about 533ms", got)
	}
	// Semtech calculator reference: SF7, 125 kHz, CR 4/5, 20 bytes, 8-symbol preamble = 56.58 ms.
	if got := LoRaAirtime(7, 125_000, 5, 20); got < 56*time.Millisecond || got > 57*time.Millisecond {
		t.Fatalf("SF7/125k/20B airtime = %v, want about 56.6ms", got)
	}
	// SF12 at 125 kHz uses low-data-rate optimisation: 20 bytes take about 1.32 s.
	if got := LoRaAirtime(12, 125_000, 5, 20); got < 1300*time.Millisecond || got > 1340*time.Millisecond {
		t.Fatalf("SF12/125k/20B airtime = %v, want about 1.32s", got)
	}
	if LoRaAirtime(7, 125_000, 5, 200) <= LoRaAirtime(7, 125_000, 5, 20) {
		t.Fatal("airtime must grow with the payload")
	}
	for _, bad := range []func() time.Duration{
		func() time.Duration { return LoRaAirtime(4, 125_000, 5, 20) },
		func() time.Duration { return LoRaAirtime(13, 125_000, 5, 20) },
		func() time.Duration { return LoRaAirtime(7, 0, 5, 20) },
		func() time.Duration { return LoRaAirtime(7, 125_000, 4, 20) },
		func() time.Duration { return LoRaAirtime(7, 125_000, 9, 20) },
		func() time.Duration { return LoRaAirtime(7, 125_000, 5, -1) },
	} {
		if bad() != 0 {
			t.Fatal("invalid parameters must yield zero airtime")
		}
	}
}
