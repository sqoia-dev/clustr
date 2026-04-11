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
	ListenAddr string    `json:"listen_addr"` // default ":8080"
	ImageDir   string    `json:"image_dir"`   // default "/var/lib/clonr/images"
	DBPath     string    `json:"db_path"`     // default "/var/lib/clonr/clonr.db"
	AuthToken  string    `json:"auth_token"`  // from CLONR_AUTH_TOKEN; empty = auth disabled
	LogLevel   string    `json:"log_level"`   // debug, info, warn, error — default "info"
	PXE        PXEConfig `json:"pxe"`
}

// PXEConfig holds configuration for the built-in PXE (DHCP + TFTP) server.
type PXEConfig struct {
	// Enabled activates the PXE server on startup (CLONR_PXE_ENABLED).
	Enabled bool `json:"enabled"`
	// Interface is the network interface to bind the DHCP server to
	// (CLONR_PXE_INTERFACE). Empty means auto-detect.
	Interface string `json:"interface"`
	// IPRange is the DHCP pool as "start-end" (CLONR_PXE_RANGE).
	// Default: "10.99.0.100-10.99.0.200".
	IPRange string `json:"ip_range"`
	// ServerIP is the IP advertised as next-server in DHCP offers
	// (CLONR_PXE_SERVER_IP). Auto-detected from Interface when empty.
	ServerIP string `json:"server_ip"`
	// BootDir is where the kernel and initramfs are stored
	// (CLONR_BOOT_DIR). Default: "/var/lib/clonr/boot".
	BootDir string `json:"boot_dir"`
	// TFTPDir is where TFTP-served boot files (ipxe.efi, undionly.kpxe)
	// live (CLONR_TFTP_DIR). Default: "/var/lib/clonr/tftpboot".
	TFTPDir string `json:"tftp_dir"`
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
		PXE:        LoadPXEConfig(),
	}
}

// LoadPXEConfig populates PXEConfig from environment variables.
func LoadPXEConfig() PXEConfig {
	return PXEConfig{
		Enabled:   os.Getenv("CLONR_PXE_ENABLED") == "true",
		Interface: os.Getenv("CLONR_PXE_INTERFACE"),
		IPRange:   envOrDefault("CLONR_PXE_RANGE", "10.99.0.100-10.99.0.200"),
		ServerIP:  os.Getenv("CLONR_PXE_SERVER_IP"),
		BootDir:   envOrDefault("CLONR_BOOT_DIR", "/var/lib/clonr/boot"),
		TFTPDir:   envOrDefault("CLONR_TFTP_DIR", "/var/lib/clonr/tftpboot"),
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
