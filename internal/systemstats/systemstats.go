// Package systemstats collects host-level system details — machine memory,
// CPU information, disk space and SQLite database size — for the opt-in
// "system" section of GET /v1/status. It is a capability package: it imports
// only the standard library and is composed in internal/node.
//
// Collection happens only when a caller asks (there is no background
// sampler), and Throttled bounds that cost to one collection per configured
// interval, serving repeated callers a cached snapshot.
package systemstats

import (
	"sync"
	"time"
)

// Snapshot is one host observation. Unsupported values are nil, never zero:
// each platform file fills what it can and leaves the rest alone.
type Snapshot struct {
	CollectedAt time.Time

	Memory *MemoryStats
	CPU    *CPUStats
	Disk   *DiskStats
	DB     *DBStats
}

// MemoryStats is machine RAM in bytes. Available follows the Linux
// MemAvailable semantics: memory a workload can claim without swapping.
type MemoryStats struct {
	TotalBytes     uint64
	AvailableBytes uint64
	UsedBytes      uint64
	UsedPercent    float64
}

// CPUStats summarises the processor. Load averages are the portable
// top-summary signal; instantaneous usage percentages are deliberately not
// collected.
type CPUStats struct {
	Model   *string
	Cores   int
	Load1m  *float64
	Load5m  *float64
	Load15m *float64
}

// DiskStats is the filesystem that holds the SQLite database — the only
// mount KAM can exhaust. Available is free to unprivileged users.
type DiskStats struct {
	Path           string
	TotalBytes     uint64
	AvailableBytes uint64
	UsedBytes      uint64
	UsedPercent    float64
}

// DBStats is the size of the SQLite database and its WAL companion as seen
// on disk. WalSizeBytes is nil when there is no WAL file right now.
type DBStats struct {
	Path         string
	SizeBytes    uint64
	WalSizeBytes *uint64
}

// Throttled wraps a collect function and serves its snapshot at most once
// per interval. Callers inside the window receive the cached snapshot
// unchanged, with CollectedAt telling how old it is; interval 0 collects on
// every call. Collection happens only inside Collect — there is no
// background work.
type Throttled struct {
	collect   func(dbPath string) Snapshot
	interval  time.Duration
	dbPath    string
	mu        sync.Mutex
	cached    Snapshot
	collected time.Time
	now       func() time.Time
}

// NewThrottled builds a throttled collector over dbPath, the resolved SQLite
// database path the size fields report on.
func NewThrottled(collect func(dbPath string) Snapshot, interval time.Duration, dbPath string) *Throttled {
	return &Throttled{collect: collect, interval: interval, dbPath: dbPath, now: time.Now}
}

// Collect returns a fresh snapshot when the interval has elapsed and a cached
// one otherwise. Source errors degrade to nil fields inside Collect; Collect
// itself never fails.
func (t *Throttled) Collect() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.interval > 0 && now.Sub(t.collected) < t.interval {
		return t.cached
	}
	snapshot := t.collect(t.dbPath)
	t.cached, t.collected = snapshot, now
	return snapshot
}
