package parsers

import (
	"crypto/ed25519"
	"encoding/binary"
	"log/slog"
	"time"
)

// AdvertPacket represents a parsed node advertisement.
type AdvertPacket struct {
	PublicKey        []byte
	Timestamp        time.Time
	Signature        []byte
	IsChatNode       bool
	IsRepeater       bool
	IsRoomServer     bool
	IsSensor         bool
	Nickname         string
	HasLocation      bool
	Latitude         float32
	Longitude        float32
	TimeDriftSeconds int64
	SignatureValid   bool
	RawPayload       []byte
	NodeHash         byte

	// HopCount is the number of relay hops recorded in the inbound packet's
	// path field. Populated by the router, not the parser, since it is a
	// network-layer attribute parsed from the path frame outside the advert
	// payload.
	HopCount int

	// HopStations is the list of per-hop path hashes (raw bytes, size per the
	// packet's path hash size code). Populated by the router.
	HopStations [][]byte
}

// ParseAdvert decodes and verifies a MeshCore ADVERT payload.
func ParseAdvert(payload []byte) AdvertPacket {
	if len(payload) < 100 {
		slog.Warn("[Parse Advert] Dropping malformed ADVERT packet: payload too short",
			"expected_min", 100,
			"actual", len(payload),
		)
		return AdvertPacket{}
	}

	// Unpack core crypto layer components
	pubKey := payload[0:32]
	rawTimestampBytes := payload[32:36]
	signature := payload[36:100]

	// Convert timestamp to time entity separately
	rawTimestamp := binary.LittleEndian.Uint32(rawTimestampBytes)
	txTime := time.Unix(int64(rawTimestamp), 0)

	// Verify the signature by building the slice buffer accurately
	appData := payload[100:]
	// Message length is now: 32 (PubKey) + 4 (Timestamp) + len(appData)
	signedMessage := make([]byte, 36+len(appData))

	// Copy PubKey + Timestamp (bytes 0 to 36) into the front
	copy(signedMessage[0:36], payload[0:36])
	// Copy the rest of the application data after it
	copy(signedMessage[36:], appData)

	signatureValid := true
	if !ed25519.Verify(pubKey, signedMessage, signature) {
		signatureValid = false
	}

	// absolute time difference between txTime and current time should be within the timeWindow
	timeDelta := time.Since(txTime).Abs()

	advert := AdvertPacket{
		PublicKey:        pubKey,
		Timestamp:        txTime,
		Signature:        signature,
		TimeDriftSeconds: int64(timeDelta.Seconds()),
		SignatureValid:   signatureValid,
		RawPayload:       append([]byte(nil), payload...),
		NodeHash:         pubKey[0],
	}

	if len(payload) > 100 {
		flags := payload[100]
		cursor := 101

		nodeType := flags & 0x0F
		switch nodeType {
		case 0x01:
			advert.IsChatNode = true
		case 0x02:
			advert.IsRepeater = true
		case 0x03:
			advert.IsRoomServer = true
		case 0x04:
			advert.IsSensor = true
		}

		if (flags & 0x10) != 0 {
			advert.HasLocation = true
			if len(payload) >= cursor+8 {
				advert.Latitude = float32(binary.LittleEndian.Uint32(payload[cursor:cursor+4])) / 1e6
				advert.Longitude = float32(binary.LittleEndian.Uint32(payload[cursor+4:cursor+8])) / 1e6
				cursor += 8
			}
		}

		if (flags & 0x20) != 0 {
			cursor += 2
		}
		if (flags & 0x40) != 0 {
			cursor += 2
		}

		if (flags&0x80) != 0 && len(payload) > cursor {
			advert.Nickname = string(payload[cursor:])
		}
	}

	slog.Debug("[Parse Advert] Processed and Authenticated",
		"name", advert.Nickname,
		"node_type", advertNodeTypeString(advert),
		"has_gps", advert.HasLocation,
		"time_drift_sec", advert.TimeDriftSeconds,
		"signature_valid", advert.SignatureValid,
	)

	if advert.HasLocation {
		slog.Debug("[Parse Advert] Location Data",
			"latitude", advert.Latitude,
			"longitude", advert.Longitude,
		)
	}

	return advert
}

func advertNodeTypeString(a AdvertPacket) string {
	switch {
	case a.IsChatNode:
		return "chat"
	case a.IsRepeater:
		return "repeater"
	case a.IsRoomServer:
		return "room_server"
	case a.IsSensor:
		return "sensor"
	default:
		return "unknown"
	}
}
