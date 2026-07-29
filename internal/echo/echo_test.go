package echo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

var errSentinel = errors.New("send failed")

type fakeSender struct {
	mu    sync.Mutex
	calls []fakeCall
	err   error
}

type fakeCall struct {
	channel string
	body    string
}

func (f *fakeSender) Send(_ context.Context, channel, body string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{channel: channel, body: body})
	f.mu.Unlock()
	return f.err
}

func (f *fakeSender) callsSnapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// blockingSender blocks every Send call until release is closed, then records
// the call. entered is closed on the first Send so tests can wait until the
// worker is known to be inside Send. releaseSend is idempotent so test bodies
// and t.Cleanup can both safely release.
type blockingSender struct {
	once        sync.Once
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	mu          sync.Mutex
	calls       []fakeCall
}

func (b *blockingSender) Send(ctx context.Context, channel, body string) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		// Treat ctx cancel as release for recording purposes in tests.
	}
	b.mu.Lock()
	b.calls = append(b.calls, fakeCall{channel: channel, body: body})
	b.mu.Unlock()
	return nil
}

func (b *blockingSender) releaseSend() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingSender) callsSnapshot() []fakeCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]fakeCall, len(b.calls))
	copy(out, b.calls)
	return out
}

func newTestService(t *testing.T, sender Sender, nodeName string, channels []string, mention string) *Service {
	t.Helper()
	s := newService(sender, nodeName, channels, mention, 16, 100*time.Millisecond)
	t.Cleanup(s.Stop)
	return s
}

// waitForReplies polls the sender until want calls are recorded or the deadline.
func waitForReplies(t *testing.T, w callsViewer, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(w.callsSnapshot()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout: got %d replies, want %d", len(w.callsSnapshot()), want)
}

// assertNoReply waits a short grace period and fails if any reply arrived.
func assertNoReply(t *testing.T, w callsViewer) {
	t.Helper()
	time.Sleep(80 * time.Millisecond)
	if got := len(w.callsSnapshot()); got != 0 {
		t.Fatalf("expected no reply, got %d: %+v", got, w.callsSnapshot())
	}
}

type callsViewer interface {
	callsSnapshot() []fakeCall
}

func mentionMsg(sender, body string, hops int) parsers.GroupTextMessage {
	return parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      sender,
		Body:        body,
		HopCount:    hops,
	}
}

func TestServiceRepliesToMention(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	s.Handle(mentionMsg("alice", "@kam are you there?", 2))

	waitForReplies(t, fs, 1)
	calls := fs.callsSnapshot()
	if calls[0].channel != "#public" {
		t.Fatalf("reply channel: got %q, want #public", calls[0].channel)
	}
	want := "heard alice in 2 hops, drift 0s"
	if calls[0].body != want {
		t.Fatalf("reply body: got %q, want %q", calls[0].body, want)
	}
}

func TestServiceSkipsSelf(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	s.Handle(mentionMsg("kam", "@kam ping", 0))
	assertNoReply(t, fs)
}

func TestServiceSkipsNonMention(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	s.Handle(mentionMsg("alice", "anyone there?", 1))
	assertNoReply(t, fs)
}

func TestServiceSkipsOtherChannel(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	msg := mentionMsg("alice", "@kam hi", 1)
	msg.ChannelName = "#mesh-dev"
	s.Handle(msg)
	assertNoReply(t, fs)
}

func TestServiceRepliesWithHopCount(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	s.Handle(mentionMsg("bob", "@kam", 5))
	waitForReplies(t, fs, 1)
	if got := fs.callsSnapshot()[0].body; got != "heard bob in 5 hops, drift 0s" {
		t.Fatalf("reply body: got %q, want %q", got, "heard bob in 5 hops, drift 0s")
	}
}

func TestServiceWordBoundaryMention(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	cases := []struct {
		body  string
		reply bool
	}{
		{"@kam are you there?", true},
		{"@kam", true},
		{"hey @kam!", true},
		{"@kameron nope", false},
		{"@kam_x nope", false},
		{"prefix @kam", true},
	}
	for _, c := range cases {
		fs.mu.Lock()
		fs.calls = fs.calls[:0]
		fs.mu.Unlock()
		s.Handle(mentionMsg("alice", c.body, 1))
		if c.reply {
			waitForReplies(t, fs, 1)
		} else {
			assertNoReply(t, fs)
		}
	}
}

func TestServiceDefaultsMentionFromNodeName(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "")
	s.Handle(mentionMsg("alice", "@kam ping", 0))
	waitForReplies(t, fs, 1)
}

