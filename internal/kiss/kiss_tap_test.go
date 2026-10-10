package kiss

import (
	"bytes"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/mesh"
)

func TestKissConstantsMatchMesh(t *testing.T) {
	pairs := [][2]int{
		{ROUTE_TYPE_TRANSPORT_FLOOD, mesh.RouteTransportFlood}, {ROUTE_TYPE_FLOOD, mesh.RouteFlood},
		{ROUTE_TYPE_DIRECT, mesh.RouteDirect}, {ROUTE_TYPE_TRANSPORT_DIRECT, mesh.RouteTransportDirect},
		{PAYLOAD_TYPE_REQ, mesh.PayloadReq}, {PAYLOAD_TYPE_RESPONSE, mesh.PayloadResponse},
		{PAYLOAD_TYPE_TXT_MSG, mesh.PayloadTxtMsg}, {PAYLOAD_TYPE_ACK, mesh.PayloadAck},
		{PAYLOAD_TYPE_ADVERT, mesh.PayloadAdvert}, {PAYLOAD_TYPE_GRP_TXT, mesh.PayloadGrpTxt},
		{PAYLOAD_TYPE_GRP_DATA, mesh.PayloadGrpData}, {PAYLOAD_TYPE_ANON_REQ, mesh.PayloadAnonReq},
		{PAYLOAD_TYPE_PATH, mesh.PayloadPath}, {PAYLOAD_TYPE_TRACE, mesh.PayloadTrace},
		{PAYLOAD_TYPE_MULTIPART, mesh.PayloadMultipart}, {PAYLOAD_TYPE_CONTROL, mesh.PayloadControl},
		{PAYLOAD_TYPE_RAW_CUSTOM, mesh.PayloadRawCustom}, {PAYLOAD_VERSION_V1, mesh.VersionV1},
	}
	for _, pair := range pairs {
		if pair[0] != pair[1] {
			t.Fatalf("kiss constant %#x differs from mesh constant %#x", pair[0], pair[1])
		}
	}
}

type tapRecorder struct{ packets []RxPacket }

func (r *tapRecorder) handler() PacketHandler {
	return func(p RxPacket) { r.packets = append(r.packets, p) }
}

func encodePacket(t *testing.T, p mesh.Packet) []byte {
	t.Helper()
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRouterDeliversPeerPacketsToTapWithRouting(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	var rec tapRecorder
	m.SubscribePackets(rec.handler())

	scoped := mesh.Packet{
		Header:         mesh.MakeHeader(mesh.RouteTransportFlood, mesh.PayloadAnonReq),
		TransportCodes: [2]uint16{0xBEEF, 0},
		PathLen:        0x42, // two 2-byte hops
		Path:           []byte{1, 2, 3, 4},
		Payload:        []byte{0x10, 0x20, 0x30},
	}
	direct := mesh.Packet{
		Header:  mesh.MakeHeader(mesh.RouteDirect, mesh.PayloadTxtMsg),
		PathLen: 0x01,
		Path:    []byte{0xAA},
		Payload: []byte{0x40},
	}
	for _, p := range []mesh.Packet{scoped, direct} {
		m.routePacket(encodePacket(t, p))
	}

	if len(rec.packets) != 2 {
		t.Fatalf("tap received %d packets, want 2", len(rec.packets))
	}
	got := rec.packets[0]
	if got.RouteType() != mesh.RouteTransportFlood || got.TransportCodes != scoped.TransportCodes || got.PathLen != 0x42 ||
		!bytes.Equal(got.Path, scoped.Path) || !bytes.Equal(got.Payload, scoped.Payload) || got.PayloadType() != mesh.PayloadAnonReq {
		t.Fatalf("scoped packet lost routing data: %+v", got)
	}
	if got.ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt not set")
	}
	if rec.packets[1].RouteType() != mesh.RouteDirect || rec.packets[1].HashCount() != 1 {
		t.Fatalf("direct packet with remaining path must reach the tap unchanged: %+v", rec.packets[1])
	}
}

