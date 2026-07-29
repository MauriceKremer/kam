package echo

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

const (
	defaultQueueSize   = 16
	defaultSendTimeout = 500 * time.Millisecond
)

// Sender sends an outbound group text on a channel. The concrete
// *kiss/send.Sender satisfies this.
type Sender interface {
	Send(ctx context.Context, channelName, body string) error
}

// Service replies to group-text messages that mention the local node, as a
// liveness test for end-to-end messaging. It is fire-and-forget and fully
// decoupled from the router: Handle only applies cheap filters and enqueues
// the message to a bounded queue; a dedicated worker goroutine does the
// (potentially blocking) Build+Send. This guarantees Handle never blocks the
// route loop, even when the radio TX path is backed up.
type Service struct {
	sender      Sender
	nodeName    string
	mention     string
	channels    map[string]bool
	sendTimeout time.Duration

	queue  chan parsers.GroupTextMessage
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// New creates an echo Service and starts its worker goroutine. channels is the
// set of monitored channel names (normalized with a leading '#'). If mention
// is empty it defaults to "@"+nodeName. Call Stop to shut down the worker.
func New(sender Sender, nodeName string, channels []string, mention string) *Service {
	return newService(sender, nodeName, channels, mention, defaultQueueSize, defaultSendTimeout)
}

// newService is the testable constructor with explicit queue size and send
// timeout. It starts the worker goroutine.
func newService(sender Sender, nodeName string, channels []string, mention string, queueSize int, sendTimeout time.Duration) *Service {
	ch := make(map[string]bool, len(channels))
	for _, c := range channels {
		ch[normalizeChannel(c)] = true
	}
	m := mention
	if m == "" {
		m = "@" + nodeName
	}
	s := &Service{
		sender:      sender,
		nodeName:    strings.TrimSpace(nodeName),
		mention:     m,
		channels:    ch,
		sendTimeout: sendTimeout,
		queue:       make(chan parsers.GroupTextMessage, queueSize),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	go s.worker()
	return s
}

// Handle observes an inbound group text. It applies cheap filters (channel,
// self, mention) and enqueues the message non-blocking. If the worker queue is
// full the reply is dropped with a log line. Handle never blocks on Send.
func (s *Service) Handle(msg parsers.GroupTextMessage) {
	if !s.channels[normalizeChannel(msg.ChannelName)] {
		return
	}
	if msg.Sender == s.nodeName {
		return
	}
	if !containsMention(msg.Body, s.mention) {
		return
	}
	select {
	case s.queue <- msg:
	default:
		slog.Warn("[Echo] reply dropped: worker queue full",
			"channel", msg.ChannelName, "sender", msg.Sender)
	}
}

// worker drains the queue and processes replies sequentially. It exits when
// Stop is called. Sequential processing avoids concurrent crypto (Build) and
// serializes Send against the radio TX path.
func (s *Service) worker() {
	defer close(s.doneCh)
	for {
		select {
		case <-s.stopCh:
			return
		case msg := <-s.queue:
			s.process(msg)
		}
	}
}

// process builds and sends a single echo reply. Errors are logged, not
// propagated; echo is best-effort liveness.
func (s *Service) process(msg parsers.GroupTextMessage) {
	sender := msg.Sender
	if sender == "" {
		sender = "someone"
	}
	body := formatReply(sender, msg)

	ctx, cancel := context.WithTimeout(context.Background(), s.sendTimeout)
	defer cancel()
	if err := s.sender.Send(ctx, msg.ChannelName, body); err != nil {
		slog.Warn("[Echo] reply failed",
			"channel", msg.ChannelName,
			"mention_sender", msg.Sender,
			"error", err)
		return
	}
	slog.Info("[Echo] replied",
		"channel", msg.ChannelName,
		"inbound_sender", msg.Sender,
		"hop_count", msg.HopCount,
		"snr", msg.SNR,
		"rssi", msg.RSSI,
		"has_rx_meta", msg.HasRxMeta,
		"drift_seconds", msg.TimeDriftSeconds)
}

// Stop signals the worker to exit and blocks until it has. Safe to call
// multiple times. Must be called before the KISS manager is stopped so the
// worker cannot enqueue TX frames into a drained write loop.
func (s *Service) Stop() {
	s.once.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

// formatReply builds the echo reply body (without the "<nodeName>: " prefix,
// which the KISS sender adds). It reports all available per-message stats:
// hop count, radio RX quality (when the modem emitted RxMeta), and clock drift.
func formatReply(sender string, msg parsers.GroupTextMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "heard %s in %d hops", sender, msg.HopCount)
	if msg.HasRxMeta {
		fmt.Fprintf(&b, ", SNR %.2f dB, RSSI %d dBm", float64(msg.SNR)/4.0, msg.RSSI)
	}
	fmt.Fprintf(&b, ", drift %ds", msg.TimeDriftSeconds)
	return b.String()
}

// containsMention reports whether body contains mention such that the character
// immediately after the mention (if any) is not a word character. This avoids
// "@kam" matching "@kameron".
func containsMention(body, mention string) bool {
	idx := strings.Index(body, mention)
	if idx < 0 {
		return false
	}
	end := idx + len(mention)
	if end >= len(body) {
		return true
	}
	return !isWordChar(body[end])
}

func isWordChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '_':
		return true
	}
	return false
}

func normalizeChannel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	if !strings.HasPrefix(n, "#") {
		n = "#" + n
	}
	return n
}
