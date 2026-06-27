package kiss

// AdvertPersistence is the sink used by Manager to persist parsed advert
// packets without blocking the packet router.
type AdvertPersistence interface {
	Enqueue(advert AdvertPacket) bool
	Close() error
}
