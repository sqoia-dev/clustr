// Package config manages clonr runtime configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ServerConfig holds all runtime configuration for clonr-serverd.
// Values can be loaded from a JSON file or from environment variables.
type ServerConfig struct {
	ListenAddr      string        `json:"listen_addr"`      // default ":8080"
	ImageDir        string        `json:"image_dir"`        // default "/var/lib/clonr/images"
	DBPath          string        `json:"db_path"`          // default "/var/lib/clonr/clonr.db"
	AuthToken       string        `json:"auth_token"`       // legacy: from CLONR_AUTH_TOKEN; superseded by api_keys table
	AuthDevMode     bool          `json:"auth_dev_mode"`    // from CLONR_AUTH_DEV_MODE=1; bypasses auth for local dev ONLY
	SessionSecret   string        `json:"session_secret"`   // CLONR_SESSION_SECRET: HMAC key for browser session tokens (32+ bytes)
	SessionSecure   bool          `json:"session_secure"`   // CLONR_SESSION_SECURE=1: set Secure flag on session cookie (requires TLS)
	LogLevel        string        `json:"log_level"`        // debug, info, warn, error — default "info"
	LogRetention    time.Duration `json:"log_retention"`    // from CLONR_LOG_RETENTION; default 14d
	ClonrBinPath    string        `json:"clonr_bin_path"`   // CLONR_BIN_PATH: abs path to clonr CLI binary baked into initramfs; default /usr/local/bin/clonr
	// VerifyTimeout is the duration after deploy_completed_preboot_at within which
	// the deployed OS must phone home via POST /verify-boot. ADR-0008.
	// From CLONR_VERIFY_TIMEOUT (Go duration string, e.g. "5m"). Default: 5 minutes.
	VerifyTimeout   time.Duration `json:"verify_timeout"`   // CLONR_VERIFY_TIMEOUT; default 5m
	PXE             PXEConfig     `json:"pxe"`
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
	// HTTPPort is the port the clonr-serverd HTTP API listens on, used by the
	// DHCP server when building the iPXE chainload URL. Populated at runtime
	// from ListenAddr — not a user-facing config field.
	HTTPPort string `json:"-"`
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
		ListenAddr:    envOrDefault("CLONR_LISTEN_ADDR", ":8080"),
		ImageDir:      envOrDefault("CLONR_IMAGE_DIR", "/var/lib/clonr/images"),
		DBPath:        envOrDefault("CLONR_DB_PATH", "/var/lib/clonr/clonr.db"),
		AuthToken:     os.Getenv("CLONR_AUTH_TOKEN"), // legacy, no longer used for auth enforcement
		AuthDevMode:   os.Getenv("CLONR_AUTH_DEV_MODE") == "1",
		SessionSecret: os.Getenv("CLONR_SESSION_SECRET"),
		SessionSecure: os.Getenv("CLONR_SESSION_SECURE") == "1",
		LogLevel:      envOrDefault("CLONR_LOG_LEVEL", "info"),
		LogRetention:  parseLogRetention(),
		ClonrBinPath:  envOrDefault("CLONR_BIN_PATH", "/usr/local/bin/clonr"),
		VerifyTimeout: parseVerifyTimeout(),
		PXE:           LoadPXEConfig(),
	}
}

// parseVerifyTimeout parses CLONR_VERIFY_TIMEOUT as a Go duration string.
// Minimum: 2m (to allow slow hardware POST sequences). Maximum: 30m.
// Falls back to 5m on parse error or when the env var is not set.
// ADR-0008.
func parseVerifyTimeout() time.Duration {
	const defaultTimeout = 5 * time.Minute
	const minTimeout = 2 * time.Minute
	const maxTimeout = 30 * time.Minute

	v := os.Getenv("CLONR_VERIFY_TIMEOUT")
	if v == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultTimeout
	}
	if d < minTimeout {
		return minTimeout
	}
	if d > maxTimeout {
		return maxTimeout
	}
	return d
}

// parseLogRetention parses CLONR_LOG_RETENTION as a Go duration string.
// Falls back to 0 (meaning "use the server default") on parse error or
// when the env var is not set. The server's runLogPurger treats 0 as 14d.
func parseLogRetention() time.Duration {
	v := os.Getenv("CLONR_LOG_RETENTION")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
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
