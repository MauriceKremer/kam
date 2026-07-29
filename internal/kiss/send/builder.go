package send

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

const (
	// MaxPacketPayload is the firmware MAX_PACKET_PAYLOAD: the application-layer
	// payload (after header + path) of a MeshCore packet. See docs/packet_format.md.
	MaxPacketPayload = 184

	// CipherBudget is the max ciphertext bytes: MAX_PACKET_PAYLOAD minus the
	// 1-byte channel_hash and 2-byte MAC that prefix the ciphertext.
	CipherBudget = MaxPacketPayload - 1 - 2 // 181

	// PlaintextMaxLen is the largest padded plaintext that fits CipherBudget.
	// AES-128 block size is 16, so the padded plaintext must be a multiple of
	// 16 <= CipherBudget. 181 -> 176. Capping the UNPADDED plaintext here would
	// let a 181-byte plaintext pad to 192 bytes and blow the radio limit.
	PlaintextMaxLen = (CipherBudget / 16) * 16 // 176

	// PlaintextHeader is the fixed prefix of the plaintext: 4-byte LE timestamp
	// + 1-byte flags.
	PlaintextHeader = 5
)

// Builder creates raw MeshCore group-text radio packets.
type Builder struct {
	NodeName string
	Now      func() time.Time
}

// NewBuilder creates a Builder with the local sender name.
func NewBuilder(nodeName string) *Builder {
	return &Builder{
		NodeName: strings.TrimSpace(nodeName),
		Now:      time.Now,
	}
}

// Build returns a raw radio packet (without KISS framing) for the given channel
// and body. Header, path_length, channel_hash, MAC, and ciphertext are included.
func (b *Builder) Build(channelName, body string) ([]byte, error) {
	if b.NodeName == "" {
		return nil, errors.New("node_name is required")
	}

	secret, hash, _, ok := parsers.LookupTopic(channelName)
	if !ok {
		return nil, fmt.Errorf("unknown channel %q", channelName)
	}

	body = strings.TrimSpace(body)
	message := fmt.Sprintf("%s: %s", b.NodeName, body)
	maxMessageLen := PlaintextMaxLen - PlaintextHeader
	if len(message) > maxMessageLen {
		body = body[:maxMessageLen-len(b.NodeName)-2]
		message = fmt.Sprintf("%s: %s", b.NodeName, body)
	}

	plainLen := PlaintextHeader + len(message)
	paddedLen := plainLen
	if r := plainLen % 16; r != 0 {
		paddedLen += 16 - r
	}

	plaintext := make([]byte, paddedLen)
	binary.LittleEndian.PutUint32(plaintext[0:4], uint32(b.Now().Unix()))
	plaintext[4] = 0x00
	copy(plaintext[5:], message)

	macAndCipher := parsers.EncryptThenMAC(secret, plaintext)
	if macAndCipher == nil {
		return nil, errors.New("encryption failed")
	}

	raw := make([]byte, 0, 2+1+len(macAndCipher))
	raw = append(raw,
		(kiss.PAYLOAD_VERSION_V1<<6)|(kiss.PAYLOAD_TYPE_GRP_TXT<<2)|kiss.ROUTE_TYPE_FLOOD,
		0x00,
		hash,
	)
	raw = append(raw, macAndCipher...)

	// Defense in depth: the plaintext cap above should keep the application
	// payload within the radio limit, but assert both bounds explicitly so a
	// future constant change cannot silently produce packets the firmware drops.
	if appPayload := len(raw) - 2; appPayload > MaxPacketPayload {
		return nil, fmt.Errorf("app payload %d exceeds MAX_PACKET_PAYLOAD %d", appPayload, MaxPacketPayload)
	}
	if len(raw) > 255 {
		return nil, errors.New("raw packet exceeds 255 bytes")
	}

	return raw, nil
}
