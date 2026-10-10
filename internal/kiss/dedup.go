package kiss

import (
	"crypto/sha256"
	"sync"
	"time"
)

const (
	// defaultDedupWindow covers flood echoes and client retry timescales.
	defaultDedupWindow = 2 * time.Minute
	// defaultDedupEntries bounds memory regardless of traffic.
	defaultDedupEntries = 512
)

type dedupKey [8]byte

type dedupEntry struct {
	key  dedupKey
	seen time.Time
}

// dedupCache suppresses duplicate packets. A packet is identified by
// SHA-256(payload_type | payload) truncated to 8 bytes, the same identity
// MeshCore uses (Packet::calculatePacketHash); the path is excluded so the same
// flood arriving over different routes counts once. Entries expire after the
// window and the cache never holds more than max entries. The zero value is
// ready to use.
type dedupCache struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	seen   map[dedupKey]time.Time
	order  []dedupEntry
	now    func() time.Time
}

func newDedupKey(payloadType byte, payload []byte) dedupKey {
	h := sha256.New()
	h.Write([]byte{payloadType})
	h.Write(payload)
	var key dedupKey
	copy(key[:], h.Sum(nil))
	return key
}

// Check reports whether the packet was seen within the window and records it.
func (c *dedupCache) Check(payloadType byte, payload []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkLocked(newDedupKey(payloadType, payload))
}

// Mark records a packet without reporting, for packets this node transmitted
// so an echo from a repeater is not processed as new traffic.
func (c *dedupCache) Mark(payloadType byte, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkLocked(newDedupKey(payloadType, payload))
}

func (c *dedupCache) checkLocked(key dedupKey) bool {
	if c.seen == nil {
		c.seen = make(map[dedupKey]time.Time)
	}
	window, limit := c.window, c.max
	if window <= 0 {
		window = defaultDedupWindow
	}
	if limit <= 0 {
		limit = defaultDedupEntries
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}

	if at, ok := c.seen[key]; ok && now.Sub(at) < window {
		return true
	}
	c.seen[key] = now
	c.order = append(c.order, dedupEntry{key: key, seen: now})

	// Drop expired entries first, then the oldest until the bound holds. An
	// order entry whose key was refreshed later no longer owns the map slot.
	for len(c.order) > 0 {
		oldest := c.order[0]
		expired := now.Sub(oldest.seen) >= window
		if !expired && len(c.order) <= limit {
			break
		}
		if at, ok := c.seen[oldest.key]; ok && at.Equal(oldest.seen) {
			delete(c.seen, oldest.key)
		}
		c.order = c.order[1:]
	}
	return false
}

// Len returns the number of tracked packets. Intended for tests.
func (c *dedupCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
