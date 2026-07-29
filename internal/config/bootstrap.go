package config

import (
	"fmt"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/models"
)

// BootstrapChannels initializes the channel database and loads all configured
// channels into the parser topic registry at startup.
//
// It opens (or creates) the channel DB, ensures its schema, upserts the seed
// channels from cfg, then verifies and applies the #public channel before
// registering every other enabled channel via applyChannelRecord.
func BootstrapChannels(cfg *models.KamConfig, dbPath string) error {
	d, err := db.OpenKamDB(dbPath)
	if err != nil {
		return fmt.Errorf("open channels db: %w", err)
	}
	defer d.Close()

	if err := db.EnsureChannelsSchema(d); err != nil {
		return fmt.Errorf("ensure channels schema: %w", err)
	}

	if err := db.UpsertSeedChannels(d, cfg); err != nil {
		return fmt.Errorf("upsert seed channels: %w", err)
	}

	publicChannel, err := db.GetChannelByName(d, "#public")
	if err != nil {
		return fmt.Errorf("load #public from db: %w", err)
	}
	if publicChannel.KeyStrategy != models.KeyStrategyPublic {
		return fmt.Errorf("#public in db must use %q strategy", models.KeyStrategyPublic)
	}
	if err := parsers.SetPublicChannelSecretHex(publicChannel.KeyHex); err != nil {
		return fmt.Errorf("apply #public key: %w", err)
	}

	parsers.ResetTopicRegistry()

	channels, err := db.ListEnabledNonPublicChannels(d)
	if err != nil {
		return fmt.Errorf("load enabled channels from db: %w", err)
	}

	for _, channel := range channels {
		if err := applyChannelRecord(channel); err != nil {
			return err
		}
	}

	return nil
}

// UpsertRuntimeChannelAndApply persists a runtime-provided channel to the DB
// and immediately applies it to the parser topic registry, so the new or
// updated channel takes effect without a full restart. The channel is
// normalized before persistence and reloaded from the DB after upsert so the
// applied record matches what is stored.
func UpsertRuntimeChannelAndApply(dbPath string, channel models.ChannelConfig) error {
	normalized, err := models.NormalizeChannelConfig(channel)
	if err != nil {
		return fmt.Errorf("normalize runtime channel: %w", err)
	}

	d, err := db.OpenKamDB(dbPath)
	if err != nil {
		return fmt.Errorf("open channels db: %w", err)
	}
	defer d.Close()

	if err := db.EnsureChannelsSchema(d); err != nil {
		return fmt.Errorf("ensure channels schema: %w", err)
	}

	if err := db.UpsertRuntimeChannel(d, normalized); err != nil {
		return fmt.Errorf("upsert runtime channel %q: %w", normalized.Name, err)
	}

	record, err := db.GetChannelByName(d, normalized.Name)
	if err != nil {
		return fmt.Errorf("load upserted channel %q: %w", normalized.Name, err)
	}

	if err := applyChannelRecord(*record); err != nil {
		return err
	}

	return nil
}

// applyChannelRecord reconciles the parser topic registry with a single
// channel DB record.
//
// The #public channel is special-cased: it must use the public key strategy,
// cannot be disabled, and its secret hex is pushed into the parsers package.
// For any other channel, a disabled record is unregistered, while an enabled
// record is registered with either a derived or explicit key depending on its
// KeyStrategy.
func applyChannelRecord(channel db.ChannelRecord) error {
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
