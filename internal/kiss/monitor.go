package kiss

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

type monitorObserver struct {
	sink     models.MonitorSink
	mu       sync.Mutex
	pending  *models.MonitorEvent
	timer    *time.Timer
	txLabels map[*byte]struct{ origin, room string }
	rooms    map[byte]string
}

// SetMonitor installs the event recorder. The radio settings are kept as a
// separate atom so a radio change (PUT /v1/modem/radio) can update them
// without rebuilding the rest of the observer.
func (m *Manager) SetMonitor(sink models.MonitorSink, radio models.RadioConfig, addresses ...map[byte]string) {
	if sink == nil {
		return
	}
	m.UpdateRadioSettings(radio)
	rooms := make(map[byte]string)
	if len(addresses) > 0 {
		for hash, id := range addresses[0] {
			rooms[hash] = id
		}
	}
	m.observer.Store(&monitorObserver{sink: sink, txLabels: make(map[*byte]struct{ origin, room string }), rooms: rooms})
}

// UpdateRadioSettings recomputes airtime estimates after the radio settings
// were applied again, without touching the observer and its locks.
func (m *Manager) UpdateRadioSettings(radio models.RadioConfig) {
	model := radio
	m.radioModel.Store(&model)
}

func (m *Manager) TryTransmit(frame []byte, origin, room string) bool {
	observer := m.observer.Load()
	if observer == nil || len(frame) == 0 {
		select {
		case m.txChan <- frame:
			return true
		default:
			return false
		}
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.txLabels[&frame[0]] = struct{ origin, room string }{origin, room}
	select {
	case m.txChan <- frame:
		return true
	default:
		delete(observer.txLabels, &frame[0])
		return false
	}
}

func recordObservation(observer *monitorObserver, event models.MonitorEvent) {
	defer func() {
		if recover() != nil {
			slog.Error("monitor recorder panicked")
		}
	}()
	observer.sink.Record(event)
}

func (m *Manager) flushObservation() {
	observer := m.observer.Load()
	if observer == nil {
		return
	}
	observer.mu.Lock()
	pending := observer.pending
	observer.pending = nil
	if observer.timer != nil {
		observer.timer.Stop()
	}
	observer.mu.Unlock()
	if pending != nil {
		recordObservation(observer, *pending)
	}
}

func (m *Manager) holdObservation(event models.MonitorEvent) {
	observer := m.observer.Load()
	if observer == nil {
		return
	}
	observer.mu.Lock()
	observer.pending = &event
	timeout := m.rxMetaTimeout
	if timeout <= 0 {
		timeout = defaultRxMetaTimeout
	}
	observer.timer = time.AfterFunc(timeout, func() {
		observer.mu.Lock()
		if observer.pending != &event {
			observer.mu.Unlock()
			return
		}
		observer.pending = nil
		observer.mu.Unlock()
		recordObservation(observer, event)
	})
	observer.mu.Unlock()
}

func (m *Manager) attachObservation(snr, rssi int) {
	observer := m.observer.Load()
	if observer == nil {
		return
	}
	observer.mu.Lock()
	pending := observer.pending
	observer.pending = nil
	if observer.timer != nil {
		observer.timer.Stop()
	}
	observer.mu.Unlock()
	if pending != nil {
		signal := float64(snr) / 4
		pending.SNRDB, pending.RSSIDBM = &signal, &rssi
		recordObservation(observer, *pending)
	}
}

func (m *Manager) packetObservation(packet mesh.Packet, length int, kind string) models.MonitorEvent {
	event := models.MonitorEvent{At: time.Now(), Kind: kind, PayloadType: PayloadType(packet.PayloadType()).String(), Route: RouteType(packet.RouteType()).String(), Hops: packet.HashCount(), Len: length}
	names := map[byte]string{0: "REQ", 1: "RESPONSE", 2: "TXT_MSG", 3: "ACK", 4: "ADVERT", 5: "GRP_TXT", 6: "GRP_DATA", 7: "ANON_REQ", 8: "PATH", 9: "TRACE", 10: "MULTIPART", 11: "CONTROL", 15: "RAW_CUSTOM"}
	if name, ok := names[packet.PayloadType()]; ok {
		event.PayloadType = name
	}
	switch packet.RouteType() {
	case mesh.RouteFlood:
		event.Route = "flood"
	case mesh.RouteDirect:
		event.Route = "direct"
	case mesh.RouteTransportFlood:
		event.Route = "transport_flood"
	case mesh.RouteTransportDirect:
		event.Route = "transport_direct"
	}
	if packet.PayloadType() > mesh.PayloadControl && packet.PayloadType() != mesh.PayloadRawCustom {
		event.PayloadType = fmt.Sprintf("UNKNOWN_%d", packet.PayloadType())
	}
	radioModel := m.radioModel.Load()
	if radioModel != nil && radioModel.FrequencyHz != 0 {
		duration := LoRaAirtime(radioModel.SpreadingFactor, radioModel.BandwidthHz, radioModel.CodingRate, length)
		if duration > 0 {
			milliseconds := duration.Milliseconds()
			event.AirtimeMS = &milliseconds
		}
	}
	return event
}

func (m *Manager) observeRX(packet mesh.Packet, length int, duplicate bool) {
	if m.observer.Load() == nil {
		return
	}
	event := m.packetObservation(packet, length, "rx")
	event.Duplicate = duplicate
	switch packet.PayloadType() {
	case mesh.PayloadReq, mesh.PayloadResponse, mesh.PayloadTxtMsg, mesh.PayloadAnonReq, mesh.PayloadPath:
		if len(packet.Payload) > 0 {
			hash := fmt.Sprintf("%02x", packet.Payload[0])
			event.DestHash = &hash
			if id := m.observer.Load().rooms[packet.Payload[0]]; id != "" {
				event.Room = &id
			}
		}
	}
	switch packet.PayloadType() {
	case mesh.PayloadGrpTxt:
		if message, ok := parsers.ParseGroupText(packet.Payload); ok {
			event.Channel = &message.ChannelName
			event.Name = &message.Sender
		}
	case mesh.PayloadAdvert:
		advert := parsers.ParseAdvert(packet.Payload)
		if advert.SignatureValid {
			event.Name = &advert.Nickname
		}
	}
	m.holdObservation(event)
}

func (m *Manager) observeTX(frame []byte) *models.MonitorEvent {
	if m.observer.Load() == nil || len(frame) < 2 {
		return nil
	}
	packet, err := mesh.Parse(frame[1:])
	if err != nil {
		return nil
	}
	event := m.packetObservation(packet, len(frame)-1, "tx")
	event.Origin = "other"
	observer := m.observer.Load()
	observer.mu.Lock()
	label := observer.txLabels[&frame[0]]
	delete(observer.txLabels, &frame[0])
	observer.mu.Unlock()
	if label.origin != "" {
		event.Origin = label.origin
	}
	if label.room != "" {
		event.Room = &label.room
	}
	switch packet.PayloadType() {
	case mesh.PayloadAdvert:
		event.Origin = "advert"
	case mesh.PayloadGrpTxt:
		if message, ok := parsers.ParseGroupText(packet.Payload); ok {
			event.Channel = &message.ChannelName
		}
	}
	return &event
}

func (m *Manager) finishObservation(event *models.MonitorEvent, result *bool) {
	if event == nil {
		return
	}
	event.OK = result
	if result == nil || !*result {
		event.AirtimeMS = nil
	}
	if observer := m.observer.Load(); observer != nil {
		recordObservation(observer, *event)
	}
}
