// Package mesh implements the MeshCore wire primitives KAM needs to act as a
// mesh node: packet framing, the shared-secret envelope, Ed25519 identities
// with X25519 key exchange, signed adverts, and region transport codes.
//
// Facts are taken from MeshCore src/Packet.cpp, src/Mesh.cpp, src/Utils.cpp and
// lib/ed25519 at the commit pinned in docs/FeatureRoomServer.md. The package has
// no dependency on the KISS transport so it can be shared by the router, the
// transmit scheduler and the room server.
package mesh

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Route types (header bits 0-1).
const (
	RouteTransportFlood  = 0x00
	RouteFlood           = 0x01
	RouteDirect          = 0x02
	RouteTransportDirect = 0x03
)

// Payload types (header bits 2-5).
const (
	PayloadReq       = 0x00
	PayloadResponse  = 0x01
	PayloadTxtMsg    = 0x02
	PayloadAck       = 0x03
	PayloadAdvert    = 0x04
	PayloadGrpTxt    = 0x05
	PayloadGrpData   = 0x06
	PayloadAnonReq   = 0x07
	PayloadPath      = 0x08
	PayloadTrace     = 0x09
	PayloadMultipart = 0x0A
	PayloadControl   = 0x0B
	PayloadRawCustom = 0x0F
)

// VersionV1 is the only packet version this package emits (header bits 6-7).
const VersionV1 = 0x00

// Size limits from MeshCore.h.
const (
	MaxPacketPayload = 184
	MaxPathSize      = 64
	MaxTransportUnit = 255
	PubKeySize       = 32
	SignatureSize    = 64
	SeedSize         = 32
)

// OutPathUnknown is the stored path_len meaning "no direct route known". It is
// not a valid encoded path_len, so it cannot collide with a real route.
const OutPathUnknown = 0xFF

// Errors returned by Parse and Encode.
var (
	ErrTruncated      = errors.New("mesh: packet truncated")
	ErrInvalidPath    = errors.New("mesh: invalid path length encoding")
	ErrPayloadTooLong = errors.New("mesh: payload exceeds MAX_PACKET_PAYLOAD")
	ErrEmptyPayload   = errors.New("mesh: payload is empty")
)

// Packet is one MeshCore packet without KISS framing.
type Packet struct {
	Header         byte
	TransportCodes [2]uint16
	// PathLen is the encoded path_len byte: bits 0-5 hop count, bits 6-7 hash
	// size minus one.
	PathLen byte
	Path    []byte
	Payload []byte
}

// MakeHeader builds a header byte for the given route and payload type.
func MakeHeader(routeType, payloadType byte) byte {
	return VersionV1<<6 | (payloadType&0x0F)<<2 | routeType&0x03
}

// RouteType returns header bits 0-1.
func (p Packet) RouteType() byte { return p.Header & 0x03 }

// PayloadType returns header bits 2-5.
func (p Packet) PayloadType() byte { return (p.Header >> 2) & 0x0F }

// Version returns header bits 6-7.
func (p Packet) Version() byte { return (p.Header >> 6) & 0x03 }

// HasTransportCodes reports whether the route type carries the 4-byte codes.
func (p Packet) HasTransportCodes() bool {
	rt := p.RouteType()
	return rt == RouteTransportFlood || rt == RouteTransportDirect
}

// IsFlood reports whether the packet is flood-routed (scoped or not).
func (p Packet) IsFlood() bool {
	rt := p.RouteType()
	return rt == RouteFlood || rt == RouteTransportFlood
}

// IsDirect reports whether the packet is direct-routed (with or without codes).
func (p Packet) IsDirect() bool { return !p.IsFlood() }

// HashSize returns the bytes per path hash (1-3 when valid).
func (p Packet) HashSize() int { return int(p.PathLen>>6) + 1 }

// HashCount returns the number of path hashes.
func (p Packet) HashCount() int { return int(p.PathLen & 0x3F) }

// ValidPathLen reports whether an encoded path_len is acceptable: hash size
// code 0b11 is reserved and the path may not exceed MaxPathSize bytes.
func ValidPathLen(pathLen byte) bool {
	hashSize := int(pathLen>>6) + 1
	if hashSize == 4 {
		return false
	}
	return int(pathLen&0x3F)*hashSize <= MaxPathSize
}

