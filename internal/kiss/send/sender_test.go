package send

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

func TestSenderPayloadRoundTrip(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	raw, err := b.Build("#public", "round trip")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	appPayload := raw[2:]
	msg, ok := parsers.ParseGroupText(appPayload)
	if !ok {
		t.Fatal("parse failed")
	}
	if msg.Body != "round trip" {
		t.Fatalf("body mismatch: got %q", msg.Body)
	}
}

func TestSenderKISSFrameFormat(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	raw, err := b.Build("#public", "hello from sender")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	frame := append([]byte{kiss.CMD_DATA}, raw...)
	framed := kiss.Frame(frame)

	if framed[0] != 0xC0 || framed[len(framed)-1] != 0xC0 {
		t.Fatal("invalid KISS frame boundaries")
	}
	if framed[1] != 0x00 {
		t.Fatalf("expected CMD_DATA 0x00, got 0x%02x", framed[1])
	}
}

func TestSenderContextCancel(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	m := &kiss.Manager{}
	sender := NewSender(m, "kam")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sender.Send(ctx, "#public", "hi")
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSenderSendEnqueuesFrame(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	m := &kiss.Manager{}
	m.TestInit()
	sender := NewSender(m, "kam")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := sender.Send(ctx, "#public", "queued"); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case frame := <-m.TestTxChan():
		if len(frame) == 0 || frame[0] != kiss.CMD_DATA {
			t.Fatalf("unexpected frame: %x", frame)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for frame")
	}
}

func TestLongBodyTruncationRoundTrip(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("node")
	body := strings.Repeat("a", 300)
	raw, err := b.Build("#public", body)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	appPayload := raw[2:]
	msg, ok := parsers.ParseGroupText(appPayload)
	if !ok {
		t.Fatal("parse failed")
	}
	if len(msg.Body) > PlaintextMaxLen-PlaintextHeader-len("node: ") {
		t.Fatalf("body too long: %d", len(msg.Body))
	}
}

func TestSenderUnknownChannel(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	_, err := b.Build("#missing", "hi")
	if err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

func TestChannelHashHex(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)

	h := parsers.ChannelHash(secret)
	if h == 0 {
		t.Fatal("channel hash must not be zero")
	}
}
