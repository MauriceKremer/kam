package models

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestLoadKamConfigValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfig(t, path, `
version: 1
node_name: "kam"
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
    enabled: true
  - name: "mesh-dev"
    key_strategy: "derived"
    enabled: true
`)

	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Version != 1 {
		t.Fatalf("version: expected 1, got %d", cfg.Version)
	}
	if cfg.Radio.Port != "/dev/ttyUSB0" || cfg.Radio.Baud != 115200 {
		t.Fatalf("radio mismatch: %+v", cfg.Radio)
	}
	if cfg.Radio.FrequencyHz != 869618000 || cfg.Radio.BandwidthHz != 62500 {
		t.Fatalf("radio rf mismatch: %+v", cfg.Radio)
	}
	if cfg.Radio.SpreadingFactor != 7 || cfg.Radio.CodingRate != 5 {
		t.Fatalf("radio modulation mismatch: %+v", cfg.Radio)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(cfg.Channels))
	}
	if cfg.Channels[0].Name != "#public" || cfg.Channels[0].KeyStrategy != KeyStrategyPublic {
		t.Fatalf("#public channel mismatch: %+v", cfg.Channels[0])
	}
	if cfg.Channels[1].Name != "#mesh-dev" || cfg.Channels[1].KeyStrategy != KeyStrategyDerived {
		t.Fatalf("#mesh-dev channel mismatch: %+v", cfg.Channels[1])
	}
}

func TestLoadKamConfigMissingRadioFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfig(t, path, `
version: 1
node_name: "kam"
radio:
  port: "/dev/ttyUSB0"
  baud: 115200
channels:
  - name: "#public"
    key_strategy: "public"
    key_hex: "8b3387e9c5cdea6ac9e5edbaa115cd72"
    enabled: true
`)

	_, err := LoadKamConfig(path)
	if err == nil {
		t.Fatal("expected error for missing radio fields")
	}
}

func TestLoadKamConfigMissingChannels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfig(t, path, `
version: 1
node_name: "kam"
radio:
  port: "/dev/ttyUSB0"
  baud: 115200
  frequency_hz: 869618000
  bandwidth_hz: 62500
  spreading_factor: 7
  coding_rate: 5
channels: []
`)

	_, err := LoadKamConfig(path)
	if err == nil {
		t.Fatal("expected error for empty channels")
	}
}

func TestLoadKamConfigDuplicateChannels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfig(t, path, `
version: 1
node_name: "kam"
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
    enabled: true
  - name: "public"
    key_strategy: "derived"
    enabled: true
`)

	_, err := LoadKamConfig(path)
	if err == nil {
		t.Fatal("expected error for duplicate normalized channel name")
	}
}

func TestLoadKamConfigIgnoresRetiredEchoBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	// ECHO is now a Starlark script; the old block must not break startup, even when it was invalid.
	writeConfig(t, path, `
version: 1
node_name: "kam"
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
    enabled: true
echo:
  enabled: true
  channels: ["#ghost"]
  mention: "@kam"
`)

	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load config with retired echo block: %v", err)
	}
	if len(cfg.Channels) != 1 || cfg.Channels[0].Name != "#public" {
		t.Fatalf("unexpected channels: %+v", cfg.Channels)
	}
}

func TestPublicChannelKeyHex(t *testing.T) {
	cfg := &KamConfig{
		Version: 1,
		Channels: []ChannelConfig{
			{
				Name:        "#public",
				KeyStrategy: KeyStrategyPublic,
				KeyHex:      "8b3387e9c5cdea6ac9e5edbaa115cd72",
				Enabled:     true,
			},
		},
	}

	key, err := cfg.PublicChannelKeyHex()
	if err != nil {
		t.Fatalf("public key hex: %v", err)
	}
	if key != "8b3387e9c5cdea6ac9e5edbaa115cd72" {
		t.Fatalf("unexpected key hex: %s", key)
	}
}

func TestSaveKamConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfig(t, path, `
version: 1
node_name: "kam"
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
    enabled: true
  - name: "#mesh-dev"
    key_strategy: "derived"
    enabled: true
`)
	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	out := filepath.Join(dir, "out.yml")
	if err := SaveKamConfig(out, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := LoadKamConfig(out)
	if err != nil {
		t.Fatalf("reload saved: %v", err)
	}
	if len(reloaded.Channels) != 2 {
		t.Fatalf("expected 2 channels after round-trip, got %d", len(reloaded.Channels))
	}
	if reloaded.Channels[1].Name != "#mesh-dev" || reloaded.Channels[1].KeyStrategy != KeyStrategyDerived {
		t.Fatalf("derived channel not preserved: %+v", reloaded.Channels[1])
	}
	if _, err := LoadKamConfig(path); err != nil {
		t.Fatalf("original still readable: %v", err)
	}
}

func TestChannelIndex(t *testing.T) {
	cfg := &KamConfig{Channels: []ChannelConfig{
		{Name: "#public"},
		{Name: "#mesh-dev"},
	}}
	if cfg.ChannelIndex("#mesh-dev") != 1 {
		t.Fatalf("expected index 1 for #mesh-dev, got %d", cfg.ChannelIndex("#mesh-dev"))
	}
	if cfg.ChannelIndex("#missing") != -1 {
		t.Fatalf("expected -1 for missing channel")
	}
}

func TestRadioSettingsValidateRanges(t *testing.T) {
	power := uint8(17)
	base := KamConfig{
		Version:  1,
		NodeName: "kam",
		Radio: RadioConfig{
			Port: "/dev/null", Baud: 115200, FrequencyHz: 869618000, BandwidthHz: 62500,
			SpreadingFactor: 7, CodingRate: 5, TxPowerDBm: &power,
			CSMA: &CSMAConfig{TxDelayMS: 500, Persistence: 63, SlotTimeMS: 100},
		},
		Channels: []ChannelConfig{{
			Name: "#public", KeyStrategy: KeyStrategyPublic,
			KeyHex: "8b3387e9c5cdea6ac9e5edbaa115cd72",
		}},
	}

	cases := []struct {
		name string
		edit func(*KamConfig)
		key  string
	}{
		{"frequency", func(cfg *KamConfig) { cfg.Radio.FrequencyHz = 299_999_999 }, "frequency_hz"},
		{"bandwidth", func(cfg *KamConfig) { cfg.Radio.BandwidthHz = 7_799 }, "bandwidth_hz"},
		{"spreading factor", func(cfg *KamConfig) { cfg.Radio.SpreadingFactor = 4 }, "spreading_factor"},
		{"coding rate", func(cfg *KamConfig) { cfg.Radio.CodingRate = 9 }, "coding_rate"},
		{"TX power", func(cfg *KamConfig) { value := uint8(23); cfg.Radio.TxPowerDBm = &value }, "tx_power_dbm"},
		{"TX delay", func(cfg *KamConfig) { cfg.Radio.CSMA.TxDelayMS = 501 }, "txdelay_ms"},
		{"persistence", func(cfg *KamConfig) { cfg.Radio.CSMA.Persistence = 256 }, "persistence"},
		{"slot time", func(cfg *KamConfig) { cfg.Radio.CSMA.SlotTimeMS = 101 }, "slot_time_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			csma := *base.Radio.CSMA
			cfg.Radio.CSMA = &csma
			tc.edit(&cfg)
			if _, err := normalizeAndValidate(&cfg); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("validation error = %v, want key %q", err, tc.key)
			}
		})
	}
	if _, err := normalizeAndValidate(&base); err != nil {
		t.Fatalf("valid optional radio settings rejected: %v", err)
	}
}

func TestPartialCSMAConfigUsesFirmwareDefaults(t *testing.T) {
	var config CSMAConfig
	if err := yaml.Unmarshal([]byte("full_duplex: true\n"), &config); err != nil {
		t.Fatalf("decode partial CSMA config: %v", err)
	}
	want := CSMAConfig{TxDelayMS: 500, Persistence: 63, SlotTimeMS: 100, FullDuplex: true}
	if config != want {
		t.Fatalf("CSMA config = %+v, want %+v", config, want)
	}
}
