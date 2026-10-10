package room

import "time"

// Login attempts cost an X25519 exchange each and let an attacker guess
// passwords, so they are rate limited per source key and overall (RS-AUTH-8).
const (
	globalLoginRate  = 10.0 // attempts per second, all sources
	globalLoginBurst = 20.0
	keyLoginRate     = 0.5 // attempts per second per source key
	keyLoginBurst    = 5.0
	maxTrackedKeys   = 256
)

type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) take(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rate
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type loginLimiter struct {
	global bucket
	perKey map[[32]byte]*bucket
}

func newLoginLimiter() loginLimiter {
	return loginLimiter{perKey: make(map[[32]byte]*bucket)}
}

// allow reports whether a login attempt from key may be processed. Attempts
// refused by the per-key bucket do not consume global budget.
func (l *loginLimiter) allow(key [32]byte, now time.Time) bool {
	b := l.perKey[key]
	if b == nil {
		if len(l.perKey) >= maxTrackedKeys {
			l.evict(now)
		}
		b = &bucket{}
		l.perKey[key] = b
	}
	if !b.take(now, keyLoginRate, keyLoginBurst) {
		return false
	}
	return l.global.take(now, globalLoginRate, globalLoginBurst)
}

// evict makes room by dropping idle entries, or any entry if none is idle.
func (l *loginLimiter) evict(now time.Time) {
	for key, b := range l.perKey {
		if now.Sub(b.last) > time.Minute {
			delete(l.perKey, key)
		}
	}
	if len(l.perKey) < maxTrackedKeys {
		return
	}
	for key := range l.perKey {
		delete(l.perKey, key)
		return
	}
}
