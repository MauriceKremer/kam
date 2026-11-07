//go:build !darwin && !linux

package systemstats

import (
	"os"
	"time"
)

// Collect on unsupported platforms reports only what pure Go can know: the
// database size. Machine memory, CPU model, load averages and disk space
// stay nil until a metric library earns its keep (docs/FeatureSystemStatus.md D3).
func Collect(dbPath string) Snapshot {
	snapshot := Snapshot{CollectedAt: time.Now().UTC().Truncate(time.Millisecond)}

	if db := otherDatabaseStats(dbPath); db != nil {
		snapshot.DB = db
	}
	return snapshot
}

func otherDatabaseStats(dbPath string) *DBStats {
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil
	}
	stats := DBStats{Path: dbPath, SizeBytes: uint64(info.Size())}
	if wal, err := os.Stat(walPath(dbPath)); err == nil {
		size := uint64(wal.Size())
		stats.WalSizeBytes = &size
	}
	return &stats
}

func walPath(dbPath string) string {
	return dbPath + "-wal"
}
