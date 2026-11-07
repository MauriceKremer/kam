//go:build darwin

package systemstats

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Collect gathers macOS host statistics through sysctls and statfs. The
// memory figure is approximate by design: the kernel keeps precise
// accounting in Mach calls the standard library does not expose, so
// available approximates the free + speculative pages `top` reports as unused.
func Collect(dbPath string) Snapshot {
	snapshot := Snapshot{CollectedAt: time.Now().UTC().Truncate(time.Millisecond)}

	snapshot.Memory = darwinMemory()
	snapshot.CPU = darwinCPU()

	if disk := diskStats(dbPath); disk != nil {
		snapshot.Disk = disk
	}
	if db := databaseStats(dbPath); db != nil {
		snapshot.DB = db
	}
	return snapshot
}

// darwinMemory approximates the Activity Monitor model: available counts the
// free and speculative page cache. Recent macOS releases dropped the
// vm.stats.vm.* sysctls, so the legacy vm.page_* counters are read first and
// the vm.stats pair stays as the fallback for older hosts.
func darwinMemory() *MemoryStats {
	pageSize, ok := sysctlUint64("hw.pagesize")
	if !ok || pageSize == 0 {
		return nil
	}
	total, ok := sysctlUint64("hw.memsize")
	if !ok {
		return nil
	}
	free := firstUint32("vm.page_free_count", "vm.stats.vm.v_free_count")
	idle := firstUint32("vm.page_speculative_count", "vm.stats.vm.v_inactive_count")
	if free == nil || idle == nil {
		return nil
	}
	available := (uint64(*free) + uint64(*idle)) * pageSize
	if available > total {
		available = total
	}
	used := total - available
	return &MemoryStats{
		TotalBytes:     total,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    percent(used, total),
	}
}

func darwinCPU() *CPUStats {
	stats := CPUStats{Cores: runtime.NumCPU()}
	if model, err := sysctlString("machdep.cpu.brand_string"); err == nil && strings.TrimSpace(model) != "" {
		value := strings.TrimSpace(model)
		stats.Model = &value
	}
	load := loadAverages()
	stats.Load1m, stats.Load5m, stats.Load15m = load[0], load[1], load[2]
	return &stats
}

// loadAverages reads vm.loadavg, formatted as "{ 1.23 4.56 7.89 }".
func loadAverages() [3]*float64 {
	var result [3]*float64
	text, err := sysctlString("vm.loadavg")
	if err != nil {
		return result
	}
	text = strings.Trim(text, "{} ")
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return result
	}
	var values [3]float64
	for i := 0; i < 3; i++ {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return result
		}
		values[i] = value
	}
	for i := 0; i < 3; i++ {
		result[i] = &values[i]
	}
	return result
}

// diskStats stats the filesystem that holds dbPath.
func diskStats(dbPath string) *DiskStats {
	directory := filepath.Dir(dbPath)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return nil
	}
	total := stat.Blocks * uint64(stat.Bsize)
	available := stat.Bavail * uint64(stat.Bsize)
	used := (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
	resolved := directory
	if absolute, err := filepath.Abs(directory); err == nil {
		resolved = absolute
	}
	return &DiskStats{
		Path:           resolved,
		TotalBytes:     total,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    percent(used, total),
	}
}

// databaseStats sizes the database file and its WAL companion.
func databaseStats(dbPath string) *DBStats {
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

func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	value := float64(used) / float64(total) * 100
	return float64(int(value*10+0.5)) / 10
}

func sysctlUint32(name string) (uint32, bool) {
	value, err := syscall.SysctlUint32(name)
	return value, err == nil
}

// firstUint32 returns the first readable sysctl of the list, nil when none is.
func firstUint32(names ...string) *uint32 {
	for _, name := range names {
		if value, ok := sysctlUint32(name); ok {
			return &value
		}
	}
	return nil
}

func sysctlUint64(name string) (uint64, bool) {
	value, err := unix.SysctlUint64(name)
	return value, err == nil
}

func sysctlString(name string) (string, error) {
	return syscall.Sysctl(name)
}
