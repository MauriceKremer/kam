package models

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveGroupTextsDefaults(t *testing.T) {
	// No block reads as the defaults.
	resolved, problems := (&KamConfig{}).ResolveGroupTexts()
	if resolved.MaxPosts != 1000 || resolved.MaxAgeDays != 30 || resolved.Channels != nil {
		t.Fatalf("missing block: %+v, %v", resolved, problems)
	}
	if len(problems) != 0 {
		t.Fatalf("missing block reported problems: %v", problems)
	}

	// An explicit block keeps defaults for omitted keys — the same guarantee
	// the YAML unmarshaler makes for a half-written block.
	var fromYaml KamConfig
	if err := yaml.Unmarshal([]byte("group_texts:\n  channels:\n    '#x': {max_posts: 5}\n"), &fromYaml); err != nil {
		t.Fatal(err)
	}
	// An override is a complete replacement pair: omitted keys are 0, i.e.
	// unlimited, not inherited
	resolved, problems = fromYaml.ResolveGroupTexts()
	if resolved.MaxPosts != 1000 || resolved.MaxAgeDays != 30 || len(problems) != 0 {
		t.Fatalf("half-written yaml block: %+v, %v", resolved, problems)
	}
	if x := resolved.Channels["#x"]; x.MaxPosts != 5 || x.MaxAgeDays != 0 {
		t.Fatalf("per-channel override after yaml decode: %+v", resolved.Channels)
	}

	// Both bounds together, on defaults and per channel.
	resolved, problems = (&KamConfig{GroupTexts: &GroupTextsConfig{
		GroupTextRetention: GroupTextRetention{MaxPosts: 10, MaxAgeDays: 3},
		Channels:           map[string]GroupTextRetention{"Kam Test": {MaxPosts: 1, MaxAgeDays: 1}},
	}}).ResolveGroupTexts()
	if len(problems) != 0 || resolved.MaxPosts != 10 || resolved.MaxAgeDays != 3 {
		t.Fatalf("values: %+v, %v", resolved, problems)
	}
	override, ok := resolved.Channels["#kam test"]
	if !ok || override.MaxPosts != 1 || override.MaxAgeDays != 1 {
		t.Fatalf("override keys: %+v", resolved.Channels)
	}
}

func TestResolveGroupTextsProblemsFallBackToDefaults(t *testing.T) {
	resolved, problems := (&KamConfig{GroupTexts: &GroupTextsConfig{
		GroupTextRetention: GroupTextRetention{MaxPosts: -1, MaxAgeDays: -5},
		Channels: map[string]GroupTextRetention{
			"#neg":  {MaxPosts: -2, MaxAgeDays: -3},
			"   ":   {},
			"dup a": {MaxPosts: 0, MaxAgeDays: 0},
			"dup A": {MaxPosts: 1, MaxAgeDays: 1},
		},
	}}).ResolveGroupTexts()
	if resolved.MaxPosts != 1000 || resolved.MaxAgeDays != 30 {
		t.Fatalf("negative bounds did not fall back: %+v", resolved)
	}
	type limits struct {
		maxPosts, maxAgeDays int
	}
	want := map[string]limits{
		"#neg": {1000, 30},
		// Sorted iteration keeps the collision deterministic: "dup A" sorts
		// before "dup a", so its entry wins.
		"#dup a": {1, 1},
	}
	if len(resolved.Channels) != len(want) {
		t.Fatalf("normalized overrides: %+v", resolved.Channels)
	}
	for name, expect := range want {
		got, ok := resolved.Channels[name]
		if !ok || got.MaxPosts != expect.maxPosts || got.MaxAgeDays != expect.maxAgeDays {
			t.Fatalf("%q override: %+v, want %+v", name, got, expect)
		}
	}
	// Six problems: two negative defaults, two negative override fields, one
	// dropped empty name, one collision between the case duplicates.
	if len(problems) != 6 {
		t.Fatalf("problems = %d (%v)", len(problems), problems)
	}
}
