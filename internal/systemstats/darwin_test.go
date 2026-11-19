//go:build darwin

package systemstats

import "testing"

// TestDecodeLoadavgFixture decodes a 24-byte capture of the vm.loadavg
// sysctl from the development machine: ldavg { 2877, 4746, 6074 } and
// fscale 2048, which sysctl(1) printed as "{ 1.41 2.32 2.97 }".
func TestDecodeLoadavgFixture(t *testing.T) {
	raw := []byte{
		0x3d, 0x0b, 0x00, 0x00, 0x8a, 0x12, 0x00, 0x00,
		0xba, 0x17, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	load := decodeLoadavg(raw)
	for i, want := range []float64{1.40, 2.32, 2.97} {
		if load[i] == nil || *load[i] != want {
			t.Fatalf("load_%dm = %v, want %.2f", i+1, load[i], want)
		}
	}
}

func TestDecodeLoadavgDegenerateInputs(t *testing.T) {
	allZero24 := make([]byte, 24) // fscale = 0, guards division
	for _, raw := range [][]byte{nil, {0x01}, make([]byte, 12), allZero24} {
		load := decodeLoadavg(raw)
		for i := 0; i < 3; i++ {
			if load[i] != nil {
				t.Fatalf("input % x: load_%dm must be nil, got %v", raw, i+1, load[i])
			}
		}
	}
}

// TestLoadAveragesReadsThisHost checks the sysctl path on the real machine.
func TestLoadAveragesReadsThisHost(t *testing.T) {
	load := loadAverages()
	for i := 0; i < 3; i++ {
		if load[i] == nil || *load[i] < 0 || *load[i] > 10000 {
			t.Fatalf("load_%dm must be a plausible load on this host: %v", i+1, load[i])
		}
	}
}
