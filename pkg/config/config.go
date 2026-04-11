// Package config manages clonr runtime configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// ServerConfig holds all runtime configuration for clonr-serverd.
// Values can be loaded from a JSON file or from environment variables.
type ServerConfig struct {
	ListenAddr string `json:"listen_addr"` // default ":8080"
	ImageDir   string `json:"image_dir"`   // default "/var/lib/clonr/images"
	DBPath     string `json:"db_path"`     // default "/var/lib/clonr/clonr.db"
	AuthToken  string `json:"auth_token"`  // from CLONR_AUTH_TOKEN; empty = auth disabled
	LogLevel   string `json:"log_level"`   // debug, info, warn, error — default "info"
}

// Config holds the full runtime configuration for clonr components.
// Kept for JSON-file based loading compatibility.
type Config struct {
	Server ServerConfig `json:"server"`
}

// LoadServerConfig populates ServerConfig from environment variables with
// sensible production defaults. Environment variables take precedence over defaults.
func LoadServerConfig() ServerConfig {
	return ServerConfig{
		ListenAddr: envOrDefault("CLONR_LISTEN_ADDR", ":8080"),
		ImageDir:   envOrDefault("CLONR_IMAGE_DIR", "/var/lib/clonr/images"),
		DBPath:     envOrDefault("CLONR_DB_PATH", "/var/lib/clonr/clonr.db"),
		AuthToken:  os.Getenv("CLONR_AUTH_TOKEN"),
		LogLevel:   envOrDefault("CLONR_LOG_LEVEL", "info"),
	}
}

// Default returns a Config with sensible production defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			ListenAddr: ":8080",
			ImageDir:   "/var/lib/clonr/images",
			DBPath:     "/var/lib/clonr/clonr.db",
			LogLevel:   "info",
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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
