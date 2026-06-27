package config

import (
	"fmt"

	"kam.bytecoder.nl/internal/kiss"
)

func BootstrapChannels(cfg *ChannelsConfig, dbPath string) error {
	db, err := OpenChannelsDB(dbPath)
	if err != nil {
		return fmt.Errorf("open channels db: %w", err)
	}
	defer db.Close()

	if err := EnsureChannelsSchema(db); err != nil {
		return fmt.Errorf("ensure channels schema: %w", err)
	}

	if err := UpsertSeedChannels(db, cfg); err != nil {
		return fmt.Errorf("upsert seed channels: %w", err)
	}

	publicChannel, err := GetChannelByName(db, "#public")
	if err != nil {
		return fmt.Errorf("load #public from db: %w", err)
	}
	if publicChannel.KeyStrategy != KeyStrategyPublic {
		return fmt.Errorf("#public in db must use %q strategy", KeyStrategyPublic)
	}
	if err := kiss.SetPublicChannelSecretHex(publicChannel.KeyHex); err != nil {
		return fmt.Errorf("apply #public key: %w", err)
	}

	kiss.ResetTopicRegistry()

	channels, err := ListEnabledNonPublicChannels(db)
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

func UpsertRuntimeChannelAndApply(dbPath string, channel ChannelConfig) error {
	normalized, err := NormalizeChannelConfig(channel)
	if err != nil {
		return fmt.Errorf("normalize runtime channel: %w", err)
	}

	db, err := OpenChannelsDB(dbPath)
	if err != nil {
		return fmt.Errorf("open channels db: %w", err)
	}
	defer db.Close()

	if err := EnsureChannelsSchema(db); err != nil {
		return fmt.Errorf("ensure channels schema: %w", err)
	}

	if err := UpsertRuntimeChannel(db, normalized); err != nil {
		return fmt.Errorf("upsert runtime channel %q: %w", normalized.Name, err)
	}

	record, err := GetChannelByName(db, normalized.Name)
	if err != nil {
		return fmt.Errorf("load upserted channel %q: %w", normalized.Name, err)
	}

	if err := applyChannelRecord(*record); err != nil {
		return err
	}

	return nil
}

func applyChannelRecord(channel ChannelRecord) error {
	if channel.Name == "#public" {
		if channel.KeyStrategy != KeyStrategyPublic {
			return fmt.Errorf("#public must use %q strategy", KeyStrategyPublic)
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
	case KeyStrategyDerived:
		kiss.RegisterTopic(channel.Name)
	case KeyStrategyExplicit:
		if _, err := kiss.RegisterTopicWithKeyHex(channel.Name, channel.KeyHex); err != nil {
			return fmt.Errorf("register explicit-key channel %q: %w", channel.Name, err)
		}
	default:
		return fmt.Errorf("unsupported key strategy %q for channel %q", channel.KeyStrategy, channel.Name)
	}

	return nil
}
