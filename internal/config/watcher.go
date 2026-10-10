package config

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"time"

	"kam.bytecoder.nl/internal/models"
)

const defaultConfigWatchInterval = 500 * time.Millisecond

// WatchConfig watches a YAML file for content changes and applies each valid
// version until ctx is cancelled. Invalid versions are logged and ignored.
func WatchConfig(ctx context.Context, path string, interval time.Duration, apply func(*models.KamConfig) error) {
	if interval <= 0 {
		interval = defaultConfigWatchInterval
	}

	var lastHash [sha256.Size]byte

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastReadError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			content, err := os.ReadFile(path)
			if err != nil {
				if lastReadError != err.Error() {
					slog.Warn("Could not read config while watching", "path", path, "error", err)
					lastReadError = err.Error()
				}
				continue
			}
			lastReadError = ""

			contentHash := sha256.Sum256(content)
			if contentHash == lastHash {
				continue
			}
			lastHash = contentHash

			cfg, err := models.ParseKamConfig(content)
			if err != nil {
				slog.Error("Config reload rejected", "path", path, "error", err)
				continue
			}
			if err := apply(cfg); err != nil {
				slog.Error("Config reload could not be applied", "path", path, "error", err)
				continue
			}
			slog.Info("Reloaded channel configuration", "path", path, "channels", len(cfg.Channels))
		}
	}
}
