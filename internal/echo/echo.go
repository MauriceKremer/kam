package echo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

// errSentinel is exposed for tests that need a stable sender error.
var errSentinel = errors.New("echo: send failed")

// Sender sends an outbound group text on a channel. The concrete
// *kiss/send.Sender satisfies this.
type Sender interface {
	Send(ctx context.Context, channelName, body string) error
}

// Service replies to group-text messages that mention the local node, as a
// liveness test for end-to-end messaging. It is fire-and-forget: a reply is
// best-effort and sender errors are logged, not propagated.
type Service struct {
	sender      Sender
	nodeName    string
	mention     string
	channels    map[string]bool
	sendTimeout time.Duration
}

// New creates an echo Service. channels is the set of monitored channel names
// (normalized with a leading '#'). If mention is empty it defaults to
// "@"+nodeName.
func New(sender Sender, nodeName string, channels []string, mention string) *Service {
	ch := make(map[string]bool, len(channels))
	for _, c := range channels {
		ch[normalizeChannel(c)] = true
	}
	if mention == "" {
		mention = "@" + nodeName
	}
	return &Service{
		sender:      sender,
		nodeName:    nodeName,
		mention:     mention,
		channels:    ch,
		sendTimeout: 2 * time.Second,
	}
}

// Handle observes an inbound group text and replies if it mentions the local
// node on a monitored channel. It never blocks longer than sendTimeout and
// never panics on sender errors.
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
