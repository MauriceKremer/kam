package kiss

import (
	"crypto/ed25519"
	"encoding/binary"
	"log/slog"
	"time"
)

type AdvertPacket struct {
	PublicKey        []byte
	Timestamp        time.Time
	Signature        []byte
	IsChatNode       bool
	Nickname         string
	HasLocation      bool
	Latitude         float32
	Longitude        float32
	TimeDriftSeconds int64
	SignatureValid   bool
}

func (m *Manager) handleAdvert(payload []byte) {
	if len(payload) < 100 {
		slog.Warn("[Parse Advert] Dropping malformed ADVERT packet: payload too short",
			"expected_min", 100,
			"actual", len(payload),
		)
		return
	}

	// Unpack core crypto layer components
	pubKey := payload[0:32]
	rawTimestampBytes := payload[32:36] // FIXED: Maintain slice reference for verification
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
	}

	if len(payload) > 100 {
		flags := payload[100]
		cursor := 101

		advert.IsChatNode = (flags & 0x01) != 0

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

	slog.Info("[Parse Advert] Processed and Authenticated",
		"name", advert.Nickname,
		"can_chat", advert.IsChatNode,
		"has_gps", advert.HasLocation,
		"time_drift_sec", advert.TimeDriftSeconds,
		"signature_valid", advert.SignatureValid,
	)

	if advert.HasLocation {
		slog.Info("[Parse Advert] Location Data",
			"latitude", advert.Latitude,
			"longitude", advert.Longitude,
		)
	}

	if m.AdvertStore != nil {
		m.AdvertStore.Enqueue(advert)
	}
}