func TestRouterTapIgnoresAdvertsGroupTextsAndMalformedPackets(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	var rec tapRecorder
	m.SubscribePackets(rec.handler())

	for _, raw := range [][]byte{
		encodePacket(t, mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadAdvert), Payload: []byte{1}}),
		encodePacket(t, mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadGrpTxt), Payload: []byte{1, 2}}),
		encodePacket(t, mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadTrace), Payload: []byte{1}}),
		{0x41, 0x00, 0xAA},       // unsupported packet version
		{0x0D, 0xC0, 0xAA, 0xBB}, // reserved path hash size
		{0x0D, 0x03, 0xAA},       // path runs past the end of the frame
		{0x0D, 0x00},             // no payload
		{0x0C, 1, 2, 3},          // transport codes cut off
		append([]byte{0x0D, 0x00}, make([]byte, mesh.MaxPacketPayload+1)...),
	} {
		m.routePacket(raw)
	}
	if len(rec.packets) != 0 {
		t.Fatalf("tap received %d unexpected packets", len(rec.packets))
	}
}

func TestRouterTapCancelStopsDelivery(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	var rec tapRecorder
	cancel := m.SubscribePackets(rec.handler())
	raw := encodePacket(t, mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadReq), Payload: []byte{1}})
	m.routePacket(raw)
	cancel()
	m.routePacket(encodePacket(t, mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadReq), Payload: []byte{2}}))
	if len(rec.packets) != 1 {
		t.Fatalf("got %d packets after cancel, want 1", len(rec.packets))
	}
}

func TestRouterDropsDuplicatesByTypeAndPayloadIgnoringPath(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	var rec tapRecorder
	m.SubscribePackets(rec.handler())

	viaA := mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadTxtMsg), PathLen: 1, Path: []byte{0xA1}, Payload: []byte{9, 9}}
	viaB := mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadTxtMsg), PathLen: 1, Path: []byte{0xB2}, Payload: []byte{9, 9}}
	otherType := mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadReq), PathLen: 1, Path: []byte{0xA1}, Payload: []byte{9, 9}}
	for _, p := range []mesh.Packet{viaA, viaB, otherType} {
		m.routePacket(encodePacket(t, p))
	}
	if len(rec.packets) != 2 {
		t.Fatalf("got %d packets, want 2: same type+payload over another path is a duplicate, a different type is not", len(rec.packets))
	}
	if rec.packets[0].PayloadType() != mesh.PayloadTxtMsg || rec.packets[1].PayloadType() != mesh.PayloadReq {
		t.Fatalf("unexpected packets: %+v", rec.packets)
	}
}

func TestMarkSeenSuppressesEchoOfOwnTransmission(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	var rec tapRecorder
	m.SubscribePackets(rec.handler())

	own := encodePacket(t, mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadAck), Payload: []byte{1, 2, 3, 4}})
	m.MarkSeen(own)
	m.MarkSeen([]byte{0xFF}) // malformed input is ignored
	m.routePacket(own)
	if len(rec.packets) != 0 {
		t.Fatal("echo of an own transmission reached the tap")
	}
}

func TestDedupWindowAndBound(t *testing.T) {
	now := time.Unix(1_000, 0)
	cache := dedupCache{window: time.Minute, max: 3, now: func() time.Time { return now }}

	if cache.Check(2, []byte("a")) {
		t.Fatal("first sighting reported as duplicate")
	}
	if !cache.Check(2, []byte("a")) {
		t.Fatal("repeat inside the window not reported")
	}
	if cache.Check(3, []byte("a")) {
		t.Fatal("same payload with another payload type must not be a duplicate")
	}

	now = now.Add(61 * time.Second)
	if cache.Check(2, []byte("a")) {
		t.Fatal("repeat after the window must be accepted again")
	}
	if !cache.Check(2, []byte("a")) {
		t.Fatal("refreshed entry not tracked")
	}
	if cache.Len() > 3 {
		t.Fatalf("cache holds %d entries, bound is 3", cache.Len())
	}

	for i := byte(0); i < 20; i++ {
		cache.Check(4, []byte{i})
	}
	if cache.Len() > 3 {
		t.Fatalf("cache holds %d entries, bound is 3", cache.Len())
	}
	if cache.Check(4, []byte{19}) != true {
		t.Fatal("newest entry was evicted")
	}
	if cache.Check(4, []byte{0}) {
		t.Fatal("oldest entry survived eviction")
	}
}

func TestDedupZeroValueIsUsable(t *testing.T) {
	var cache dedupCache
	if cache.Check(1, []byte("x")) || !cache.Check(1, []byte("x")) {
		t.Fatal("zero-value cache misbehaves")
	}
}
