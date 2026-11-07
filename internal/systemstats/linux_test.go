//go:build linux

package systemstats

import (
	"os"
	"path/filepath"
	"testing"
)

// procLoadAvgFixture is a Raspberry-Pi-style /proc/loadavg.
const procLoadAvgFixture = "0.52 0.41 0.33 2/1234 5678\n"

func TestParseLoadAvg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loadavg")
	if err := os.WriteFile(path, []byte(procLoadAvgFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	result := parseLoadAvgFile(path)
	if result[0] == nil || *result[0] != 0.52 || result[1] == nil || *result[1] != 0.41 || result[2] == nil || *result[2] != 0.33 {
		t.Fatalf("load averages must parse: %+v", result)
	}
}

func TestParseLoadAvgMalformed(t *testing.T) {
	for name, content := range map[string]string{
		"missing file": "",
		"two fields":   "1.0 2.0\n",
		"not a number": "a b c d/e f\n",
		"empty":        "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "loadavg")
			if content != "" {
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result := parseLoadAvgFile(path)
			for i, value := range result {
				if value != nil {
					t.Fatalf("field %d must stay nil: %+v", i, result)
				}
			}
		})
	}
}

// procMemInfoFixture is a Raspberry-Pi-style /proc/meminfo, values in kB.
const procMemInfoFixture = `MemTotal:       3862692 kB
MemFree:          946932 kB
MemAvailable:    1618940 kB
Buffers:          134708 kB
Cached:           902256 kB
SwapTotal:             0 kB
`

func TestParseMemInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(path, []byte(procMemInfoFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	stats := parseMemInfoFile(path)
	if stats == nil {
		t.Fatal("meminfo fixture must parse")
	}
	if stats.TotalBytes != 3862692*1024 || stats.AvailableBytes != 1618940*1024 {
		t.Fatalf("kilobyte values must scale to bytes: %+v", stats)
	}
	if stats.UsedBytes != stats.TotalBytes-stats.AvailableBytes {
		t.Fatalf("used must be total minus available: %+v", stats)
	}
}

func TestParseMemInfoMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemFree: 100 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stats := parseMemInfoFile(path); stats != nil {
		t.Fatalf("meminfo without MemTotal or MemAvailable must degrade: %+v", stats)
	}
}

// procCPUInfoFixture is a Raspberry-Pi-style /proc/cpuinfo.
const procCPUInfoFixture = `processor	: 0
model name	: ARMv7 Processor rev 4 (v7l)
BogoMIPS	: 38.40
Features	: half thumb fastmult vfp
CPU implementer	: 0x41
`

func TestParseCPUModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte(procCPUInfoFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if model := procCPUModelFrom(path); model != "ARMv7 Processor rev 4 (v7l)" {
		t.Fatalf("model name must parse: %q", model)
	}
}

func TestParseCPUModelMissing(t *testing.T) {
	if model := procCPUModelFrom(filepath.Join(t.TempDir(), "absent")); model != "" {
		t.Fatalf("absent cpuinfo must degrade to empty: %q", model)
	}
}
