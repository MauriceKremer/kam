package config

import (
	"fmt"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/models"
)

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
	if err := kiss.SetPublicChannelSecretHex(publicChannel.KeyHex); err != nil {
		return fmt.Errorf("apply #public key: %w", err)
	}

	kiss.ResetTopicRegistry()

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

func applyChannelRecord(channel db.ChannelRecord) error {
	if channel.Name == "#public" {
		if channel.KeyStrategy != models.KeyStrategyPublic {
			return fmt.Errorf("#public must use %q strategy", models.KeyStrategyPublic)
		}
		if !channel.Enabled {
			return fmt.Errorf("#public cannot be disabled")
		}
		if err := kiss.SetPublicChannelSecretHex(channel.KeyHex); err != nil {
			return fmt.Errorf("apply #public key: %w", err)
		}
		return nil
	}

	if !channel.Enabled {
		kiss.UnregisterTopic(channel.Name)
		return nil
	}

	switch channel.KeyStrategy {
	case models.KeyStrategyDerived:
		kiss.RegisterTopic(channel.Name)
	case models.KeyStrategyExplicit:
		if _, err := kiss.RegisterTopicWithKeyHex(channel.Name, channel.KeyHex); err != nil {
			return fmt.Errorf("register explicit-key channel %q: %w", channel.Name, err)
		}
	default:
		return fmt.Errorf("unsupported key strategy %q for channel %q", channel.KeyStrategy, channel.Name)
	}

	return nil
}
