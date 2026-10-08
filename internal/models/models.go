package models

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	KeyStrategyPublic   = "public"
	KeyStrategyDerived  = "derived"
	KeyStrategyExplicit = "explicit"
)

// KamConfig is the top-level configuration file for the application.
type KamConfig struct {
	Version  int             `yaml:"version"`
	NodeName string          `yaml:"node_name"`
	DBPath   string          `yaml:"db_path"`
	Radio    RadioConfig     `yaml:"radio"`
	Channels []ChannelConfig `yaml:"channels"`
	Echo     EchoConfig      `yaml:"echo"`
}

// EchoConfig controls the optional echo liveness service that replies to
// group-text messages mentioning the local node on monitored channels.
type EchoConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Channels []string `yaml:"channels"`
	Mention  string   `yaml:"mention,omitempty"`
}

// RadioConfig defines the default serial and RF settings.
type RadioConfig struct {
	Port            string `yaml:"port"`
	Baud            int    `yaml:"baud"`
	FrequencyHz     uint32 `yaml:"frequency_hz"`
	BandwidthHz     uint32 `yaml:"bandwidth_hz"`
	SpreadingFactor uint8  `yaml:"spreading_factor"`
	CodingRate      uint8  `yaml:"coding_rate"`
}

type ChannelConfig struct {
	Name        string `yaml:"name"`
	KeyStrategy string `yaml:"key_strategy"`
	KeyHex      string `yaml:"key_hex,omitempty"`
	Enabled     bool   `yaml:"enabled"`
}

func NormalizeChannelConfig(ch ChannelConfig) (ChannelConfig, error) {
	return normalizeChannelEntry(0, ch, make(map[string]struct{}))
}

func normalizeChannelEntry(i int, ch ChannelConfig, seen map[string]struct{}) (ChannelConfig, error) {
	name := normalizeChannelName(ch.Name)
	if name == "" || name == "#" {
		return ChannelConfig{}, fmt.Errorf("channel[%d]: invalid name %q", i, ch.Name)
	}

	if _, exists := seen[name]; exists {
		return ChannelConfig{}, fmt.Errorf("channel[%d]: duplicate normalized name %q", i, name)
	}
	seen[name] = struct{}{}

	strategy := strings.ToLower(strings.TrimSpace(ch.KeyStrategy))
	if strategy != KeyStrategyPublic && strategy != KeyStrategyDerived && strategy != KeyStrategyExplicit {
		return ChannelConfig{}, fmt.Errorf("channel[%d]: invalid key_strategy %q", i, ch.KeyStrategy)
	}

	keyHex := strings.ToLower(strings.TrimSpace(ch.KeyHex))
	switch strategy {
	case KeyStrategyPublic:
		if name != "#public" {
			return ChannelConfig{}, fmt.Errorf("channel[%d]: key_strategy public is only valid for #public", i)
		}
		if keyHex == "" {
			return ChannelConfig{}, fmt.Errorf("channel[%d]: key_hex is required for public strategy", i)
		}
		if err := validateHexLen(keyHex, 16); err != nil {
			return ChannelConfig{}, fmt.Errorf("channel[%d]: %w", i, err)
		}
	case KeyStrategyDerived:
		if keyHex != "" {
			return ChannelConfig{}, fmt.Errorf("channel[%d]: key_hex must be empty for derived strategy", i)
		}
	case KeyStrategyExplicit:
		if keyHex == "" {
			return ChannelConfig{}, fmt.Errorf("channel[%d]: key_hex is required for explicit strategy", i)
		}
		if err := validateHexLenEither(keyHex, 16, 32); err != nil {
			return ChannelConfig{}, fmt.Errorf("channel[%d]: %w", i, err)
		}
	}

	if name == "#public" && strategy != KeyStrategyPublic {
		return ChannelConfig{}, fmt.Errorf("channel[%d]: #public must use key_strategy public", i)
	}

	return ChannelConfig{
		Name:        name,
		KeyStrategy: strategy,
		KeyHex:      keyHex,
		Enabled:     ch.Enabled,
	}, nil
}

// SaveKamConfig writes cfg to path as YAML. The write is atomic: it writes
// to a temp file in the same directory and renames it over the target, so a
// crash mid-write never leaves a half-written config. The normalized in-memory
// representation is what gets persisted; original file comments are not
// preserved.
func SaveKamConfig(path string, cfg *KamConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal kam config: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kam.yml.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	tmp = nil

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp config to %q: %w", path, err)
	}
	return nil
}

// ChannelIndex returns the index of the channel with the given normalized
// name, or -1 if not present.
func (cfg *KamConfig) ChannelIndex(name string) int {
	for i, ch := range cfg.Channels {
		if ch.Name == name {
			return i
		}
	}
	return -1
}

