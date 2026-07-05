package kiss

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

type RouteType byte
type PayloadType byte
type PayloadVersion byte

// Bits 0-1 - 2-bits - Route Type
const (
	ROUTE_TYPE_TRANSPORT_FLOOD  = 0x00 // Flood Routing + Transport Codes
	ROUTE_TYPE_FLOOD            = 0x01 // Flood Routing
	ROUTE_TYPE_DIRECT           = 0x02 // Direct Routing
	ROUTE_TYPE_TRANSPORT_DIRECT = 0x03 // Direct Routing + Transport Codes
)

// Bits 2-5 - 4-bits - Payload Type
const (
	PAYLOAD_TYPE_REQ        = 0x00 // Request (destination/source hashes + MAC)
	PAYLOAD_TYPE_RESPONSE   = 0x01 // Response to REQ or ANON_REQ
	PAYLOAD_TYPE_TXT_MSG    = 0x02 // Plain text message
	PAYLOAD_TYPE_ACK        = 0x03 // Acknowledgment
	PAYLOAD_TYPE_ADVERT     = 0x04 // Node advertisement
	PAYLOAD_TYPE_GRP_TXT    = 0x05 // Group text message (unverified)
	PAYLOAD_TYPE_GRP_DATA   = 0x06 // Group datagram (unverified)
	PAYLOAD_TYPE_ANON_REQ   = 0x07 // Anonymous request
	PAYLOAD_TYPE_PATH       = 0x08 // Returned path
	PAYLOAD_TYPE_TRACE      = 0x09 // Trace a path, collecting SNR for each hop
	PAYLOAD_TYPE_MULTIPART  = 0x0A // Packet is part of a sequence of packets
	PAYLOAD_TYPE_CONTROL    = 0x0B // Control packet data (unencrypted)
	PAYLOAD_TYPE_RESERVED1  = 0x0C // reserved
	PAYLOAD_TYPE_RESERVED2  = 0x0D // reserved
	PAYLOAD_TYPE_RESERVED3  = 0x0E // reserved
	PAYLOAD_TYPE_RAW_CUSTOM = 0x0F // Custom packet (raw bytes, custom encryption)
)

// Bits 6-7 - 2-bits - Payload Version
const (
	PAYLOAD_VERSION_V1 = 0x00 // v1 - 1-byte src/dest hashes, 2-byte MAC
	PAYLOAD_VERSION_V2 = 0x01 // v2 - Future version (e.g., 2-byte hashes, 4-byte MAC)
	PAYLOAD_VERSION_V3 = 0x02 // v3 - Future version
	PAYLOAD_VERSION_V4 = 0x03 // v4 - Future version
)

func (m *Manager) routeLoop(ctx context.Context) {
	for {
		select {
		case <-m.stopChan:
			return
		case <-ctx.Done():
			return
		case frame := <-m.rxChan:
			if len(frame) == 0 {
				continue
			}

			cmdByte := frame[0]
			payload := frame[1:]

			switch cmdByte {
			case CMD_SETHARDWARE:
				if len(payload) == 0 {
					continue
				}
				subCmd := payload[0]
				subPayload := payload[1:]
				// Dispatch by sub-command for SetHardware extensions
				m.registry.Dispatch(subCmd, subPayload)

			case CMD_DATA:

				// Packet payload must be at least 2 bytes to read the header safely
				if len(payload) < 2 {
					slog.Info("[Router] Warning: Received malformed packet with insufficient payload length")
					continue
				}

				slog.Debug("[Router] Received packet", "cmdByte", cmdByte, "payload", hex.EncodeToString(payload))

				header := payload[0]

				// 1. Decode Header bit-masks
				routeType := header & 0x03
				payloadType := (header >> 2) & 0x0F
				version := (header >> 6) & 0x03

				if version != PAYLOAD_VERSION_V1 {
					slog.Info("[Router] Warning: Received unsupported packet version", "version", version)
					continue
				}

				slog.Info("[Router] Decoded packet header...", "routeType", RouteType(routeType), "payloadType", PayloadType(payloadType), "version", PayloadVersion(version))

				// --- Dynamic Header Slicing Logic ---
				cursor := 1 // Account for the 1-byte header already processed

				if routeType == ROUTE_TYPE_TRANSPORT_FLOOD || routeType == ROUTE_TYPE_TRANSPORT_DIRECT {
					// Skip the 4-byte transport codes block
					cursor += 4
				}

				// Parse the variable-length path frame present for all route types
				if cursor < len(payload) {
					pathFlags := payload[cursor]
					cursor++ // Consume the path flags byte

					// Extract number of hops from the lower 6 bits
					numHops := int(pathFlags & 0x3F)

					// Determine the hash size per hop from the upper 2 bits
					hashSize := 1
					switch (pathFlags >> 6) & 0x03 {
					case 0:
						hashSize = 1
					case 1:
						hashSize = 2
					case 2:
						hashSize = 3
					case 3:
						slog.Warn("[Router] Invalid path hash size code 0b11, dropping packet")
						continue
					}

					// Skip the accumulated node hop hashes
					cursor += numHops * hashSize
				}

				// Safety check to avoid slicing out of bounds
				if cursor > len(payload) {
					slog.Info("[Router] Warning: Packet layout truncated or malformed parsing network headers")
					continue
				}

				// Extract the clean application-layer payload
				appPayload := payload[cursor:]

				// Perform deduplication immediately upon receiving raw data
				// Pass the 'payload' (which starts with the header byte) to the filter
				if m.isDuplicate(appPayload) {
					slog.Info("[Router] Dropping duplicate packet")
					continue
				}

				slog.Info("[Router] Dispatching packet...", "payload", hex.EncodeToString(appPayload))

				// 2. Dispatch to the appropriate handler
				switch payloadType {
				case PAYLOAD_TYPE_ADVERT:
					advert := parsers.ParseAdvert(appPayload)
					if m.Store != nil {
						m.Store.EnqueueAdvert(advert)
					}
				case PAYLOAD_TYPE_GRP_TXT:
					msg, ok := parsers.ParseGroupText(appPayload)
					if ok && m.Store != nil {
						m.Store.EnqueueGroupText(msg)
					}
				default:
					slog.Info("[Router] Warning: Received unhandled payload type", "payloadType", PayloadType(payloadType))
				} // Closes switch payloadType
			} // Closes switch cmdByte
		} // Closes select
	} // Closes for
} // Closes routeLoop function

