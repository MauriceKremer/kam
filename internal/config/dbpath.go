package config

import "kam.bytecoder.nl/internal/models"

// DefaultDBPath is the SQLite database path used when neither the -db flag
// nor kam.yml specifies one.
const DefaultDBPath = "kam.db"

// ResolveDBPath picks the SQLite database path with precedence:
//
//	flagValue (-db flag)  >  cfg.DBPath (kam.yml)  >  DefaultDBPath
func ResolveDBPath(flagValue string, cfg *models.KamConfig) string {
	switch {
	case flagValue != "":
		return flagValue
	case cfg.DBPath != "":
		return cfg.DBPath
	default:
		return DefaultDBPath
	}
}
