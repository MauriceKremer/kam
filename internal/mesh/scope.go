package mesh

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// TransportKey is a 16-byte region key.
type TransportKey [16]byte

// IsZero reports whether the key is all zero, which MeshCore treats as "no
// scope".
func (k TransportKey) IsZero() bool { return k == TransportKey{} }

// RegionKey derives the key of a public (hashtag) region: the first 16 bytes
// of SHA-256 over the name with exactly one leading '#'. A name given without
// '#' gets one prepended (RegionMap::getTransportKeysFor,
// TransportKeyStore::getAutoKeyFor).
func RegionKey(name string) TransportKey {
	name = "#" + strings.TrimPrefix(strings.TrimSpace(name), "#")
	sum := sha256.Sum256([]byte(name))
	var key TransportKey
	copy(key[:], sum[:])
	return key
}

// Code returns the 16-bit transport code for a packet: the first two bytes of
// HMAC-SHA-256(key, payload_type | payload) read little endian, with 0x0000
// and 0xFFFF reserved (TransportKey::calcTransportCode).
func (k TransportKey) Code(payloadType byte, payload []byte) uint16 {
	mac := hmac.New(sha256.New, k[:])
	mac.Write([]byte{payloadType})
	mac.Write(payload)
	code := binary.LittleEndian.Uint16(mac.Sum(nil))
	switch code {
	case 0x0000:
		return 0x0001
	case 0xFFFF:
		return 0xFFFE
	}
	return code
}
