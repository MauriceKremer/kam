package echo

import (
	"context"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

type fakeSender struct {
	calls []fakeCall
	err   error
}

type fakeCall struct {
	channel string
	body    string
}

func (f *fakeSender) Send(_ context.Context, channel, body string) error {
	f.calls = append(f.calls, fakeCall{channel: channel, body: body})
	return f.err
}

func newTestService(t *testing.T, nodeName string, channels []string, mention string) *Service {
	t.Helper()
	s := New(&fakeSender{}, nodeName, channels, mention)
	s.sendTimeout = 100 * time.Millisecond
	return s
}

func mustHandle(t *testing.T, s *Service, msg parsers.GroupTextMessage) {
	t.Helper()
	s.Handle(msg)
}

func senderOf(s *Service) *fakeSender {
	return s.sender.(*fakeSender)
}

func TestServiceRepliesToMention(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "alice",
		Body:        "@kam are you there?",
		HopCount:    2,
	})

	calls := senderOf(s).calls
	if len(calls) != 1 {
		t.Fatalf("expected 1 reply, got %d: %+v", len(calls), calls)
	}
	if calls[0].channel != "#public" {
		t.Fatalf("reply channel: got %q, want #public", calls[0].channel)
	}
	want := "heard alice in 2 hops, drift 0s"
	if calls[0].body != want {
		t.Fatalf("reply body: got %q, want %q", calls[0].body, want)
	}
}

func TestServiceSkipsSelf(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "kam",
		Body:        "@kam ping",
		HopCount:    0,
	})
	if got := len(senderOf(s).calls); got != 0 {
		t.Fatalf("self message must not echo, got %d replies", got)
	}
}

func TestServiceSkipsNonMention(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "alice",
		Body:        "anyone there?",
		HopCount:    1,
	})
	if got := len(senderOf(s).calls); got != 0 {
		t.Fatalf("non-mention must not echo, got %d replies", got)
	}
}

func TestServiceSkipsOtherChannel(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#mesh-dev",
		Sender:      "alice",
		Body:        "@kam hi",
		HopCount:    1,
	})
	if got := len(senderOf(s).calls); got != 0 {
		t.Fatalf("non-monitored channel must not echo, got %d replies", got)
	}
}

func TestServiceRepliesWithHopCount(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "bob",
		Body:        "@kam",
		HopCount:    5,
	})
	if got := senderOf(s).calls[0].body; got != "heard bob in 5 hops, drift 0s" {
		t.Fatalf("reply body: got %q, want %q", got, "heard bob in 5 hops, drift 0s")
	}
}

func TestServiceWordBoundaryMention(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
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
		senderOf(s).calls = nil
		mustHandle(t, s, parsers.GroupTextMessage{
			ChannelName: "#public",
			Sender:      "alice",
			Body:        c.body,
			HopCount:    1,
		})
		got := len(senderOf(s).calls) == 1
		if got != c.reply {
			t.Fatalf("body %q: reply=%v want=%v", c.body, got, c.reply)
		}
	}
}

func TestServiceDefaultsMentionFromNodeName(t *testing.T) {
	s := New(&fakeSender{}, "kam", []string{"#public"}, "")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "alice",
		Body:        "@kam ping",
		HopCount:    0,
	})
	if got := len(senderOf(s).calls); got != 1 {
		t.Fatalf("default mention @<nodeName> should trigger, got %d", got)
	}
}

func TestServiceEmptySenderUsesSomeone(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "",
		Body:        "@kam",
		HopCount:    3,
	})
	if got := senderOf(s).calls[0].body; got != "heard someone in 3 hops, drift 0s" {
		t.Fatalf("empty sender reply: got %q, want %q", got, "heard someone in 3 hops, drift 0s")
	}
}

func TestServiceReplyIncludesRxMetaStats(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName:      "#public",
		Sender:           "alice",
		Body:             "@kam report",
		HopCount:         1,
		HasRxMeta:        true,
		SNR:              29, // 7.25 dB
		RSSI:             -64,
		TimeDriftSeconds: 3,
	})
	want := "heard alice in 1 hops, SNR 7.25 dB, RSSI -64 dBm, drift 3s"
	if got := senderOf(s).calls[0].body; got != want {
		t.Fatalf("reply body: got %q, want %q", got, want)
	}
}

func TestServiceReplyOmitsRxMetaWhenAbsent(t *testing.T) {
	s := newTestService(t, "kam", []string{"#public"}, "@kam")
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName:      "#public",
		Sender:           "alice",
		Body:             "@kam",
		HopCount:         0,
		HasRxMeta:        false,
		TimeDriftSeconds: 7,
	})
	want := "heard alice in 0 hops, drift 7s"
	if got := senderOf(s).calls[0].body; got != want {
		t.Fatalf("reply body: got %q, want %q", got, want)
	}
}

func TestServiceSenderErrorIsLoggedNotPropagated(t *testing.T) {
	fs := &fakeSender{err: errSentinel}
	s := New(fs, "kam", []string{"#public"}, "@kam")
	s.sendTimeout = 100 * time.Millisecond
	// Must not panic / hang; handler is fire-and-forget.
	mustHandle(t, s, parsers.GroupTextMessage{
		ChannelName: "#public",
		Sender:      "alice",
		Body:        "@kam",
		HopCount:    1,
	})
	if len(fs.calls) != 1 {
		t.Fatalf("send still attempted on error sentinel, got %d calls", len(fs.calls))
	}
}