// EncodePathLen builds an encoded path_len from a hash size (1-3) and count.
func EncodePathLen(hashSize, count int) (byte, error) {
	if hashSize < 1 || hashSize > 3 || count < 0 || count > 63 {
		return 0, fmt.Errorf("%w: hash size %d, count %d", ErrInvalidPath, hashSize, count)
	}
	pathLen := byte(hashSize-1)<<6 | byte(count)
	if !ValidPathLen(pathLen) {
		return 0, fmt.Errorf("%w: hash size %d, count %d", ErrInvalidPath, hashSize, count)
	}
	return pathLen, nil
}

// Parse decodes a raw packet. It mirrors Packet::readFrom: a bad path
// encoding, a missing payload or an oversized payload is rejected. The
// version bits are not checked; callers decide what to do with them.
func Parse(raw []byte) (Packet, error) {
	if len(raw) < 2 {
		return Packet{}, ErrTruncated
	}
	p := Packet{Header: raw[0]}
	i := 1
	if p.HasTransportCodes() {
		if len(raw) < i+4+1 {
			return Packet{}, ErrTruncated
		}
		p.TransportCodes[0] = binary.LittleEndian.Uint16(raw[i:])
		p.TransportCodes[1] = binary.LittleEndian.Uint16(raw[i+2:])
		i += 4
	}
	p.PathLen = raw[i]
	i++
	if !ValidPathLen(p.PathLen) {
		return Packet{}, ErrInvalidPath
	}
	pathBytes := p.HashSize() * p.HashCount()
	if i+pathBytes >= len(raw) {
		return Packet{}, ErrTruncated
	}
	p.Path = append([]byte(nil), raw[i:i+pathBytes]...)
	i += pathBytes
	if len(raw)-i > MaxPacketPayload {
		return Packet{}, ErrPayloadTooLong
	}
	p.Payload = append([]byte(nil), raw[i:]...)
	return p, nil
}

// Encode serialises the packet. The result is at most 254 bytes.
func (p Packet) Encode() ([]byte, error) {
	if !ValidPathLen(p.PathLen) {
		return nil, ErrInvalidPath
	}
	if len(p.Path) != p.HashSize()*p.HashCount() {
		return nil, fmt.Errorf("%w: path has %d bytes, path_len says %d", ErrInvalidPath, len(p.Path), p.HashSize()*p.HashCount())
	}
	if len(p.Payload) == 0 {
		return nil, ErrEmptyPayload
	}
	if len(p.Payload) > MaxPacketPayload {
		return nil, ErrPayloadTooLong
	}
	out := make([]byte, 0, 6+len(p.Path)+len(p.Payload))
	out = append(out, p.Header)
	if p.HasTransportCodes() {
		out = binary.LittleEndian.AppendUint16(out, p.TransportCodes[0])
		out = binary.LittleEndian.AppendUint16(out, p.TransportCodes[1])
	}
	out = append(out, p.PathLen)
	out = append(out, p.Path...)
	out = append(out, p.Payload...)
	return out, nil
}

// NewFloodPacket builds a flood packet with an empty path. A non-nil scope
// makes it a scoped flood: transport_codes[0] is the region code over the
// payload and transport_codes[1] is zero (Mesh::sendFlood with codes).
func NewFloodPacket(payloadType byte, payload []byte, hashSize int, scope *TransportKey) (Packet, error) {
	pathLen, err := EncodePathLen(hashSize, 0)
	if err != nil {
		return Packet{}, err
	}
	p := Packet{Header: MakeHeader(RouteFlood, payloadType), PathLen: pathLen, Payload: append([]byte(nil), payload...)}
	if scope != nil && !scope.IsZero() {
		p.Header = MakeHeader(RouteTransportFlood, payloadType)
		p.TransportCodes[0] = scope.Code(payloadType, payload)
	}
	return p, nil
}

// NewDirectPacket builds a direct packet that follows an encoded route
// (path_len plus path bytes), as stored for a client's out path.
func NewDirectPacket(payloadType byte, payload []byte, pathLen byte, path []byte) (Packet, error) {
	if !ValidPathLen(pathLen) {
		return Packet{}, ErrInvalidPath
	}
	p := Packet{
		Header:  MakeHeader(RouteDirect, payloadType),
		PathLen: pathLen,
		Path:    append([]byte(nil), path...),
		Payload: append([]byte(nil), payload...),
	}
	if len(p.Path) != p.HashSize()*p.HashCount() {
		return Packet{}, ErrInvalidPath
	}
	return p, nil
}

// NewZeroHopPacket builds a direct packet with an empty path, the form used
// for adverts that must not be forwarded.
func NewZeroHopPacket(payloadType byte, payload []byte) Packet {
	return Packet{Header: MakeHeader(RouteDirect, payloadType), Payload: append([]byte(nil), payload...)}
}
