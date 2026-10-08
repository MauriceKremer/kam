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

func TestLoadKamConfigEchoValid(t *testing.T) {
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
echo:
  enabled: true
  channels: ["#public", "mesh-dev"]
  mention: "@kam"
`)

	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.Echo.Enabled {
		t.Fatal("echo should be enabled")
	}
	if cfg.Echo.Mention != "@kam" {
		t.Fatalf("mention: got %q, want @kam", cfg.Echo.Mention)
	}
	if len(cfg.Echo.Channels) != 2 {
		t.Fatalf("echo channels: got %d, want 2", len(cfg.Echo.Channels))
	}
	if cfg.Echo.Channels[0] != "#public" || cfg.Echo.Channels[1] != "#mesh-dev" {
		t.Fatalf("echo channels normalized mismatch: %+v", cfg.Echo.Channels)
	}
}

func TestLoadKamConfigEchoChannelNotConfigured(t *testing.T) {
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
echo:
  enabled: true
  channels: ["#ghost"]
`)

	_, err := LoadKamConfig(path)
	if err == nil {
		t.Fatal("expected error for echo channel not in channels")
	}
}

func TestLoadKamConfigEchoEnabledNoChannels(t *testing.T) {
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
echo:
  enabled: true
`)

	_, err := LoadKamConfig(path)
	if err == nil {
		t.Fatal("expected error for echo enabled without channels")
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
