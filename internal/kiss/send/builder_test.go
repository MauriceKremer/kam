package send

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

func TestBuilderPublicChannelRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")
	parsers.ResetTopicRegistry()

	b := NewBuilder("kam")
	b.Now = func() time.Time { return time.Now() }

	raw, err := b.Build("#public", "hello")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if len(raw) < 3 {
		t.Fatalf("raw packet too short: %x", raw)
	}

	header := raw[0]
	if header != 0x15 {
		t.Fatalf("header mismatch: got 0x%02x, want 0x15", header)
	}

	pathLen := raw[1]
	if pathLen != 0x00 {
		t.Fatalf("path_length mismatch: got 0x%02x, want 0x00", pathLen)
	}

	appPayload := raw[2:]
	msg, ok := parsers.ParseGroupText(appPayload)
	if !ok {
		t.Fatal("round-trip parse failed")
	}
	if msg.Sender != "kam" {
		t.Fatalf("sender mismatch: got %q, want %q", msg.Sender, "kam")
	}
	if msg.Body != "hello" {
		t.Fatalf("body mismatch: got %q, want %q", msg.Body, "hello")
	}
	if msg.ChannelName != "#public" {
		t.Fatalf("channel mismatch: got %q, want #public", msg.ChannelName)
	}
	if msg.Timestamp.Unix() == 0 {
		t.Fatal("timestamp should be set")
	}
}

func TestBuilderDerivedChannelRoundTrip(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.RegisterTopic("#mesh-dev")

	b := NewBuilder("alice")
	b.Now = func() time.Time { return time.Now() }

	raw, err := b.Build("#mesh-dev", "derived secret works")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	appPayload := raw[2:]
	msg, ok := parsers.ParseGroupText(appPayload)
	if !ok {
		t.Fatal("round-trip parse failed")
	}
	if msg.Sender != "alice" {
		t.Fatalf("sender mismatch: got %q", msg.Sender)
	}
	if msg.Body != "derived secret works" {
		t.Fatalf("body mismatch: got %q", msg.Body)
	}
	if msg.ChannelName != "#mesh-dev" {
		t.Fatalf("channel mismatch: got %q", msg.ChannelName)
	}
}

func TestBuilderTruncatesLongBody(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("longname")
	// "longname: " = 10 bytes. Need plaintext ≤ 168 bytes. Message max = 158 bytes.
	body := strings.Repeat("x", 300)

	raw, err := b.Build("#public", body)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	appPayload := raw[2:]
	msg, ok := parsers.ParseGroupText(appPayload)
	if !ok {
		t.Fatal("round-trip parse failed")
	}

	wantLen := PlaintextMaxLen - len("longname: ") - PlaintextHeader
	if len(msg.Body) != wantLen {
		t.Fatalf("body length: got %d, want %d", len(msg.Body), wantLen)
	}
}

func TestBuilderRejectsUnknownChannel(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	_, err := b.Build("#unknown", "hi")
	if err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

func TestBuilderRawPacketMaxSize(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	raw, err := b.Build("#public", strings.Repeat("x", 300))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(raw) > 255 {
		t.Fatalf("raw packet too long: %d", len(raw))
	}
}

// TestBuilderAppPayloadFitsRadioLimit guards the AES-128 block-padding budget.
// The radio firmware drops packets whose payload (after header+path) exceeds
// MAX_PACKET_PAYLOAD (184). channel_hash(1)+mac(2)+ciphertext must stay <= 184,
// so the padded plaintext must be the largest multiple of 16 <= 181 = 176.
// A naive cap of 181 on the unpadded plaintext lets a 176-byte message pad to
// 192 bytes of ciphertext -> 195-byte payload -> firmware drop.
func TestBuilderAppPayloadFitsRadioLimit(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	raw, err := b.Build("#public", strings.Repeat("x", 300))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	appPayload := raw[2:] // skip header + path_length
	if len(appPayload) > MaxPacketPayload {
		t.Fatalf("appPayload %d exceeds MAX_PACKET_PAYLOAD %d; firmware would drop this packet",
			len(appPayload), MaxPacketPayload)
	}
}

func TestBuilderMaxMessageRoundTripFitsLimit(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	raw, err := b.Build("#public", strings.Repeat("x", 300))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	appPayload := raw[2:]
	// At the cap, padded plaintext = 176, ciphertext = 176, appPayload = 1+2+176 = 179.
	if len(appPayload) != 1+2+176 {
		t.Fatalf("expected appPayload 179 at cap, got %d", len(appPayload))
	}
	// Round-trip must still succeed.
	msg, ok := parsers.ParseGroupText(appPayload)
	if !ok {
		t.Fatal("round-trip parse failed at max length")
	}
	if msg.Sender != "kam" {
		t.Fatalf("sender: got %q", msg.Sender)
	}
}

func TestBuilderFlagsAndTimestampEncoding(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	fixed := time.Unix(0x11223344, 0)
	b.Now = func() time.Time { return fixed }

	raw, err := b.Build("#public", "test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	appPayload := raw[2:]
	macAndData := appPayload[1:]
	const macSize = 2
	ciphertext := macAndData[macSize:]

	// Decrypt manually to inspect plaintext prefix.
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)

	pt, err := parsers.AESECBDecrypt(secret[:16], ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	ts := binary.LittleEndian.Uint32(pt[0:4])
	if ts != 0x11223344 {
		t.Fatalf("timestamp mismatch: got 0x%08x, want 0x11223344", ts)
	}
	if pt[4] != 0x00 {
		t.Fatalf("flags mismatch: got 0x%02x, want 0x00", pt[4])
	}
	msg := pt[5:]
	if i := bytes.IndexByte(msg, 0); i >= 0 {
		msg = msg[:i]
	}
	if string(msg) != "kam: test" {
		t.Fatalf("message mismatch: got %q", string(msg))
	}
}

func TestKISSFrame(t *testing.T) {
	parsers.ResetTopicRegistry()
	parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	b := NewBuilder("kam")
	raw, err := b.Build("#public", "hi")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	frame := kiss.Frame(append([]byte{kiss.CMD_DATA}, raw...))
	if len(frame) == 0 {
		t.Fatal("empty frame")
	}
	if frame[0] != 0xC0 {
		t.Fatalf("frame must start with FEND 0xC0, got 0x%02x", frame[0])
	}
	if frame[len(frame)-1] != 0xC0 {
		t.Fatalf("frame must end with FEND 0xC0, got 0x%02x", frame[len(frame)-1])
	}
	if frame[1] != 0x00 {
		t.Fatalf("KISS command must be CMD_DATA 0x00, got 0x%02x", frame[1])
	}
}