func TestServiceEmptySenderUsesSomeone(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	msg := mentionMsg("", "@kam", 3)
	s.Handle(msg)
	waitForReplies(t, fs, 1)
	if got := fs.callsSnapshot()[0].body; got != "heard someone in 3 hops, drift 0s" {
		t.Fatalf("empty sender reply: got %q, want %q", got, "heard someone in 3 hops, drift 0s")
	}
}

func TestServiceReplyIncludesRxMetaStats(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	msg := mentionMsg("alice", "@kam report", 1)
	msg.HasRxMeta = true
	msg.SNR = 29
	msg.RSSI = -64
	msg.TimeDriftSeconds = 3
	s.Handle(msg)
	waitForReplies(t, fs, 1)
	want := "heard alice in 1 hops, SNR 7.25 dB, RSSI -64 dBm, drift 3s"
	if got := fs.callsSnapshot()[0].body; got != want {
		t.Fatalf("reply body: got %q, want %q", got, want)
	}
}

func TestServiceReplyOmitsRxMetaWhenAbsent(t *testing.T) {
	fs := &fakeSender{}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	msg := mentionMsg("alice", "@kam", 0)
	msg.TimeDriftSeconds = 7
	s.Handle(msg)
	waitForReplies(t, fs, 1)
	want := "heard alice in 0 hops, drift 7s"
	if got := fs.callsSnapshot()[0].body; got != want {
		t.Fatalf("reply body: got %q, want %q", got, want)
	}
}

func TestServiceSenderErrorIsLoggedNotPropagated(t *testing.T) {
	fs := &fakeSender{err: errSentinel}
	s := newTestService(t, fs, "kam", []string{"#public"}, "@kam")
	s.Handle(mentionMsg("alice", "@kam", 1))
	waitForReplies(t, fs, 1)
}

func TestServiceHandleDoesNotBlockOnSlowSender(t *testing.T) {
	bs := &blockingSender{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	// Long send timeout so the worker stays blocked inside Send, not ctx.
	s := newService(bs, "kam", []string{"#public"}, "@kam", 16, 2*time.Second)
	t.Cleanup(func() { bs.releaseSend(); s.Stop() })

	start := time.Now()
	s.Handle(mentionMsg("alice", "@kam slow?", 1))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Handle must not block on a slow sender; blocked %v", d)
	}

	// The reply is still pending (worker blocked). Release and expect it.
	bs.releaseSend()
	waitForReplies(t, bs, 1)
}

func TestServiceDropsWhenWorkerQueueFull(t *testing.T) {
	bs := &blockingSender{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	// queue size 1: worker takes msg1 and blocks in Send; msg2 fills the
	// queue; msg3 must be dropped, not block Handle.
	s := newService(bs, "kam", []string{"#public"}, "@kam", 1, 2*time.Second)
	t.Cleanup(func() { bs.releaseSend(); s.Stop() })

	s.Handle(mentionMsg("alice", "@kam one", 1))
	<-bs.entered // worker is now blocked inside Send(msg1); queue is empty

	// msg2 fills the single-slot queue.
	s.Handle(mentionMsg("alice", "@kam two", 2))

	// msg3 must drop without blocking Handle.
	start := time.Now()
	s.Handle(mentionMsg("alice", "@kam three", 3))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Handle must not block when queue full; blocked %v", d)
	}

	// Release the worker; only msg1 and msg2 should have been sent (msg3 dropped).
	bs.releaseSend()
	waitForReplies(t, bs, 2)
}

func TestServiceStopIsIdempotentAndExitsWorker(t *testing.T) {
	fs := &fakeSender{}
	s := newService(fs, "kam", []string{"#public"}, "@kam", 4, 100*time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.Stop()
		s.Stop() // second call must not panic / hang
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within timeout")
	}
}

func TestServiceStopStopsProcessing(t *testing.T) {
	fs := &fakeSender{}
	s := newService(fs, "kam", []string{"#public"}, "@kam", 4, 100*time.Millisecond)
	s.Handle(mentionMsg("alice", "@kam", 1))
	waitForReplies(t, fs, 1)
	before := len(fs.callsSnapshot())
	s.Stop()
	// After Stop, new Handle calls just enqueue (or drop) but the worker is
	// gone, so no new reply can be produced.
	s.Handle(mentionMsg("alice", "@kam after stop", 1))
	time.Sleep(80 * time.Millisecond)
	if after := len(fs.callsSnapshot()); after != before {
		t.Fatalf("expected no new reply after Stop, got %d new", after-before)
	}
}
