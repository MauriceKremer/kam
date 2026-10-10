package mesh

import (
	"crypto/sha256"
	"unicode/utf8"
)

// Text message types (txt_type, the upper six bits of the flags byte).
const (
	TxtTypePlain       = 0x00
	TxtTypeCLIData     = 0x01
	TxtTypeSignedPlain = 0x02
)

// AckHash returns the 4-byte acknowledgement for a text or request: the first
// four bytes of SHA-256(data | pubkey), where pubkey is the sender's key.
func AckHash(data, pubKey []byte) [4]byte {
	h := sha256.New()
	h.Write(data)
	h.Write(pubKey)
	var out [4]byte
	copy(out[:], h.Sum(nil))
	return out
}

// TruncateUTF8 returns at most max bytes of s without splitting a UTF-8
// sequence. Invalid bytes are kept as they are.
func TruncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
