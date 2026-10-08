package config

import (
	"os"
	"path/filepath"
	"testing"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/models"
)

const baseConfig = `
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
`

func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestUpsertRuntimeChannelAndApplyInsertsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfigFile(t, path, baseConfig)

	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := BootstrapChannels(cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// New explicit-key channel.
	ch := models.ChannelConfig{
		Name:        "#ops",
		KeyStrategy: models.KeyStrategyExplicit,
		KeyHex:      "00112233445566778899aabbccddeeff",
		Enabled:     true,
	}
	if err := UpsertRuntimeChannelAndApply(path, ch); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Persisted to YAML.
	reloaded, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	idx := reloaded.ChannelIndex("#ops")
	if idx < 0 {
		t.Fatalf("#ops not persisted to config file")
	}
	if reloaded.Channels[idx].KeyStrategy != models.KeyStrategyExplicit {
		t.Fatalf("persisted strategy mismatch: %q", reloaded.Channels[idx].KeyStrategy)
	}

	// Applied to the in-memory registry without a restart.
	if !parsers.TopicRegistered("#ops") {
		t.Fatalf("#ops not registered in parser topic registry")
	}
}

func TestUpsertRuntimeChannelAndApplyUpdatesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfigFile(t, path, baseConfig)

	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := BootstrapChannels(cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Disable an existing channel via runtime upsert.
	if err := UpsertRuntimeChannelAndApply(path, models.ChannelConfig{
		Name:        "#mesh-dev",
		KeyStrategy: models.KeyStrategyDerived,
		Enabled:     false,
	}); err != nil {
		t.Fatalf("upsert disable: %v", err)
	}

	reloaded, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	idx := reloaded.ChannelIndex("#mesh-dev")
	if idx < 0 || reloaded.Channels[idx].Enabled {
		t.Fatalf("#mesh-dev not disabled in file: %+v", reloaded.Channels[idx])
	}
	if parsers.TopicRegistered("#mesh-dev") {
		t.Fatalf("#mesh-dev still registered after disable")
	}
}

func TestUpsertRuntimeChannelRejectsPublic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kam.yml")
	writeConfigFile(t, path, baseConfig)

	err := UpsertRuntimeChannelAndApply(path, models.ChannelConfig{
		Name:        "#public",
		KeyStrategy: models.KeyStrategyPublic,
		KeyHex:      "8b3387e9c5cdea6ac9e5edbaa115cd72",
		Enabled:     true,
	})
	if err == nil {
		t.Fatal("expected error upserting #public at runtime")
	}
}
