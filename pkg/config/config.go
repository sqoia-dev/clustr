// Package config manages clonr runtime configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config holds the full runtime configuration for clonr components.
type Config struct {
	Server   ServerConfig `json:"server"`
	Storage  StorageConfig `json:"storage"`
	Log      LogConfig    `json:"log"`
}

// ServerConfig controls the clonr-serverd HTTP listener.
type ServerConfig struct {
	Addr string `json:"addr"` // default ":8080"
}

// StorageConfig controls where images and the metadata database are stored.
type StorageConfig struct {
	ImageDir string `json:"image_dir"` // default /var/lib/clonr/images
	DBPath   string `json:"db_path"`   // default /var/lib/clonr/clonr.db
}

// LogConfig controls structured logging output.
type LogConfig struct {
	Level  string `json:"level"`  // debug, info, warn, error
	Format string `json:"format"` // json, console
}

// Default returns a Config with sensible production defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Addr: ":8080",
		},
		Storage: StorageConfig{
			ImageDir: "/var/lib/clonr/images",
			DBPath:   "/var/lib/clonr/clonr.db",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

// Load reads a JSON config file at path. Missing fields fall back to defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}
