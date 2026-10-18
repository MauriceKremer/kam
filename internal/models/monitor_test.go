package models

import (
	"gopkg.in/yaml.v3"
	"testing"
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
