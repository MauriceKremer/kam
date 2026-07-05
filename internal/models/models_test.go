package models

import (
	"os"
	"path/filepath"
	"testing"
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
