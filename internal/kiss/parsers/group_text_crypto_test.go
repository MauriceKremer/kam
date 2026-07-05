package parsers

import (
	"bytes"
	"crypto/aes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestAESECBEncryptRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)

	plaintext := []byte("hello world!1234") // exactly 16 bytes
	ciphertext := AESECBEncrypt(secret[:16], plaintext)

	if len(ciphertext) != len(plaintext) {
		t.Fatalf("ciphertext length %d != plaintext length %d", len(ciphertext), len(plaintext))
	}

	block, _ := aes.NewCipher(secret[:16])
	decrypted := make([]byte, 16)
	block.Decrypt(decrypted, ciphertext)
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypt mismatch: %x != %x", decrypted, plaintext)
	}
}

func TestEncryptThenMACHasMACAndCiphertext(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)

	plaintext := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	out := EncryptThenMAC(secret, plaintext)

	if len(out) < 2+16 {
		t.Fatalf("output too short: %d", len(out))
	}

	mac := out[:2]
	ciphertext := out[2:]
	if len(ciphertext)%16 != 0 {
		t.Fatalf("ciphertext not block-aligned: %d", len(ciphertext))
	}

	// Verify existing macThenDecrypt accepts it.
	pt := macThenDecrypt(secret, out)
	if pt == nil {
		t.Fatal("macThenDecrypt failed to verify own MAC")
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("plaintext mismatch: %x != %x", pt, plaintext)
	}

	_ = mac
}

func TestChannelHashForPublicChannel(t *testing.T) {
	secret := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(secret, psk)

	h := ChannelHash(secret)
	// Hash input is only first 16 bytes because trailing bytes are zero.
	expected := sha256FirstByte(secret[:16])
	if h != expected {
		t.Fatalf("channel hash mismatch: got 0x%02x, want 0x%02x", h, expected)
	}
}

func TestChannelHashForFull32ByteKey(t *testing.T) {
	secret := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}

	h := ChannelHash(secret)
	expected := sha256FirstByte(secret)
	if h != expected {
		t.Fatalf("channel hash mismatch: got 0x%02x, want 0x%02x", h, expected)
	}
}

func sha256FirstByte(data []byte) byte {
	sum := sha256.Sum256(data)
	return sum[0]
}
