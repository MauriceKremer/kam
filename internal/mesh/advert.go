package mesh

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Advert node types (low nibble of the appdata flags).
const (
	AdvertTypeChat     = 0x01
	AdvertTypeRepeater = 0x02
	AdvertTypeRoom     = 0x03
	AdvertTypeSensor   = 0x04
)

// Advert appdata flag bits.
const (
	advertFlagLocation = 0x10
	advertFlagName     = 0x80
)

// MaxAdvertDataSize is MAX_ADVERT_DATA_SIZE: the largest appdata a node signs.
const MaxAdvertDataSize = 32

// MaxAdvertNameLen returns the longest name that fits in the advert appdata:
// 31 bytes, or 23 when a location (8 bytes) is included.
func MaxAdvertNameLen(hasLocation bool) int {
	if hasLocation {
		return MaxAdvertDataSize - 1 - 8
	}
	return MaxAdvertDataSize - 1
}

// AdvertInfo describes a node for its advert appdata.
type AdvertInfo struct {
	Type        byte
	Name        string
	HasLocation bool
	Latitude    float64
	Longitude   float64
}

// AppData encodes the advert appdata: flags | [lat int32 | lon int32] | name.
// Coordinates are degrees scaled by 1e6, little endian.
func (a AdvertInfo) AppData() ([]byte, error) {
	if a.Name == "" {
		return nil, fmt.Errorf("mesh: advert name is empty")
	}
	if len(a.Name) > MaxAdvertNameLen(a.HasLocation) {
		return nil, fmt.Errorf("mesh: advert name is %d bytes, limit is %d", len(a.Name), MaxAdvertNameLen(a.HasLocation))
	}
	flags := a.Type&0x0F | advertFlagName
	if a.HasLocation {
		flags |= advertFlagLocation
	}
	data := make([]byte, 0, MaxAdvertDataSize)
	data = append(data, flags)
	if a.HasLocation {
		data = binary.LittleEndian.AppendUint32(data, uint32(int32(math.Round(a.Latitude*1e6))))
		data = binary.LittleEndian.AppendUint32(data, uint32(int32(math.Round(a.Longitude*1e6))))
	}
	data = append(data, a.Name...)
	return data, nil
}

// AdvertPayload builds a signed advert payload: pubkey | timestamp | signature
// | appdata. The signature covers pubkey | timestamp | appdata
// (Mesh::createAdvert).
func AdvertPayload(id *Identity, timestamp uint32, appData []byte) ([]byte, error) {
	if len(appData) > MaxAdvertDataSize {
		return nil, fmt.Errorf("mesh: advert appdata is %d bytes, limit is %d", len(appData), MaxAdvertDataSize)
	}
	pub := id.PublicKey()
	signed := make([]byte, 0, PubKeySize+4+len(appData))
	signed = append(signed, pub[:]...)
	signed = binary.LittleEndian.AppendUint32(signed, timestamp)
	signed = append(signed, appData...)
	signature := id.Sign(signed)

	payload := make([]byte, 0, PubKeySize+4+SignatureSize+len(appData))
	payload = append(payload, pub[:]...)
	payload = binary.LittleEndian.AppendUint32(payload, timestamp)
	payload = append(payload, signature...)
	payload = append(payload, appData...)
	return payload, nil
}
