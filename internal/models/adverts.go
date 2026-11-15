package models

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// AdvertsConfig bounds how much advert history the database keeps. Both keys
// are optional; 0 disables that bound.
type AdvertsConfig struct {
	// KeepDays deletes adverts not heard for this many days; 0 keeps for ever.
	KeepDays int `yaml:"keep_days"`
	// MaxKept caps the adverts table at this many rows, deleting the oldest-
	// heard adverts first; 0 keeps all.
	MaxKept int `yaml:"max_kept"`
}

// DefaultAdvertsConfig returns the settings used when kam.yml has no adverts
// block: fresh adverts replace reheard ones anyway, so a bounded history is
// harmless and keeps kam.db from growing for ever.
func DefaultAdvertsConfig() AdvertsConfig {
	return AdvertsConfig{KeepDays: 30, MaxKept: 10000}
}

// UnmarshalYAML keeps defaults for keys the file omits.
func (a *AdvertsConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain AdvertsConfig
	p := plain(DefaultAdvertsConfig())
	if err := node.Decode(&p); err != nil {
		return err
	}
	*a = AdvertsConfig(p)
	return nil
}

// ResolveAdverts returns the advert retention settings, applying the defaults
// when the adverts block is missing. An invalid key falls back to its default
// and is reported as a problem; like mesh problems, these never stop the
// radio from ingesting.
func (cfg *KamConfig) ResolveAdverts() (AdvertsConfig, []error) {
	resolved := DefaultAdvertsConfig()
	if cfg.Adverts != nil {
		resolved = *cfg.Adverts
	}
	var problems []error
	defaults := DefaultAdvertsConfig()
	if resolved.KeepDays < 0 {
		problems = append(problems, fmt.Errorf("adverts.keep_days must be 0 or more, got %d; using default %d", resolved.KeepDays, defaults.KeepDays))
		resolved.KeepDays = defaults.KeepDays
	}
	if resolved.MaxKept < 0 {
		problems = append(problems, fmt.Errorf("adverts.max_kept must be 0 or more, got %d; using default %d", resolved.MaxKept, defaults.MaxKept))
		resolved.MaxKept = defaults.MaxKept
	}
	return resolved, problems
}
