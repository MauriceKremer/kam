package kiss

// Persistence is the sink used by Manager to persist parsed advert packets
// and decrypted group text messages without blocking the packet router.
type Persistence interface {
	EnqueueAdvert(advert AdvertPacket) bool
	EnqueueGroupText(msg GroupTextMessage) bool
	Close() error
}
