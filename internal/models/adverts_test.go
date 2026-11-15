package models

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveAdverts(t *testing.T) {
	// No adverts block: defaults apply with no problems.
	got, problems := (&KamConfig{}).ResolveAdverts()
	if got.KeepDays != 30 || got.MaxKept != 10000 || len(problems) != 0 {
		t.Fatalf("defaults = %+v, problems = %v", got, problems)
	}

	// Explicit values are kept verbatim, including 0 (= disabled).
	got, problems = (&KamConfig{Adverts: &AdvertsConfig{KeepDays: 7, MaxKept: 0}}).ResolveAdverts()
	if got.KeepDays != 7 || got.MaxKept != 0 || len(problems) != 0 {
		t.Fatalf("explicit = %+v, problems = %v", got, problems)
	}

	// Invalid values fall back to the default and are reported.
	got, problems = (&KamConfig{Adverts: &AdvertsConfig{KeepDays: -1, MaxKept: 5}}).ResolveAdverts()
	if got.KeepDays != 30 || got.MaxKept != 5 || len(problems) != 1 {
		t.Fatalf("invalid = %+v, problems = %v", got, problems)
	}
}

func TestAdvertsConfigYAMLSeedsDefaults(t *testing.T) {
	var cfg KamConfig
	if err := yaml.Unmarshal([]byte("adverts:\n  max_kept: 5\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Adverts == nil {
		t.Fatal("adverts block missing after decode")
	}
	if cfg.Adverts.KeepDays != 30 || cfg.Adverts.MaxKept != 5 {
		t.Fatalf("decoded = %+v, want keep_days default 30", cfg.Adverts)
	}
}
