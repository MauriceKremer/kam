package config

import (
	"fmt"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/models"
)

// BootstrapChannels loads every configured channel from the in-memory config
// (already parsed from kam.yml) into the parser topic registry at startup.
//
// System configuration lives in the YAML file only; the durable store is not
// used for channel state. The #public channel is verified and applied first,
// then every other enabled channel is registered via applyChannelConfig.
func BootstrapChannels(cfg *models.KamConfig) error {
	publicHex, err := cfg.PublicChannelKeyHex()
	if err != nil {
		return fmt.Errorf("apply #public key: %w", err)
	}
	if err := parsers.SetPublicChannelSecretHex(publicHex); err != nil {
		return fmt.Errorf("apply #public key: %w", err)
	}

	parsers.ResetTopicRegistry()

	for _, channel := range cfg.Channels {
		if channel.Name == "#public" {
			// Already applied above; skip so applyChannelConfig's #public
			// special-casing does not run twice.
			continue
		}
		if err := applyChannelConfig(channel); err != nil {
			return err
		}
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

	return applyChannelConfig(normalized)
}

// applyChannelConfig reconciles the parser topic registry with a single
// channel config entry.
//
// The #public channel is special-cased: it must use the public key strategy,
// cannot be disabled, and its secret hex is pushed into the parsers package.
// For any other channel, a disabled entry is unregistered, while an enabled
// entry is registered with either a derived or explicit key depending on its
// KeyStrategy.
func applyChannelConfig(channel models.ChannelConfig) error {
	if channel.Name == "#public" {
		if channel.KeyStrategy != models.KeyStrategyPublic {
			return fmt.Errorf("#public must use %q strategy", models.KeyStrategyPublic)
		}
		if !channel.Enabled {
			return fmt.Errorf("#public cannot be disabled")
		}
		if err := parsers.SetPublicChannelSecretHex(channel.KeyHex); err != nil {
			return fmt.Errorf("apply #public key: %w", err)
		}
		return nil
	}

	if !channel.Enabled {
		parsers.UnregisterTopic(channel.Name)
		return nil
	}

	switch channel.KeyStrategy {
	case models.KeyStrategyDerived:
		parsers.RegisterTopic(channel.Name)
	case models.KeyStrategyExplicit:
		if _, err := parsers.RegisterTopicWithKeyHex(channel.Name, channel.KeyHex); err != nil {
			return fmt.Errorf("register explicit-key channel %q: %w", channel.Name, err)
		}
	default:
		return fmt.Errorf("unsupported key strategy %q for channel %q", channel.KeyStrategy, channel.Name)
	}

	return nil
}
