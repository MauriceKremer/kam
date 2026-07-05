package kiss

import "kam.bytecoder.nl/internal/kiss/parsers"

// Persistence is the sink used by Manager to persist parsed advert packets
// and decrypted group text messages without blocking the packet router.
type Persistence interface {
	EnqueueAdvert(advert parsers.AdvertPacket) bool
	EnqueueGroupText(msg parsers.GroupTextMessage) bool
	Close() error
}
