package kiss

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/mesh"
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
				// RxMeta (0xF9) is sent unsolicited right after each data frame;
				// attach it to the pending group text instead of routing to the
				// response registry.
				if subCmd == SUB_RX_META && len(subPayload) >= 2 {
					m.attachRxMeta(int(int8(subPayload[0])), int(int8(subPayload[1])))
					continue
				}
				m.dispatchHardwareResponse(subCmd, subPayload)

			case CMD_DATA:
				// Packet payload must be at least 2 bytes to read the header safely
				if len(payload) < 2 {
					slog.Info("[Router] Warning: Received malformed packet with insufficient payload length")
					continue
				}

				slog.Debug("[Router] Received packet", "cmdByte", cmdByte, "payload", hex.EncodeToString(payload))
				m.routePacket(payload)
			} // Closes switch cmdByte
		} // Closes select
	} // Closes for
} // Closes routeLoop function

// routePacket decodes one inbound radio packet, drops duplicates, and
// dispatches it by payload type. Adverts and group texts keep their dedicated
// paths; peer packets go to the packet tap.
func (m *Manager) routePacket(raw []byte) {
	pkt, err := mesh.Parse(raw)
	if err != nil {
		slog.Warn("[Router] Dropping malformed packet", "error", err)
		return
	}
	if pkt.Version() != PAYLOAD_VERSION_V1 {
		slog.Info("[Router] Warning: Received unsupported packet version", "version", pkt.Version())
		return
	}

	slog.Info("[Router] Decoded packet header...", "routeType", RouteType(pkt.RouteType()), "payloadType", PayloadType(pkt.PayloadType()), "version", PayloadVersion(pkt.Version()))

	// Capture the per-hop path hashes (relay node identity hashes) so handlers
	// and persistence can report which stations relayed the packet.
	numHops := pkt.HashCount()
	var pathStations [][]byte
	if numHops > 0 {
		hashSize := pkt.HashSize()
		pathStations = make([][]byte, 0, numHops)
		for i := 0; i < numHops; i++ {
			pathStations = append(pathStations, append([]byte(nil), pkt.Path[i*hashSize:(i+1)*hashSize]...))
		}
	}

	appPayload := pkt.Payload
	if m.dedup.Check(pkt.PayloadType(), appPayload) {
		slog.Info("[Router] Dropping duplicate packet")
		return
	}

	slog.Info("[Router] Dispatching packet...", "payload", hex.EncodeToString(appPayload))

	switch pkt.PayloadType() {
	case PAYLOAD_TYPE_ADVERT:
		advert := parsers.ParseAdvert(appPayload)
		advert.HopCount = numHops
		advert.HopStations = pathStations
		if m.Store != nil {
			m.Store.EnqueueAdvert(advert)
		}
	case PAYLOAD_TYPE_GRP_TXT:
		msg, ok := parsers.ParseGroupText(appPayload)
		if ok {
			msg.HopCount = numHops
			msg.HopStations = pathStations
			// Stage for dispatch and wait briefly for the modem's RxMeta
			// (0xF9) frame so SNR/RSSI can be attached. holdGroupText
			// enqueues to Store and fires the tap once meta arrives or the
			// timeout elapses.
			m.holdGroupText(msg)
		}
	case PAYLOAD_TYPE_REQ, PAYLOAD_TYPE_TXT_MSG, PAYLOAD_TYPE_ACK, PAYLOAD_TYPE_ANON_REQ, PAYLOAD_TYPE_PATH, PAYLOAD_TYPE_MULTIPART:
		m.dispatchPacket(RxPacket{Packet: pkt, ReceivedAt: time.Now()})
	default:
		slog.Info("[Router] Warning: Received unhandled payload type", "payloadType", PayloadType(pkt.PayloadType()))
	}
}

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

func (m *Manager) dispatchHardwareResponse(command byte, payload []byte) {
	if command == SUB_TX_DONE {
		if len(payload) < 1 {
			m.counters.rxUnmatchedFrames.Add(1)
			slog.Debug("malformed TxDone event", "payload", payload)
			return
		}
		done := TxDone{OK: payload[0] != 0}
		if done.OK {
			m.counters.txDoneOK.Add(1)
		} else {
			m.counters.txDoneFailed.Add(1)
		}
		select {
		case m.txGateEvents <- txGateEvent{done: true}:
		default:
			slog.Warn("TX gate event queue full; TxDone may not release transmission")
		}
		m.publishModemEvent(done)
		return
	}

	m.requestMu.Lock()
	if m.barrier {
		if command != SUB_RESP_PONG {
			m.requestMu.Unlock()
			m.counters.rxUnmatchedFrames.Add(1)
			return
		}
		m.barrier = false
		m.unsynced = false
	}

	request := m.pending
	if command == SUB_ERROR {
		code := byte(0)
		if len(payload) > 0 {
			code = payload[0]
		}
		if code == MODEM_ERROR_TX_BUSY {
			m.counters.txBusy.Add(1)
		}
		if request != nil {
			select {
			case request.result <- requestResult{err: &ModemError{Code: code}}:
			default:
			}
			m.requestMu.Unlock()
			return
		}
		m.requestMu.Unlock()
		if code == MODEM_ERROR_TX_BUSY {
			select {
			case m.txGateEvents <- txGateEvent{busy: true}:
			default:
				slog.Warn("TX gate event queue full; TxBusy may not be handled")
			}
			m.publishModemEvent(TxBusy{})
		} else {
			m.publishModemEvent(ModemFault{Code: code})
		}
		return
	}
	if request == nil {
		m.requestMu.Unlock()
		m.counters.rxUnmatchedFrames.Add(1)
		slog.Debug("unmatched modem response", "command", command, "payload", payload)
		return
	}
	if request == nil {
		m.requestMu.Unlock()
		m.counters.rxUnmatchedFrames.Add(1)
		slog.Debug("unmatched modem response", "command", command, "payload", payload)
		return
	}
	if _, ok := request.expected[command]; !ok {
		m.requestMu.Unlock()
		m.counters.rxUnmatchedFrames.Add(1)
		slog.Debug("unexpected modem response", "command", command, "payload", payload)
		return
	}
	select {
	case request.result <- requestResult{command: command, payload: append([]byte(nil), payload...)}:
	default:
	}
	m.requestMu.Unlock()
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
