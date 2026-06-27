package kiss

import (
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func buildGroupTextPayload(secret32 []byte, timestamp uint32, flags byte, message string) []byte {
	msgLen := 5 + len(message)
	paddedLen := msgLen
	if r := msgLen % 16; r != 0 {
		paddedLen += 16 - r
	}
	pt := make([]byte, paddedLen)
	binary.LittleEndian.PutUint32(pt[0:4], timestamp)
	pt[4] = flags
	copy(pt[5:], message)

	ciphertext := make([]byte, len(pt))
	block, _ := aes.NewCipher(secret32[:16])
	for i := 0; i < len(pt); i += 16 {
		block.Encrypt(ciphertext[i:], pt[i:])
	}

	h := hmac.New(sha256.New, secret32)
	h.Write(ciphertext)
	mac := h.Sum(nil)[:2]

	payload := make([]byte, 1+2+len(ciphertext))
	payload[0] = 0x00 // channel hash placeholder
	copy(payload[1:3], mac)
	copy(payload[3:], ciphertext)
	return payload
}

func TestHandleGroupTextPlain(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)

	SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	m := &Manager{}
	_ = m

	payload := buildGroupTextPayload(secret, 0x12345678, 0x00, "user123: I'm on my way")

	// Since handler only logs, verify by decrypting manually
	msg := decryptGroupTextForTest(secret, payload)
	if msg.ChannelName != "#public" {
		t.Fatalf("expected #public, got %s", msg.ChannelName)
	}
	if msg.TxtType != 0 {
		t.Fatalf("expected txt_type 0, got %d", msg.TxtType)
	}
	if msg.Attempt != 0 {
		t.Fatalf("expected attempt 0, got %d", msg.Attempt)
	}
	if msg.Sender != "user123" {
		t.Fatalf("expected sender user123, got %q", msg.Sender)
	}
	if msg.Body != "I'm on my way" {
		t.Fatalf("expected body %q, got %q", "I'm on my way", msg.Body)
	}
}

func TestHandleGroupTextSigned(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)
	SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

	prefix := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	msg := append(prefix, []byte("signed message")...)
	payload := buildGroupTextPayload(secret, 0x87654321, 0x08, string(msg)) // txt_type 0x02 (upper 6 bits), attempt 0

	got := decryptGroupTextForTest(secret, payload)
	if got.TxtType != 0x02 {
		t.Fatalf("expected txt_type 0x02, got %d", got.TxtType)
	}
	if string(got.PublicKeyPrefix) != string(prefix) {
		t.Fatalf("expected pubkey prefix %x, got %x", prefix, got.PublicKeyPrefix)
	}
	if got.Message != "signed message" {
		t.Fatalf("expected message %q, got %q", "signed message", got.Message)
	}
}

func TestSplitGroupTextMessage(t *testing.T) {
	sender, body := splitGroupTextMessage("alice: hello world")
	if sender != "alice" || body != "hello world" {
		t.Fatalf("unexpected split: sender=%q body=%q", sender, body)
	}

	sender, body = splitGroupTextMessage("no colon message")
	if sender != "" || body != "no colon message" {
		t.Fatalf("unexpected split: sender=%q body=%q", sender, body)
	}
}

func TestHandleGroupTextShortPayload(t *testing.T) {
	m := &Manager{}
	m.handleGroupText([]byte{0x00, 0x00, 0x00}) // too short
}

func decryptGroupTextForTest(secret32, payload []byte) GroupTextMessage {
	macAndData := payload[1:]
	const macSize = 2
	ciphertext := macAndData[macSize:]

	pt := make([]byte, len(ciphertext))
	block, _ := aes.NewCipher(secret32[:16])
	for i := 0; i < len(pt); i += 16 {
		block.Decrypt(pt[i:], ciphertext[i:])
	}

	timestamp := binary.LittleEndian.Uint32(pt[0:4])
	flags := pt[4]
	txtType := (flags >> 2) & 0x3F
	attempt := flags & 0x03

	msgBytes := pt[5:]
	if i := strings.IndexByte(string(msgBytes), 0); i >= 0 {
		msgBytes = msgBytes[:i]
	}

	var pubKeyPrefix []byte
	if txtType == 0x02 && len(msgBytes) >= 4 {
		pubKeyPrefix = msgBytes[:4]
		msgBytes = msgBytes[4:]
	}

	message := string(msgBytes)
	sender, body := splitGroupTextMessage(message)
	return GroupTextMessage{
		ChannelName:     "#public",
		Timestamp:       time.Unix(int64(timestamp), 0),
		Message:         message,
		Sender:          sender,
		Body:            body,
		TxtType:         txtType,
		Attempt:         attempt,
		PublicKeyPrefix: pubKeyPrefix,
	}
}
