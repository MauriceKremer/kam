package systemstats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestThrottledCachesWithinInterval(t *testing.T) {
	calls := 0
	stub := func(dbPath string) Snapshot {
		calls++
		return Snapshot{CollectedAt: time.Time{}.Add(time.Duration(calls) * time.Second), DB: &DBStats{Path: dbPath, SizeBytes: uint64(calls)}}
	}
	collector := NewThrottled(stub, 5*time.Second, "kam.db")
	clock := time.Unix(0, 0)
	collector.now = func() time.Time { return clock }

	first := collector.Collect()
	if calls != 1 || first.DB.SizeBytes != 1 {
		t.Fatalf("first Collect must collect fresh: calls=%d snapshot=%+v", calls, first)
	}
	clock = clock.Add(1 * time.Second)
	second := collector.Collect()
	if calls != 1 || second.DB.SizeBytes != 1 || !second.CollectedAt.Equal(first.CollectedAt) {
		t.Fatalf("Collect inside the window must serve the cache: calls=%d snapshot=%+v", calls, second)
	}
	clock = clock.Add(5 * time.Second)
	third := collector.Collect()
	if calls != 2 || third.DB.SizeBytes != 2 {
		t.Fatalf("Collect after the window must collect fresh: calls=%d snapshot=%+v", calls, third)
	}
}

func TestThrottledZeroIntervalAlwaysCollects(t *testing.T) {
	calls := 0
	stub := func(dbPath string) Snapshot {
		calls++
		return Snapshot{}
	}
	collector := NewThrottled(stub, 0, "kam.db")
	collector.now = func() time.Time { return time.Unix(0, 0) }
	collector.Collect()
	collector.Collect()
	if calls != 2 {
		t.Fatalf("interval 0 must collect on every call: calls=%d", calls)
	}
}

// TestCollectReportsThisHost runs the real GOOS collector against the test's
// own temporary database: the darwin and linux files must fill their groups
// on the platform they build for, other.go only the database group.
func TestCollectReportsThisHost(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kam.db")
	if err := os.WriteFile(dbPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := Collect(dbPath)
	if snapshot.CollectedAt.IsZero() {
		t.Fatal("CollectedAt must be set")
	}
	if snapshot.DB == nil || snapshot.DB.SizeBytes != 10 || snapshot.DB.Path != dbPath {
		t.Fatalf("database stats must report the real file: %+v", snapshot.DB)
	}

	switch {
	case snapshot.Memory != nil:
		if snapshot.Memory.TotalBytes == 0 || snapshot.Memory.AvailableBytes == 0 || snapshot.Memory.UsedBytes != snapshot.Memory.TotalBytes-snapshot.Memory.AvailableBytes {
			t.Fatalf("memory stats must be self-consistent: %+v", snapshot.Memory)
		}
	default:
		t.Log("memory unsupported on this GOOS (expected on windows and others)")
	}
	if snapshot.CPU != nil {
		if snapshot.CPU.Cores <= 0 {
			t.Fatalf("cpu cores must be positive: %+v", snapshot.CPU)
		}
	} else {
		t.Log("cpu unsupported on this GOOS (expected on windows and others)")
	}
	if snapshot.Disk != nil {
		if snapshot.Disk.TotalBytes == 0 || snapshot.Disk.AvailableBytes == 0 {
			t.Fatalf("disk stats must report the filesystem: %+v", snapshot.Disk)
		}
		if filepath.Dir(dbPath) != snapshot.Disk.Path && filepath.Dir(dbPath) != "." {
			t.Logf("disk path %q resolved from %q", snapshot.Disk.Path, dbPath)
		}
	} else {
		t.Log("disk unsupported on this GOOS (expected on windows and others)")
	}
}

func TestCollectMissingDatabaseDegrades(t *testing.T) {
	snapshot := Collect(filepath.Join(t.TempDir(), "absent.db"))
	if snapshot.DB != nil {
		t.Fatalf("a missing database file must leave the database group nil: %+v", snapshot.DB)
	}
	if snapshot.CollectedAt.IsZero() {
		t.Fatal("CollectedAt must still be set")
	}
}
