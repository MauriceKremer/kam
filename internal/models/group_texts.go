package models

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// GroupTextRetention is one pair of retention bounds for group-text history.
// Both keys are optional; 0 disables that bound.
type GroupTextRetention struct {
	// MaxPosts caps each channel's stored group texts at this many,
	// deleting the oldest first; 0 keeps all.
	MaxPosts int `yaml:"max_posts"`
	// MaxAgeDays deletes group texts older than this many days; 0 keeps
	// them for ever.
	MaxAgeDays int `yaml:"max_age_days"`
}

// GroupTextsConfig bounds how much group-text history kam.db keeps, per
// channel. The top-level bounds apply to every channel; entries under
// channels override them for one name — also for names no longer in
// channels:, because their old rows live on in the table.
type GroupTextsConfig struct {
	GroupTextRetention `yaml:",inline"`
	Channels           map[string]GroupTextRetention `yaml:"channels,omitempty"`
}

// DefaultGroupTextsConfig returns the settings used when kam.yml has no
// group_texts block: mirrors the room defaults and keeps kam.db from growing
// for ever on a busy channel.
func DefaultGroupTextsConfig() GroupTextsConfig {
	return GroupTextsConfig{GroupTextRetention: GroupTextRetention{MaxPosts: 1000, MaxAgeDays: 30}}
}

// UnmarshalYAML keeps defaults for keys the file omits.
func (g *GroupTextsConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain GroupTextsConfig
	p := plain(DefaultGroupTextsConfig())
	if err := node.Decode(&p); err != nil {
		return err
	}
	*g = GroupTextsConfig(p)
	return nil
}

// ResolveGroupTexts returns the group-text retention settings, applying the
// defaults when the group_texts block is missing. An invalid value falls back
// to its default and is reported as a problem; these never stop the radio
// from ingesting.
func (cfg *KamConfig) ResolveGroupTexts() (GroupTextsConfig, []error) {
	resolved := DefaultGroupTextsConfig()
	if cfg.GroupTexts != nil {
		resolved = *cfg.GroupTexts
	}
	var problems []error
	defaults := DefaultGroupTextsConfig()
	if resolved.MaxPosts < 0 {
		problems = append(problems, fmt.Errorf("group_texts.max_posts must be 0 or more, got %d; using default %d", resolved.MaxPosts, defaults.MaxPosts))
		resolved.MaxPosts = defaults.MaxPosts
	}
	if resolved.MaxAgeDays < 0 {
		problems = append(problems, fmt.Errorf("group_texts.max_age_days must be 0 or more, got %d; using default %d", resolved.MaxAgeDays, defaults.MaxAgeDays))
		resolved.MaxAgeDays = defaults.MaxAgeDays
	}

	normalized := make(map[string]GroupTextRetention, len(resolved.Channels))
	// Sorted iteration keeps the outcome of name collisions deterministic.
	names := make([]string, 0, len(resolved.Channels))
	for name := range resolved.Channels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		limits := resolved.Channels[name]
		key := normalizeChannelName(name)
		if key == "" {
			problems = append(problems, fmt.Errorf("group_texts.channels[%q]: not a channel name; entry dropped", name))
			continue
		}
		if _, exists := normalized[key]; exists {
			problems = append(problems, fmt.Errorf("group_texts.channels[%q] and [%q]: same channel; keeping the first", key, name))
			continue
		}
		if limits.MaxPosts < 0 {
			problems = append(problems, fmt.Errorf("group_texts.channels[%q].max_posts must be 0 or more, got %d; using default %d", name, limits.MaxPosts, resolved.MaxPosts))
			limits.MaxPosts = resolved.MaxPosts
		}
		if limits.MaxAgeDays < 0 {
			problems = append(problems, fmt.Errorf("group_texts.channels[%q].max_age_days must be 0 or more, got %d; using default %d", name, limits.MaxAgeDays, resolved.MaxAgeDays))
			limits.MaxAgeDays = resolved.MaxAgeDays
		}
		normalized[key] = limits
	}
	if resolved.Channels != nil {
		resolved.Channels = normalized
	}
	return resolved, problems
}
