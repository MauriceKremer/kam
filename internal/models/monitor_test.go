package models

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMonitorConfigurationDefaultsAndDisabled(t *testing.T) {
	for _, test := range []struct {
		yaml     string
		enabled  bool
		listen   string
		capacity int
	}{
		{"{}", true, "127.0.0.1:8766", 8192},
		{"monitor: {enabled: false}", false, "127.0.0.1:8766", 8192},
		{"monitor: {listen: '[::1]:0', event_ring: 256}", true, "[::1]:0", 256},
	} {
		var cfg KamConfig
		if err := yaml.Unmarshal([]byte(test.yaml), &cfg); err != nil {
			t.Fatal(err)
		}
		resolved := cfg.MonitorSettings()
		if resolved.Enabled != test.enabled || resolved.Listen != test.listen || resolved.EventRing != test.capacity {
			t.Fatalf("%s: %+v", test.yaml, resolved)
		}
		if err := resolved.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

// Regression: normalization rebuilds the config and must keep the monitor
// block, or every monitor.listen a user sets would be silently ignored.
func TestParseKamConfigKeepsMonitorSettings(t *testing.T) {
	yamlText := `
version: 1
node_name: "n"
monitor:
  listen: "127.0.0.1:48766"
  event_ring: 256
radio:
  port: "/dev/ttyUSB0"
  baud: 115200
  frequency_hz: 869618000
  bandwidth_hz: 62500
  spreading_factor: 7
  coding_rate: 5
channels:
  - name: "#public"
    key_strategy: "public"
    key_hex: "8b3387e9c5cdea6ac9e5edbaa115cd72"
`
	cfg, err := ParseKamConfig([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	resolved := cfg.MonitorSettings()
	if resolved.Listen != "127.0.0.1:48766" || resolved.EventRing != 256 {
		t.Fatalf("monitor settings lost in normalization: %+v", resolved)
	}
}