func (rt RouteType) String() string {
	switch rt {
	case ROUTE_TYPE_TRANSPORT_FLOOD:
		return "Transport Flood"
	case ROUTE_TYPE_FLOOD:
		return "Flood"
	case ROUTE_TYPE_DIRECT:
		return "Direct"
	case ROUTE_TYPE_TRANSPORT_DIRECT:
		return "Transport Direct"
	default:
		return "Unknown"
	}
}

func (pt PayloadType) String() string {
	switch pt {
	case PAYLOAD_TYPE_REQ:
		return "Request"
	case PAYLOAD_TYPE_RESPONSE:
		return "Response"
	case PAYLOAD_TYPE_TXT_MSG:
		return "Text Message"
	case PAYLOAD_TYPE_ACK:
		return "Acknowledgment"
	case PAYLOAD_TYPE_ADVERT:
		return "Advertisement"
	case PAYLOAD_TYPE_GRP_TXT:
		return "Group Text Message"
	case PAYLOAD_TYPE_GRP_DATA:
		return "Group Datagram"
	case PAYLOAD_TYPE_ANON_REQ:
		return "Anonymous Request"
	case PAYLOAD_TYPE_PATH:
		return "Path"
	case PAYLOAD_TYPE_TRACE:
		return "Trace"
	case PAYLOAD_TYPE_MULTIPART:
		return "Multipart"
	case PAYLOAD_TYPE_CONTROL:
		return "Control Packet Data"
	case PAYLOAD_TYPE_RESERVED1, PAYLOAD_TYPE_RESERVED2, PAYLOAD_TYPE_RESERVED3:
		return "Reserved"
	case PAYLOAD_TYPE_RAW_CUSTOM:
		return "Raw Custom Packet"
	default:
		return "Unknown"
	}
}

func (pv PayloadVersion) String() string {
	switch pv {
	case PAYLOAD_VERSION_V1:
		return "v1"
	case PAYLOAD_VERSION_V2:
		return "v2"
	case PAYLOAD_VERSION_V3:
		return "v3"
	case PAYLOAD_VERSION_V4:
		return "v4"
	default:
		return "Unknown"
	}
}

func (m *Manager) isDuplicate(payload []byte) bool {
	// 1. Generate hash from the raw bytes
	h := sha256.Sum256(payload)
	hash := hex.EncodeToString(h[:])

	// 2. Check if we've seen it
	for _, seen := range m.seenHashes {
		if seen == hash {
			return true
		}
	}

	// 3. Update the buffer (FIFO)
	if len(m.seenHashes) >= 10 {
		m.seenHashes = m.seenHashes[1:] // Pop oldest
	}
	m.seenHashes = append(m.seenHashes, hash) // Push newest

	return false
}
