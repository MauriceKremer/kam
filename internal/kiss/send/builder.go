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
	MaxPacketPayload = 184
	PlaintextHeader  = 5                        // 4 bytes timestamp + 1 byte flags
	PlaintextMaxLen  = MaxPacketPayload - 1 - 2 // 168
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

	if len(raw) > 255 {
		return nil, errors.New("raw packet exceeds 255 bytes")
	}

	return raw, nil
}