func (cfg *KamConfig) PublicChannelKeyHex() (string, error) {
	for _, ch := range cfg.Channels {
		if ch.Name == "#public" {
			if ch.KeyStrategy != KeyStrategyPublic {
				return "", fmt.Errorf("#public must use %q strategy", KeyStrategyPublic)
			}
			if ch.KeyHex == "" {
				return "", fmt.Errorf("#public key_hex is empty")
			}
			return ch.KeyHex, nil
		}
	}
	return "", fmt.Errorf("#public channel is missing from config")
}

func LoadKamConfig(path string) (*KamConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kam config: %w", err)
	}

	var cfg KamConfig
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return nil, fmt.Errorf("parse kam config yaml: %w", err)
	}

	normalized, err := normalizeAndValidate(&cfg)
	if err != nil {
		return nil, err
	}

	return normalized, nil
}

func normalizeAndValidate(cfg *KamConfig) (*KamConfig, error) {
	if cfg.Version != 1 {
		return nil, fmt.Errorf("config version must be 1, got %d", cfg.Version)
	}
	if strings.TrimSpace(cfg.NodeName) == "" {
		return nil, fmt.Errorf("node_name is required")
	}

	if len(cfg.Channels) == 0 {
		return nil, fmt.Errorf("config must include at least one channel")
	}

	if cfg.Radio.Port == "" {
		return nil, fmt.Errorf("radio port is required")
	}
	if cfg.Radio.Baud <= 0 {
		return nil, fmt.Errorf("radio baud must be positive, got %d", cfg.Radio.Baud)
	}
	if cfg.Radio.FrequencyHz == 0 {
		return nil, fmt.Errorf("radio frequency_hz is required")
	}
	if cfg.Radio.BandwidthHz == 0 {
		return nil, fmt.Errorf("radio bandwidth_hz is required")
	}
	if cfg.Radio.SpreadingFactor == 0 {
		return nil, fmt.Errorf("radio spreading_factor is required")
	}
	if cfg.Radio.CodingRate == 0 {
		return nil, fmt.Errorf("radio coding_rate is required")
	}

	out := &KamConfig{
		Version:  cfg.Version,
		NodeName: strings.TrimSpace(cfg.NodeName),
		DBPath:   strings.TrimSpace(cfg.DBPath),
		Radio:    cfg.Radio,
		Channels: make([]ChannelConfig, 0, len(cfg.Channels)),
	}
	seen := make(map[string]struct{}, len(cfg.Channels))

	for i, ch := range cfg.Channels {
		normalized, err := normalizeChannelEntry(i, ch, seen)
		if err != nil {
			return nil, err
		}
		out.Channels = append(out.Channels, normalized)
	}

	// Normalize and validate the optional echo service config. Echo channels
	// must be a subset of the configured channels so inbound messages on
	// them can actually be decrypted.
	echo := EchoConfig{Enabled: cfg.Echo.Enabled}
	for _, ec := range cfg.Echo.Channels {
		name := normalizeChannelName(ec)
		if name == "" || name == "#" {
			return nil, fmt.Errorf("echo: invalid channel %q", ec)
		}
		if _, ok := seen[name]; !ok {
			return nil, fmt.Errorf("echo: channel %q is not in channels", name)
		}
		echo.Channels = append(echo.Channels, name)
	}
	if echo.Enabled && len(echo.Channels) == 0 {
		return nil, fmt.Errorf("echo: enabled but no channels configured")
	}
	echo.Mention = strings.TrimSpace(cfg.Echo.Mention)
	out.Echo = echo

	return out, nil
}

func normalizeChannelName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return ""
	}
	if !strings.HasPrefix(name, "#") {
		name = "#" + name
	}
	return name
}

func validateHexLen(hexValue string, byteLen int) error {
	decoded, err := hex.DecodeString(hexValue)
	if err != nil {
		return fmt.Errorf("key_hex must be valid hex: %w", err)
	}
	if len(decoded) != byteLen {
		return fmt.Errorf("key_hex must decode to %d bytes, got %d", byteLen, len(decoded))
	}
	return nil
}

func validateHexLenEither(hexValue string, byteLenA, byteLenB int) error {
	decoded, err := hex.DecodeString(hexValue)
	if err != nil {
		return fmt.Errorf("key_hex must be valid hex: %w", err)
	}
	if len(decoded) != byteLenA && len(decoded) != byteLenB {
		return fmt.Errorf("key_hex must decode to %d or %d bytes, got %d", byteLenA, byteLenB, len(decoded))
	}
	return nil
}
