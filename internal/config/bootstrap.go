package config

import (
	"fmt"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/models"
)

// BootstrapChannels atomically replaces the parser registry from validated config.
func BootstrapChannels(cfg *models.KamConfig) error {
	publicHex, err := cfg.PublicChannelKeyHex()
	if err != nil {
		return fmt.Errorf("apply #public key: %w", err)
	}
	topics := make([]parsers.TopicConfig, 0, len(cfg.Channels))
	for _, channel := range cfg.Channels {
		topics = append(topics, parsers.TopicConfig{
			Name:        channel.Name,
			KeyStrategy: channel.KeyStrategy,
			KeyHex:      channel.KeyHex,
			Enabled:     channel.Enabled,
		})
	}
	if err := parsers.ReplaceTopicRegistry(publicHex, topics); err != nil {
		return fmt.Errorf("replace parser channel registry: %w", err)
	}
	return nil
}

// UpsertRuntimeChannelAndApply persists a runtime-provided channel back into
// the YAML config file and immediately applies it to the parser topic
// registry, so the new or updated channel takes effect without a full restart.
//
// The config file is the single source of truth for system configuration: the
// channel is normalized, merged into the loaded config (inserted or replaced
// by normalized name), and the whole file is rewritten atomically. The #public
// channel is rejected here — it is structural and must be edited in the file
// directly.
func UpsertRuntimeChannelAndApply(configPath string, channel models.ChannelConfig) error {
	if err := UpsertChannelInConfigFile(configPath, channel); err != nil {
		return err
	}
	cfg, err := models.LoadKamConfig(configPath)
	if err != nil {
		return fmt.Errorf("reload config after upsert: %w", err)
	}
	return BootstrapChannels(cfg)
}

// UpsertChannelInConfigFile atomically adds or replaces a channel in YAML.
// It does not mutate process-local state, making it suitable for CLI use.
func UpsertChannelInConfigFile(configPath string, channel models.ChannelConfig) error {
	normalized, err := models.NormalizeChannelConfig(channel)
	if err != nil {
		return fmt.Errorf("normalize runtime channel: %w", err)
	}
	if normalized.Name == "#public" {
		return fmt.Errorf("#public cannot be upserted at runtime; edit the config file directly")
	}

	cfg, err := models.LoadKamConfig(configPath)
	if err != nil {
		return fmt.Errorf("reload config for upsert: %w", err)
	}

	if idx := cfg.ChannelIndex(normalized.Name); idx >= 0 {
		cfg.Channels[idx] = normalized
	} else {
		cfg.Channels = append(cfg.Channels, normalized)
	}

	if err := models.SaveKamConfig(configPath, cfg); err != nil {
		return fmt.Errorf("save config after upsert: %w", err)
	}
	return nil
}
