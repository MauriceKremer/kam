//go:build linux

package systemstats

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Collect gathers Linux host statistics from /proc and statfs.
func Collect(dbPath string) Snapshot {
	snapshot := Snapshot{CollectedAt: time.Now().UTC().Truncate(time.Millisecond)}

	snapshot.Memory = linuxMemory()
	snapshot.CPU = linuxCPU()

	if disk := linuxDiskStats(dbPath); disk != nil {
		snapshot.Disk = disk
	}
	if db := linuxDatabaseStats(dbPath); db != nil {
		snapshot.DB = db
	}
	return snapshot
}

// linuxMemory reads MemTotal and MemAvailable from /proc/meminfo, the
// probably-available figure the kernel computes for workloads like KAM.
func linuxMemory() *MemoryStats {
	values, ok := procMemInfo()
	if !ok {
		return nil
	}
	total, hasTotal := values["MemTotal"]
	available, hasAvailable := values["MemAvailable"]
	if !hasTotal || !hasAvailable {
		return nil
	}
	used := total - available
	return &MemoryStats{
		TotalBytes:     total,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    percent(used, total),
	}
}

func parseMemInfoFile(path string) *MemoryStats {
	values, ok := readMemInfo(path)
	if !ok {
		return nil
	}
	total, hasTotal := values["MemTotal"]
	available, hasAvailable := values["MemAvailable"]
	if !hasTotal || !hasAvailable {
		return nil
	}
	used := total - available
	return &MemoryStats{
		TotalBytes:     total,
		AvailableBytes: available,
		UsedBytes:      used,
		UsedPercent:    percent(used, total),
	}
}

func procMemInfo() (map[string]uint64, bool) {
	return readMemInfo("/proc/meminfo")
}

func readMemInfo(path string) (map[string]uint64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo reports kilobytes.
		values[line[:colon]] = value * 1024
	}
	return values, true
}

func linuxCPU() *CPUStats {
	stats := CPUStats{Cores: runtime.NumCPU()}
	if model := procCPUModel(); model != "" {
		stats.Model = &model
	}
	load := procLoadAvg()
	stats.Load1m, stats.Load5m, stats.Load15m = load[0], load[1], load[2]
	return &stats
}

// procCPUModel takes the first "model name" from /proc/cpuinfo.
func procCPUModel() string {
	return procCPUModelFrom("/proc/cpuinfo")
}

func procCPUModelFrom(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "model name") {
			colon := strings.IndexByte(line, ':')
			if colon < 0 {
				return ""
			}
			return strings.TrimSpace(line[colon+1:])
		}
	}
	return ""
}

// procLoadAvg reads the three load averages from /proc/loadavg.
func procLoadAvg() [3]*float64 {
	return parseLoadAvgFile("/proc/loadavg")
}

func parseLoadAvgFile(path string) [3]*float64 {
	var result [3]*float64
	content, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	fields := strings.Fields(string(content))
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

// linuxDiskStats stats the filesystem that holds dbPath.
func linuxDiskStats(dbPath string) *DiskStats {
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

// linuxDatabaseStats sizes the database file and its WAL companion.
func linuxDatabaseStats(dbPath string) *DBStats {
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

// walPath names the SQLite write-ahead-log companion of dbPath.
func walPath(dbPath string) string {
	return dbPath + "-wal"
}

// percent rounds used/total to one decimal, tolerating a zero total.
func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	value := float64(used) / float64(total) * 100
	return float64(int(value*10+0.5)) / 10
}
