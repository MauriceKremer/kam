package config

import "kam.bytecoder.nl/internal/models"

// LoadKamConfig is a thin wrapper around models.LoadKamConfig.
func LoadKamConfig(path string) (*models.KamConfig, error) {
	return models.LoadKamConfig(path)
}
