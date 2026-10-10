package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/models"
)

func TestWatchConfigAppliesValidUpdateAndKeepsRegistryOnInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.yml")
	writeConfigFile(t, path, baseConfig)

	cfg, err := LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	if err := BootstrapChannels(cfg); err != nil {
		t.Fatalf("bootstrap initial config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applied := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		WatchConfig(ctx, path, 10*time.Millisecond, func(cfg *models.KamConfig) error {
			if err := BootstrapChannels(cfg); err != nil {
				return err
			}
			applied <- struct{}{}
			return nil
		})
	}()

	if err := UpsertChannelInConfigFile(path, models.ChannelConfig{
		Name:        "#watch-test",
		KeyStrategy: models.KeyStrategyDerived,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("upsert channel: %v", err)
	}

	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not apply the updated YAML")
	}
	if !parsers.TopicRegistered("#watch-test") {
		t.Fatal("updated channel was not registered")
	}

	if err := os.WriteFile(path, []byte("version: [invalid"), 0o644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	select {
	case <-applied:
		t.Fatal("watcher applied invalid YAML")
	case <-time.After(50 * time.Millisecond):
	}
	if !parsers.TopicRegistered("#watch-test") {
		t.Fatal("invalid YAML replaced the last valid registry")
	}

	cancel()
	select {
	case <-watchDone:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop after cancellation")
	}
}
